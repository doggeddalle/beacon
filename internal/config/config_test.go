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
