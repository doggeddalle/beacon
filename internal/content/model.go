// Package content defines the browsable media tree that backs the UPnP
// ContentDirectory service: the object model, the object-ID scheme, the
// media-type tables, and the Backend interface an implementation must satisfy.
// The production implementation is library.Backend, over SQLite.
package content

import "context"

// RootID is the ContentDirectory root container, "0" by UPnP convention.
const RootID = "0"

// Object is a single node in the browse tree — either a container (folder) or a
// playable item (a media file). IDs are opaque strings; "0" is always the root
// container (per the UPnP ContentDirectory spec).
type Object struct {
	ID          string
	ParentID    string
	Title       string
	Class       string // UPnP class, e.g. "object.container.storageFolder"
	IsContainer bool

	// Container-only.
	ChildCount int

	// Item-only.
	Date      string // optional, "2006-01-02"
	Resources []Resource

	// Subtitle, if a sidecar subtitle exists, exposes it to clients. SubtitleID
	// is the object ID used to fetch it over HTTP; SubtitleKind is "srt", "ass",
	// "vtt", etc. Empty SubtitleID means no subtitle.
	SubtitleID   string
	SubtitleKind string

	// ArtworkID, if set, is the object ID used to fetch a thumbnail/poster
	// image over HTTP. Empty means no artwork is advertised.
	ArtworkID string
	// ArtworkMime is the content type the artwork will be served as. Defaults to
	// image/jpeg when empty; set it so the advertised <res> matches the bytes.
	ArtworkMime string
}

// Resource describes one way to fetch a media item. Phase 1 emits exactly one
// per item (the original file, served over HTTP with byte-range support).
type Resource struct {
	// ProtocolInfo is the DLNA `res@protocolInfo` value, e.g.
	// "http-get:*:video/mp4:DLNA.ORG_OP=01;...".
	ProtocolInfo string
	// MediaID identifies the file to stream; the HTTP server turns it into a
	// URL. For the filesystem backend this is the object's own ID.
	MediaID string
	// MimeType is the resource's content type (used for HTTP serving).
	MimeType string
	Size     int64  // bytes, 0 if unknown
	Duration string // "H:MM:SS", empty until Phase 4 metadata
	// Resolution is "WIDTHxHEIGHT", empty until Phase 4 metadata.
	Resolution string
}

// Page selects a window of a container's children. It exists so paging happens
// in the backend's query rather than by slicing a fully-loaded folder — a large
// directory should cost the same to browse as a small one.
type Page struct {
	Offset int  // rows to skip
	Limit  int  // maximum rows to return; <= 0 means no limit
	Desc   bool // reverse the alphabetical order
}

// Backend is the browsable content source. Implementations must be safe for
// concurrent use.
type Backend interface {
	// Object returns metadata for a single object (BrowseMetadata).
	Object(id string) (Object, error)
	// Children returns one page of a container's direct children
	// (BrowseDirectChildren) along with the total number of children, which the
	// UPnP response reports as TotalMatches.
	Children(id string, page Page) (objs []Object, total int, err error)
	// FilePath resolves a media object ID to an absolute filesystem path for
	// streaming. It must reject any ID that escapes the configured roots.
	FilePath(id string) (string, error)
}

// SubtitleProvider is optionally implemented by a Backend that can serve sidecar
// subtitles. SubtitlePath resolves a media object ID to its subtitle file path
// and kind, or an error if there is none.
type SubtitleProvider interface {
	SubtitlePath(objectID string) (path, kind string, err error)
}

// ArtworkProvider is optionally implemented by a Backend that can produce a
// thumbnail/poster image for a media object. Artwork returns a ready-to-serve
// image file path and its content type. It may be slow (generating a thumbnail)
// so it takes a context.
type ArtworkProvider interface {
	Artwork(ctx context.Context, objectID string) (path, contentType string, err error)
}
