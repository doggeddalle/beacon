package meta

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindSubtitleExactMatch(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Movie.mp4"))
	touch(t, filepath.Join(dir, "Movie.srt"))

	path, kind := FindSubtitle(filepath.Join(dir, "Movie.mp4"))
	if path == "" {
		t.Fatal("expected to find Movie.srt")
	}
	if filepath.Base(path) != "Movie.srt" {
		t.Errorf("path = %q, want Movie.srt", path)
	}
	if kind != "srt" {
		t.Errorf("kind = %q, want srt", kind)
	}
}

func TestFindSubtitleLanguageTagged(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Show.mkv"))
	touch(t, filepath.Join(dir, "Show.en.ass"))

	path, kind := FindSubtitle(filepath.Join(dir, "Show.mkv"))
	if filepath.Base(path) != "Show.en.ass" {
		t.Errorf("path = %q, want Show.en.ass", path)
	}
	if kind != "ass" {
		t.Errorf("kind = %q, want ass (from .ass)", kind)
	}
}

func TestFindSubtitleNone(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Lonely.mp4"))
	touch(t, filepath.Join(dir, "Other.srt")) // different basename, must not match

	if path, _ := FindSubtitle(filepath.Join(dir, "Lonely.mp4")); path != "" {
		t.Errorf("expected no subtitle, got %q", path)
	}
}

func TestProberEmptyIsUnavailable(t *testing.T) {
	p := &Prober{} // no ffprobe located
	if p.Available() {
		t.Error("empty prober should report unavailable")
	}
	if _, err := p.Probe(context.Background(), "whatever.mp4"); err != ErrUnavailable {
		t.Errorf("Probe error = %v, want ErrUnavailable", err)
	}
}

func TestNewProberRejectsBogusConfiguredPath(t *testing.T) {
	p := NewProber(filepath.Join(t.TempDir(), "does-not-exist"))
	if p.Path() != "" && !fileExists(p.Path()) {
		t.Errorf("prober accepted a non-existent path: %q", p.Path())
	}
}
