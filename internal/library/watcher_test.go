package library

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"beacon/internal/store"
)

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

func TestWatcherRealtimeChanges(t *testing.T) {
	dir := t.TempDir()
	movies := filepath.Join(dir, "movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(movies, "Existing.mp4"))

	st, err := store.Open(filepath.Join(dir, "beacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	roots := []Root{{Name: "Movies", Path: movies}}
	ix := NewIndexer(st, roots, discardLog())
	if err := ix.FullScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	var changes atomic.Int32
	w := NewWatcher(st, roots, 80*time.Millisecond, discardLog(), func() { changes.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // let initial watches establish

	has := func(p string) func() bool {
		return func() bool { _, err := st.Get(p); return err == nil }
	}
	gone := func(p string) func() bool {
		return func() bool { _, err := st.Get(p); return err != nil }
	}

	// 1. New file appears in real time.
	newFile := filepath.Join(movies, "New Movie.mp4")
	write(t, newFile)
	waitFor(t, has(newFile), 4*time.Second, "New Movie.mp4 to be indexed")

	// 2. New subdirectory with a file (created after watch is established).
	sub := filepath.Join(movies, "Series")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	waitFor(t, has(sub), 4*time.Second, "Series subdir to be indexed")
	deep := filepath.Join(sub, "Ep1.mkv")
	write(t, deep)
	waitFor(t, has(deep), 4*time.Second, "Ep1.mkv inside the new subdir to be indexed")

	// 3. A whole pre-populated folder moved in atomically (e.g. copying a movie
	//    folder in one operation). indexTree must index its contents.
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(staging, "Feature.mp4"))
	moved := filepath.Join(movies, "Dropped")
	if err := os.Rename(staging, moved); err != nil {
		t.Fatal(err)
	}
	waitFor(t, has(filepath.Join(moved, "Feature.mp4")), 4*time.Second, "file inside a moved-in folder to be indexed")

	// 4. Delete a file -> removed from the index.
	if err := os.Remove(newFile); err != nil {
		t.Fatal(err)
	}
	waitFor(t, gone(newFile), 4*time.Second, "deleted file to be removed from the index")

	// 5. Delete a whole subdirectory -> its subtree is removed.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	waitFor(t, gone(deep), 4*time.Second, "file under a deleted subdir to be removed (subtree prune)")

	if changes.Load() == 0 {
		t.Error("onChange was never called despite multiple library changes")
	}

	// The original file must still be present throughout — nothing destructive.
	if _, err := st.Get(filepath.Join(movies, "Existing.mp4")); err != nil {
		t.Error("pre-existing file was lost during watch activity")
	}
}
