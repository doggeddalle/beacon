package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"beacon/internal/config"
	"beacon/internal/logging"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestServer builds a server over a temp library on an ephemeral port.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	movies := filepath.Join(dir, "movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movies, "Film.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Server.HTTPPort = 0 // ephemeral: never collide with a real Beacon
	cfg.Server.UUID = "11111111-2222-3333-4444-555555555555"
	cfg.Server.DataDir = filepath.Join(dir, "data")
	cfg.Library.Folders = []config.Folder{{Name: "Movies", Path: movies}}
	// Keep the periodic tiers out of the way; these tests drive scans directly.
	cfg.Index.ReconcileInterval = config.Duration(time.Hour)
	cfg.Index.IntegrityInterval = config.Duration(0)

	_, ring := logging.Setup("error", "text")
	srv, err := New(&cfg, discardLog(), ring, "test")
	if err != nil {
		t.Skipf("cannot build server in this environment: %v", err)
	}
	return srv
}

// Shutdown must not return until the background workers have stopped. Run's
// deferred store.Close() used to fire while the enricher was mid-batch and the
// indexer mid-walk, producing "database is closed" errors on every exit.
func TestRunWaitsForWorkersBeforeClosingStore(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Let startup get as far as the initial scan and the watcher.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	// The WaitGroup must be fully drained: a non-zero counter here means a worker
	// outlived the store it writes to.
	drained := make(chan struct{})
	go func() { defer close(drained); srv.workers.Wait() }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Error("background workers still running after Run returned")
	}
}

// Cancelling before Run even starts must still shut down cleanly.
func TestRunWithAlreadyCancelledContext(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("Run did not return for an already-cancelled context")
	}
}

// A full scan must complete and index the library.
func TestRunScanIndexesLibrary(t *testing.T) {
	srv := newTestServer(t)
	defer srv.store.Close()

	srv.runScan(context.Background())

	n, err := srv.store.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 { // the Movies root plus Film.mp4
		t.Errorf("library has %d rows after a scan, want at least 2", n)
	}
	if srv.scanning.Load() {
		t.Error("scanning flag left set after the scan finished")
	}
}
