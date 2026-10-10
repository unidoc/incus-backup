package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestCACommands(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "server.yaml")
	url := testdb.New(t)
	state := filepath.Join(dir, "state")
	if out, err := run(t, cfg, "init", "--state-dir", state, "--database-url", url, "--repository", "custody", "--datastore", filepath.Join(dir, "ds"), "--san", "127.0.0.1"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	// an enrolled host shows up
	ctx := context.Background()
	d, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	svc := service.New(d, state, "test", nil)
	tok, _ := svc.CreateToken(ctx, "custody", "benfica", api.ScopeBackup, time.Hour)
	_, csr, _ := pki.NewKeyAndCSR("benfica")
	if _, err := svc.Enroll(ctx, api.EnrollRequest{Token: tok, CSR: string(csr)}); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, cfg, "ca", "status")
	if err != nil || !strings.Contains(out, "custody/benfica (backup)") || !strings.Contains(out, "server certificate") || !strings.Contains(out, "overall: ok") {
		t.Fatalf("ca status: %v\n%s", err, out)
	}
	out, err = run(t, cfg, "ca", "status", "--json")
	var rep service.CertReport
	if err != nil || json.Unmarshal([]byte(out), &rep) != nil || len(rep.Hosts) != 1 || rep.CA.DaysLeft < 3600 {
		t.Fatalf("ca status --json: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "ca", "sans"); err != nil || strings.TrimSpace(out) != "127.0.0.1" {
		t.Fatalf("ca sans: %v %q", err, out)
	}
	if out, err = run(t, cfg, "ca", "sans", "127.0.0.1,backup.example"); err != nil || !strings.Contains(out, "backup.example") {
		t.Fatalf("ca sans set: %v %q", err, out)
	}
	if out, _ = run(t, cfg, "ca", "status"); !strings.Contains(out, "SANs 127.0.0.1,backup.example") && !strings.Contains(out, "SANs backup.example,127.0.0.1") {
		t.Fatalf("SANs not reissued:\n%s", out)
	}
	if _, err = run(t, cfg, "ca", "sans", "bad name"); err == nil {
		t.Fatal("invalid SAN accepted")
	}
	if out, err = run(t, cfg, "ca", "rotate"); err != nil || !strings.Contains(out, "ca promote") {
		t.Fatalf("rotate: %v\n%s", err, out)
	}
	if out, _ = run(t, cfg, "ca", "status"); !strings.Contains(out, "next CA (rotation)") || !strings.Contains(out, "1 identity(ies) pending: custody/benfica/backup") {
		t.Fatalf("status during rotation:\n%s", out)
	}
	if _, err = run(t, cfg, "ca", "promote"); err == nil || !strings.Contains(err.Error(), "custody/benfica/backup") {
		t.Fatalf("promote with a pending host: %v", err)
	}
	if out, err = run(t, cfg, "ca", "promote", "--force"); err != nil || !strings.Contains(out, "now presented") {
		t.Fatalf("promote --force: %v\n%s", err, out)
	}
	// the identity that never renewed is now cut off, and the report says so
	out, err = run(t, cfg, "ca", "status")
	if err == nil || !strings.Contains(out, "no longer trusted") || !strings.Contains(out, "overall: critical") {
		t.Fatalf("status after a forced promotion: %v\n%s", err, out)
	}
}
