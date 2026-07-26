package content

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotFound is returned when an object ID does not resolve to anything.
var ErrNotFound = errors.New("content: object not found")

// RootID is the ContentDirectory root container ID (fixed by the UPnP spec).
const RootID = "0"

// Root is one configured, top-level media folder.
type Root struct {
	Name string
	Path string // absolute
}

// FS is a filesystem-backed Backend. Object IDs (other than the root "0") are
// base64url-encoded absolute paths. Every path is validated to live inside one
// of the configured roots before it is read or served, so a malicious control
// point cannot browse or stream arbitrary files.
type FS struct {
	roots []Root
}

// NewFS builds a filesystem backend over the given roots. Root paths are made
// absolute.
func NewFS(roots []Root) *FS {
	out := make([]Root, 0, len(roots))
	for _, r := range roots {
		if abs, err := filepath.Abs(r.Path); err == nil {
			r.Path = abs
		}
		out = append(out, r)
	}
	return &FS{roots: out}
}

// encodeID turns an absolute path into an opaque object ID.
func encodeID(path string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(path))
}

// decodeID reverses encodeID.
func decodeID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("content: bad object id %q: %w", id, err)
	}
	return string(b), nil
}

// resolve validates that id maps to a path inside a configured root and returns
// the cleaned absolute path plus the owning root.
func (f *FS) resolve(id string) (path string, root Root, err error) {
	raw, err := decodeID(id)
	if err != nil {
		return "", Root{}, err
	}
	clean := filepath.Clean(raw)
	for _, r := range f.roots {
		if clean == r.Path || strings.HasPrefix(clean, r.Path+string(os.PathSeparator)) {
			return clean, r, nil
		}
	}
	return "", Root{}, fmt.Errorf("%w: %s outside configured roots", ErrNotFound, clean)
}

// Object implements Backend.
func (f *FS) Object(id string) (Object, error) {
	if id == RootID {
		return Object{
			ID:          RootID,
			ParentID:    "-1",
			Title:       "Beacon",
			Class:       "object.container.storageFolder",
			IsContainer: true,
			ChildCount:  len(f.roots),
		}, nil
	}
	path, root, err := f.resolve(id)
	if err != nil {
		return Object{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Object{}, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	parentID := RootID
	if parent := filepath.Dir(path); parent != root.Path && strings.HasPrefix(parent, root.Path) {
		parentID = encodeID(parent)
	}
	title := fi.Name()
	if path == root.Path {
		title = root.Name
	}
	return f.buildObject(path, fi, parentID, title)
}

// Children implements Backend.
func (f *FS) Children(id string) ([]Object, error) {
	// Root lists the configured folders.
	if id == RootID {
		out := make([]Object, 0, len(f.roots))
		for _, r := range f.roots {
			fi, err := os.Stat(r.Path)
			if err != nil || !fi.IsDir() {
				continue // skip roots that don't exist yet
			}
			obj, err := f.buildObject(r.Path, fi, RootID, r.Name)
			if err == nil {
				out = append(out, obj)
			}
		}
		return out, nil
	}

	path, _, err := f.resolve(id)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", path, err)
	}

	var out []Object
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // skip hidden files
		}
		child := filepath.Join(path, name)
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if _, ok := lookupMedia(name); !ok {
				continue // skip non-media files
			}
		}
		obj, err := f.buildObject(child, fi, id, name)
		if err != nil {
			continue
		}
		out = append(out, obj)
	}
	sortObjects(out)
	return out, nil
}

// buildObject constructs an Object for a stat'd path.
func (f *FS) buildObject(path string, fi os.FileInfo, parentID, title string) (Object, error) {
	if fi.IsDir() {
		return Object{
			ID:          encodeID(path),
			ParentID:    parentID,
			Title:       title,
			Class:       "object.container.storageFolder",
			IsContainer: true,
			ChildCount:  f.countChildren(path),
		}, nil
	}
	info, ok := lookupMedia(fi.Name())
	if !ok {
		return Object{}, fmt.Errorf("%s: not a media file", fi.Name())
	}
	id := encodeID(path)
	return Object{
		ID:       id,
		ParentID: parentID,
		Title:    stripExt(title),
		Class:    info.class,
		Date:     fi.ModTime().Format("2006-01-02"),
		Resources: []Resource{{
			ProtocolInfo: dlnaProtocolInfo(info.mime),
			MediaID:      id,
			MimeType:     info.mime,
			Size:         fi.Size(),
		}},
	}, nil
}

// countChildren returns how many browsable children a directory has (media
// files + subdirectories). Best-effort; errors count as zero.
func (f *FS) countChildren(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			n++
			continue
		}
		if _, ok := lookupMedia(name); ok {
			n++
		}
	}
	return n
}

// FilePath implements Backend.
func (f *FS) FilePath(id string) (string, error) {
	path, _, err := f.resolve(id)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return path, nil
}

// sortObjects orders a listing: containers first, then items, each alphabetical
// (case-insensitive) — the ordering most DLNA clients expect.
func sortObjects(objs []Object) {
	sort.SliceStable(objs, func(i, j int) bool {
		if objs[i].IsContainer != objs[j].IsContainer {
			return objs[i].IsContainer // containers first
		}
		return strings.ToLower(objs[i].Title) < strings.ToLower(objs[j].Title)
	})
}

func stripExt(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}
