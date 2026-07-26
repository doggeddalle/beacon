// Package thumbs generates and caches JPEG thumbnails for media using ffmpeg.
// Generation is lazy (on first request) and bounded by a semaphore so it never
// overwhelms a low-power NAS; results are cached on disk keyed by path+mtime.
package thumbs

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable is returned when no ffmpeg binary is available.
var ErrUnavailable = errors.New("thumbs: ffmpeg not available")

// Thumbnailer produces cached JPEG thumbnails.
type Thumbnailer struct {
	ffmpeg   string
	cacheDir string
	width    int
	sem      chan struct{}
	log      *slog.Logger
}

// New creates a Thumbnailer. ffmpegPath may be "" (then Available() is false and
// only sidecar poster files are used). workers bounds concurrent ffmpeg jobs.
func New(ffmpegPath, cacheDir string, workers, width int, log *slog.Logger) *Thumbnailer {
	if workers < 1 {
		workers = 1
	}
	if width <= 0 {
		width = 320
	}
	_ = os.MkdirAll(cacheDir, 0o755)
	return &Thumbnailer{
		ffmpeg: ffmpegPath, cacheDir: cacheDir, width: width,
		sem: make(chan struct{}, workers), log: log,
	}
}

// Available reports whether ffmpeg was found.
func (t *Thumbnailer) Available() bool { return t.ffmpeg != "" }

// cachePath returns the deterministic cache file for a source path+mtime.
func (t *Thumbnailer) cachePath(src string, mtime int64) string {
	h := sha1.Sum([]byte(src))
	name := hex.EncodeToString(h[:]) + "_" + strconv.FormatInt(mtime, 10) + ".jpg"
	return filepath.Join(t.cacheDir, name)
}

// Frame returns a cached thumbnail path for a video, generating it (a single
// scaled frame at seekSecs) on first use. seekSecs<=0 falls back to a small
// offset. Returns ErrUnavailable if ffmpeg is missing.
func (t *Thumbnailer) Frame(ctx context.Context, mediaPath string, mtime int64, seekSecs int) (string, error) {
	if t.ffmpeg == "" {
		return "", ErrUnavailable
	}
	out := t.cachePath(mediaPath, mtime)
	if fileExists(out) {
		return out, nil
	}
	if seekSecs <= 0 {
		seekSecs = 5
	}

	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// Re-check the cache after acquiring the slot (another request may have just
	// produced it).
	if fileExists(out) {
		return out, nil
	}

	if err := t.runFrame(ctx, mediaPath, seekSecs, out); err != nil {
		// Retry from the very start — seeking past a short clip's end yields no
		// frame.
		if err2 := t.runFrame(ctx, mediaPath, 0, out); err2 != nil {
			return "", fmt.Errorf("thumbs: %w", err)
		}
	}
	return out, nil
}

func (t *Thumbnailer) runFrame(ctx context.Context, src string, seekSecs int, out string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// -ss before -i = fast (keyframe) seek. scale width, keep aspect (-2 keeps
	// height even). -frames:v 1 grabs one frame.
	cmd := exec.CommandContext(ctx, t.ffmpeg,
		"-nostdin", "-y",
		"-ss", strconv.Itoa(seekSecs),
		"-i", src,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", t.width),
		"-q:v", "4",
		out,
	)
	if err := cmd.Run(); err != nil {
		return err
	}
	if !fileExists(out) {
		return errors.New("ffmpeg produced no output")
	}
	return nil
}

// Cover extracts embedded cover art (e.g. from an audio file) to a cached JPEG.
func (t *Thumbnailer) Cover(ctx context.Context, mediaPath string, mtime int64) (string, error) {
	if t.ffmpeg == "" {
		return "", ErrUnavailable
	}
	out := t.cachePath(mediaPath, mtime)
	if fileExists(out) {
		return out, nil
	}
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if fileExists(out) {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, t.ffmpeg,
		"-nostdin", "-y", "-i", mediaPath,
		"-an", "-vframes", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", t.width),
		out,
	)
	if err := cmd.Run(); err != nil || !fileExists(out) {
		return "", fmt.Errorf("thumbs: no embedded cover in %s", filepath.Base(mediaPath))
	}
	return out, nil
}

// posterNames are directory-level artwork filenames to prefer over generating a
// frame (checked case-insensitively).
var posterNames = []string{"poster.jpg", "poster.png", "folder.jpg", "folder.png", "cover.jpg", "cover.png"}

// FindPoster looks for artwork alongside a media file: first a same-basename
// image ("Movie.jpg"), then a well-known directory poster ("poster.jpg" etc.).
// Returns the image path and true if found.
func FindPoster(mediaPath string) (string, bool) {
	dir := filepath.Dir(mediaPath)
	base := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	for _, ext := range []string{".jpg", ".jpeg", ".png"} {
		if c := filepath.Join(dir, base+ext); fileExists(c) {
			return c, true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		low := strings.ToLower(e.Name())
		for _, want := range posterNames {
			if low == want {
				return filepath.Join(dir, e.Name()), true
			}
		}
	}
	return "", false
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
