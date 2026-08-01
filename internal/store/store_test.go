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
	if it.SubPath != "" || it.Probed != ProbePending {
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

	if err := st.UpdateMetadata("/m/a.mp4", Metadata{Duration: "1:30:00", Resolution: "1920x1080", SubPath: "/m/a.srt", State: ProbeDone}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get("/m/a.mp4")
	if got.Duration != "1:30:00" || got.Resolution != "1920x1080" || got.SubPath != "/m/a.srt" {
		t.Errorf("metadata not stored: %+v", got)
	}
	if got.Probed != ProbeDone {
		t.Errorf("item should be ProbeDone after UpdateMetadata, got %v", got.Probed)
	}

	// Now it should no longer appear as needing metadata.
	if need, _ := st.ItemsNeedingMetadata(10); len(need) != 0 {
		t.Errorf("probed item still queued for metadata: %d", len(need))
	}
}

// Databases written before `probed` became tri-state recorded failed probes as
// success. Videos marked probed with no duration are that case and must be
// re-queued exactly once — repeatedly would spin on genuinely broken files.
func TestRepairRequeuesPreviouslyFailedVideoProbes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beacon.db")

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A video that "succeeded" with no duration (the old ffprobe-missing case),
	// and a photo that legitimately has none.
	st.Put(Item{Path: "/m/a.mp4", Parent: "/m", Name: "a.mp4", RootName: "M",
		Class: "object.item.videoItem", Size: 1, MTime: 1})
	st.Put(Item{Path: "/m/b.jpg", Parent: "/m", Name: "b.jpg", RootName: "M",
		Class: "object.item.imageItem.photo", Size: 1, MTime: 1})
	st.UpdateMetadata("/m/a.mp4", Metadata{State: ProbeDone})
	st.UpdateMetadata("/m/b.jpg", Metadata{State: ProbeDone})
	// Clear the marker to simulate a database from before the repair existed.
	if _, err := st.db.Exec(`DELETE FROM meta WHERE key='probe_state_repair_v1'`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	vid, _ := st2.Get("/m/a.mp4")
	if vid.Probed != ProbePending {
		t.Errorf("video with no duration should be re-queued, got %v", vid.Probed)
	}
	img, _ := st2.Get("/m/b.jpg")
	if img.Probed != ProbeDone {
		t.Errorf("photo legitimately has no duration and must not be re-queued, got %v", img.Probed)
	}

	// Applying the repair again must be a no-op, or unprobeable videos would be
	// re-queued on every startup.
	st2.UpdateMetadata("/m/a.mp4", Metadata{State: ProbeFailed})
	if err := st2.repairProbeStates(); err != nil {
		t.Fatal(err)
	}
	again, _ := st2.Get("/m/a.mp4")
	if again.Probed != ProbeFailed {
		t.Errorf("repair ran a second time (state %v); it must be one-shot", again.Probed)
	}
}

func TestPutPreservesMetadataButResetsProbeOnChange(t *testing.T) {
	st := openTemp(t)
	base := Item{Path: "/m/a.mp4", Parent: "/m", Name: "a.mp4", RootName: "M", Size: 100, MTime: 10}
	st.Put(base)
	st.UpdateMetadata("/m/a.mp4", Metadata{Duration: "1:00:00", Resolution: "1280x720", State: ProbeDone})

	// Re-Put with identical size/mtime (a no-op reconcile): metadata AND probed
	// must be preserved.
	st.Put(base)
	got, _ := st.Get("/m/a.mp4")
	if got.Duration != "1:00:00" || got.Probed != ProbeDone {
		t.Errorf("no-op reconcile clobbered metadata/probed: %+v", got)
	}

	// Re-Put with a changed size (a genuine edit): probed resets so it re-probes,
	// but old metadata stays until the new probe overwrites it.
	changed := base
	changed.Size = 200
	st.Put(changed)
	got, _ = st.Get("/m/a.mp4")
	if got.Probed != ProbePending {
		t.Errorf("probe state should reset to pending when the file size changes, got %v", got.Probed)
	}
	if got.Duration != "1:00:00" {
		t.Errorf("old duration should persist until re-probe, got %q", got.Duration)
	}
}

// Pagination has to happen in SQL. Slicing a fully-loaded folder meant a Browse
// asking for 20 rows still materialised all 20,000.
func TestChildrenPaginatesAndCounts(t *testing.T) {
	st := openTemp(t)
	sep := string(filepath.Separator)
	parent := sep + "m"
	st.Put(Item{Path: parent, Parent: RootParent, Name: "m", RootName: "M", IsDir: true})
	// Two containers and eight files; containers must sort first.
	for _, d := range []string{"b-dir", "a-dir"} {
		st.Put(Item{Path: parent + sep + d, Parent: parent, Name: d, RootName: "M", IsDir: true})
	}
	names := []string{"e.mp4", "c.mp4", "a.mp4", "g.mp4", "b.mp4", "d.mp4", "f.mp4", "h.mp4"}
	for _, n := range names {
		st.Put(Item{Path: parent + sep + n, Parent: parent, Name: n, RootName: "M"})
	}

	total, err := st.CountChildren(parent)
	if err != nil {
		t.Fatal(err)
	}
	if total != 10 {
		t.Fatalf("CountChildren = %d, want 10", total)
	}

	// First page: containers first, alphabetical.
	page, err := st.Children(parent, 0, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowNames(page); !equalSlices(got, []string{"a-dir", "b-dir", "a.mp4"}) {
		t.Errorf("page 1 = %v, want [a-dir b-dir a.mp4]", got)
	}

	// Offset into the middle.
	page, err = st.Children(parent, 3, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowNames(page); !equalSlices(got, []string{"b.mp4", "c.mp4", "d.mp4"}) {
		t.Errorf("page 2 = %v, want [b.mp4 c.mp4 d.mp4]", got)
	}

	// Past the end yields nothing rather than erroring.
	page, err = st.Children(parent, 100, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Errorf("offset past the end returned %d rows, want 0", len(page))
	}

	// Descending flips the name order but keeps containers first.
	page, err = st.Children(parent, 0, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowNames(page); !equalSlices(got, []string{"b-dir", "a-dir", "h.mp4"}) {
		t.Errorf("descending page = %v, want [b-dir a-dir h.mp4]", got)
	}
}

// Child counts must come from the browse query itself, not one COUNT per row.
func TestChildrenReportsChildCount(t *testing.T) {
	st := openTemp(t)
	sep := string(filepath.Separator)
	root, sub := sep+"m", sep+"m"+sep+"sub"
	st.Put(Item{Path: root, Parent: RootParent, Name: "m", RootName: "M", IsDir: true})
	st.Put(Item{Path: sub, Parent: root, Name: "sub", RootName: "M", IsDir: true})
	for _, n := range []string{"x.mp4", "y.mp4", "z.mp4"} {
		st.Put(Item{Path: sub + sep + n, Parent: sub, Name: n, RootName: "M"})
	}
	st.Put(Item{Path: root + sep + "loose.mp4", Parent: root, Name: "loose.mp4", RootName: "M"})

	rows, err := st.Children(root, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d children, want 2", len(rows))
	}
	if rows[0].Name != "sub" || rows[0].ChildCount != 3 {
		t.Errorf("container row = %q with childCount %d, want sub with 3", rows[0].Name, rows[0].ChildCount)
	}
	if rows[1].ChildCount != 0 {
		t.Errorf("file row childCount = %d, want 0", rows[1].ChildCount)
	}
}

func rowNames(rows []ChildRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
