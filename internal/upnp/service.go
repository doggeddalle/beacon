package upnp

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"beacon/internal/content"
)

// Handler serves every HTTP endpoint of the MediaServer: the device
// description, the two service descriptions (SCPD), SOAP control, GENA event
// subscription (stubbed), and the media byte stream itself.
type Handler struct {
	info    DeviceInfo
	backend content.Backend
	subs    content.SubtitleProvider // non-nil if backend supports subtitles
	art     content.ArtworkProvider  // non-nil if backend supports artwork
	cd      *ContentDirectory
	cm      *ConnectionManager
	log     *slog.Logger
	mux     *http.ServeMux

	deviceDesc []byte
}

// NewHandler builds the MediaServer HTTP handler over the given content backend.
func NewHandler(info DeviceInfo, backend content.Backend, log *slog.Logger) (*Handler, error) {
	desc, err := deviceDescription(info)
	if err != nil {
		return nil, err
	}
	h := &Handler{
		info:       info,
		backend:    backend,
		cd:         NewContentDirectory(backend, log),
		cm:         NewConnectionManager(),
		log:        log,
		mux:        http.NewServeMux(),
		deviceDesc: desc,
	}
	if sp, ok := backend.(content.SubtitleProvider); ok {
		h.subs = sp
	}
	if ap, ok := backend.(content.ArtworkProvider); ok {
		h.art = ap
	}
	h.routes()
	return h, nil
}

// ContentDirectory exposes the CD service (used by later phases to bump the
// system update ID when the library changes).
func (h *Handler) ContentDirectory() *ContentDirectory { return h.cd }

func (h *Handler) routes() {
	h.mux.HandleFunc("GET "+PathDeviceDesc, h.serveXML(h.deviceDesc))
	h.mux.HandleFunc("GET "+PathSCPDContentDir, h.serveXML(scpdContentDirectory))
	h.mux.HandleFunc("GET "+PathSCPDConnMgr, h.serveXML(scpdConnectionManager))

	h.mux.HandleFunc("POST "+PathCtlContentDir, h.serveContentDirControl)
	h.mux.HandleFunc("POST "+PathCtlConnMgr, func(w http.ResponseWriter, r *http.Request) {
		h.cm.handleControl(w, r)
	})

	// GENA eventing is not implemented in Phase 1. Acknowledge SUBSCRIBE so
	// clients don't treat the device as broken.
	h.mux.HandleFunc(PathEvtContentDir, h.stubEvent)
	h.mux.HandleFunc(PathEvtConnMgr, h.stubEvent)

	h.mux.HandleFunc("GET "+PathMediaPrefix, h.serveMedia)
	h.mux.HandleFunc("HEAD "+PathMediaPrefix, h.serveMedia)

	if h.subs != nil {
		h.mux.HandleFunc("GET "+PathSubtitlePrefix, h.serveSubtitle)
		h.mux.HandleFunc("HEAD "+PathSubtitlePrefix, h.serveSubtitle)
	}
	if h.art != nil {
		h.mux.HandleFunc("GET "+PathThumbPrefix, h.serveThumb)
		h.mux.HandleFunc("HEAD "+PathThumbPrefix, h.serveThumb)
	}

	h.mux.HandleFunc("GET /{$}", h.serveRoot)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) serveXML(doc []byte) http.HandlerFunc {
	// Content-Length is explicit because both SCPD documents are over 3 KB, past
	// the point where Go falls back to chunked transfer-encoding — which several
	// older TV DLNA stacks cannot parse.
	length := strconv.Itoa(len(doc))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.Header().Set("Content-Length", length)
		_, _ = w.Write(doc)
	}
}

// setDLNAHeaders writes the transfer-mode and content-features headers a DLNA
// renderer expects on a media, subtitle or artwork response.
//
// contentFeatures is sent unconditionally rather than only when the client sets
// getcontentFeatures.dlna.org: the spec allows either, but many renderers read
// the header without ever asking for it. Subtitle and artwork responses
// previously carried neither header, and several Samsung firmwares silently drop
// a subtitle track whose response does not include them.
func setDLNAHeaders(w http.ResponseWriter, r *http.Request, mime, defaultMode string) {
	if tm := r.Header.Get("transferMode.dlna.org"); tm != "" {
		w.Header().Set("transferMode.dlna.org", tm) // echo what the client asked for
	} else {
		w.Header().Set("transferMode.dlna.org", defaultMode)
	}
	w.Header().Set("contentFeatures.dlna.org", content.ContentFeatures(mime))
}

func (h *Handler) serveContentDirControl(w http.ResponseWriter, r *http.Request) {
	// res URLs are absolute and host-relative to the requesting client.
	resURL := func(mediaID string) string {
		return "http://" + r.Host + PathMediaPrefix + mediaID
	}
	subURL := func(objectID string) string {
		return "http://" + r.Host + PathSubtitlePrefix + objectID
	}
	artURL := func(objectID string) string {
		return "http://" + r.Host + PathThumbPrefix + objectID
	}
	h.cd.handleControl(w, r, resURL, subURL, artURL)
}

func (h *Handler) stubEvent(w http.ResponseWriter, r *http.Request) {
	// Reply 200 to SUBSCRIBE/UNSUBSCRIBE without maintaining subscriptions.
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) serveMedia(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, PathMediaPrefix)
	if id == "" {
		http.NotFound(w, r)
		return
	}
	path, err := h.backend.FilePath(id)
	if err != nil {
		h.log.Warn("media not found", "id", id, "err", err)
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "cannot open file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot stat file", http.StatusInternalServerError)
		return
	}

	mime, _ := content.MimeType(fi.Name())
	if mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	setDLNAHeaders(w, r, mime, content.TransferModeFor(mime))

	// http.ServeContent handles Range requests, If-Modified-Since, HEAD and the
	// Accept-Ranges header — everything needed for smooth seeking.
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

func (h *Handler) serveSubtitle(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, PathSubtitlePrefix)
	if id == "" || h.subs == nil {
		http.NotFound(w, r)
		return
	}
	path, kind, err := h.subs.SubtitlePath(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot stat subtitle", http.StatusInternalServerError)
		return
	}
	subMime := subtitleMime(kind)
	w.Header().Set("Content-Type", subMime)
	// Samsung fetches subtitles with getcontentFeatures.dlna.org: 1 and expects
	// the DLNA headers back; without them the track is silently ignored.
	setDLNAHeaders(w, r, subMime, "Interactive")
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

func (h *Handler) serveThumb(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, PathThumbPrefix)
	if id == "" || h.art == nil {
		http.NotFound(w, r)
		return
	}
	path, ctype, err := h.art.Artwork(r.Context(), id)
	if err != nil {
		http.NotFound(w, r) // no artwork available; clients fall back gracefully
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot stat artwork", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "max-age=86400")
	// Artwork is fetched interactively, not streamed.
	setDLNAHeaders(w, r, ctype, "Interactive")
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

func (h *Handler) serveRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Beacon DLNA MediaServer\nDevice description: " + PathDeviceDesc + "\n"))
}
