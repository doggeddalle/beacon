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

// A sibling whose name merely starts with the media basename must not have its
// subtitles stolen. This is the common sequel / multi-part / versioned-episode
// case, where a bare HasPrefix silently attaches the wrong track.
func TestFindSubtitleDoesNotStealFromSiblings(t *testing.T) {
	cases := []struct {
		name    string
		media   string
		files   []string
		wantSub string // "" means no subtitle should be found
	}{
		{
			name:    "numbered sequel",
			media:   "Movie.mp4",
			files:   []string{"Movie.mp4", "Movie 2.mp4", "Movie 2.en.srt"},
			wantSub: "",
		},
		{
			name:    "sequel keeps its own",
			media:   "Movie 2.mp4",
			files:   []string{"Movie.mp4", "Movie 2.mp4", "Movie 2.en.srt"},
			wantSub: "Movie 2.en.srt",
		},
		{
			name:    "versioned episode",
			media:   "S01E01.mkv",
			files:   []string{"S01E01.mkv", "S01E01v2.mkv", "S01E01v2.srt"},
			wantSub: "",
		},
		{
			name:    "part1 vs part10",
			media:   "Part1.mp4",
			files:   []string{"Part1.mp4", "Part10.mp4", "Part10.srt"},
			wantSub: "",
		},
		{
			name:    "dash separated tag still matches",
			media:   "Film.mp4",
			files:   []string{"Film.mp4", "Film-forced.srt"},
			wantSub: "Film-forced.srt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				touch(t, filepath.Join(dir, f))
			}
			got, _ := FindSubtitle(filepath.Join(dir, tc.media))
			if tc.wantSub == "" {
				if got != "" {
					t.Errorf("%s got subtitle %q, want none", tc.media, filepath.Base(got))
				}
				return
			}
			if filepath.Base(got) != tc.wantSub {
				t.Errorf("%s got subtitle %q, want %q", tc.media, filepath.Base(got), tc.wantSub)
			}
		})
	}
}

func TestDurationHMS(t *testing.T) {
	cases := []struct {
		secs float64
		want string
	}{
		{0, ""},
		{-1, ""},
		// A still image's nominal frame duration must not become "0:00:00" on a
		// photo item.
		{0.04, ""},
		{0.4, ""},
		{1, "0:00:01"},
		{59.6, "0:01:00"},
		{3661, "1:01:01"},
		{5400, "1:30:00"},
	}
	for _, tc := range cases {
		if got := (Info{DurationSecs: tc.secs}).DurationHMS(); got != tc.want {
			t.Errorf("DurationHMS(%v) = %q, want %q", tc.secs, got, tc.want)
		}
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
