package library

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"beacon/internal/meta"
	"beacon/internal/store"
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
	enrichBatch  = 64
	enrichPoll   = 5 * time.Second
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

	for {
		n := e.processBatch(ctx)
		if ctx.Err() != nil {
			return
		}
		if n > 0 {
			e.onUpdate()
			continue // drain remaining work before sleeping
		}
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
		case <-time.After(enrichPoll):
		}
	}
}

// processBatch enriches one batch of unprobed items and returns how many were
// processed.
func (e *Enricher) processBatch(ctx context.Context) int {
	items, err := e.store.ItemsNeedingMetadata(enrichBatch)
	if err != nil {
		e.log.Warn("enricher query failed", "err", err)
		return 0
	}
	if len(items) == 0 {
		return 0
	}

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
			e.enrich(ctx, it)
		}(it)
	}
	wg.Wait()
	return len(items)
}

// enrich probes one item and records its metadata + subtitle.
func (e *Enricher) enrich(ctx context.Context, it store.Item) {
	var duration, resolution string
	if e.prober.Available() {
		if info, err := e.prober.Probe(ctx, it.Path); err == nil {
			duration = info.DurationHMS()
			resolution = info.Resolution()
		} else if ctx.Err() == nil {
			e.log.Debug("probe failed", "path", it.Path, "err", err)
		}
	}
	subPath, _ := meta.FindSubtitle(it.Path)

	if err := e.store.UpdateMetadata(it.Path, duration, resolution, subPath); err != nil {
		e.log.Warn("update metadata failed", "path", it.Path, "err", err)
		return
	}
	if subPath != "" {
		e.log.Debug("found subtitle", "media", it.Path, "subtitle", subPath)
	}
}
