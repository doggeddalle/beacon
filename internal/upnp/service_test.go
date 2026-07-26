package upnp_test

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"beacon/internal/content"
	"beacon/internal/upnp"
)

// buildTestServer creates a MediaServer over a temp library:
//
//	root/
//	  Movies/
//	    Sample.mp4   (12 bytes of known content)
func buildTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	movies := filepath.Join(dir, "Movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	const payload = "0123456789AB"
	if err := os.WriteFile(filepath.Join(movies, "Sample.mp4"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	backend := content.NewFS([]content.Root{{Name: "Movies", Path: movies}})
	info := upnp.DeviceInfo{FriendlyName: "Test & Beacon", UDN: "uuid:test-1234"}
	h, err := upnp.NewHandler(info, backend, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(h), payload
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

func browse(t *testing.T, base, objectID, flag string) didlDoc {
	t.Helper()
	soapBody := `<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<ObjectID>` + objectID + `</ObjectID>` +
		`<BrowseFlag>` + flag + `</BrowseFlag>` +
		`<Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount>` +
		`<SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`

	req, _ := http.NewRequest("POST", base+upnp.PathCtlContentDir, strings.NewReader(soapBody))
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("Browse status = %d: %s", resp.StatusCode, b)
	}

	// Extract <Result> (the escaped DIDL) from the SOAP response.
	var env struct {
		Result string `xml:"Body>BrowseResponse>Result"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode SOAP response: %v", err)
	}
	var doc didlDoc
	if err := xml.Unmarshal([]byte(env.Result), &doc); err != nil {
		t.Fatalf("decode DIDL %q: %v", env.Result, err)
	}
	return doc
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
