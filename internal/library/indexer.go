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
	for _, r := range roots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := ix.scanRoot(ctx, r)
		total += n
		if err != nil {
			ix.log.Warn("scan root failed", "root", r.Name, "path", r.Path, "err", err)
		}
	}
	if count, err := ix.store.Count(); err == nil {
		ix.log.Info("full scan complete", "roots", len(roots), "visited", total, "library_size", count, "took", time.Since(start).Round(time.Millisecond).String())
	}
	return nil
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

	walkErr := filepath.WalkDir(r.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep going
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
			_ = ix.store.Put(store.Item{
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
		_ = ix.store.Put(store.Item{
			Path: path, Parent: filepath.Dir(path), Name: name, RootName: r.Name,
			IsDir: false, Class: class, Mime: mime, Size: info.Size(),
			MTime: info.ModTime().Unix(), DateAdded: now, SeenGen: gen,
		})
		count++
		return nil
	})

	deleted, err := ix.store.DeleteStale(r.Name, gen)
	if err != nil {
		ix.log.Warn("prune stale failed", "root", r.Name, "err", err)
	}
	ix.log.Info("scanned root", "root", r.Name, "indexed", count, "pruned", deleted)
	return count, walkErr
}
