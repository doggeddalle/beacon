package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"beacon/internal/content"
	"beacon/internal/meta"
	"beacon/internal/store"
)

// TestEnricherDetectsSubtitles verifies the enrichment path end-to-end without
// ffprobe: a media file with a sidecar .srt gets its sub_path filled in, and
// the browse backend then exposes it as a subtitle.
func TestEnricherDetectsSubtitles(t *testing.T) {
	dir := t.TempDir()
	movies := filepath.Join(dir, "movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(movies, "Film.mp4"))
	write(t, filepath.Join(movies, "Film.srt")) // sidecar subtitle
	write(t, filepath.Join(movies, "NoSubs.mp4"))

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

	// No ffprobe needed; the enricher still detects subtitles. Use an empty
	// prober so the test is deterministic regardless of the host.
	enr := NewEnricher(st, &meta.Prober{}, 2, discardLog(), nil)
	if done, pending := enr.ProcessBatch(context.Background()); done < 2 {
		t.Fatalf("expected to process the media files, got %d done of %d pending", done, pending)
	}

	be := NewBackend(st, func() int { n, _ := st.CountChildren(store.RootParent); return n }, nil)

	film, err := st.Get(filepath.Join(movies, "Film.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if film.SubPath == "" {
		t.Fatal("Film.mp4 sidecar subtitle was not detected")
	}
	obj := mustObject(t, be, content.EncodeID(film.Path))
	if obj.SubtitleID == "" || obj.SubtitleKind != "srt" {
		t.Errorf("browse object missing subtitle: id=%q kind=%q", obj.SubtitleID, obj.SubtitleKind)
	}

	// The subtitle provider resolves to the real file.
	subPath, kind, err := be.SubtitlePath(obj.SubtitleID)
	if err != nil {
		t.Fatalf("SubtitlePath failed: %v", err)
	}
	if filepath.Base(subPath) != "Film.srt" || kind != "srt" {
		t.Errorf("SubtitlePath = %q (%s), want Film.srt (srt)", subPath, kind)
	}

	// A file without subs must have none.
	nosubs := mustObject(t, be, content.EncodeID(filepath.Join(movies, "NoSubs.mp4")))
	if nosubs.SubtitleID != "" {
		t.Error("NoSubs.mp4 should have no subtitle")
	}
}

// Running without ffprobe must not permanently blank an item's metadata. The old
// code marked every item "probed" regardless of outcome, and store.Put only
// re-queues on a size/mtime change — so installing ffmpeg later never backfilled
// the durations. Failures must be recorded as retryable instead.
func TestProbeFailureIsRetriedNotCached(t *testing.T) {
	dir := t.TempDir()
	movies := filepath.Join(dir, "movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(movies, "Film.mp4"))

	st, err := store.Open(filepath.Join(dir, "beacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ix := NewIndexer(st, []Root{{Name: "Movies", Path: movies}}, discardLog())
	if err := ix.FullScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	// First run: no ffprobe available.
	enr := NewEnricher(st, &meta.Prober{}, 2, discardLog(), nil)
	if done, _ := enr.ProcessBatch(context.Background()); done != 1 {
		t.Fatalf("expected the item to be recorded, got %d", done)
	}

	film := filepath.Join(movies, "Film.mp4")
	got, err := st.Get(film)
	if err != nil {
		t.Fatal(err)
	}
	if got.Probed != store.ProbeFailed {
		t.Fatalf("probe without ffprobe recorded as %v, want ProbeFailed", got.Probed)
	}

	// The item must not be re-served while ffprobe is still missing, or the
	// enricher spins on it forever.
	if need, _ := st.ItemsNeedingMetadata(10); len(need) != 0 {
		t.Errorf("failed item still queued while ffprobe is absent: %d", len(need))
	}

	// Second run, ffprobe now installed: startup re-queues the failure.
	n, err := st.ResetFailedProbes()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ResetFailedProbes re-queued %d items, want 1", n)
	}
	need, _ := st.ItemsNeedingMetadata(10)
	if len(need) != 1 {
		t.Fatalf("item not re-queued after ffprobe became available: %d", len(need))
	}
}

func mustObject(t *testing.T, be *Backend, id string) content.Object {
	t.Helper()
	obj, err := be.Object(id)
	if err != nil {
		t.Fatalf("Object(%q): %v", id, err)
	}
	return obj
}
