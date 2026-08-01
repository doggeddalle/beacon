// Package library bridges the SQLite store to the UPnP content model: it scans
// the configured media folders into the database (Indexer) and serves browse
// requests from it (Backend). This is the DB-backed replacement for the
// Phase-1 live-filesystem backend.
package library

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"beacon/internal/content"
	"beacon/internal/store"
)

// scanBatchSize is how many rows a scan accumulates before committing. Large
// enough to amortise transaction overhead, small enough that the buffer stays
// trivial next to the rest of the process.
const scanBatchSize = 500

// Root is one configured, top-level media folder.
type Root struct {
	Name string
	Path string // absolute
}

// Indexer walks the configured roots and keeps the store in sync.
type Indexer struct {
	store *store.Store
	log   *slog.Logger

	mu    sync.Mutex
	roots []Root

	// onVisit, when non-nil, is called for each entry the walk reaches. Tests use
	// it to interrupt a scan at a deterministic point; it is nil in production.
	onVisit func(path string)
}

// NewIndexer creates an indexer over the given store and roots.
func NewIndexer(st *store.Store, roots []Root, log *slog.Logger) *Indexer {
	return &Indexer{store: st, log: log, roots: absRoots(roots)}
}

// SetRoots replaces the indexer's roots (used when folders change at runtime).
func (ix *Indexer) SetRoots(roots []Root) {
	ix.mu.Lock()
	ix.roots = absRoots(roots)
	ix.mu.Unlock()
}

func (ix *Indexer) currentRoots() []Root {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]Root, len(ix.roots))
	copy(out, ix.roots)
	return out
}

// absRoots returns a copy of roots with absolute paths.
func absRoots(roots []Root) []Root {
	abs := make([]Root, 0, len(roots))
	for _, r := range roots {
		if p, err := filepath.Abs(r.Path); err == nil {
			r.Path = p
		}
		abs = append(abs, r)
	}
	return abs
}

// FullScan indexes every configured root. It is non-destructive: existing rows
// are updated in place and only files that have genuinely disappeared are
// pruned (via the per-scan generation marker). Safe to run repeatedly.
func (ix *Indexer) FullScan(ctx context.Context) error {
	start := time.Now()
	roots := ix.currentRoots()
	total := 0
	var firstErr error
	for _, r := range roots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := ix.scanRoot(ctx, r)
		total += n
		if err != nil {
			ix.log.Warn("scan root failed", "root", r.Name, "path", r.Path, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if count, err := ix.store.Count(); err == nil {
		ix.log.Info("full scan complete", "roots", len(roots), "visited", total, "library_size", count, "took", time.Since(start).Round(time.Millisecond).String())
	}
	return firstErr
}

// scanRoot walks a single root, upserting every directory and media file with
// the current scan generation, then prunes rows not seen this pass.
func (ix *Indexer) scanRoot(ctx context.Context, r Root) (int, error) {
	fi, err := os.Stat(r.Path)
	if err != nil || !fi.IsDir() {
		ix.log.Warn("skipping root — not a directory", "root", r.Name, "path", r.Path)
		return 0, nil
	}

	gen := time.Now().UnixNano()
	now := time.Now().Unix()
	count := 0
	// Rows are committed in batches rather than one autocommit each; a 50k-file
	// library meant 50k separate transactions on a cold start.
	batch := make([]store.Item, 0, scanBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := ix.store.PutBatch(batch); err != nil {
			ix.log.Warn("index batch write failed", "root", r.Name, "items", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	add := func(it store.Item) {
		batch = append(batch, it)
		if len(batch) >= scanBatchSize {
			flush()
		}
	}
	// unreadableDirs counts directories we could not descend into. Their contents
	// were never visited, so their rows still carry the previous generation — if
	// we pruned now we would delete a live subtree.
	unreadableDirs := 0

	walkErr := filepath.WalkDir(r.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read hides an unknown number of live files;
			// a file that errors has usually just vanished, which is exactly what
			// pruning is for. Only the former makes the prune unsafe.
			if d != nil && d.IsDir() {
				unreadableDirs++
			}
			return nil // skip it, keep going
		}
		if ix.onVisit != nil {
			ix.onVisit(path)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()

		// Skip hidden files/dirs (but never the root itself).
		if path != r.Path && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		if d.IsDir() {
			parent := store.RootParent
			itemName := r.Name
			if path != r.Path {
				parent = filepath.Dir(path)
				itemName = name
			}
			add(store.Item{
				Path: path, Parent: parent, Name: itemName, RootName: r.Name,
				IsDir: true, MTime: info.ModTime().Unix(), DateAdded: now, SeenGen: gen,
			})
			count++
			return nil
		}

		class, mime, ok := content.Classify(name)
		if !ok {
			return nil // not a media file
		}
		add(store.Item{
			Path: path, Parent: filepath.Dir(path), Name: name, RootName: r.Name,
			IsDir: false, Class: class, Mime: mime, Size: info.Size(),
			MTime: info.ModTime().Unix(), DateAdded: now, SeenGen: gen,
		})
		count++
		return nil
	})
	// Commit whatever the walk left in the batch before deciding on the prune —
	// an unflushed tail would otherwise look like rows that were never seen.
	flush()

	// Pruning deletes every row this pass did not touch, so it is only safe when
	// the walk actually completed. An aborted walk (shutdown mid-scan, a network
	// mount going away) or an unreadable directory means "not seen" no longer
	// implies "gone" — skipping the prune leaves a few stale rows, which the next
	// clean scan removes. Pruning anyway would wipe the library.
	switch {
	case walkErr != nil:
		ix.log.Warn("scan incomplete — skipping prune to avoid deleting live entries",
			"root", r.Name, "indexed", count, "err", walkErr)
	case ctx.Err() != nil:
		ix.log.Warn("scan cancelled — skipping prune to avoid deleting live entries",
			"root", r.Name, "indexed", count)
	case unreadableDirs > 0:
		ix.log.Warn("some directories were unreadable — skipping prune to avoid deleting live entries",
			"root", r.Name, "indexed", count, "unreadable_dirs", unreadableDirs)
	default:
		deleted, err := ix.store.DeleteStale(r.Name, gen)
		if err != nil {
			ix.log.Warn("prune stale failed", "root", r.Name, "err", err)
		}
		ix.log.Info("scanned root", "root", r.Name, "indexed", count, "pruned", deleted)
	}
	return count, walkErr
}
