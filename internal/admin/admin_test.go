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

// The dashboard is deliberately unauthenticated, so cross-site requests are the
// whole attack surface. A form posting text/plain is a CORS "simple request":
// no preflight, sent by any page a LAN user visits. Both write endpoints must
// refuse it.
func TestCrossOriginWritesAreRefused(t *testing.T) {
	c := &fakeCtrl{}
	srv := newTestServer(c)
	defer srv.Close()

	cases := []struct {
		name        string
		method, url string
		ctype, body string
		origin      string
		referer     string
		wantStatus  int
	}{
		{
			name: "cross-origin text/plain form post", method: "POST",
			url: "/admin/api/folders", ctype: "text/plain", body: `{"path":"/"}`,
			origin: "http://evil.example", wantStatus: http.StatusForbidden,
		},
		{
			name: "cross-origin rescan", method: "POST", url: "/admin/api/rescan",
			origin: "http://evil.example", wantStatus: http.StatusForbidden,
		},
		{
			name: "cross-origin delete", method: "DELETE", url: "/admin/api/folders?path=/m",
			origin: "http://evil.example", wantStatus: http.StatusForbidden,
		},
		{
			name: "cross-origin via referer", method: "POST", url: "/admin/api/rescan",
			referer: "http://evil.example/page", wantStatus: http.StatusForbidden,
		},
		{
			// Same-origin but not JSON: still not something the dashboard sends.
			name: "same-origin text/plain body", method: "POST",
			url: "/admin/api/folders", ctype: "text/plain", body: `{"path":"/"}`,
			wantStatus: http.StatusUnsupportedMediaType,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req, err := http.NewRequest(tc.method, srv.URL+tc.url, body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.ctype != "" {
				req.Header.Set("Content-Type", tc.ctype)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				req.Header.Set("Referer", tc.referer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}

	if len(c.added) != 0 {
		t.Errorf("a cross-site request reached AddFolder: %+v", c.added)
	}
	if c.rescans != 0 {
		t.Errorf("a cross-site request triggered %d rescans", c.rescans)
	}
	if len(c.removed) != 0 {
		t.Errorf("a cross-site request reached RemoveFolder: %+v", c.removed)
	}
}

// The dashboard's own requests must keep working: same-origin JSON for the one
// call with a body, and no Content-Type at all for the bodyless ones.
func TestSameOriginWritesStillWork(t *testing.T) {
	c := &fakeCtrl{}
	srv := newTestServer(c)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	post := func(url, ctype, body string) int {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, _ := http.NewRequest("POST", srv.URL+url, r)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		req.Header.Set("Origin", "http://"+host) // what a browser sends
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("/admin/api/rescan", "", ""); got != http.StatusOK {
		t.Errorf("same-origin rescan = %d, want 200", got)
	}
	if got := post("/admin/api/folders", "application/json", `{"name":"M","path":"/m"}`); got != http.StatusOK {
		t.Errorf("same-origin add folder = %d, want 200", got)
	}
	if c.rescans != 1 || len(c.added) != 1 {
		t.Errorf("dashboard requests did not reach the controller: rescans=%d added=%+v", c.rescans, c.added)
	}
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
