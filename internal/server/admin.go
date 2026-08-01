package server

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"beacon/internal/admin"
	"beacon/internal/config"
)

// The Server implements admin.Controller to back the dashboard.

// Status reports a live snapshot of the server.
func (s *Server) Status() admin.Status {
	s.mu.Lock()
	folders := make([]admin.Folder, 0, len(s.cfg.Library.Folders))
	for _, f := range s.cfg.Library.Folders {
		folders = append(folders, admin.Folder{Name: f.Name, Path: f.Path})
	}
	reconcile := s.cfg.Index.ReconcileInterval.String()
	integrity := s.cfg.Index.IntegrityInterval.String()
	s.mu.Unlock()

	degraded := false
	if w := s.watcher.Load(); w != nil {
		degraded = w.Degraded()
	}

	return admin.Status{
		Version:           s.version,
		HTTPAddr:          s.addr,
		LibrarySize:       s.librarySize(),
		Folders:           folders,
		WatcherActive:     s.watcherActive.Load(),
		WatcherDegraded:   degraded,
		FFprobe:           s.prober.Available(),
		FFmpeg:            s.thumbs.Available(),
		ReconcileInterval: reconcile,
		IntegrityInterval: integrity,
		UptimeSeconds:     int64(time.Since(s.startTime).Seconds()),
		Scanning:          s.scanning.Load(),
	}
}

// countTTL bounds how stale the dashboard's library size may be. The dashboard
// polls status every 3s per open tab, and COUNT(*) over items is a full scan —
// on a 100k-item library that was 20 table scans a minute, per tab, forever.
const countTTL = 10 * time.Second

// librarySize returns the indexed item count, recomputed at most once per
// countTTL.
func (s *Server) librarySize() int {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	if time.Since(s.countAt) < countTTL {
		return s.countVal
	}
	n, err := s.store.Count()
	if err != nil {
		return s.countVal // keep the last good value rather than reporting zero
	}
	s.countVal, s.countAt = n, time.Now()
	return n
}

// invalidateCount forces the next librarySize call to re-query. Called when the
// library is known to have changed.
func (s *Server) invalidateCount() {
	s.countMu.Lock()
	s.countAt = time.Time{}
	s.countMu.Unlock()
}

// Rescan triggers a background full scan.
func (s *Server) Rescan() {
	if s.rootCtx != nil {
		s.goTracked(func() { s.runScan(s.rootCtx) })
	}
}

// Logs returns the recent log lines retained in memory.
func (s *Server) Logs() []string { return s.logRing.Lines() }

// AddFolder validates and adds a media folder, then applies it live.
func (s *Server) AddFolder(name, path string) error {
	s.log.Info("add folder requested", "name", fmt.Sprintf("%q", name), "path", fmt.Sprintf("%q", path))
	abs, err := filepath.Abs(path)
	if err != nil {
		s.log.Warn("add folder rejected (invalid path)", "path", fmt.Sprintf("%q", path), "err", err)
		return fmt.Errorf("invalid path: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		s.log.Warn("add folder rejected (stat failed)", "path", fmt.Sprintf("%q", abs), "err", err)
		return fmt.Errorf("path does not exist or is not readable: %s", abs)
	}
	if !fi.IsDir() {
		s.log.Warn("add folder rejected (not a directory)", "path", fmt.Sprintf("%q", abs))
		return fmt.Errorf("not a directory: %s", abs)
	}
	// Resolve symlinks before the confinement check, or a link inside an allowed
	// directory could point anywhere on the NAS.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	s.mu.Lock()
	// The dashboard is unauthenticated, so an unconfined add would let any LAN
	// peer index and stream the entire filesystem.
	if !s.cfg.PathAllowed(abs) {
		allowed := s.cfg.AllowedRootsDescription()
		s.mu.Unlock()
		s.log.Warn("add folder rejected (outside the allowed roots)",
			"path", fmt.Sprintf("%q", abs), "allowed", allowed)
		return fmt.Errorf("path is outside the allowed library roots (%s); "+
			"add it to library.allowed_parents in the config file to permit it", allowed)
	}
	for _, f := range s.cfg.Library.Folders {
		if f.Path == abs {
			s.mu.Unlock()
			return fmt.Errorf("folder already added: %s", abs)
		}
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	name = s.uniqueNameLocked(name)
	newFolders := make([]config.Folder, len(s.cfg.Library.Folders))
	copy(newFolders, s.cfg.Library.Folders)
	newFolders = append(newFolders, config.Folder{Name: name, Path: abs})
	s.mu.Unlock()

	return s.applyFolders(newFolders)
}

// RemoveFolder removes a media folder (matched by path) and applies it live.
func (s *Server) RemoveFolder(path string) error {
	abs, _ := filepath.Abs(path)
	s.mu.Lock()
	var newFolders []config.Folder
	found := false
	for _, f := range s.cfg.Library.Folders {
		if f.Path == abs || f.Path == path {
			found = true
			continue
		}
		newFolders = append(newFolders, f)
	}
	s.mu.Unlock()
	if !found {
		return fmt.Errorf("folder not found: %s", path)
	}
	return s.applyFolders(newFolders)
}

// applyFolders persists the new folder set, purges removed roots, re-points the
// indexer/watcher and kicks off a rescan.
func (s *Server) applyFolders(newFolders []config.Folder) error {
	s.mu.Lock()
	old := s.cfg.Library.Folders
	s.cfg.Library.Folders = newFolders
	if err := s.cfg.Save(); err != nil {
		s.cfg.Library.Folders = old // roll back on failure
		s.mu.Unlock()
		return fmt.Errorf("save config: %w", err)
	}
	removed := removedFolders(old, newFolders)
	s.mu.Unlock()

	s.indexer.SetRoots(foldersToRoots(newFolders))
	for _, f := range removed {
		if n, err := s.store.DeleteSubtree(f.Path); err == nil && n > 0 {
			s.log.Info("removed folder from library", "path", f.Path, "rows", n)
		}
	}
	if s.rootCtx != nil {
		s.startWatcher(s.rootCtx)
		go s.runScan(s.rootCtx)
	}
	s.cd.BumpUpdateID()
	return nil
}

// uniqueNameLocked returns a folder name not already in use (caller holds mu).
func (s *Server) uniqueNameLocked(name string) string {
	taken := func(n string) bool {
		for _, f := range s.cfg.Library.Folders {
			if f.Name == n {
				return true
			}
		}
		return false
	}
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)", name, i)
		if !taken(cand) {
			return cand
		}
	}
}

// removedFolders returns folders present in old but not in new (matched by path).
func removedFolders(old, new []config.Folder) []config.Folder {
	keep := map[string]bool{}
	for _, f := range new {
		keep[f.Path] = true
	}
	var removed []config.Folder
	for _, f := range old {
		if !keep[f.Path] {
			removed = append(removed, f)
		}
	}
	return removed
}
