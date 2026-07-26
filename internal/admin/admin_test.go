package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeCtrl struct {
	status  Status
	rescans int
	added   []Folder
	removed []string
}

func (f *fakeCtrl) Status() Status { return f.status }
func (f *fakeCtrl) Rescan()        { f.rescans++ }
func (f *fakeCtrl) AddFolder(name, path string) error {
	f.added = append(f.added, Folder{Name: name, Path: path})
	return nil
}
func (f *fakeCtrl) RemoveFolder(path string) error {
	f.removed = append(f.removed, path)
	return nil
}
func (f *fakeCtrl) Logs() []string { return []string{"line one", "line two"} }

func newTestServer(c Controller) *httptest.Server {
	return httptest.NewServer(New(c, slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func TestDashboardAndStatus(t *testing.T) {
	c := &fakeCtrl{status: Status{Version: "test", LibrarySize: 42, WatcherActive: true,
		Folders: []Folder{{Name: "Movies", Path: "/m"}}}}
	srv := newTestServer(c)
	defer srv.Close()

	html := getBody(t, srv.URL+"/")
	if !strings.Contains(html, "<title>Beacon</title>") {
		t.Error("dashboard HTML not served at /")
	}

	var st Status
	decode(t, srv.URL+"/admin/api/status", &st)
	if st.LibrarySize != 42 || !st.WatcherActive || len(st.Folders) != 1 {
		t.Errorf("status JSON wrong: %+v", st)
	}
}

func TestRescanAndFolders(t *testing.T) {
	c := &fakeCtrl{}
	srv := newTestServer(c)
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/admin/api/rescan", "", nil); err != nil {
		t.Fatal(err)
	}
	if c.rescans != 1 {
		t.Errorf("rescan not invoked: %d", c.rescans)
	}

	body := strings.NewReader(`{"name":"Music","path":"/music"}`)
	resp, err := http.Post(srv.URL+"/admin/api/folders", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(c.added) != 1 || c.added[0].Path != "/music" {
		t.Errorf("AddFolder not invoked correctly: %+v", c.added)
	}

	// Missing path -> 400.
	resp, _ = http.Post(srv.URL+"/admin/api/folders", "application/json", strings.NewReader(`{"name":"X"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty path should be 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func decode(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
