// Package content provides the browsable media tree that backs the UPnP
// ContentDirectory service. Phase 1 implements it directly over the filesystem
// (see fstree.go); a later phase swaps in a SQLite-backed implementation with
// the same Backend interface.
package content

import "context"

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

// Backend is the browsable content source. Implementations must be safe for
// concurrent use.
type Backend interface {
	// Object returns metadata for a single object (BrowseMetadata).
	Object(id string) (Object, error)
	// Children returns the direct children of a container (BrowseDirectChildren).
	Children(id string) ([]Object, error)
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
