package content

import "testing"

func TestEncodeDecodeIDRoundTrip(t *testing.T) {
	paths := []string{
		"/volume1/movies/Film.mp4",
		`C:\Users\ac\Movies\Film.mp4`,
		"/media/Ünïcødé — dash/Ep 1.mkv",
		"/media/with spaces/and+plus/and=equals.mp4",
		"/",
	}
	for _, p := range paths {
		id := EncodeID(p)
		// IDs travel in URL paths, so they must not need escaping.
		for _, r := range id {
			ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Errorf("EncodeID(%q) = %q contains URL-unsafe %q", p, id, r)
			}
		}
		got, err := DecodeID(id)
		if err != nil {
			t.Errorf("DecodeID(%q): %v", id, err)
			continue
		}
		if got != p {
			t.Errorf("round trip: got %q, want %q", got, p)
		}
	}
}

func TestDecodeIDRejectsGarbage(t *testing.T) {
	if _, err := DecodeID("not base64!!"); err == nil {
		t.Error("expected an error for a malformed ID")
	}
}

func TestDisplayTitleStripsExtension(t *testing.T) {
	cases := map[string]string{
		"Film.mp4":           "Film",
		"Show.S01E01.mkv":    "Show.S01E01",
		"NoExtension":        "NoExtension",
		"Trailing.dots..mp4": "Trailing.dots.",
		"archive.tar.gz":     "archive.tar",
	}
	for in, want := range cases {
		if got := DisplayTitle(in); got != want {
			t.Errorf("DisplayTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyKnownAndUnknown(t *testing.T) {
	cases := []struct {
		name      string
		wantClass string
		wantMime  string
		wantOK    bool
	}{
		{"Film.mp4", "object.item.videoItem", "video/mp4", true},
		{"Film.MKV", "object.item.videoItem", "video/x-matroska", true}, // case-insensitive
		{"Song.flac", "object.item.audioItem.musicTrack", "audio/flac", true},
		{"Photo.jpeg", "object.item.imageItem.photo", "image/jpeg", true},
		{"notes.txt", "", "", false},
		{"Film.srt", "", "", false}, // subtitles are sidecars, not browsable items
		{"noext", "", "", false},
	}
	for _, tc := range cases {
		class, mime, ok := Classify(tc.name)
		if ok != tc.wantOK || class != tc.wantClass || mime != tc.wantMime {
			t.Errorf("Classify(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.name, class, mime, ok, tc.wantClass, tc.wantMime, tc.wantOK)
		}
	}
}

func TestProtocolInfoShape(t *testing.T) {
	got := ProtocolInfo("video/mp4")
	want := "http-get:*:video/mp4:" + DLNAFlags
	if got != want {
		t.Errorf("ProtocolInfo = %q, want %q", got, want)
	}
}

func TestKnownMimeTypesIsSortedAndDeduped(t *testing.T) {
	got := KnownMimeTypes()
	if len(got) == 0 {
		t.Fatal("KnownMimeTypes returned nothing")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("not sorted/deduped at %d: %q then %q", i, got[i-1], got[i])
		}
	}
}
