package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestCertCommandsAndDoctor(t *testing.T) {
	n := testsupport.NewNode(t)
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Host = "benfica"
	cfg.StateDir, cfg.SpoolDir, cfg.KeyFile = filepath.Join(dir, "state"), filepath.Join(dir, "spool"), filepath.Join(dir, "etc", "key.json")
	cfgPath := filepath.Join(dir, "etc", "client.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	inc := fake.New(t.TempDir())
	connectIncus = func(string) (incusx.API, error) { return inc, nil }
	defer func() { connectIncus = func(s string) (incusx.API, error) { return incusx.Connect(s) } }()
	defer func() { certNow = time.Now }()
	tok, _ := n.Svc.CreateToken(context.Background(), "custody", "benfica", api.ScopeBackup, time.Hour)
	if out, err := run(t, cfgPath, "init", "--server", n.Addr(), "--token", tok, "--name", "cikarang", "--i-stored-the-recovery-key"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	certNow = func() time.Time { return n.Now }
	out, err := run(t, cfgPath, "cert", "status")
	if err != nil || !strings.Contains(out, "cikarang") || !strings.Contains(out, " ok ") || !strings.Contains(out, "never") {
		t.Fatalf("cert status: %v\n%s", err, out)
	}
	crtPath := filepath.Join(dir, "state", "servers", "cikarang", "client.crt")
	before, _ := os.ReadFile(crtPath)
	out, err = run(t, cfgPath, "cert", "renew")
	if err != nil || !strings.Contains(out, "cikarang: renewed") || !strings.Contains(out, "confirmed") {
		t.Fatalf("cert renew: %v\n%s", err, out)
	}
	if after, _ := os.ReadFile(crtPath); string(after) == string(before) {
		t.Fatal("certificate file not replaced")
	}
	if _, err := os.Stat(crtPath + ".prev"); err != nil {
		t.Fatal("no .prev backup")
	}
	if _, err := run(t, cfgPath, "cert", "renew", "--server", "nope"); err == nil {
		t.Fatal("unknown server accepted")
	}
	// doctor prints the certificate line (and the renewed certificate works)
	out, err = run(t, cfgPath, "doctor")
	if !strings.Contains(out, "certificate cikarang") || !strings.Contains(out, "renewed automatically") || !strings.Contains(out, "OK    server cikarang") {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	// a long-expired certificate: status fails and says what to do
	certNow = func() time.Time { return n.Now.Add(800 * 24 * time.Hour) }
	if out, err = run(t, cfgPath, "cert", "status"); err == nil || !strings.Contains(out, "expired") {
		t.Fatalf("expired: %v\n%s", err, out)
	}
}
