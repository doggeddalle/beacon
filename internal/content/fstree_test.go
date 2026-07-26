package content

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func setupLibrary(t *testing.T) (*FS, string) {
	t.Helper()
	dir := t.TempDir()
	movies := filepath.Join(dir, "movies")
	if err := os.MkdirAll(filepath.Join(movies, "Action"), 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(movies, "Top.mp4"))
	must(filepath.Join(movies, "Action", "Boom.mkv"))
	must(filepath.Join(movies, "notes.txt")) // non-media, must be ignored
	// A secret file OUTSIDE the library root.
	must(filepath.Join(dir, "secret.mp4"))

	return NewFS([]Root{{Name: "Movies", Path: movies}}), dir
}

func TestChildrenRootListsFolders(t *testing.T) {
	fs, _ := setupLibrary(t)
	kids, err := fs.Children(RootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].Title != "Movies" || !kids[0].IsContainer {
		t.Fatalf("root children = %+v, want single 'Movies' container", kids)
	}
}

func TestChildrenSkipsNonMediaAndHidden(t *testing.T) {
	fs, _ := setupLibrary(t)
	root, _ := fs.Children(RootID)
	kids, err := fs.Children(root[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: "Action" (container) + "Top" (item). notes.txt excluded.
	if len(kids) != 2 {
		t.Fatalf("got %d children, want 2 (Action, Top); notes.txt should be skipped: %+v", len(kids), kids)
	}
	if !kids[0].IsContainer { // containers sort first
		t.Errorf("first child should be the Action container, got %+v", kids[0])
	}
}

func TestFilePathRejectsTraversal(t *testing.T) {
	fs, dir := setupLibrary(t)
	// Forge an object ID pointing at the secret file outside the root.
	secret := filepath.Join(dir, "secret.mp4")
	forged := base64.RawURLEncoding.EncodeToString([]byte(secret))
	if _, err := fs.FilePath(forged); err == nil {
		t.Fatal("FilePath resolved a path OUTSIDE the library root — path traversal!")
	}
	// A "../" escape attempt should also fail.
	root, _ := fs.Children(RootID)
	moviesPath, _ := decodeID(root[0].ID)
	escape := filepath.Join(moviesPath, "..", "secret.mp4")
	forged2 := base64.RawURLEncoding.EncodeToString([]byte(escape))
	if _, err := fs.FilePath(forged2); err == nil {
		t.Fatal("FilePath resolved a ../ traversal outside the root")
	}
}

func TestFilePathAllowsInRoot(t *testing.T) {
	fs, _ := setupLibrary(t)
	root, _ := fs.Children(RootID)
	kids, _ := fs.Children(root[0].ID)
	var itemID string
	for _, k := range kids {
		if !k.IsContainer {
			itemID = k.ID
		}
	}
	if itemID == "" {
		t.Fatal("no media item found")
	}
	path, err := fs.FilePath(itemID)
	if err != nil {
		t.Fatalf("FilePath rejected a valid in-root item: %v", err)
	}
	if runtime.GOOS != "windows" && !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
}
