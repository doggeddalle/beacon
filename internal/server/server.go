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

	mu            sync.Mutex // guards cfg.Library.Folders and the watcher lifecycle
	rootCtx       context.Context
	watcherCancel context.CancelFunc
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
	enricher := library.NewEnricher(st, prober, cfg.Index.Workers, log, func() {
		cd.BumpUpdateID()
	})

	addr := net.JoinHostPort(ip.String(), strconv.Itoa(cfg.Server.HTTPPort))
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("cannot bind %s: %w\n"+
			"  Another DLNA/UPnP server is probably using this port — Asustor's built-in\n"+
			"  media server defaults to 8200. Change server.http_port in the config to a\n"+
			"  free port (e.g. 8322), or disable the other server", addr, err)
	}
	actualAddr := ln.Addr().String()

	ssdpSrv, err := ssdp.New(ssdp.Config{
		UDN:        info.UDN,
		DeviceType: upnp.DeviceType,
		Services:   []string{upnp.ServiceContentDirectory, upnp.ServiceConnectionManager},
		Location:   "http://" + actualAddr + upnp.PathDeviceDesc,
		Logger:     log,
	})
	if err != nil {
		ln.Close()
		st.Close()
		return nil, err
	}

	s := &Server{
		cfg: cfg, log: log, logRing: logRing, version: version,
		ln: ln, ssdp: ssdpSrv, addr: actualAddr,
		store: st, indexer: indexer, enricher: enricher, prober: prober, thumbs: tn, cd: cd,
		startTime: time.Now(),
	}

	// The admin dashboard is the Controller-backed handler; a small dispatcher
	// routes "/" and "/admin/*" to it and everything else to the UPnP handler.
	adminHandler := admin.New(s, log)
	s.http = &http.Server{
		Handler:      rootHandler(upnpHandler, adminHandler),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // media streams can be long-lived
		IdleTimeout:  60 * time.Second,
	}
	return s, nil
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
	// periodic Tier-2/Tier-3 scans.
	go s.enricher.Run(ctx)
	go func() {
		s.log.Info("starting initial library scan")
		s.runScan(ctx)
		if ctx.Err() == nil {
			s.startWatcher(ctx)
		}
	}()
	go s.runPeriodicScan(ctx, s.cfg.Index.ReconcileInterval.D(), "reconcile (tier 2)")
	go s.runPeriodicScan(ctx, s.cfg.Index.IntegrityInterval.D(), "integrity (tier 3)")

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http: %w", err)
		}
	}()
	go func() {
		if err := s.ssdp.Run(ctx); err != nil {
			s.log.Error("ssdp discovery unavailable — TVs may not auto-find the server", "err", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		s.log.Error("server error", "err", err)
		s.shutdownHTTP()
		return err
	}
	s.shutdownHTTP()
	return nil
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

	w := library.NewWatcher(s.store, roots, s.cfg.Index.WriteSettleDelay.D(), s.log, func() {
		s.cd.BumpUpdateID()
		s.enricher.Kick()
	})
	s.watcherActive.Store(true)
	go func() {
		err := w.Run(wctx)
		if wctx.Err() == nil { // exited on its own (not a reload/shutdown)
			s.watcherActive.Store(false)
			if err != nil {
				s.log.Error("real-time watcher stopped — periodic scans still run", "err", err)
			}
		}
	}()
}

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
