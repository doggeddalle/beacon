package upnp_test

import (
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// BenchmarkBrowsePagedFolder is the load-bearing one: a fixed 50-row page against
// folders of wildly different sizes. The numbers should be flat, because
// pagination now happens in SQL.
//
// Previously every Browse loaded and converted the entire folder before slicing,
// so a 50-row request against 5000 files cost 100× what it cost against 50 —
// then the DIDL was copied and escaped through several growing strings.Builders
// on top of that.
func BenchmarkBrowsePagedFolder(b *testing.B) {
	for _, files := range []int{100, 5000} {
		b.Run(fmt.Sprintf("folder=%d/page=50", files), func(b *testing.B) {
			srv, moviesID := benchLibrary(b, files)
			defer srv.Close()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				drain(b, srv.URL, moviesID, 50)
			}
		})
	}
}

// BenchmarkBrowseUnboundedRequest uses RequestedCount=0 ("all remaining"), the
// shape VLC, Windows Explorer and several TVs send. Cost must track the server's
// page cap rather than the folder size, or a big folder is an OOM on a 512 MB NAS.
func BenchmarkBrowseUnboundedRequest(b *testing.B) {
	for _, files := range []int{100, 5000} {
		b.Run(fmt.Sprintf("folder=%d/page=all", files), func(b *testing.B) {
			srv, moviesID := benchLibrary(b, files)
			defer srv.Close()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				drain(b, srv.URL, moviesID, 0)
			}
		})
	}
}

func benchLibrary(b *testing.B, files int) (srv *httptest.Server, moviesID string) {
	b.Helper()
	srv, _, _ = buildTestServerWithLibrary(b, func(movies string) {
		for i := range files {
			writeFile(b, filepath.Join(movies, fmt.Sprintf("Film %05d.mp4", i)), "x")
		}
	})
	root := browse(b, srv.URL, "0", "BrowseDirectChildren")
	if len(root.Containers) == 0 {
		b.Fatal("benchmark library has no root container")
	}
	return srv, root.Containers[0].ID
}

func drain(b *testing.B, base, objectID string, count int) {
	b.Helper()
	resp := browseRequest(b, base, objectID, "BrowseDirectChildren", "", 0, count, "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
