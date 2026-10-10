package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChownIDsWalksTree(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("x"), 0o600)
	if err := chownIDs(os.Getuid(), os.Getgid(), dir); err != nil {
		t.Fatal(err)
	}
	if err := chownIDs(os.Getuid(), os.Getgid(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing path must be reported")
	}
	// not root: chownTree must be a harmless no-op
	if os.Geteuid() != 0 {
		if err := chownTree("incus-backup", dir); err != nil {
			t.Fatal(err)
		}
	}
}
