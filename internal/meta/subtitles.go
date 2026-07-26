package meta

import (
	"os"
	"path/filepath"
	"strings"
)

// subtitleExts are the sidecar subtitle formats we recognise, in preference
// order.
var subtitleExts = []string{".srt", ".ass", ".ssa", ".vtt", ".sub", ".smi"}

// FindSubtitle looks for a sidecar subtitle next to a media file. It matches
// both "Movie.srt" and language-tagged variants like "Movie.en.srt". It returns
// the subtitle's absolute path and its kind ("srt", "ass", "vtt", ...), or two
// empty strings if none is found.
func FindSubtitle(mediaPath string) (path, kind string) {
	dir := filepath.Dir(mediaPath)
	base := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))

	// Fast path: exact "base.ext" match.
	for _, ext := range subtitleExts {
		cand := filepath.Join(dir, base+ext)
		if fileExists(cand) {
			return cand, kindForExt(ext)
		}
	}

	// Slower path: any file "base*.ext" (e.g. "Movie.en.srt", "Movie.forced.srt").
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	lowBase := strings.ToLower(base)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		low := strings.ToLower(e.Name())
		if !strings.HasPrefix(low, lowBase) {
			continue
		}
		for _, ext := range subtitleExts {
			if strings.HasSuffix(low, ext) {
				return filepath.Join(dir, e.Name()), kindForExt(ext)
			}
		}
	}
	return "", ""
}

// KindForPath returns the subtitle kind for a subtitle file path.
func KindForPath(subPath string) string {
	return kindForExt(filepath.Ext(subPath))
}

// kindForExt maps a subtitle file extension to a short kind token.
func kindForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".ssa", ".ass":
		return "ass"
	case ".vtt":
		return "vtt"
	case ".sub":
		return "sub"
	case ".smi":
		return "smi"
	default:
		return "srt"
	}
}

// SubtitleMime returns the HTTP content type for a subtitle kind.
func SubtitleMime(kind string) string {
	switch kind {
	case "ass":
		return "text/x-ssa"
	case "vtt":
		return "text/vtt"
	case "smi":
		return "application/smil"
	default:
		return "text/srt"
	}
}
