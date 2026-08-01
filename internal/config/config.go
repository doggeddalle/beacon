// Package config loads and validates Beacon's on-disk configuration.
//
// The config file is TOML (human-friendly, comments allowed). Any field left
// unset falls back to a sensible default, so a minimal file — even an empty
// one — produces a working server. A stable server UUID is generated on first
// run and written back to the file so DLNA clients keep recognising the server
// across restarts.
package config

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the fully-resolved runtime configuration (defaults already applied).
type Config struct {
	Server  ServerConfig  `toml:"server"`
	Library LibraryConfig `toml:"library"`
	Index   IndexConfig   `toml:"index"`
	Meta    MetaConfig    `toml:"meta"`
	Log     LogConfig     `toml:"log"`

	// path is the file this config was loaded from; used by Save.
	path string `toml:"-"`
}

// MetaConfig controls media metadata extraction and thumbnails.
type MetaConfig struct {
	// FFprobePath is the path to the ffprobe binary. Empty = auto-detect (next
	// to the beacon binary, then the system PATH). If none is found, durations
	// and resolutions are left blank; sidecar subtitles still work.
	FFprobePath string `toml:"ffprobe_path"`
	// FFmpegPath is the path to the ffmpeg binary, used for thumbnails. Empty =
	// auto-detect. If none is found, thumbnails are disabled (sidecar/folder
	// poster images are still used).
	FFmpegPath string `toml:"ffmpeg_path"`
	// ThumbnailWidth is the width in pixels of generated thumbnails (height
	// scales to keep aspect). 0 = default (320).
	ThumbnailWidth int `toml:"thumbnail_width"`
}

// ServerConfig controls how the server presents itself on the network.
type ServerConfig struct {
	// FriendlyName is shown to DLNA clients (e.g. on your TV's source list).
	FriendlyName string `toml:"friendly_name"`
	// UUID uniquely identifies this MediaServer. Auto-generated if empty.
	UUID string `toml:"uuid"`
	// Interface is the network interface/IP to bind to. Empty = auto-detect
	// the primary LAN address.
	Interface string `toml:"interface"`
	// HTTPPort serves media, the device description, SOAP control and the
	// admin dashboard.
	HTTPPort int `toml:"http_port"`
	// DataDir holds the SQLite database, generated thumbnails and other state.
	DataDir string `toml:"data_dir"`
}

// Folder is one watched media source.
type Folder struct {
	// Name is the label shown as a top-level container to clients.
	Name string `toml:"name"`
	// Path is the absolute directory to index and serve.
	Path string `toml:"path"`
}

// LibraryConfig lists the media sources to index.
type LibraryConfig struct {
	Folders []Folder `toml:"folders"`
	// AllowedParents confines which directories the dashboard may add. The
	// dashboard is unauthenticated, so without this any LAN peer could add "/"
	// and stream every file on the NAS. Empty means "the parents of the folders
	// already configured", which keeps existing setups working while still
	// refusing arbitrary paths.
	AllowedParents []string `toml:"allowed_parents"`
}

// IndexConfig tunes the three-tier auto-update engine.
type IndexConfig struct {
	// Workers bounds concurrent metadata/thumbnail jobs. Keep low on 1GB RAM.
	Workers int `toml:"workers"`
	// ReconcileInterval is the Tier-2 periodic delta scan cadence.
	ReconcileInterval Duration `toml:"reconcile_interval"`
	// IntegrityInterval is the Tier-3 deep verify cadence (0 disables).
	IntegrityInterval Duration `toml:"integrity_interval"`
	// WriteSettleDelay is how long a file's size must stay stable before it is
	// treated as fully written (avoids indexing half-copied files).
	WriteSettleDelay Duration `toml:"write_settle_delay"`
}

// LogConfig controls logging output.
type LogConfig struct {
	// Level is one of: debug, info, warn, error.
	Level string `toml:"level"`
	// Format is "text" or "json".
	Format string `toml:"format"`
}

// Defaults returns a Config populated with recommended defaults for the
// AS1102TL (quad-core ARM64, 1GB RAM).
func Defaults() Config {
	return Config{
		Server: ServerConfig{
			FriendlyName: "Beacon",
			// 8200 is MiniDLNA's default and is often already taken by a NAS's
			// built-in media server, so default to a less-contended port.
			HTTPPort: 8322,
			DataDir:  "./data",
		},
		Index: IndexConfig{
			Workers:           2,
			ReconcileInterval: Duration(15 * time.Minute),
			IntegrityInterval: Duration(24 * time.Hour),
			WriteSettleDelay:  Duration(3 * time.Second),
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads the TOML config at path, applying defaults for any missing field.
// If the file does not exist it is created from defaults. A server UUID is
// generated and persisted on first run.
func Load(path string) (*Config, error) {
	cfg := Defaults()
	cfg.path = path

	existed := true
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case os.IsNotExist(err):
		existed = false // First run: keep defaults and write the file below.
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Unmarshal doesn't touch unexported fields, so re-set the path explicitly.
	cfg.path = path

	changed, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Persist on first run, or when normalize filled in a generated UUID.
	if !existed || changed {
		if err := cfg.Save(); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return &cfg, nil
}

// normalize fills derived defaults (like a generated UUID) and reports whether
// anything changed and should be written back to disk.
func (c *Config) normalize() (changed bool, err error) {
	if strings.TrimSpace(c.Server.UUID) == "" {
		u, err := newUUIDv4()
		if err != nil {
			return false, err
		}
		c.Server.UUID = u
		changed = true
	}
	if c.Server.DataDir != "" {
		if abs, err := filepath.Abs(c.Server.DataDir); err == nil {
			c.Server.DataDir = abs
		}
	}
	for i := range c.Library.Folders {
		if abs, err := filepath.Abs(c.Library.Folders[i].Path); err == nil {
			c.Library.Folders[i].Path = abs
		}
		if c.Library.Folders[i].Name == "" {
			c.Library.Folders[i].Name = filepath.Base(c.Library.Folders[i].Path)
		}
	}
	for i, p := range c.Library.AllowedParents {
		if abs, err := filepath.Abs(p); err == nil {
			c.Library.AllowedParents[i] = abs
		}
	}
	return changed, nil
}

// PathAllowed reports whether the dashboard may add path to the library.
//
// With no explicit allow-list the parents of the already-configured folders are
// used, so an existing install keeps working but a request for an unrelated part
// of the filesystem is still refused.
func (c *Config) PathAllowed(abs string) bool {
	for _, root := range c.allowedRoots() {
		if abs == root || isSubPath(root, abs) {
			return true
		}
	}
	return false
}

// allowedRoots is the effective allow-list.
func (c *Config) allowedRoots() []string {
	if len(c.Library.AllowedParents) > 0 {
		return c.Library.AllowedParents
	}
	roots := make([]string, 0, len(c.Library.Folders))
	for _, f := range c.Library.Folders {
		if parent := filepath.Dir(f.Path); parent != "" {
			roots = append(roots, parent)
		}
	}
	return roots
}

// AllowedRootsDescription renders the allow-list for an error message.
func (c *Config) AllowedRootsDescription() string {
	roots := c.allowedRoots()
	if len(roots) == 0 {
		return "(none configured — set library.allowed_parents in the config file)"
	}
	return strings.Join(roots, ", ")
}

// Validate checks the config for values that would prevent the server running.
func (c *Config) Validate() error {
	// 0 is allowed and means "let the OS pick a free port".
	if c.Server.HTTPPort < 0 || c.Server.HTTPPort > 65535 {
		return fmt.Errorf("server.http_port %d out of range 0-65535", c.Server.HTTPPort)
	}
	if c.Index.Workers < 1 {
		return fmt.Errorf("index.workers must be >= 1, got %d", c.Index.Workers)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q must be one of debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q must be text or json", c.Log.Format)
	}
	if strings.TrimSpace(c.Server.FriendlyName) == "" {
		return fmt.Errorf("server.friendly_name must not be empty — it is the name TVs show")
	}
	// Empty is fine: normalize() generates one before this runs. A non-empty but
	// malformed value is not — it is interpolated straight into the device
	// description as "uuid:<value>".
	if u := strings.TrimSpace(c.Server.UUID); u != "" && !validUUID(u) {
		return fmt.Errorf("server.uuid %q is not a valid UUID (expected 8-4-4-4-12 hex digits); "+
			"leave it empty to have one generated", c.Server.UUID)
	}
	// A negative interval silently disabled the tier instead of being rejected.
	// Zero is meaningful ("disabled"), negative never is.
	if c.Index.ReconcileInterval < 0 {
		return fmt.Errorf("index.reconcile_interval must not be negative, got %s", c.Index.ReconcileInterval)
	}
	if c.Index.IntegrityInterval < 0 {
		return fmt.Errorf("index.integrity_interval must not be negative, got %s", c.Index.IntegrityInterval)
	}
	if c.Index.WriteSettleDelay < 0 {
		return fmt.Errorf("index.write_settle_delay must not be negative, got %s", c.Index.WriteSettleDelay)
	}

	seenPath := map[string]bool{}
	seenName := map[string]bool{}
	for _, f := range c.Library.Folders {
		if f.Path == "" {
			return fmt.Errorf("library folder %q has empty path", f.Name)
		}
		if seenPath[f.Path] {
			return fmt.Errorf("library folder path %q listed more than once", f.Path)
		}
		seenPath[f.Path] = true

		// Stale-row pruning keys on the folder name, so two folders sharing one
		// name delete each other's rows on every scan, forever.
		if seenName[f.Name] {
			return fmt.Errorf("two library folders are both named %q — names must be unique, "+
				"because the index prunes stale entries per folder name", f.Name)
		}
		seenName[f.Name] = true
	}

	// Nested roots index the same files twice under two names, so each scan prunes
	// the other's rows and both duplicate all the ffprobe work.
	for _, a := range c.Library.Folders {
		for _, b := range c.Library.Folders {
			if a.Path == b.Path {
				continue
			}
			if isSubPath(a.Path, b.Path) {
				return fmt.Errorf("library folder %q (%s) is inside %q (%s) — "+
					"nested folders index the same files twice; keep only the outer one",
					b.Name, b.Path, a.Name, a.Path)
			}
		}
	}
	return nil
}

// isSubPath reports whether child lies beneath parent.
func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validUUID reports whether s is 8-4-4-4-12 hex digits. The value is interpolated
// straight into the device description as "uuid:<s>", so a stray "<" would emit
// malformed XML to every client.
func validUUID(s string) bool {
	groups := [...]int{8, 4, 4, 4, 12}
	parts := strings.Split(s, "-")
	if len(parts) != len(groups) {
		return false
	}
	for i, p := range parts {
		if len(p) != groups[i] {
			return false
		}
		for _, r := range p {
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// Save writes the config back to its file as TOML.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no path to save to")
	}
	if dir := filepath.Dir(c.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(c.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(fileHeader); err != nil {
		return err
	}
	return toml.NewEncoder(f).Encode(c)
}

// Path returns the file this config was loaded from.
func (c *Config) Path() string { return c.path }

const fileHeader = "# Beacon configuration. Edit and restart to apply.\n" +
	"# Any field omitted here falls back to a built-in default.\n\n"

// newUUIDv4 returns a random RFC-4122 v4 UUID string.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
