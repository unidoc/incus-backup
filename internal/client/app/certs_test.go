package app_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func certCheck(cs []app.Check, server string) app.Check {
	for _, c := range cs {
		if c.Name == "certificate "+server {
			return c
		}
	}
	return app.Check{}
}

// The daemon's renewal loop on a fake clock, with two servers renewing independently:
// one server refuses (host disabled) and is retried after its backoff, the other renews.
func TestDaemonRenewsEachServerIndependentlyWithBackoff(t *testing.T) {
	ctx := context.Background()
	n1, n2 := testsupport.NewNode(t), testsupport.NewNode(t)
	cfgPath := setupConfig(t)
	for name, n := range map[string]*testsupport.Node{"cikarang": n1, "amsterdam": n2} {
		if _, err := app.Init(ctx, app.InitOptions{ConfigPath: cfgPath, Name: name, Address: n.Addr(), Token: tokenFor(t, n, "benfica"), ConfirmRecovery: confirm}); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := config.Load(cfgPath)
	a, err := app.Load(ctx, cfg, fake.New(t.TempDir()), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := n1.Now
	a.Eng.Now = func() time.Time { return now }
	set := func(t time.Time) { now, n1.Now, n2.Now = t, t, t }
	certFile := func(s string) string {
		b, _ := os.ReadFile(filepath.Join(cfg.ServerDir(s), "client.crt"))
		return string(b)
	}
	c0, a0 := certFile("cikarang"), certFile("amsterdam")

	// at start: checked, nothing due
	if _, err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if certFile("cikarang") != c0 || certFile("amsterdam") != a0 {
		t.Fatal("renewed a fresh certificate")
	}
	if c := certCheck(a.Doctor(ctx), "cikarang"); c.Status != "ok" || !strings.Contains(c.Detail, "renewed automatically") {
		t.Fatalf("doctor, fresh: %+v", c)
	}

	// 40 days left; amsterdam refuses
	if err := n2.Svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, true); err != nil {
		t.Fatal(err)
	}
	set(now.Add(690 * 24 * time.Hour))
	if _, err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if certFile("cikarang") == c0 {
		t.Fatal("cikarang not renewed")
	}
	if certFile("amsterdam") != a0 {
		t.Fatal("amsterdam renewed although the server refused")
	}
	if _, err := a.Eng.Targets["cikarang"].Client.Whoami(ctx); err != nil {
		t.Fatalf("the engine's client does not use the renewed certificate: %v", err)
	}
	cs := a.Doctor(ctx)
	if c := certCheck(cs, "cikarang"); c.Status != "ok" {
		t.Fatalf("doctor cikarang after renewal: %+v", c)
	}
	if c := certCheck(cs, "amsterdam"); c.Status != "warn" || !strings.Contains(c.Detail, "renewal is failing") || !strings.Contains(c.Detail, "403") {
		t.Fatalf("doctor amsterdam with a failing renewal: %+v", c)
	}
	if notes := app.CertNotes(cfg, now); len(notes) != 1 || !strings.Contains(notes[0], "amsterdam") {
		t.Fatalf("status notes: %v", notes)
	}

	// within the backoff hour: no attempt
	if err := n2.Svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, false); err != nil {
		t.Fatal(err)
	}
	set(now.Add(30 * time.Minute))
	a.RenewDue(ctx)
	if st := remote.LoadRenewState(cfg.ServerDir("amsterdam")); st.Failures != 1 || certFile("amsterdam") != a0 {
		t.Fatalf("attempted during the backoff: %+v", st)
	}
	// after it: renewed
	set(now.Add(31 * time.Minute))
	a.RenewDue(ctx)
	if certFile("amsterdam") == a0 {
		t.Fatal("amsterdam not renewed after the backoff")
	}
	if st := remote.LoadRenewState(cfg.ServerDir("amsterdam")); st.Failures != 0 || st.LastError != "" || st.ConfirmPending {
		t.Fatalf("state after success: %+v", st)
	}
	if _, err := a.Eng.Targets["amsterdam"].Client.Whoami(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"cikarang", "amsterdam"} {
		if c := certCheck(a.Doctor(ctx), s); c.Status != "ok" {
			t.Fatalf("doctor %s at the end: %+v", s, c)
		}
	}
	// the cikarang check happens daily, not every tick
	c1 := certFile("cikarang")
	set(now.Add(time.Hour))
	a.RenewDue(ctx)
	if certFile("cikarang") != c1 {
		t.Fatal("renewed twice")
	}
}

func TestDoctorReportsExpiredCertificates(t *testing.T) {
	a, _, n := newApp(t)
	ctx := context.Background()
	exp := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Add(730 * 24 * time.Hour)
	a.Eng.Now = func() time.Time { return exp.Add(5 * 24 * time.Hour) }
	n.Now = a.Eng.Now()
	c := certCheck(app.CertChecks(a.Cfg, a.Eng.Now()), "cikarang")
	if c.Status != "fail" || !strings.Contains(c.Detail, "can still be renewed") {
		t.Fatalf("expired within the grace period: %+v", c)
	}
	// the daemon renews it (the server accepts an expired certificate for renewal for 30 days)
	a.RenewDue(ctx)
	if c := certCheck(app.CertChecks(a.Cfg, a.Eng.Now()), "cikarang"); c.Status != "ok" {
		t.Fatalf("after the daemon's renewal: %+v", c)
	}
	a.Eng.Now = func() time.Time { return exp.Add(5*24*time.Hour + 800*24*time.Hour) }
	if c := certCheck(app.CertChecks(a.Cfg, a.Eng.Now()), "cikarang"); c.Status != "fail" || !strings.Contains(c.Detail, "enrol again") {
		t.Fatalf("long expired: %+v", c)
	}
}
