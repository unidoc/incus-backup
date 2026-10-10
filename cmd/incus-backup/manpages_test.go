package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The packaging (-doc subpackage) generates the man pages with the hidden
// man command: one page per visible command, placeholders kept, the hidden
// commands left out, dated by SOURCE_DATE_EPOCH.
func TestManPages(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1759881600")
	dir := t.TempDir()
	root := newRoot()
	root.SetArgs([]string{"man", dir})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) < 10 {
		t.Fatalf("only %d man pages", len(ents))
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "incus-backup") || !strings.HasSuffix(e.Name(), ".1") {
			t.Errorf("unexpected file %s", e.Name())
		}
		if strings.Contains(e.Name(), "-man.") || strings.Contains(e.Name(), "-debug") {
			t.Errorf("hidden command got a page: %s", e.Name())
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "incus-backup-init.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("Oct 2025")) {
		t.Errorf("page not dated by SOURCE_DATE_EPOCH:\n%.300s", b)
	}
	if !bytes.Contains(b, []byte(".SH OPTIONS")) {
		t.Errorf("no OPTIONS section:\n%.300s", b)
	}
}

func TestVersionFlag(t *testing.T) {
	old := version
	version = "9.8.7"
	defer func() { version = old }()
	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "incus-backup version 9.8.7" {
		t.Fatalf("--version printed %q", got)
	}
}
