package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationFromOldSchema reproduces the P4 upgrade path: a database created
// before sub_path/probed existed must migrate cleanly when opened by new code.
func TestMigrationFromOldSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.db")

	// Build an "old" (pre-P4) database: the items table without sub_path/probed.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE items (
		path TEXT PRIMARY KEY, parent TEXT NOT NULL, name TEXT NOT NULL,
		root_name TEXT NOT NULL, is_dir INTEGER NOT NULL,
		class TEXT NOT NULL DEFAULT '', mime TEXT NOT NULL DEFAULT '',
		size INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL DEFAULT 0,
		duration TEXT NOT NULL DEFAULT '', resolution TEXT NOT NULL DEFAULT '',
		date_added INTEGER NOT NULL DEFAULT 0, seen_gen INTEGER NOT NULL DEFAULT 0);
		INSERT INTO items(path,parent,name,root_name,is_dir) VALUES('/m/a.mp4','/m','a.mp4','M',0);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Opening with current code must migrate without error.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("migration of old-schema DB failed: %v", err)
	}
	defer st.Close()

	// The pre-existing row survives and gets sane defaults for the new columns.
	it, err := st.Get("/m/a.mp4")
	if err != nil {
		t.Fatalf("pre-existing row lost after migration: %v", err)
	}
	if it.SubPath != "" || it.Probed {
		t.Errorf("migrated defaults wrong: %+v", it)
	}
	if need, _ := st.ItemsNeedingMetadata(10); len(need) != 1 {
		t.Errorf("migrated row should be queued for metadata, got %d", len(need))
	}
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "beacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMetadataRoundTripAndProbedFlag(t *testing.T) {
	st := openTemp(t)
	it := Item{Path: "/m/a.mp4", Parent: "/m", Name: "a.mp4", RootName: "M", Size: 100, MTime: 10}
	if err := st.Put(it); err != nil {
		t.Fatal(err)
	}

	// Freshly inserted -> needs metadata.
	need, _ := st.ItemsNeedingMetadata(10)
	if len(need) != 1 {
		t.Fatalf("expected 1 item needing metadata, got %d", len(need))
	}

	if err := st.UpdateMetadata("/m/a.mp4", "1:30:00", "1920x1080", "/m/a.srt"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get("/m/a.mp4")
	if got.Duration != "1:30:00" || got.Resolution != "1920x1080" || got.SubPath != "/m/a.srt" {
		t.Errorf("metadata not stored: %+v", got)
	}
	if !got.Probed {
		t.Error("item should be marked probed after UpdateMetadata")
	}

	// Now it should no longer appear as needing metadata.
	if need, _ := st.ItemsNeedingMetadata(10); len(need) != 0 {
		t.Errorf("probed item still queued for metadata: %d", len(need))
	}
}

func TestPutPreservesMetadataButResetsProbeOnChange(t *testing.T) {
	st := openTemp(t)
	base := Item{Path: "/m/a.mp4", Parent: "/m", Name: "a.mp4", RootName: "M", Size: 100, MTime: 10}
	st.Put(base)
	st.UpdateMetadata("/m/a.mp4", "1:00:00", "1280x720", "")

	// Re-Put with identical size/mtime (a no-op reconcile): metadata AND probed
	// must be preserved.
	st.Put(base)
	got, _ := st.Get("/m/a.mp4")
	if got.Duration != "1:00:00" || !got.Probed {
		t.Errorf("no-op reconcile clobbered metadata/probed: %+v", got)
	}

	// Re-Put with a changed size (a genuine edit): probed resets so it re-probes,
	// but old metadata stays until the new probe overwrites it.
	changed := base
	changed.Size = 200
	st.Put(changed)
	got, _ = st.Get("/m/a.mp4")
	if got.Probed {
		t.Error("probed flag should reset when the file size changes")
	}
	if got.Duration != "1:00:00" {
		t.Errorf("old duration should persist until re-probe, got %q", got.Duration)
	}
}

func TestDeleteSubtree(t *testing.T) {
	st := openTemp(t)
	sep := string(filepath.Separator)
	dir := sep + "m" + sep + "sub"
	st.Put(Item{Path: dir, Parent: sep + "m", Name: "sub", RootName: "M", IsDir: true})
	st.Put(Item{Path: dir + sep + "x.mp4", Parent: dir, Name: "x.mp4", RootName: "M"})
	st.Put(Item{Path: dir + sep + "deep" + sep + "y.mp4", Parent: dir + sep + "deep", Name: "y.mp4", RootName: "M"})
	st.Put(Item{Path: sep + "m" + sep + "keep.mp4", Parent: sep + "m", Name: "keep.mp4", RootName: "M"})

	n, err := st.DeleteSubtree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("DeleteSubtree removed %d rows, want 3 (dir + 2 descendants)", n)
	}
	if _, err := st.Get(sep + "m" + sep + "keep.mp4"); err != nil {
		t.Error("sibling outside the subtree was wrongly deleted")
	}
}
