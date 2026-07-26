package upnp

import (
	"strings"
	"testing"

	"beacon/internal/content"
)

func TestBuildDIDLIncludesSubtitleAndMetadata(t *testing.T) {
	objs := []content.Object{{
		ID:       "item1",
		ParentID: "0",
		Title:    "Film",
		Class:    "object.item.videoItem",
		Resources: []content.Resource{{
			ProtocolInfo: "http-get:*:video/mp4:*",
			MediaID:      "item1",
			MimeType:     "video/mp4",
			Duration:     "1:30:00",
			Resolution:   "1920x1080",
		}},
		SubtitleID:   "item1",
		SubtitleKind: "srt",
		ArtworkID:    "item1",
	}}
	res := func(id string) string { return "http://h/media/" + id }
	sub := func(id string) string { return "http://h/subtitle/" + id }
	art := func(id string) string { return "http://h/thumb/" + id }

	out, err := buildDIDL(objs, res, sub, art)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`duration="1:30:00"`,
		`resolution="1920x1080"`,
		"sec:CaptionInfoEx",
		"pv:subtitleFileUri",
		"http://h/subtitle/item1",
		"http-get:*:text/srt:*",
		"upnp:albumArtURI",
		`dlna:profileID="JPEG_TN"`,
		"http://h/thumb/item1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DIDL missing %q\nfull:\n%s", want, out)
		}
	}
}

func TestBuildDIDLNoSubtitleWhenAbsent(t *testing.T) {
	objs := []content.Object{{
		ID: "item1", ParentID: "0", Title: "Film", Class: "object.item.videoItem",
		Resources: []content.Resource{{ProtocolInfo: "http-get:*:video/mp4:*", MediaID: "item1"}},
	}}
	res := func(id string) string { return "http://h/media/" + id }
	sub := func(id string) string { return "http://h/subtitle/" + id }
	art := func(id string) string { return "http://h/thumb/" + id }
	out, err := buildDIDL(objs, res, sub, art)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "CaptionInfo") || strings.Contains(out, "subtitle") {
		t.Errorf("DIDL should have no subtitle markup:\n%s", out)
	}
	if strings.Contains(out, "albumArtURI") {
		t.Errorf("DIDL should have no artwork markup when ArtworkID is empty:\n%s", out)
	}
}
