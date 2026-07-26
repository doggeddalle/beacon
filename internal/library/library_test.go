package library

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"beacon/internal/content"
	"beacon/internal/store"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setup builds a temp library and an indexed store over it:
//
//	movies/
//	  Top.mp4
//	  notes.txt        (ignored: not media)
//	  Action/
//	    Boom.mkv
func setup(t *testing.T) (*store.Store, *Indexer, *Backend, string) {
	t.Helper()
	dir := t.TempDir()
	movies := filepath.Join(dir, "movies")
	if err := os.MkdirAll(filepath.Join(movies, "Action"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(movies, "Top.mp4"))
	write(t, filepath.Join(movies, "notes.txt"))
	write(t, filepath.Join(movies, "Action", "Boom.mkv"))

	st, err := store.Open(filepath.Join(dir, "beacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	roots := []Root{{Name: "Movies", Path: movies}}
	ix := NewIndexer(st, roots, discardLog())
	be := NewBackend(st, func() int { n, _ := st.CountChildren(store.RootParent); return n }, nil)
	if err := ix.FullScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st, ix, be, movies
}

func write(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBrowseFromIndex(t *testing.T) {
	_, _, be, _ := setup(t)

	// Root -> one "Movies" container.
	rootKids, err := be.Children(content.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootKids) != 1 || rootKids[0].Title != "Movies" || !rootKids[0].IsContainer {
		t.Fatalf("root children = %+v, want single Movies container", rootKids)
	}
	if rootKids[0].ChildCount != 2 { // Action + Top.mp4 (notes.txt excluded)
		t.Errorf("Movies childCount = %d, want 2", rootKids[0].ChildCount)
	}

	// Into Movies: Action (container, sorts first) + Top (item).
	kids, err := be.Children(rootKids[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("Movies children = %d, want 2 (notes.txt must be excluded): %+v", len(kids), kids)
	}
	if !kids[0].IsContainer || kids[0].Title != "Action" {
		t.Errorf("first child = %+v, want Action container", kids[0])
	}
	item := kids[1]
	if item.Title != "Top" { // extension stripped
		t.Errorf("item title = %q, want Top", item.Title)
	}
	if len(item.Resources) != 1 || item.Resources[0].MimeType != "video/mp4" {
		t.Errorf("item resource = %+v, want one video/mp4 resource", item.Resources)
	}
}

func TestFilePathResolvesIndexedOnly(t *testing.T) {
	st, _, be, movies := setup(t)

	top, err := st.Get(filepath.Join(movies, "Top.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := be.FilePath(content.EncodeID(top.Path))
	if err != nil {
		t.Fatalf("FilePath on indexed item failed: %v", err)
	}
	if path != top.Path {
		t.Errorf("FilePath = %q, want %q", path, top.Path)
	}

	// A path NOT in the index must not resolve (DB is the allow-list).
	forged := content.EncodeID(filepath.Join(movies, "..", "escape.mp4"))
	if _, err := be.FilePath(forged); err == nil {
		t.Fatal("FilePath resolved a non-indexed path — allow-list breach")
	}
}

func TestRescanIsNonDestructiveAndPrunes(t *testing.T) {
	st, ix, be, movies := setup(t)

	// Capture Boom's original insert time; it must survive a rescan unchanged.
	boomPath := filepath.Join(movies, "Action", "Boom.mkv")
	before, err := st.Get(boomPath)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate the library: remove Top.mp4, add New.mp4.
	if err := os.Remove(filepath.Join(movies, "Top.mp4")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(movies, "New.mp4"))

	if err := ix.FullScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Top gone, New present.
	if _, err := st.Get(filepath.Join(movies, "Top.mp4")); err == nil {
		t.Error("Top.mp4 should have been pruned after deletion")
	}
	if _, err := st.Get(filepath.Join(movies, "New.mp4")); err != nil {
		t.Error("New.mp4 should have been indexed on rescan")
	}

	// Non-destructive: Boom's row (incl. original date_added) is preserved, not
	// wiped-and-reinserted the way MiniDLNA's nightly rebuild would.
	after, err := st.Get(boomPath)
	if err != nil {
		t.Fatalf("Boom.mkv vanished across a rescan — index was destroyed: %v", err)
	}
	if after.DateAdded != before.DateAdded {
		t.Errorf("Boom date_added changed across rescan (%d -> %d): row was recreated, not preserved",
			before.DateAdded, after.DateAdded)
	}

	// Browsing reflects the new state.
	root, _ := be.Children(content.RootID)
	kids, _ := be.Children(root[0].ID)
	var titles []string
	for _, k := range kids {
		titles = append(titles, k.Title)
	}
	// Expect: Action (container), New (item). Top removed.
	if len(kids) != 2 {
		t.Fatalf("after rescan Movies has %d children %v, want 2 (Action, New)", len(kids), titles)
	}
}
