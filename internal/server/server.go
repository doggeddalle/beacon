// Package server wires the content backend, UPnP HTTP services, the admin
// dashboard and SSDP discovery into a single runnable media server.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"beacon/internal/admin"
	"beacon/internal/config"
	"beacon/internal/library"
	"beacon/internal/logging"
	"beacon/internal/meta"
	"beacon/internal/netutil"
	"beacon/internal/ssdp"
	"beacon/internal/store"
	"beacon/internal/thumbs"
	"beacon/internal/upnp"
)

// Server is a fully-assembled DLNA MediaServer.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	logRing *logging.Ring
	version string

	http *http.Server
	ln   net.Listener
	ssdp *ssdp.Server
	addr string // actual bound "ip:port"

	store    *store.Store
	indexer  *library.Indexer
	enricher *library.Enricher
	prober   *meta.Prober
	thumbs   *thumbs.Thumbnailer
	cd       *upnp.ContentDirectory

	startTime time.Time

	workers sync.WaitGroup // background goroutines that touch the store

	countMu  sync.Mutex // guards the cached dashboard library size
	countVal int
	countAt  time.Time

	mu            sync.Mutex // guards cfg.Library.Folders and the watcher lifecycle
	rootCtx       context.Context
	watcherCancel context.CancelFunc
	watcher       atomic.Pointer[library.Watcher]
	watcherActive atomic.Bool
	scanning      atomic.Bool
}

// New builds the server from configuration.
func New(cfg *config.Config, log *slog.Logger, logRing *logging.Ring, version string) (*Server, error) {
	ip, err := netutil.PrimaryIPv4(cfg.Server.Interface)
	if err != nil {
		return nil, fmt.Errorf("determine LAN IP: %w", err)
	}

	dbPath := filepath.Join(cfg.Server.DataDir, "beacon.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open library db %s: %w", dbPath, err)
	}

	indexer := library.NewIndexer(st, foldersToRoots(cfg.Library.Folders), log)

	ffmpegPath := meta.LocateExecutable("ffmpeg", cfg.Meta.FFmpegPath)
	tn := thumbs.New(ffmpegPath, filepath.Join(cfg.Server.DataDir, "thumbs"),
		cfg.Index.Workers, cfg.Meta.ThumbnailWidth, log)
	if tn.Available() {
		log.Info("thumbnails active", "ffmpeg", ffmpegPath)
	} else {
		log.Warn("ffmpeg not found — thumbnails disabled; sidecar/folder poster images still used")
	}

	backend := library.NewBackend(st, func() int {
		n, _ := st.CountChildren(store.RootParent)
		return n
	}, tn)

	info := upnp.DeviceInfo{
		FriendlyName: cfg.Server.FriendlyName,
		UDN:          "uuid:" + cfg.Server.UUID,
		Manufacturer: "Beacon",
		ModelName:    "Beacon DLNA MediaServer",
		ModelNumber:  "1",
	}
	upnpHandler, err := upnp.NewHandler(info, backend, log)
	if err != nil {
		st.Close()
		return nil, err
	}
	cd := upnpHandler.ContentDirectory()

	prober := meta.NewProber(cfg.Meta.FFprobePath)
	// Items whose probe failed while ffprobe was missing or broken are re-queued
	// once it is available again, so installing ffmpeg after the fact fills in the
	// durations that were previously left blank.
	if prober.Available() {
		if n, err := st.ResetFailedProbes(); err != nil {
			log.Warn("could not re-queue failed probes", "err", err)
		} else if n > 0 {
			log.Info("re-queued items for metadata probing", "count", n)
		}
	}
	enricher := library.NewEnricher(st, prober, cfg.Index.Workers, log, func() {
		cd.BumpUpdateID()
	})

	// Bind every interface unless one was configured explicitly.
	//
	// Binding a single detected address made the server unreachable from any
	// other subnet on a multi-homed NAS, and left the socket bound to an address
	// the host no longer owned after a DHCP renewal. Media URLs are already built
	// per-request from r.Host, so nothing downstream depends on the bind address.
	bindHost := ""
	if cfg.Server.Interface != "" {
		bindHost = ip.String()
	}
	addr := net.JoinHostPort(bindHost, strconv.Itoa(cfg.Server.HTTPPort))
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("cannot bind %s: %w\n"+
			"  Another DLNA/UPnP server is probably using this port — Asustor's built-in\n"+
			"  media server defaults to 8200. Change server.http_port in the config to a\n"+
			"  free port (e.g. 8322), or disable the other server", addr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// The address shown to the user and used for log links; SSDP advertises the
	// right per-interface address itself.
	displayAddr := net.JoinHostPort(ip.String(), strconv.Itoa(port))

	ssdpSrv, err := ssdp.New(ssdp.Config{
		UDN:          info.UDN,
		DeviceType:   upnp.DeviceType,
		Services:     []string{upnp.ServiceContentDirectory, upnp.ServiceConnectionManager},
		LocationPath: upnp.PathDeviceDesc,
		HTTPPort:     port,
		Interface:    cfg.Server.Interface,
		ServerString: serverString(version),
		Logger:       log,
	})
	if err != nil {
		ln.Close()
		st.Close()
		return nil, err
	}

	s := &Server{
		cfg: cfg, log: log, logRing: logRing, version: version,
		ln: ln, ssdp: ssdpSrv, addr: displayAddr,
		store: st, indexer: indexer, enricher: enricher, prober: prober, thumbs: tn, cd: cd,
		startTime: time.Now(),
	}

	// The admin dashboard is the Controller-backed handler; a small dispatcher
	// routes "/" and "/admin/*" to it and everything else to the UPnP handler.
	adminHandler := admin.New(s, log)
	s.http = &http.Server{
		Handler:      withServerHeader(rootHandler(upnpHandler, adminHandler), serverString(version)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // media streams can be long-lived
		IdleTimeout:  60 * time.Second,
	}
	return s, nil
}

// serverString is the SERVER header advertised over SSDP and HTTP. DLNA expects
// "<OS>/<ver> UPnP/1.0 <product>/<ver>"; it was previously a hardcoded constant
// that still read "Beacon/0.1" several releases later.
func serverString(version string) string {
	return fmt.Sprintf("%s/%s UPnP/1.0 Beacon/%s", osName(), osVersion(), version)
}

func osName() string {
	if runtime.GOOS == "linux" {
		return "Linux"
	}
	return runtime.GOOS
}

// osVersion is intentionally coarse: clients only match on the shape.
func osVersion() string { return "1.0" }

// withServerHeader stamps the DLNA SERVER header on every response. The
// guidelines require a DMS to identify itself this way, and Go sends no SERVER
// header of its own.
func withServerHeader(next http.Handler, server string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", server)
		next.ServeHTTP(w, r)
	})
}

// rootHandler dispatches the dashboard/API vs. the DLNA endpoints.
func rootHandler(upnpH, adminH http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == "/" || strings.HasPrefix(p, "/admin") {
			adminH.ServeHTTP(w, r)
			return
		}
		upnpH.ServeHTTP(w, r)
	})
}

// Run starts everything and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	defer s.store.Close()
	s.rootCtx = ctx

	s.log.Info("media server listening",
		"http", s.addr,
		"dashboard", "http://"+s.addr+"/",
		"device_desc", "http://"+s.addr+upnp.PathDeviceDesc,
	)

	// Auto-update engine: enricher + initial scan + Tier-1 watcher, plus the
	// periodic Tier-2/Tier-3 scans. Every one of these touches the store, so they
	// are tracked and waited on before it closes.
	s.goTracked(func() { s.enricher.Run(ctx) })
	s.goTracked(func() {
		s.log.Info("starting initial library scan")
		s.runScan(ctx)
		if ctx.Err() == nil {
			s.startWatcher(ctx)
			s.sweepThumbs()
		}
	})
	s.goTracked(func() { s.runPeriodicScan(ctx, s.cfg.Index.ReconcileInterval.D(), "reconcile (tier 2)") })
	s.goTracked(func() { s.runPeriodicScan(ctx, s.cfg.Index.IntegrityInterval.D(), "integrity (tier 3)") })

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http: %w", err)
		}
	}()

	// SSDP is waited on separately: its byebye is sent during shutdown, and if the
	// process exits first every client keeps a stale entry for up to max-age.
	ssdpDone := make(chan struct{})
	go func() {
		defer close(ssdpDone)
		if err := s.ssdp.Run(ctx); err != nil {
			s.log.Error("ssdp discovery unavailable — TVs may not auto-find the server", "err", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		s.log.Error("server error", "err", err)
		runErr = err
	}

	s.shutdownHTTP()
	s.waitForWorkers(ssdpDone)
	return runErr
}

// goTracked runs fn in a goroutine the shutdown path will wait for.
func (s *Server) goTracked(fn func()) {
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		fn()
	}()
}

// waitForWorkers blocks until the background workers have stopped, so the store
// is only closed once nothing is still writing to it.
//
// Without this the deferred store.Close() in Run raced the enricher mid-batch and
// the indexer mid-walk, producing "database is closed" errors on every shutdown.
func (s *Server) waitForWorkers(ssdpDone <-chan struct{}) {
	// The watcher runs under its own cancellable context, derived from the root
	// context, so cancelling the root already told it to stop.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.workers.Wait()
		<-ssdpDone
	}()

	select {
	case <-done:
	case <-time.After(shutdownGrace):
		s.log.Warn("background workers did not stop in time; closing the database anyway",
			"grace", shutdownGrace.String())
	}
}

// runScan performs one non-destructive full scan, guarded so scans never
// overlap, updating the "scanning" indicator and nudging the enricher.
func (s *Server) runScan(ctx context.Context) {
	if !s.scanning.CompareAndSwap(false, true) {
		return // a scan is already in progress
	}
	defer s.scanning.Store(false)
	if err := s.indexer.FullScan(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("library scan failed", "err", err)
	}
	s.invalidateCount()
	s.enricher.Kick()
	s.cd.BumpUpdateID()
}

func (s *Server) runPeriodicScan(ctx context.Context, interval time.Duration, label string) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.log.Info("periodic scan", "tier", label)
			s.runScan(ctx)
		}
	}
}

// Thumbnail cache limits. Keys include the source mtime, so every edit orphans
// the previous file and nothing else ever reclaims them — on ADM the cache sits
// on the small system partition.
const (
	thumbCacheMaxAge   = 60 * 24 * time.Hour
	thumbCacheMaxBytes = 256 << 20 // 256 MiB
)

// sweepThumbs reclaims orphaned and stale thumbnails.
func (s *Server) sweepThumbs() {
	if removed, freed := s.thumbs.Sweep(thumbCacheMaxAge, thumbCacheMaxBytes); removed > 0 {
		s.log.Info("swept thumbnail cache", "removed", removed, "freed_bytes", freed)
	}
}

// startWatcher stops any running watcher and starts a fresh one over the
// current folder set (called at startup and whenever folders change).
func (s *Server) startWatcher(parent context.Context) {
	s.mu.Lock()
	if s.watcherCancel != nil {
		s.watcherCancel()
	}
	wctx, cancel := context.WithCancel(parent)
	s.watcherCancel = cancel
	roots := foldersToRoots(s.cfg.Library.Folders)
	s.mu.Unlock()

	w := library.NewWatcher(s.store, roots, s.cfg.Index.WriteSettleDelay.D(), s.log,
		func() { // a change was applied
			s.invalidateCount()
			s.cd.BumpUpdateID()
			s.enricher.Kick()
		},
		func() { // events were lost; only a rescan can restore accuracy
			s.goTracked(func() { s.runScan(wctx) })
		},
	)
	s.watcher.Store(w)
	s.watcherActive.Store(true)
	s.goTracked(func() {
		err := w.Run(wctx)
		if wctx.Err() == nil { // exited on its own (not a reload/shutdown)
			s.watcherActive.Store(false)
			if err != nil {
				s.log.Error("real-time watcher stopped — periodic scans still run", "err", err)
			}
		}
	})
}

// shutdownGrace bounds how long shutdown waits for background workers. A scan
// mid-walk stops promptly on context cancellation; this only guards against a
// worker wedged on unresponsive storage.
const shutdownGrace = 10 * time.Second

func (s *Server) shutdownHTTP() {
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutCtx); err != nil {
		s.log.Warn("http shutdown", "err", err)
	}
}

func foldersToRoots(folders []config.Folder) []library.Root {
	roots := make([]library.Root, 0, len(folders))
	for _, f := range folders {
		roots = append(roots, library.Root{Name: f.Name, Path: f.Path})
	}
	return roots
}
