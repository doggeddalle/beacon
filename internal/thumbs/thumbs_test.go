package thumbs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
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
