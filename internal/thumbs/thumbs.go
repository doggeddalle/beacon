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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable is returned when no ffmpeg binary is available.
var ErrUnavailable = errors.New("thumbs: ffmpeg not available")

// failTTL is how long a failed generation is remembered. Without it, a video
// ffmpeg cannot decode is retried on every browse, each attempt holding a worker
// slot for up to two minutes.
const failTTL = 30 * time.Minute

// Thumbnailer produces cached JPEG thumbnails.
type Thumbnailer struct {
	ffmpeg   string
	cacheDir string
	width    int
	sem      chan struct{}
	log      *slog.Logger

	mu       sync.Mutex
	inflight map[string]*genResult // cache path -> in-progress generation
	failed   map[string]time.Time  // cache path -> when generation last failed
}

// genResult is a single in-flight generation that concurrent callers wait on.
type genResult struct {
	done chan struct{}
	path string
	err  error
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
		inflight: make(map[string]*genResult),
		failed:   make(map[string]time.Time),
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
	if seekSecs <= 0 {
		seekSecs = 5
	}
	return t.generate(ctx, t.cachePath(mediaPath, mtime), func(out string) error {
		err := t.runFrame(ctx, mediaPath, seekSecs, out)
		if err != nil {
			// Retry from the very start — seeking past a short clip's end yields
			// no frame.
			if err2 := t.runFrame(ctx, mediaPath, 0, out); err2 == nil {
				return nil
			}
		}
		return err
	})
}

// generate returns the cached file at out, producing it via run exactly once even
// when many requests arrive together.
//
// The previous check-semaphore-check pattern only coalesced when workers == 1:
// with the default 2, two requests could both miss the cache, both acquire a
// slot, and both run `ffmpeg -y` against the same output path, interleaving
// writes into a corrupt JPEG that was then cached forever.
func (t *Thumbnailer) generate(ctx context.Context, out string, run func(out string) error) (string, error) {
	if fileExists(out) {
		return out, nil
	}

	t.mu.Lock()
	if at, ok := t.failed[out]; ok {
		if time.Since(at) < failTTL {
			t.mu.Unlock()
			return "", fmt.Errorf("thumbs: generation previously failed for %s", filepath.Base(out))
		}
		delete(t.failed, out)
	}
	if r, ok := t.inflight[out]; ok {
		t.mu.Unlock() // someone else is already producing it; wait for their result
		select {
		case <-r.done:
			return r.path, r.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	r := &genResult{done: make(chan struct{})}
	t.inflight[out] = r
	t.mu.Unlock()

	r.path, r.err = t.produce(ctx, out, run)
	close(r.done)

	t.mu.Lock()
	delete(t.inflight, out)
	// Only remember genuine failures; a cancelled request says nothing about
	// whether the thumbnail is producible.
	if r.err != nil && ctx.Err() == nil {
		t.failed[out] = time.Now()
	}
	t.mu.Unlock()
	return r.path, r.err
}

// produce acquires a worker slot and runs the generator, then re-checks the cache
// in case another process produced the file while we queued.
func (t *Thumbnailer) produce(ctx context.Context, out string, run func(out string) error) (string, error) {
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if fileExists(out) {
		return out, nil
	}
	if err := run(out); err != nil {
		// Belt and braces: generators write via runToTemp, but if one ever leaves
		// debris at the cache path a later fileExists() would serve it forever.
		os.Remove(out)
		return "", fmt.Errorf("thumbs: %w", err)
	}
	return out, nil
}

func (t *Thumbnailer) runFrame(ctx context.Context, src string, seekSecs int, out string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// -ss before -i = fast (keyframe) seek. scale width, keep aspect (-2 keeps
	// height even). -frames:v 1 grabs one frame.
	return t.runToTemp(ctx, out, func(tmp string) *exec.Cmd {
		return exec.CommandContext(ctx, t.ffmpeg,
			"-nostdin", "-y",
			"-ss", strconv.Itoa(seekSecs),
			"-i", src,
			"-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:-2", t.width),
			"-q:v", "4",
			tmp,
		)
	})
}

// runToTemp writes ffmpeg's output to a scratch file and only moves it into place
// once the process has exited cleanly. Writing straight to the cache path meant a
// timeout or an OOM kill left a truncated JPEG that every later request happily
// served, forever.
func (t *Thumbnailer) runToTemp(ctx context.Context, out string, build func(tmp string) *exec.Cmd) error {
	f, err := os.CreateTemp(t.cacheDir, ".tmp-*"+filepath.Ext(out))
	if err != nil {
		return err
	}
	tmp := f.Name()
	f.Close() // ffmpeg writes it; we only needed a unique name
	defer os.Remove(tmp)

	// ffmpeg picks its muxer from the extension, so the temp name keeps ".jpg".
	if err := build(tmp).Run(); err != nil {
		return err
	}
	fi, err := os.Stat(tmp)
	if err != nil || fi.Size() == 0 {
		return errors.New("ffmpeg produced no output")
	}
	return os.Rename(tmp, out)
}

// Cover extracts embedded cover art (e.g. from an audio file) to a cached JPEG.
func (t *Thumbnailer) Cover(ctx context.Context, mediaPath string, mtime int64) (string, error) {
	if t.ffmpeg == "" {
		return "", ErrUnavailable
	}
	return t.generate(ctx, t.cachePath(mediaPath, mtime), func(out string) error {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := t.runToTemp(cctx, out, func(tmp string) *exec.Cmd {
			return exec.CommandContext(cctx, t.ffmpeg,
				"-nostdin", "-y", "-i", mediaPath,
				"-an", "-vframes", "1",
				"-vf", fmt.Sprintf("scale=%d:-2", t.width),
				tmp,
			)
		}); err != nil {
			return fmt.Errorf("no embedded cover in %s", filepath.Base(mediaPath))
		}
		return nil
	})
}

// Sweep deletes cached thumbnails not read for maxAge, then, if the cache is
// still over maxBytes, the oldest entries until it fits. Cache keys include the
// source mtime, so every edit orphans the previous file; nothing else ever
// removes them and the cache lives on the NAS's small system partition.
func (t *Thumbnailer) Sweep(maxAge time.Duration, maxBytes int64) (removed int, freed int64) {
	entries, err := os.ReadDir(t.cacheDir)
	if err != nil {
		return 0, 0
	}
	type entry struct {
		path string
		mod  time.Time
		size int64
	}
	var kept []entry
	var total int64
	cutoff := time.Now().Add(-maxAge)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(t.cacheDir, e.Name())
		// Sweep abandoned scratch files too: a crash mid-generation leaves them.
		stale := strings.HasPrefix(e.Name(), ".tmp-") && fi.ModTime().Before(time.Now().Add(-time.Hour))
		if stale || (maxAge > 0 && fi.ModTime().Before(cutoff)) {
			if os.Remove(p) == nil {
				removed++
				freed += fi.Size()
			}
			continue
		}
		kept = append(kept, entry{p, fi.ModTime(), fi.Size()})
		total += fi.Size()
	}

	if maxBytes <= 0 || total <= maxBytes {
		return removed, freed
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].mod.Before(kept[j].mod) })
	for _, e := range kept {
		if total <= maxBytes {
			break
		}
		if os.Remove(e.path) == nil {
			removed++
			freed += e.size
			total -= e.size
		}
	}
	return removed, freed
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
