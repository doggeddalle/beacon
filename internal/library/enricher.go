package library

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"beacon/internal/meta"
	"beacon/internal/store"
	"beacon/internal/thumbs"
)

// Enricher fills in metadata (duration/resolution via ffprobe) and detects
// sidecar subtitles for indexed media, using a bounded worker pool so it never
// overwhelms the NAS. It processes items the indexer/watcher marked unprobed,
// then sleeps until kicked or a poll interval elapses.
type Enricher struct {
	store    *store.Store
	prober   *meta.Prober
	workers  int
	log      *slog.Logger
	onUpdate func()

	kick chan struct{}
}

const (
	enrichBatch = 64
	enrichPoll  = 5 * time.Second
	// Backoff bounds for the case where work is pending but every write fails
	// (DB locked, disk full, read-only mount). Without this the loop spins at
	// 100% CPU re-reading the same batch forever.
	enrichBackoffMin = 1 * time.Second
	enrichBackoffMax = 5 * time.Minute
)

// NewEnricher creates an enricher. onUpdate is called after a batch changes
// anything (to bump the ContentDirectory update ID); it may be nil.
func NewEnricher(st *store.Store, prober *meta.Prober, workers int, log *slog.Logger, onUpdate func()) *Enricher {
	if workers < 1 {
		workers = 1
	}
	if onUpdate == nil {
		onUpdate = func() {}
	}
	return &Enricher{
		store: st, prober: prober, workers: workers, log: log, onUpdate: onUpdate,
		kick: make(chan struct{}, 1),
	}
}

// Kick asks the enricher to process pending items promptly (non-blocking).
func (e *Enricher) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run processes pending items until ctx is cancelled.
func (e *Enricher) Run(ctx context.Context) {
	if e.prober.Available() {
		e.log.Info("metadata enricher active", "ffprobe", e.prober.Path(), "workers", e.workers)
	} else {
		e.log.Warn("ffprobe not found — durations/resolutions will be blank; " +
			"sidecar subtitles still work. Set [meta] ffprobe_path or place ffprobe next to the beacon binary")
	}

	backoff := enrichBackoffMin
	for {
		done, pending := e.ProcessBatch(ctx)
		if ctx.Err() != nil {
			return
		}
		if done > 0 {
			e.onUpdate()
			backoff = enrichBackoffMin
			continue // drain remaining work before sleeping
		}

		// Nothing succeeded. If items were pending, every write failed — back off
		// so a persistent error degrades to a slow retry instead of a busy loop.
		wait := enrichPoll
		if pending > 0 {
			wait = backoff
			e.log.Warn("enrichment made no progress — backing off",
				"pending", pending, "retry_in", backoff.String())
			if backoff *= 2; backoff > enrichBackoffMax {
				backoff = enrichBackoffMax
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
			backoff = enrichBackoffMin
		case <-time.After(wait):
		}
	}
}

// ProcessBatch enriches one batch of unprobed items. It returns how many were
// successfully recorded and how many were pending, so the caller can tell "no
// work left" apart from "all the work failed".
func (e *Enricher) ProcessBatch(ctx context.Context) (done, pending int) {
	items, err := e.store.ItemsNeedingMetadata(enrichBatch)
	if err != nil {
		e.log.Warn("enricher query failed", "err", err)
		return 0, 0
	}
	if len(items) == 0 {
		return 0, 0
	}

	var ok atomic.Int64
	sem := make(chan struct{}, e.workers)
	var wg sync.WaitGroup
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it store.Item) {
			defer wg.Done()
			defer func() { <-sem }()
			if e.enrich(ctx, it) {
				ok.Add(1)
			}
		}(it)
	}
	wg.Wait()
	return int(ok.Load()), len(items)
}

// enrich probes one item and records its metadata + subtitle. It reports whether
// the row was updated.
//
// A failed probe is recorded as ProbeFailed rather than ProbeDone: `store.Put`
// only re-queues an item when its size or mtime changes, so marking a failure as
// done would blank the item's duration permanently — most visibly for anyone who
// installs ffmpeg *after* first run.
func (e *Enricher) enrich(ctx context.Context, it store.Item) bool {
	var duration, resolution string
	state := store.ProbeDone
	if e.prober.Available() {
		if info, err := e.prober.Probe(ctx, it.Path); err == nil {
			duration = info.DurationHMS()
			resolution = info.Resolution()
		} else if ctx.Err() == nil {
			e.log.Debug("probe failed", "path", it.Path, "err", err)
			state = store.ProbeFailed
		} else {
			return false // shutting down; leave the item pending
		}
	} else {
		// No ffprobe at all. Sidecar subtitles below still work, but the metadata
		// is genuinely missing — queue it for retry once ffprobe shows up.
		state = store.ProbeFailed
	}
	subPath, _ := meta.FindSubtitle(it.Path)
	// Resolved once here rather than on every browse, where it cost an os.ReadDir
	// per item.
	artPath, _ := thumbs.FindPoster(it.Path)

	if err := e.store.UpdateMetadata(it.Path, store.Metadata{
		Duration:   duration,
		Resolution: resolution,
		SubPath:    subPath,
		ArtPath:    artPath,
		State:      state,
	}); err != nil {
		e.log.Warn("update metadata failed", "path", it.Path, "err", err)
		return false
	}
	if subPath != "" {
		e.log.Debug("found subtitle", "media", it.Path, "subtitle", subPath)
	}
	return true
}
