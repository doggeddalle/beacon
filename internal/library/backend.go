package library

import (
	"context"
	"fmt"
	"strings"
	"time"

	"beacon/internal/content"
	"beacon/internal/meta"
	"beacon/internal/store"
	"beacon/internal/thumbs"
)

// Backend serves UPnP ContentDirectory browse requests from the SQLite store.
// It implements content.Backend, so it is a drop-in replacement for the
// Phase-1 filesystem backend.
type Backend struct {
	store     *store.Store
	rootCount func() int // number of configured top-level folders
	thumbs    *thumbs.Thumbnailer
}

// NewBackend builds a store-backed content backend. rootCount reports how many
// top-level folders exist (used for the synthetic root container's childCount).
// tn may be nil to disable artwork.
func NewBackend(st *store.Store, rootCount func() int, tn *thumbs.Thumbnailer) *Backend {
	return &Backend{store: st, rootCount: rootCount, thumbs: tn}
}

// Object implements content.Backend.
func (b *Backend) Object(id string) (content.Object, error) {
	if id == content.RootID {
		return content.Object{
			ID:          content.RootID,
			ParentID:    "-1",
			Title:       "Beacon",
			Class:       "object.container.storageFolder",
			IsContainer: true,
			ChildCount:  b.rootCount(),
		}, nil
	}
	path, err := content.DecodeID(id)
	if err != nil {
		return content.Object{}, fmt.Errorf("library: bad id %q: %w", id, err)
	}
	it, err := b.store.Get(path)
	if err != nil {
		return content.Object{}, err
	}
	childCount := 0
	if it.IsDir {
		childCount, _ = b.store.CountChildren(it.Path)
	}
	return b.toObject(it, childCount), nil
}

// Children implements content.Backend.
func (b *Backend) Children(id string, page content.Page) ([]content.Object, int, error) {
	parent := store.RootParent
	if id != content.RootID {
		p, err := content.DecodeID(id)
		if err != nil {
			return nil, 0, fmt.Errorf("library: bad id %q: %w", id, err)
		}
		parent = p
	}
	total, err := b.store.CountChildren(parent)
	if err != nil {
		return nil, 0, err
	}
	rows, err := b.store.Children(parent, page.Offset, page.Limit, page.Desc)
	if err != nil {
		return nil, 0, err
	}
	out := make([]content.Object, 0, len(rows))
	for _, r := range rows {
		out = append(out, b.toObject(r.Item, r.ChildCount))
	}
	return out, total, nil
}

// FilePath implements content.Backend. Only paths present in the index resolve,
// so the database itself acts as the allow-list against arbitrary file access.
func (b *Backend) FilePath(id string) (string, error) {
	path, err := content.DecodeID(id)
	if err != nil {
		return "", fmt.Errorf("library: bad id %q: %w", id, err)
	}
	it, err := b.store.Get(path)
	if err != nil {
		return "", err
	}
	if it.IsDir {
		return "", fmt.Errorf("library: %s is a directory", path)
	}
	return it.Path, nil
}

// toObject converts a stored item into a UPnP content object. childCount comes
// from the caller so a listing does not issue one COUNT per container.
func (b *Backend) toObject(it store.Item, childCount int) content.Object {
	parentID := content.RootID
	if it.Parent != store.RootParent {
		parentID = content.EncodeID(it.Parent)
	}
	id := content.EncodeID(it.Path)

	if it.IsDir {
		return content.Object{
			ID:          id,
			ParentID:    parentID,
			Title:       it.Name,
			Class:       "object.container.storageFolder",
			IsContainer: true,
			ChildCount:  childCount,
		}
	}

	date := ""
	if it.MTime > 0 {
		date = time.Unix(it.MTime, 0).Format("2006-01-02")
	}
	obj := content.Object{
		ID:       id,
		ParentID: parentID,
		Title:    content.DisplayTitle(it.Name),
		Class:    it.Class,
		Date:     date,
		Resources: []content.Resource{{
			ProtocolInfo: content.ProtocolInfo(it.Mime),
			MediaID:      id,
			MimeType:     it.Mime,
			Size:         it.Size,
			Duration:     it.Duration,
			Resolution:   it.Resolution,
		}},
	}
	if it.SubPath != "" {
		obj.SubtitleID = id
		obj.SubtitleKind = meta.KindForPath(it.SubPath)
	}
	// Advertise artwork for videos (frame thumbnail) and for anything with a
	// known sidecar/folder poster. Generation is lazy, in Artwork().
	//
	// A generated frame needs ffmpeg; without it only a real poster file counts,
	// otherwise every video advertises an albumArtURI that 404s.
	//
	// The poster path is read from the row, recorded once by the enricher. Probing
	// the filesystem here meant an os.ReadDir per item per browse: a 2000-track
	// folder did 2000 directory reads every time a client opened it.
	if b.thumbs != nil {
		switch {
		case it.ArtPath != "":
			// A real poster wins, and it may be a PNG.
			obj.ArtworkID = id
			obj.ArtworkMime = posterMime(it.ArtPath)
		case b.thumbs.Available() && strings.Contains(it.Class, "videoItem"):
			obj.ArtworkID = id
			obj.ArtworkMime = "image/jpeg" // ffmpeg-generated frame
		}
	}
	return obj
}

// Artwork implements content.ArtworkProvider: a sidecar/folder poster if one
// exists, otherwise a generated frame (video) or embedded cover (audio).
func (b *Backend) Artwork(ctx context.Context, objectID string) (string, string, error) {
	if b.thumbs == nil {
		return "", "", fmt.Errorf("library: artwork disabled")
	}
	path, err := content.DecodeID(objectID)
	if err != nil {
		return "", "", fmt.Errorf("library: bad id %q: %w", objectID, err)
	}
	it, err := b.store.Get(path)
	if err != nil {
		return "", "", err
	}

	// 1. A real poster image next to the media wins (no transcoding needed).
	if poster, ok := thumbs.FindPoster(it.Path); ok {
		return poster, posterMime(poster), nil
	}

	// 2. Generate from the media itself.
	switch {
	case strings.Contains(it.Class, "videoItem"):
		seek := seekSeconds(it.Duration)
		jpg, err := b.thumbs.Frame(ctx, it.Path, it.MTime, seek)
		if err != nil {
			return "", "", err
		}
		return jpg, "image/jpeg", nil
	case strings.Contains(it.Class, "audioItem"):
		jpg, err := b.thumbs.Cover(ctx, it.Path, it.MTime)
		if err != nil {
			return "", "", err
		}
		return jpg, "image/jpeg", nil
	default:
		return "", "", fmt.Errorf("library: no artwork for %s", it.Path)
	}
}

// seekSeconds picks a thumbnail timestamp: ~10% into the video, capped, so we
// skip intros/black frames without needing to know the exact length.
func seekSeconds(duration string) int {
	total := parseHMS(duration)
	if total <= 0 {
		return 10
	}
	s := total / 10
	if s < 4 {
		s = 4
	}
	if s > 600 {
		s = 600
	}
	return s
}

// parseHMS parses "H:MM:SS" back into seconds (0 if unparseable).
func parseHMS(s string) int {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	var h, m, sec int
	if _, err := fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec); err != nil {
		return 0
	}
	return h*3600 + m*60 + sec
}

func posterMime(path string) string {
	if strings.HasSuffix(strings.ToLower(path), ".png") {
		return "image/png"
	}
	return "image/jpeg"
}

// SubtitlePath implements content.SubtitleProvider.
func (b *Backend) SubtitlePath(objectID string) (string, string, error) {
	path, err := content.DecodeID(objectID)
	if err != nil {
		return "", "", fmt.Errorf("library: bad id %q: %w", objectID, err)
	}
	it, err := b.store.Get(path)
	if err != nil {
		return "", "", err
	}
	if it.SubPath == "" {
		return "", "", fmt.Errorf("library: no subtitle for %s", path)
	}
	return it.SubPath, meta.KindForPath(it.SubPath), nil
}
