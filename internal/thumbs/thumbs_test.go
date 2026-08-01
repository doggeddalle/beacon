package thumbs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindPosterSameBasename(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Movie.mp4"))
	touch(t, filepath.Join(dir, "Movie.jpg"))

	got, ok := FindPoster(filepath.Join(dir, "Movie.mp4"))
	if !ok || filepath.Base(got) != "Movie.jpg" {
		t.Fatalf("FindPoster = %q, %v; want Movie.jpg", got, ok)
	}
}

func TestFindPosterFolderLevel(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Movie.mkv"))
	touch(t, filepath.Join(dir, "poster.jpg"))

	got, ok := FindPoster(filepath.Join(dir, "Movie.mkv"))
	if !ok || filepath.Base(got) != "poster.jpg" {
		t.Fatalf("FindPoster = %q, %v; want poster.jpg", got, ok)
	}
}

func TestFindPosterNone(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Movie.mp4"))
	if _, ok := FindPoster(filepath.Join(dir, "Movie.mp4")); ok {
		t.Error("expected no poster")
	}
}

func TestFrameUnavailableWithoutFFmpeg(t *testing.T) {
	tn := New("", t.TempDir(), 2, 320, discardLog())
	if tn.Available() {
		t.Error("thumbnailer should be unavailable with empty ffmpeg path")
	}
	if _, err := tn.Frame(context.Background(), "x.mp4", 0, 5); err != ErrUnavailable {
		t.Errorf("Frame err = %v, want ErrUnavailable", err)
	}
}

// Concurrent requests for the same thumbnail must run the generator exactly once.
// The old check-semaphore-check only coalesced when workers==1; with the default
// 2, two `ffmpeg -y` processes wrote the same path and cached a corrupt JPEG.
func TestGenerateCoalescesConcurrentRequests(t *testing.T) {
	tn := New("/fake/ffmpeg", t.TempDir(), 4, 320, discardLog())
	out := tn.cachePath("/m/a.mp4", 100)

	var runs atomic.Int64
	run := func(dst string) error {
		runs.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		return os.WriteFile(dst, []byte("jpegdata"), 0o644)
	}

	const callers = 8
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = tn.generate(context.Background(), out, run)
		}()
	}
	wg.Wait()

	if n := runs.Load(); n != 1 {
		t.Errorf("generator ran %d times, want exactly 1", n)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
		}
		if results[i] != out {
			t.Errorf("caller %d got %q, want %q", i, results[i], out)
		}
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "jpegdata" {
		t.Errorf("cached file = %q, %v; want intact content", b, err)
	}
}

// A failed generation must be remembered, or an undecodable video re-runs ffmpeg
// on every browse and holds a worker slot for up to two minutes each time.
func TestGenerateNegativeCachesFailures(t *testing.T) {
	tn := New("/fake/ffmpeg", t.TempDir(), 2, 320, discardLog())
	out := tn.cachePath("/m/broken.mp4", 100)

	var runs atomic.Int64
	run := func(string) error {
		runs.Add(1)
		return errors.New("no decodable frame")
	}

	if _, err := tn.generate(context.Background(), out, run); err == nil {
		t.Fatal("expected first generation to fail")
	}
	if _, err := tn.generate(context.Background(), out, run); err == nil {
		t.Fatal("expected second generation to fail")
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("generator ran %d times, want 1 (second call should hit the negative cache)", n)
	}
}

// A generator killed part-way must not leave a truncated file at the cache path,
// because fileExists() would then serve it forever.
func TestFailedGenerationLeavesNoCacheFile(t *testing.T) {
	tn := New("/fake/ffmpeg", t.TempDir(), 1, 320, discardLog())
	out := tn.cachePath("/m/a.mp4", 100)

	_, err := tn.generate(context.Background(), out, func(dst string) error {
		os.WriteFile(dst, []byte("truncated"), 0o644) // partial write, then failure
		return errors.New("killed")
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	if fileExists(out) {
		t.Error("a failed generation left a file at the cache path")
	}
}

func TestSweepRemovesOldAndOversizeEntries(t *testing.T) {
	dir := t.TempDir()
	tn := New("", dir, 1, 320, discardLog())

	old := filepath.Join(dir, "old.jpg")
	fresh := filepath.Join(dir, "fresh.jpg")
	touch(t, old)
	if err := os.WriteFile(fresh, make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, aged, aged); err != nil {
		t.Fatal(err)
	}

	removed, _ := tn.Sweep(24*time.Hour, 0)
	if removed != 1 {
		t.Errorf("Sweep removed %d entries, want 1", removed)
	}
	if fileExists(old) {
		t.Error("stale entry should have been swept")
	}
	if !fileExists(fresh) {
		t.Error("fresh entry should have survived")
	}

	// Size cap: the surviving 512-byte entry exceeds a 64-byte budget.
	if removed, freed := tn.Sweep(0, 64); removed != 1 || freed != 512 {
		t.Errorf("size-capped Sweep removed %d entries freeing %d bytes, want 1 and 512", removed, freed)
	}
	if fileExists(fresh) {
		t.Error("entry over the size cap should have been swept")
	}
}

func TestCachePathStableAndMtimeKeyed(t *testing.T) {
	tn := New("", t.TempDir(), 1, 320, discardLog())
	a := tn.cachePath("/m/a.mp4", 100)
	b := tn.cachePath("/m/a.mp4", 100)
	c := tn.cachePath("/m/a.mp4", 200)
	if a != b {
		t.Error("cache path should be stable for same path+mtime")
	}
	if a == c {
		t.Error("cache path should change when mtime changes (so edits regenerate)")
	}
}
