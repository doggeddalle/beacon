package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadCreatesFileWithDefaultsAndUUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.toml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.FriendlyName != "Beacon" {
		t.Errorf("FriendlyName = %q, want Beacon", cfg.Server.FriendlyName)
	}
	if cfg.Server.HTTPPort != 8322 {
		t.Errorf("HTTPPort = %d, want 8322", cfg.Server.HTTPPort)
	}
	wantVolume1, _ := filepath.Abs("/volume1")
	wantVolume2, _ := filepath.Abs("/volume2")
	if got := cfg.Library.AllowedParents; len(got) != 2 || got[0] != wantVolume1 || got[1] != wantVolume2 {
		t.Errorf("AllowedParents = %q, want /volume1 and /volume2", got)
	}
	if len(cfg.Server.UUID) != 36 {
		t.Errorf("UUID = %q, want a 36-char v4 UUID", cfg.Server.UUID)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file was not written on first run: %v", err)
	}
}

func TestLoadIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.toml")

	first, err := Load(path)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if first.Server.UUID != second.Server.UUID {
		t.Errorf("UUID changed across restarts: %q != %q", first.Server.UUID, second.Server.UUID)
	}
}

func TestDurationParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.toml")
	content := `
[index]
reconcile_interval = "5m"
write_settle_delay = "500ms"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Index.ReconcileInterval.D(); got != 5*time.Minute {
		t.Errorf("ReconcileInterval = %v, want 5m", got)
	}
	if got := cfg.Index.WriteSettleDelay.D(); got != 500*time.Millisecond {
		t.Errorf("WriteSettleDelay = %v, want 500ms", got)
	}
}

func TestValidateRejectsBadPort(t *testing.T) {
	c := Defaults()
	c.Server.HTTPPort = 70000 // out of range
	if err := c.Validate(); err == nil {
		t.Error("expected error for port 70000, got nil")
	}
}

func TestValidateAllowsAutoPickPort(t *testing.T) {
	c := Defaults()
	c.Server.HTTPPort = 0 // 0 means "OS picks a free port"
	if err := c.Validate(); err != nil {
		t.Errorf("port 0 (auto-pick) should be valid, got %v", err)
	}
}

func TestValidateRejectsDuplicateFolders(t *testing.T) {
	c := Defaults()
	c.Library.Folders = []Folder{
		{Name: "A", Path: "/media"},
		{Name: "B", Path: "/media"},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for duplicate folder paths, got nil")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			// DeleteStale keys on root_name, so same-named roots prune each other.
			name: "duplicate folder names",
			mutate: func(c *Config) {
				c.Library.Folders = []Folder{
					{Name: "Movies", Path: abs("/media/a")},
					{Name: "Movies", Path: abs("/media/b")},
				}
			},
		},
		{
			name: "nested folders",
			mutate: func(c *Config) {
				c.Library.Folders = []Folder{
					{Name: "All", Path: abs("/media")},
					{Name: "Movies", Path: abs("/media/movies")},
				}
			},
		},
		{
			// Previously accepted, then silently disabled the tier at runtime.
			name:   "negative reconcile interval",
			mutate: func(c *Config) { c.Index.ReconcileInterval = Duration(-5 * time.Minute) },
		},
		{
			name:   "negative integrity interval",
			mutate: func(c *Config) { c.Index.IntegrityInterval = Duration(-1 * time.Hour) },
		},
		{
			name:   "negative settle delay",
			mutate: func(c *Config) { c.Index.WriteSettleDelay = Duration(-1 * time.Second) },
		},
		{
			name:   "empty friendly name",
			mutate: func(c *Config) { c.Server.FriendlyName = "   " },
		},
		{
			// Interpolated into the device description as uuid:<value>.
			name:   "malformed uuid",
			mutate: func(c *Config) { c.Server.UUID = "not-a-uuid" },
		},
		{
			name:   "uuid with XML metacharacters",
			mutate: func(c *Config) { c.Server.UUID = "<script>-2222-3333-4444-555555555555" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("expected %s to be rejected, got nil", tc.name)
			}
		})
	}
}

// The dashboard is unauthenticated, so it must not be able to add arbitrary
// directories — otherwise any LAN peer can index and stream the whole NAS.
func TestPathAllowedConfinesDashboardAdds(t *testing.T) {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }

	t.Run("defaults to the parents of configured folders", func(t *testing.T) {
		c := Defaults()
		c.Library.AllowedParents = nil
		c.Library.Folders = []Folder{{Name: "Movies", Path: abs("/media/movies")}}

		if !c.PathAllowed(abs("/media/shows")) {
			t.Error("a sibling of an existing folder should be allowed")
		}
		if !c.PathAllowed(abs("/media")) {
			t.Error("the parent itself should be allowed")
		}
		if c.PathAllowed(abs("/etc")) {
			t.Error("an unrelated directory must be refused")
		}
		if c.PathAllowed(abs("/")) {
			t.Error("the filesystem root must be refused")
		}
	})

	t.Run("explicit allow-list wins", func(t *testing.T) {
		c := Defaults()
		c.Library.Folders = []Folder{{Name: "Movies", Path: abs("/media/movies")}}
		c.Library.AllowedParents = []string{abs("/volume1")}

		if !c.PathAllowed(abs("/volume1/anything/deep")) {
			t.Error("a path under an allowed parent should be permitted")
		}
		if c.PathAllowed(abs("/media/shows")) {
			t.Error("an explicit allow-list must not be widened by the folder defaults")
		}
	})

	t.Run("default NAS volumes are allowed", func(t *testing.T) {
		c := Defaults()
		for i, p := range c.Library.AllowedParents {
			c.Library.AllowedParents[i] = abs(p)
		}
		if !c.PathAllowed(abs("/volume1/kodi/movies")) {
			t.Error("a folder under /volume1 should be addable on a fresh install")
		}
		if !c.PathAllowed(abs("/volume2/kodi2/mix")) {
			t.Error("a folder under /volume2 should be addable on a fresh install")
		}
		if c.PathAllowed(abs("/etc")) {
			t.Error("a path outside the NAS media volumes must be refused")
		}
	})
}

func TestValidateAcceptsSiblingFolders(t *testing.T) {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	c := Defaults()
	c.Server.UUID = "11111111-2222-3333-4444-555555555555"
	c.Library.Folders = []Folder{
		{Name: "Movies", Path: abs("/media/movies")},
		{Name: "Shows", Path: abs("/media/shows")},
		// A sibling whose name merely prefixes another must not trip the
		// nesting check.
		{Name: "Movies Extra", Path: abs("/media/movies-extra")},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("sibling folders should be valid, got %v", err)
	}
}
