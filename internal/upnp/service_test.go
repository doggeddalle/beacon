package upnp_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"beacon/internal/library"
	"beacon/internal/meta"
	"beacon/internal/store"
	"beacon/internal/upnp"
)

// buildTestServer creates a MediaServer over a temp library:
//
//	root/
//	  Movies/
//	    Sample.mp4   (12 bytes of known content)
//
// It wires up the real SQLite-backed library.Backend, the same one production
// runs. The end-to-end protocol test previously used a filesystem backend that
// shipped only in tests, so subtitles, artwork and metadata — all of which only
// exist on the real backend — went unexercised.
func buildTestServer(t testing.TB) (*httptest.Server, string) {
	t.Helper()
	srv, payload, _ := buildTestServerWithLibrary(t, func(movies string) {
		writeFile(t, filepath.Join(movies, "Sample.mp4"), samplePayload)
	})
	return srv, payload
}

const samplePayload = "0123456789AB"

// buildTestServerWithLibrary builds the server over a library populated by
// populate, and also returns the store so tests can assert on indexed state.
func buildTestServerWithLibrary(t testing.TB, populate func(movies string)) (*httptest.Server, string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	movies := filepath.Join(dir, "Movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	populate(movies)

	st, err := store.Open(filepath.Join(dir, "beacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	roots := []library.Root{{Name: "Movies", Path: movies}}
	ix := library.NewIndexer(st, roots, discardLogger())
	if err := ix.FullScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Fill in subtitle/artwork sidecars without needing ffprobe on the host.
	enr := library.NewEnricher(st, &meta.Prober{}, 2, discardLogger(), nil)
	enr.ProcessBatch(context.Background())

	backend := library.NewBackend(st, func() int {
		n, _ := st.CountChildren(store.RootParent)
		return n
	}, nil)

	info := upnp.DeviceInfo{FriendlyName: "Test & Beacon", UDN: "uuid:test-1234"}
	h, err := upnp.NewHandler(info, backend, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(h), samplePayload, st
}

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceDescription(t *testing.T) {
	srv, _ := buildTestServer(t)
	defer srv.Close()

	body := httpGet(t, srv.URL+upnp.PathDeviceDesc)
	if !strings.Contains(body, "urn:schemas-upnp-org:device:MediaServer:1") {
		t.Error("device description missing MediaServer device type")
	}
	if !strings.Contains(body, "Test &amp; Beacon") {
		t.Error("friendly name not XML-escaped in device description")
	}
	if !strings.Contains(body, upnp.PathCtlContentDir) {
		t.Error("device description missing ContentDirectory control URL")
	}
}

func TestBrowseRootThenFolderThenStream(t *testing.T) {
	srv, payload := buildTestServer(t)
	defer srv.Close()

	// Browse root: expect one container ("Movies").
	rootDIDL := browse(t, srv.URL, "0", "BrowseDirectChildren")
	if len(rootDIDL.Containers) != 1 {
		t.Fatalf("root: got %d containers, want 1", len(rootDIDL.Containers))
	}
	movies := rootDIDL.Containers[0]
	if movies.Title != "Movies" {
		t.Errorf("container title = %q, want Movies", movies.Title)
	}

	// Browse into Movies: expect one item ("Sample") with a res URL.
	folderDIDL := browse(t, srv.URL, movies.ID, "BrowseDirectChildren")
	if len(folderDIDL.Items) != 1 {
		t.Fatalf("Movies: got %d items, want 1", len(folderDIDL.Items))
	}
	item := folderDIDL.Items[0]
	if item.Title != "Sample" {
		t.Errorf("item title = %q, want Sample (extension stripped)", item.Title)
	}
	if len(item.Res) == 0 || item.Res[0].URL == "" {
		t.Fatal("item has no <res> URL")
	}
	if !strings.Contains(item.Res[0].ProtocolInfo, "video/mp4") {
		t.Errorf("protocolInfo = %q, want video/mp4", item.Res[0].ProtocolInfo)
	}

	// Stream the whole file.
	full := httpGet(t, item.Res[0].URL)
	if full != payload {
		t.Errorf("streamed body = %q, want %q", full, payload)
	}

	// Range request: bytes 2-5 -> "2345", status 206.
	req, _ := http.NewRequest("GET", item.Res[0].URL, nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range request status = %d, want 206", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "2345" {
		t.Errorf("range body = %q, want 2345", string(b))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("missing Accept-Ranges: bytes header (seeking would break)")
	}
}

func TestBrowseMetadata(t *testing.T) {
	srv, _ := buildTestServer(t)
	defer srv.Close()

	didl := browse(t, srv.URL, "0", "BrowseMetadata")
	if len(didl.Containers) != 1 {
		t.Fatalf("BrowseMetadata root: got %d containers, want 1", len(didl.Containers))
	}
	if didl.Containers[0].ID != "0" {
		t.Errorf("root metadata id = %q, want 0", didl.Containers[0].ID)
	}
}

// Responses must carry Content-Length. Go switches to chunked transfer-encoding
// past ~2 KB, and both SCPD documents exceed that — several older Samsung, Sony
// and Panasonic DLNA stacks cannot parse chunked SOAP or SCPD.
func TestResponsesAreNotChunked(t *testing.T) {
	srv, _ := buildTestServer(t)
	defer srv.Close()

	for _, path := range []string{upnp.PathDeviceDesc, upnp.PathSCPDContentDir, upnp.PathSCPDConnMgr} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if len(resp.TransferEncoding) > 0 {
			t.Errorf("%s: Transfer-Encoding %v, want none", path, resp.TransferEncoding)
		}
		if resp.ContentLength != int64(len(body)) {
			t.Errorf("%s: Content-Length %d, body %d bytes", path, resp.ContentLength, len(body))
		}
	}

	// The SOAP control response too.
	resp := rawBrowse(t, srv.URL, "0", "BrowseDirectChildren", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(resp.TransferEncoding) > 0 {
		t.Errorf("SOAP response Transfer-Encoding %v, want none", resp.TransferEncoding)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("SOAP Content-Length %d, body %d bytes", resp.ContentLength, len(body))
	}
}

// A SOAP envelope may legally carry a <s:Header>. It used to be decoded as the
// action element, yielding empty arguments and a 402 fault for the whole request.
func TestBrowseWithSOAPHeader(t *testing.T) {
	srv, _ := buildTestServer(t)
	defer srv.Close()

	header := `<s:Header><foo xmlns="urn:example">bar</foo></s:Header>`
	resp := rawBrowse(t, srv.URL, "0", "BrowseDirectChildren", header)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("Browse with a SOAP Header returned %d: %s", resp.StatusCode, b)
	}

	var env struct {
		Result string `xml:"Body>BrowseResponse>Result"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Result, "Movies") {
		t.Errorf("Browse with a header returned %q, expected the Movies container", env.Result)
	}
}

// RequestedCount=0 legally means "all remaining", and several clients send it.
// The server must cap the page rather than building an unbounded DIDL document,
// while still reporting the true total so clients can page.
func TestBrowseCapsUnboundedRequestedCount(t *testing.T) {
	const files = 40
	srv, _, _ := buildTestServerWithLibrary(t, func(movies string) {
		for i := range files {
			writeFile(t, filepath.Join(movies, fmt.Sprintf("Film %03d.mp4", i)), "x")
		}
	})
	defer srv.Close()

	root := browse(t, srv.URL, "0", "BrowseDirectChildren")
	moviesID := root.Containers[0].ID

	// Ask for one page of 10 and check the reported total covers everything.
	returned, total := browseCounts(t, srv.URL, moviesID, 0, 10)
	if returned != 10 {
		t.Errorf("NumberReturned = %d, want 10", returned)
	}
	if total != files {
		t.Errorf("TotalMatches = %d, want %d", total, files)
	}

	// A second page must contain different objects, proving the offset reaches
	// the query rather than re-slicing the same preloaded list.
	first := browsePage(t, srv.URL, moviesID, 0, 5)
	second := browsePage(t, srv.URL, moviesID, 5, 5)
	if len(first) != 5 || len(second) != 5 {
		t.Fatalf("page sizes = %d and %d, want 5 and 5", len(first), len(second))
	}
	if first[0] == second[0] {
		t.Errorf("StartingIndex ignored: both pages begin with %q", first[0])
	}
}

// SortCriteria was parsed and then never used, so "-dc:title" silently returned
// ascending results.
func TestBrowseHonoursDescendingSort(t *testing.T) {
	srv, _, _ := buildTestServerWithLibrary(t, func(movies string) {
		for _, n := range []string{"Alpha.mp4", "Bravo.mp4", "Charlie.mp4"} {
			writeFile(t, filepath.Join(movies, n), "x")
		}
	})
	defer srv.Close()

	root := browse(t, srv.URL, "0", "BrowseDirectChildren")
	moviesID := root.Containers[0].ID

	asc := browsePageSorted(t, srv.URL, moviesID, "+dc:title")
	desc := browsePageSorted(t, srv.URL, moviesID, "-dc:title")
	if len(asc) != 3 || len(desc) != 3 {
		t.Fatalf("got %d ascending and %d descending items, want 3 each", len(asc), len(desc))
	}
	if asc[0] != "Alpha" || desc[0] != "Charlie" {
		t.Errorf("ascending starts %q, descending starts %q; want Alpha and Charlie", asc[0], desc[0])
	}
}

// --- helpers ---

type didlDoc struct {
	XMLName    xml.Name `xml:"DIDL-Lite"`
	Containers []struct {
		ID       string `xml:"id,attr"`
		ParentID string `xml:"parentID,attr"`
		Title    string `xml:"title"`
	} `xml:"container"`
	Items []struct {
		ID    string `xml:"id,attr"`
		Title string `xml:"title"`
		Res   []struct {
			ProtocolInfo string `xml:"protocolInfo,attr"`
			URL          string `xml:",chardata"`
		} `xml:"res"`
	} `xml:"item"`
}

// browseRequest posts a Browse action and returns the raw response.
func browseRequest(t testing.TB, base, objectID, flag, header string, start, count int, sort string) *http.Response {
	t.Helper()
	soapBody := `<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		header +
		`<s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<ObjectID>` + objectID + `</ObjectID>` +
		`<BrowseFlag>` + flag + `</BrowseFlag>` +
		`<Filter>*</Filter>` +
		`<StartingIndex>` + strconv.Itoa(start) + `</StartingIndex>` +
		`<RequestedCount>` + strconv.Itoa(count) + `</RequestedCount>` +
		`<SortCriteria>` + sort + `</SortCriteria></u:Browse></s:Body></s:Envelope>`

	req, _ := http.NewRequest("POST", base+upnp.PathCtlContentDir, strings.NewReader(soapBody))
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func rawBrowse(t testing.TB, base, objectID, flag, header string) *http.Response {
	t.Helper()
	return browseRequest(t, base, objectID, flag, header, 0, 0, "")
}

func browse(t testing.TB, base, objectID, flag string) didlDoc {
	t.Helper()
	resp := browseRequest(t, base, objectID, flag, "", 0, 0, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("Browse status = %d: %s", resp.StatusCode, b)
	}
	return decodeDIDL(t, resp)
}

// browseResponse is the full SOAP Browse response, including the paging counters.
type browseResponse struct {
	Result         string `xml:"Body>BrowseResponse>Result"`
	NumberReturned int    `xml:"Body>BrowseResponse>NumberReturned"`
	TotalMatches   int    `xml:"Body>BrowseResponse>TotalMatches"`
}

func decodeBrowse(t testing.TB, resp *http.Response) browseResponse {
	t.Helper()
	var env browseResponse
	if err := xml.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode SOAP response: %v", err)
	}
	return env
}

func decodeDIDL(t testing.TB, resp *http.Response) didlDoc {
	t.Helper()
	env := decodeBrowse(t, resp)
	var doc didlDoc
	if err := xml.Unmarshal([]byte(env.Result), &doc); err != nil {
		t.Fatalf("decode DIDL %q: %v", env.Result, err)
	}
	return doc
}

func browseCounts(t testing.TB, base, objectID string, start, count int) (returned, total int) {
	t.Helper()
	resp := browseRequest(t, base, objectID, "BrowseDirectChildren", "", start, count, "")
	defer resp.Body.Close()
	env := decodeBrowse(t, resp)
	return env.NumberReturned, env.TotalMatches
}

// browsePage returns the item titles of one page.
func browsePage(t testing.TB, base, objectID string, start, count int) []string {
	t.Helper()
	resp := browseRequest(t, base, objectID, "BrowseDirectChildren", "", start, count, "")
	defer resp.Body.Close()
	doc := decodeDIDL(t, resp)
	out := make([]string, 0, len(doc.Items))
	for _, it := range doc.Items {
		out = append(out, it.Title)
	}
	return out
}

func browsePageSorted(t testing.TB, base, objectID, sort string) []string {
	t.Helper()
	resp := browseRequest(t, base, objectID, "BrowseDirectChildren", "", 0, 0, sort)
	defer resp.Body.Close()
	doc := decodeDIDL(t, resp)
	out := make([]string, 0, len(doc.Items))
	for _, it := range doc.Items {
		out = append(out, it.Title)
	}
	return out
}

func httpGet(t testing.TB, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
