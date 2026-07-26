package content

import (
	"encoding/base64"
	"path/filepath"
	"strings"
)

// Object IDs (other than the root "0") are the base64url encoding of an
// absolute filesystem path. This keeps IDs opaque and stable across restarts
// while remaining stateless — any backend can decode an ID back to a path.

// EncodeID turns an absolute path into an opaque object ID.
func EncodeID(path string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(path))
}

// DecodeID reverses EncodeID.
func DecodeID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DisplayTitle returns a human-friendly title for a media filename by dropping
// its extension.
func DisplayTitle(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}
