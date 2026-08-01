// Package admin serves the browser-based admin dashboard: an embedded single
// page plus a small JSON API for status, rescans, folder management and logs.
package admin

import (
	"embed"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

//go:embed assets/index.html
var assets embed.FS

// Folder is a configured media source.
type Folder struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Status is the dashboard's live snapshot of the server.
type Status struct {
	Version           string   `json:"version"`
	HTTPAddr          string   `json:"httpAddr"`
	LibrarySize       int      `json:"librarySize"`
	Folders           []Folder `json:"folders"`
	WatcherActive     bool     `json:"watcherActive"`
	WatcherDegraded   bool     `json:"watcherDegraded"` // some dirs unwatched (inotify limit)
	FFprobe           bool     `json:"ffprobe"`
	FFmpeg            bool     `json:"ffmpeg"`
	ReconcileInterval string   `json:"reconcileInterval"`
	IntegrityInterval string   `json:"integrityInterval"`
	UptimeSeconds     int64    `json:"uptimeSeconds"`
	Scanning          bool     `json:"scanning"`
}

// Controller is implemented by the server to back the dashboard.
type Controller interface {
	Status() Status
	Rescan()
	AddFolder(name, path string) error
	RemoveFolder(path string) error
	Logs() []string
}

// Handler is the admin HTTP handler (dashboard + API).
type Handler struct {
	ctrl Controller
	log  *slog.Logger
	mux  *http.ServeMux
}

// New builds the admin handler.
func New(ctrl Controller, log *slog.Logger) *Handler {
	h := &Handler{ctrl: ctrl, log: log, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /{$}", h.serveIndex)
	h.mux.HandleFunc("GET /admin/api/status", h.getStatus)
	h.mux.HandleFunc("POST /admin/api/rescan", guard(h.postRescan))
	h.mux.HandleFunc("GET /admin/api/folders", h.getFolders)
	h.mux.HandleFunc("POST /admin/api/folders", guard(h.postFolder))
	h.mux.HandleFunc("DELETE /admin/api/folders", guard(h.deleteFolder))
	h.mux.HandleFunc("GET /admin/api/logs", h.getLogs)
	return h
}

// guard protects a mutating endpoint against cross-site requests.
//
// The dashboard has no authentication by design (trusted LAN), but that made
// every write endpoint reachable from any web page a LAN user happened to visit:
// an HTML form posting `text/plain` is a CORS "simple request", so it is sent
// without a preflight and the browser attaches no protection. Requiring JSON and
// a same-origin Origin/Referer reduces these to requests the dashboard itself
// makes; both checks are ones a cross-origin form cannot satisfy.
func guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden,
				"cross-origin request refused; use the Beacon dashboard on this host")
			return
		}
		// A body-bearing request must be JSON, which a simple form cannot send.
		if r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
			ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || ct != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType,
					"Content-Type must be application/json")
				return
			}
		}
		next(w, r)
	}
}

// sameOrigin reports whether a mutating request came from the dashboard itself.
// A request with neither Origin nor Referer is allowed: browsers always send at
// least one on cross-origin requests, while curl and the like send neither.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			u, err := url.Parse(ref)
			if err != nil {
				return false
			}
			return u.Host == r.Host
		}
		return true // not a browser-initiated cross-site request
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.ctrl.Status())
}

func (h *Handler) postRescan(w http.ResponseWriter, r *http.Request) {
	h.ctrl.Rescan()
	writeJSON(w, http.StatusOK, map[string]string{"status": "rescan started"})
}

func (h *Handler) getFolders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.ctrl.Status().Folders)
}

func (h *Handler) postFolder(w http.ResponseWriter, r *http.Request) {
	var req Folder
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	req.Name = strings.TrimSpace(req.Name)
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := h.ctrl.AddFolder(req.Name, req.Path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.ctrl.Status())
}

func (h *Handler) deleteFolder(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := h.ctrl.RemoveFolder(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.ctrl.Status())
}

func (h *Handler) getLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"lines": h.ctrl.Logs()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
