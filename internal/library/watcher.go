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

	"github.com/fsnotify/fsnotify"

	"beacon/internal/content"
	"beacon/internal/store"
)

// Watcher is Tier 1 of the auto-update engine: it watches the media folders in
// real time (inotify via fsnotify) and applies incremental, non-destructive
// changes to the store as files appear, change or vanish.
//
// Two robustness details set it apart from MiniDLNA's watcher:
//   - Write-settle debouncing: a newly created/modified file is only indexed
//     once its writes have gone quiet for settleDelay, so half-copied files are
//     never served.
//   - inotify is per-directory, so the watcher maintains watches on every
//     subdirectory and adds/removes them as the tree changes. If the kernel
//     watch limit is hit it logs and leans on the Tier-2 reconcile scan.
type Watcher struct {
	store       *store.Store
	roots       []Root
	settleDelay time.Duration
	log         *slog.Logger
	onChange    func()

	fsw    *fsnotify.Watcher
	mu     sync.Mutex
	timers map[string]*time.Timer
}

// NewWatcher creates a watcher. onChange is invoked after any applied change
// (used to bump the ContentDirectory update ID); it may be nil.
func NewWatcher(st *store.Store, roots []Root, settleDelay time.Duration, log *slog.Logger, onChange func()) *Watcher {
	if onChange == nil {
		onChange = func() {}
	}
	if settleDelay <= 0 {
		settleDelay = 2 * time.Second
	}
	abs := make([]Root, 0, len(roots))
	for _, r := range roots {
		if p, err := filepath.Abs(r.Path); err == nil {
			r.Path = p
		}
		abs = append(abs, r)
	}
	return &Watcher{
		store: st, roots: abs, settleDelay: settleDelay, log: log,
		onChange: onChange, timers: map[string]*time.Timer{},
	}
}

// Run starts watching and blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw
	defer fsw.Close()

	watched := 0
	for _, r := range w.roots {
		watched += w.addWatchesRecursive(r.Path)
	}
	w.log.Info("watcher active (tier 1: real-time)", "roots", len(w.roots), "directories_watched", watched)

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ev)
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.log.Warn("watcher error", "err", err)
		}
	}
}

// handleEvent translates one filesystem event into store changes.
func (w *Watcher) handleEvent(ev fsnotify.Event) {
	switch {
	case ev.Op&fsnotify.Create != 0:
		fi, err := os.Stat(ev.Name)
		if err != nil {
			return // created then immediately gone, or a move-away race
		}
		if fi.IsDir() {
			// New directory: index it and everything already inside, and start
			// watching it (files may have been dropped in as a whole folder).
			w.indexTree(ev.Name)
			w.log.Info("indexed new folder", "path", ev.Name, "library_size", w.size())
			w.onChange()
		} else {
			w.scheduleIndex(ev.Name)
		}

	case ev.Op&fsnotify.Write != 0:
		w.scheduleIndex(ev.Name)

	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		// Removed or moved away: drop it (and any descendants) from the index.
		w.cancelTimer(ev.Name)
		n, err := w.store.DeleteSubtree(ev.Name)
		_ = w.fsw.Remove(ev.Name) // no-op if it wasn't a watched dir
		if err == nil && n > 0 {
			w.log.Info("removed from library", "path", ev.Name, "rows", n, "library_size", w.size())
			w.onChange()
		}
	}
}

// scheduleIndex (re)arms the settle timer for a file. The file is indexed only
// after settleDelay passes with no further writes — coalescing a burst of
// writes into a single index and avoiding half-written files.
func (w *Watcher) scheduleIndex(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[path]; ok {
		t.Reset(w.settleDelay)
		return
	}
	w.timers[path] = time.AfterFunc(w.settleDelay, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()
		w.indexFile(path)
	})
}

func (w *Watcher) cancelTimer(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[path]; ok {
		t.Stop()
		delete(w.timers, path)
	}
}

// indexFile stats and indexes a single file after its settle window.
func (w *Watcher) indexFile(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		// Vanished during the settle window; make sure it's not left indexed.
		if n, derr := w.store.DeleteSubtree(path); derr == nil && n > 0 {
			w.onChange()
		}
		return
	}
	if fi.IsDir() {
		return
	}
	if w.indexFileInfo(path, fi) {
		w.log.Info("indexed change", "path", path, "library_size", w.size())
		w.onChange()
	}
}

// size returns the current indexed item count (best-effort, for logging).
func (w *Watcher) size() int {
	n, _ := w.store.Count()
	return n
}

// indexFileInfo upserts a media file. Returns true if it was a media file (and
// therefore stored).
func (w *Watcher) indexFileInfo(path string, fi os.FileInfo) bool {
	class, mime, ok := content.Classify(fi.Name())
	if !ok {
		return false
	}
	root, ok := w.rootFor(path)
	if !ok {
		return false
	}
	now := time.Now().Unix()
	err := w.store.Put(store.Item{
		Path: path, Parent: filepath.Dir(path), Name: fi.Name(), RootName: root.Name,
		IsDir: false, Class: class, Mime: mime, Size: fi.Size(),
		MTime: fi.ModTime().Unix(), DateAdded: now, SeenGen: now,
	})
	if err != nil {
		w.log.Warn("index put failed", "path", path, "err", err)
		return false
	}
	return true
}

// indexDir upserts a directory row.
func (w *Watcher) indexDir(path string, fi os.FileInfo) {
	root, ok := w.rootFor(path)
	if !ok {
		return
	}
	parent := filepath.Dir(path)
	name := fi.Name()
	if path == root.Path { // a root folder created at runtime
		parent = store.RootParent
		name = root.Name
	}
	now := time.Now().Unix()
	_ = w.store.Put(store.Item{
		Path: path, Parent: parent, Name: name, RootName: root.Name,
		IsDir: true, MTime: fi.ModTime().Unix(), DateAdded: now, SeenGen: now,
	})
}

// indexTree indexes a directory and everything under it, adding watches for
// every directory encountered. Used when a whole folder appears at once.
func (w *Watcher) indexTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if p != dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() {
			w.indexDir(p, fi)
			if err := w.fsw.Add(p); err != nil {
				w.warnWatchLimit(p, err)
			}
			return nil
		}
		w.indexFileInfo(p, fi)
		return nil
	})
}

// addWatchesRecursive adds inotify watches for dir and all subdirectories,
// returning the count added. It does not index (the initial full scan already
// did). Used once at startup.
func (w *Watcher) addWatchesRecursive(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if err := w.fsw.Add(p); err != nil {
			w.warnWatchLimit(p, err)
			return nil
		}
		n++
		return nil
	})
	return n
}

// rootFor returns the configured root that owns path.
func (w *Watcher) rootFor(path string) (Root, bool) {
	for _, r := range w.roots {
		if path == r.Path || strings.HasPrefix(path, r.Path+string(os.PathSeparator)) {
			return r, true
		}
	}
	return Root{}, false
}

func (w *Watcher) warnWatchLimit(path string, err error) {
	w.log.Warn("could not watch directory — the Tier-2 reconcile scan will still catch changes here; "+
		"consider raising fs.inotify.max_user_watches",
		"path", path, "err", err)
}
