package library

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	onDesync    func()

	fsw    *fsnotify.Watcher
	mu     sync.Mutex
	timers map[string]*time.Timer

	// watched tracks the directories we hold inotify watches on, so a removed or
	// renamed directory can have its descendants' watches released too — fsnotify
	// only ever removes the exact path it is given.
	wmu     sync.Mutex
	watched map[string]bool

	// trees carries whole-directory indexing off the event loop; doing that work
	// inline lets the kernel's inotify queue overflow while we walk.
	trees chan string

	degraded atomic.Bool // inotify coverage is incomplete (watch limit hit)
}

// treeQueueDepth bounds the backlog of directories awaiting a subtree index. If
// it fills we ask for a reconcile rather than blocking the event loop.
const treeQueueDepth = 64

// NewWatcher creates a watcher. onChange is invoked after any applied change
// (used to bump the ContentDirectory update ID); onDesync is invoked when events
// were provably lost and only a rescan can restore accuracy. Both may be nil.
func NewWatcher(st *store.Store, roots []Root, settleDelay time.Duration, log *slog.Logger, onChange, onDesync func()) *Watcher {
	if onChange == nil {
		onChange = func() {}
	}
	if onDesync == nil {
		onDesync = func() {}
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
		onChange: onChange, onDesync: onDesync, timers: map[string]*time.Timer{},
		watched: map[string]bool{}, trees: make(chan string, treeQueueDepth),
	}
}

// Degraded reports whether inotify coverage is incomplete, so the dashboard can
// say so rather than implying real-time updates are working everywhere.
func (w *Watcher) Degraded() bool { return w.degraded.Load() }

// Run starts watching and blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw
	defer fsw.Close()
	// Settle timers outlive the event loop otherwise, firing after the server has
	// closed the database and logging a burst of failed writes on every shutdown.
	defer w.stopAllTimers()

	watched, failed := 0, 0
	for _, r := range w.roots {
		n, f := w.addWatchesRecursive(r.Path)
		watched += n
		failed += f
	}
	w.reportWatchFailures(failed)
	w.log.Info("watcher active (tier 1: real-time)", "roots", len(w.roots), "directories_watched", watched)

	// Subtree indexing runs here so a folder dropped in with 10k files does not
	// stall event draining while we walk it.
	treesDone := make(chan struct{})
	go func() {
		defer close(treesDone)
		for {
			select {
			case <-ctx.Done():
				return
			case dir := <-w.trees:
				w.indexTree(dir)
				w.onChange()
			}
		}
	}()
	defer func() { <-treesDone }()

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
			// Overflow is not a diagnostic — it means the kernel discarded events
			// and the index is now silently wrong. Only a rescan can fix that.
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.log.Warn("inotify queue overflowed — events were lost, triggering a reconcile scan")
				w.onDesync()
				continue
			}
			w.log.Warn("watcher error", "err", err)
		}
	}
}

// stopAllTimers cancels every pending settle timer.
func (w *Watcher) stopAllTimers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, t := range w.timers {
		t.Stop()
		delete(w.timers, path)
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
			// Queued, not inline — see the trees channel.
			w.log.Info("indexing new folder", "path", ev.Name)
			select {
			case w.trees <- ev.Name:
			default:
				// Backlog full: folders are arriving faster than we can walk them.
				// A reconcile will pick up whatever we drop here.
				w.log.Warn("subtree index backlog full — deferring to a reconcile scan", "path", ev.Name)
				w.onDesync()
			}
		} else {
			w.scheduleIndex(ev.Name)
		}

	case ev.Op&fsnotify.Write != 0:
		w.scheduleIndex(ev.Name)

	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		// Removed or moved away: drop it (and any descendants) from the index.
		w.cancelTimer(ev.Name)
		n, err := w.store.DeleteSubtree(ev.Name)
		w.unwatchSubtree(ev.Name)
		if err == nil && n > 0 {
			w.log.Info("removed from library", "path", ev.Name, "rows", n)
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
		w.log.Info("indexed change", "path", path)
		w.onChange()
	}
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
	failed := 0
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
			if !w.addWatch(p) {
				failed++
			}
			return nil
		}
		w.indexFileInfo(p, fi)
		return nil
	})
	w.reportWatchFailures(failed)
}

// addWatchesRecursive adds inotify watches for dir and all subdirectories,
// returning how many succeeded and how many failed. It does not index (the
// initial full scan already did). Used once at startup.
func (w *Watcher) addWatchesRecursive(dir string) (added, failed int) {
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
		if w.addWatch(p) {
			added++
		} else {
			failed++
		}
		return nil
	})
	return added, failed
}

// addWatch registers an inotify watch and records it, so the descendants of a
// removed directory can be released later.
func (w *Watcher) addWatch(dir string) bool {
	if err := w.fsw.Add(dir); err != nil {
		w.log.Debug("could not watch directory", "path", dir, "err", err)
		return false
	}
	w.wmu.Lock()
	w.watched[dir] = true
	w.wmu.Unlock()
	return true
}

// unwatchSubtree releases the watch on dir and on every directory beneath it.
//
// fsnotify.Remove only drops the exact path, so renaming a folder used to strand
// its children's watch descriptors: later events inside the renamed tree arrived
// under the old, non-existent path and were silently discarded.
func (w *Watcher) unwatchSubtree(dir string) {
	prefix := dir + string(os.PathSeparator)
	w.wmu.Lock()
	var gone []string
	for p := range w.watched {
		if p == dir || strings.HasPrefix(p, prefix) {
			gone = append(gone, p)
		}
	}
	for _, p := range gone {
		delete(w.watched, p)
	}
	w.wmu.Unlock()

	for _, p := range gone {
		_ = w.fsw.Remove(p)
	}
	if len(gone) == 0 {
		_ = w.fsw.Remove(dir) // not tracked (e.g. a file); harmless no-op
	}
}

// reportWatchFailures logs one summary line per batch. Logging per directory
// turned a blown fs.inotify.max_user_watches into thousands of identical lines,
// flooding the dashboard's 300-line ring and hiding everything else.
func (w *Watcher) reportWatchFailures(failed int) {
	if failed == 0 {
		return
	}
	w.degraded.Store(true)
	w.log.Warn("some directories could not be watched — real-time updates are incomplete there; "+
		"the Tier-2 reconcile scan still catches changes. Consider raising fs.inotify.max_user_watches",
		"directories", failed)
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
