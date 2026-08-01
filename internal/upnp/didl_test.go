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
		ArtworkMime:  "image/jpeg",
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
		// JPEG_TN caps at 160x160; generated thumbnails default to 320 wide and
		// posters are larger still, so that profile was a false claim.
		`dlna:profileID="JPEG_SM"`,
		"http://h/thumb/item1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DIDL missing %q\nfull:\n%s", want, out)
		}
	}
	if strings.Contains(out, "JPEG_TN") {
		t.Errorf("DIDL still claims the JPEG_TN profile:\n%s", out)
	}
}

// PNG artwork must be advertised as PNG. The <res> hardcoded image/jpeg while
// the server served the actual PNG bytes.
func TestBuildDIDLAdvertisesRealArtworkMime(t *testing.T) {
	objs := []content.Object{{
		ID: "item1", ParentID: "0", Title: "Film", Class: "object.item.videoItem",
		ArtworkID: "item1", ArtworkMime: "image/png",
	}}
	id := func(s string) string { return "http://h/" + s }
	out, err := buildDIDL(objs, id, id, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "http-get:*:image/png:") {
		t.Errorf("PNG artwork not advertised as image/png:\n%s", out)
	}
	if strings.Contains(out, "image/jpeg") {
		t.Errorf("PNG artwork still advertised as image/jpeg:\n%s", out)
	}
	// No JPEG profile may be claimed for a PNG.
	if strings.Contains(out, "DLNA.ORG_PN=JPEG") {
		t.Errorf("PNG artwork claims a JPEG profile:\n%s", out)
	}
}

// Still images must carry the interactive transfer-mode flag, not the streaming
// one; the single flags constant used to be applied to every media kind.
func TestImageContentFeaturesUseInteractiveFlag(t *testing.T) {
	img := content.ContentFeatures("image/jpeg")
	vid := content.ContentFeatures("video/mp4")
	if img == vid {
		t.Fatal("images and video advertise identical DLNA flags")
	}
	if !strings.Contains(img, "DLNA.ORG_FLAGS=00D0") {
		t.Errorf("image flags = %q, want the TM_I word (00D0...)", img)
	}
	if content.TransferModeFor("image/png") != "Interactive" {
		t.Error("images should default to the Interactive transfer mode")
	}
	if content.TransferModeFor("video/mp4") != "Streaming" {
		t.Error("video should default to the Streaming transfer mode")
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
