// Package meta extracts media metadata (via ffprobe) and detects sidecar
// subtitle files. Everything degrades gracefully: if ffprobe is not installed,
// duration/resolution are simply left blank and subtitle detection still works.
package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable is returned by Probe when no ffprobe binary is available.
var ErrUnavailable = errors.New("meta: ffprobe not available")

// Prober runs ffprobe to extract media metadata.
type Prober struct {
	path string // path to ffprobe, "" if none was found
}

// NewProber locates an ffprobe binary (see LocateExecutable).
func NewProber(configuredPath string) *Prober {
	return &Prober{path: LocateExecutable("ffprobe", configuredPath)}
}

// LocateExecutable finds a helper binary, trying, in order: the configured
// path, a binary sitting next to the beacon executable, then the system PATH.
// Returns "" if none is found.
func LocateExecutable(name, configuredPath string) string {
	if configuredPath != "" && fileExists(configuredPath) {
		return configuredPath
	}
	binName := name
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), binName)
		if fileExists(cand) {
			return cand
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// Available reports whether an ffprobe binary was found.
func (p *Prober) Available() bool { return p.path != "" }

// Path returns the resolved ffprobe path (empty if unavailable).
func (p *Prober) Path() string { return p.path }

// Info is the subset of media metadata we expose.
type Info struct {
	DurationSecs float64
	Width        int
	Height       int
}

// Probe runs ffprobe on file and returns its metadata.
func (p *Prober) Probe(ctx context.Context, file string) (Info, error) {
	if p.path == "" {
		return Info{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.path,
		"-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", file)
	out, err := cmd.Output()
	if err != nil {
		return Info{}, fmt.Errorf("ffprobe %s: %w", filepath.Base(file), err)
	}

	var raw struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Info{}, fmt.Errorf("ffprobe json %s: %w", filepath.Base(file), err)
	}

	var info Info
	info.DurationSecs = parseFloat(raw.Format.Duration)
	for _, s := range raw.Streams {
		if s.CodecType == "video" && s.Width > 0 {
			info.Width, info.Height = s.Width, s.Height
			if info.DurationSecs == 0 {
				info.DurationSecs = parseFloat(s.Duration)
			}
			break
		}
	}
	return info, nil
}

// DurationHMS formats the duration as H:MM:SS for the DLNA res@duration
// attribute. Returns "" when unknown.
func (i Info) DurationHMS() string {
	if i.DurationSecs <= 0 {
		return ""
	}
	total := int(i.DurationSecs + 0.5)
	return fmt.Sprintf("%d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// Resolution formats the video resolution as "WIDTHxHEIGHT", or "" if unknown.
func (i Info) Resolution() string {
	if i.Width > 0 && i.Height > 0 {
		return fmt.Sprintf("%dx%d", i.Width, i.Height)
	}
	return ""
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
