package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsAndOverlay(t *testing.T) {
	t.Setenv("INCUS_BACKUP_DATABASE_URL", "postgres://x@y/z")
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil || c.Listen != ":8007" || c.DatabaseURL != "postgres://x@y/z" {
		t.Fatalf("%v %+v", err, c)
	}
	p := filepath.Join(t.TempDir(), "s.yaml")
	_ = os.WriteFile(p, []byte("listen: \":9000\"\nstate_dir: /tmp/x\n"), 0o600)
	c, err = Load(p)
	if err != nil || c.Listen != ":9000" || c.StateDir != "/tmp/x" {
		t.Fatalf("%v %+v", err, c)
	}
}

// The file is bootstrap-only: operational settings (schedules, grace, retention...) are in
// PostgreSQL and a stale key in an old file must not make the server refuse to start.
func TestOperationalKeysAreNotConfigFileSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.yaml")
	_ = os.WriteFile(p, []byte("state_dir: /tmp/x\nmaintenance:\n  prune: \"99:00\"\ngc_grace: tomorrow\n"), 0o600)
	if _, err := Load(p); err != nil {
		t.Fatalf("an obsolete key broke startup: %v", err)
	}
}

func TestValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.yaml")
	_ = os.WriteFile(p, []byte("state_dir: \"\"\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Error("accepted an empty state_dir")
	}
	if _, err := Load(func() string {
		p := filepath.Join(t.TempDir(), "bad.yaml")
		_ = os.WriteFile(p, []byte("listen: [unclosed"), 0o600)
		return p
	}()); err == nil {
		t.Error("accepted malformed yaml")
	}
}

// The shipped example must load and say what it claims.
func TestShippedExampleLoads(t *testing.T) {
	c, err := Load("../../../examples/server.yaml")
	if err != nil || c.Listen != ":8007" || c.StateDir == "" || c.DatabaseURL == "" {
		t.Fatalf("%v %+v", err, c)
	}
	if c.AdminSocketPath() != "/var/lib/incus-backup-server/admin.sock" {
		t.Fatalf("admin socket %q", c.AdminSocketPath())
	}
	c.AdminSocket = "off"
	if c.AdminSocketPath() != "" {
		t.Fatal("off must disable the socket")
	}
}
