package app_test

import (
	"context"
	"errors"
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
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func confirm(recovery string) error {
	if _, err := format.DecodeRecoveryKey(recovery); err != nil {
		return err
	}
	return nil
}

func tokenFor(t *testing.T, n *testsupport.Node, host string) string {
	tok, err := n.Svc.CreateToken(context.Background(), "custody", host, api.ScopeBackup, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func setupConfig(t *testing.T) string {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Host = "benfica"
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.SpoolDir = filepath.Join(dir, "spool")
	cfg.KeyFile = filepath.Join(dir, "etc", "key.json")
	path := filepath.Join(dir, "etc", "client.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInitCreatesKeyEnrolsAndWritesConfig(t *testing.T) {
	n := testsupport.NewNode(t)
	cfgPath := setupConfig(t)
	var shown string
	res, err := app.Init(context.Background(), app.InitOptions{ConfigPath: cfgPath, Name: "cikarang", Address: n.Addr(), Token: tokenFor(t, n, "benfica"),
		ConfirmRecovery: func(r string) error { shown = r; return confirm(r) }})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NewKey || res.Repo != "custody" || shown == "" {
		t.Fatalf("%+v", res)
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.Servers["cikarang"].Address != n.Addr() || cfg.Control != "cikarang" {
		t.Fatalf("%+v", cfg)
	}
	for _, f := range []string{cfg.KeyFile, filepath.Join(cfg.ServerDir("cikarang"), "client.key")} {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s must exist with mode 0600: %v", f, err)
		}
	}
	// the recovery key really is the key
	master, _ := format.DecodeRecoveryKey(shown)
	k, _ := format.DeriveKeys(master)
	if k.DomainID != res.DomainID {
		t.Fatal("recovery key does not reproduce the key domain")
	}
	// a second server reuses the same key (same key domain, cross-target dedup)
	n2 := testsupport.NewNode(t)
	res2, err := app.Init(context.Background(), app.InitOptions{ConfigPath: cfgPath, Name: "amsterdam", Address: n2.Addr(), Token: tokenFor(t, n2, "benfica"),
		ConfirmRecovery: func(string) error { t.Fatal("an existing key must not ask for a recovery key again"); return nil }})
	if err != nil || res2.NewKey || res2.DomainID != res.DomainID {
		t.Fatalf("%v %+v", err, res2)
	}
}

func TestInitWritesNothingWhenRecoveryKeyNotConfirmed(t *testing.T) {
	n := testsupport.NewNode(t)
	cfgPath := setupConfig(t)
	_, err := app.Init(context.Background(), app.InitOptions{ConfigPath: cfgPath, Address: n.Addr(), Token: tokenFor(t, n, "benfica"),
		ConfirmRecovery: func(string) error { return errors.New("operator declined") }})
	if err == nil {
		t.Fatal("init must fail")
	}
	cfg, _ := config.Load(cfgPath)
	if _, err := os.Stat(cfg.KeyFile); err == nil {
		t.Fatal("a key file was written although the recovery key was not confirmed")
	}
	if len(cfg.Servers) != 0 {
		t.Fatal("a server was recorded")
	}
	// and the token was not consumed by the failed attempt
	if _, err := app.Init(context.Background(), app.InitOptions{ConfigPath: cfgPath, Address: n.Addr(), Token: tokenFor(t, n, "benfica"), ConfirmRecovery: confirm}); err != nil {
		t.Fatal(err)
	}
}

func TestInitRefusesRepositoryOfAnotherKey(t *testing.T) {
	n := testsupport.NewNode(t)
	// another host pins the repository to key A
	a := setupConfig(t)
	if _, err := app.Init(context.Background(), app.InitOptions{ConfigPath: a, Address: n.Addr(), Token: tokenFor(t, n, "h1"), ConfirmRecovery: confirm}); err != nil {
		t.Fatal(err)
	}
	cfgA, _ := config.Load(a)
	keysA, _, _ := app.LoadKeys(cfgA.KeyFile, nil)
	creds := n.Enrol("pin", api.ScopeBackup)
	c := n.Client(creds, keysA.DomainID)
	if _, err := c.Begin(context.Background(), api.SessionRequest{GroupID: "0123456789abcdef0123456789abcdef", SnapshotID: "20261007230000-0a0a0a0a", Kind: "block", Project: "p", Object: "o"}); err != nil {
		t.Fatal(err)
	}
	// a host with a different key now tries to join
	b := setupConfig(t)
	_, err := app.Init(context.Background(), app.InitOptions{ConfigPath: b, Address: n.Addr(), Token: tokenFor(t, n, "h2"), ConfirmRecovery: confirm})
	if err == nil || !strings.Contains(err.Error(), "key domain") {
		t.Fatalf("a host with another key must be refused: %v", err)
	}
	cfgB, _ := config.Load(b)
	if len(cfgB.Servers) != 0 {
		t.Fatal("refused host still got a server entry")
	}
}

func newApp(t *testing.T) (*app.App, *fake.Incus, *testsupport.Node) {
	t.Helper()
	n := testsupport.NewNode(t)
	cfgPath := setupConfig(t)
	if _, err := app.Init(context.Background(), app.InitOptions{ConfigPath: cfgPath, Name: "cikarang", Address: n.Addr(), Token: tokenFor(t, n, "benfica"), ConfirmRecovery: confirm}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(cfgPath)
	cfg.Policies = map[string]config.Policy{"nightly": {Schedule: "03:00", Targets: []string{"cikarang"}, CommitRequired: "all"}}
	inc := fake.New(t.TempDir())
	a, err := app.Load(context.Background(), cfg, inc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return a, inc, n
}

func TestTickRunsDuePoliciesOnceAndRetriesFailures(t *testing.T) {
	a, inc, _ := newApp(t)
	inc.AddVM("default", "bosch", map[string]int{"root": 8 << 20}, true)
	inc.SetConfig("default", "bosch", engine.KeyPolicy, "nightly")
	inc.GuestWrite("default", "bosch", "root", 0, []byte("hello world"))
	now := time.Date(2026, 10, 8, 3, 5, 0, 0, time.UTC)
	a.Eng.Now = func() time.Time { return now }
	rep, err := a.Tick(context.Background())
	if err != nil || rep == nil || rep.Failed() != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	// second tick the same day: nothing is due
	now = now.Add(time.Hour)
	rep, _ = a.Tick(context.Background())
	if rep != nil {
		t.Fatal("ran again before the next scheduled time")
	}
	// next day: runs again
	now = now.Add(24 * time.Hour)
	rep, _ = a.Tick(context.Background())
	if rep == nil || rep.Failed() != 0 {
		t.Fatalf("not run on the next day: %+v", rep)
	}
	// a failing run is retried after 15 minutes, not after a day
	now = now.Add(24 * time.Hour)
	inc.FailNext("InstanceNBD", errors.New("incus is restarting"))
	rep, _ = a.Tick(context.Background())
	if rep == nil || rep.Failed() == 0 {
		t.Fatalf("expected a failed run: %+v", rep)
	}
	now = now.Add(5 * time.Minute)
	if rep, _ = a.Tick(context.Background()); rep != nil {
		t.Fatal("retried too early")
	}
	now = now.Add(11 * time.Minute)
	rep, _ = a.Tick(context.Background())
	if rep == nil || rep.Failed() != 0 {
		t.Fatalf("retry after 15 minutes did not happen or failed: %+v", rep)
	}
	if st := a.State.Object("default/block/instance/bosch/root"); st.LastStatus != "ok" || st.LastSnap == "" {
		t.Fatalf("state: %+v", st)
	}
}

func TestDoctorFindsProblems(t *testing.T) {
	a, inc, n := newApp(t)
	inc.AddVM("default", "bosch", map[string]int{"root": 4 << 20}, true)
	inc.AddVM("default", "bureau", map[string]int{"root": 4 << 20}, true)
	inc.AttachVolume("default", "bosch", "shared", "biggi", 1<<20, false)
	inc.AttachVolume("default", "bureau", "shared", "biggi", 1<<20, false)
	inc.SetConfig("default", "bosch", engine.KeyPolicy, "nightly")
	inc.SetConfig("default", "bureau", engine.KeyPolicy, "typo")
	cs := a.Doctor(context.Background())
	var text []string
	for _, c := range cs {
		text = append(text, c.String())
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{"server cikarang", "shared volume", "unknown policy"} {
		if !strings.Contains(all, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, all)
		}
	}
	if !app.Failed(cs) {
		t.Fatalf("an unknown policy must be a failure:\n%s", all)
	}
	// server down
	n.Srv.Close()
	cs = a.Doctor(context.Background())
	found := false
	for _, c := range cs {
		if c.Name == "server cikarang" && c.Status == "fail" {
			found = true
		}
	}
	if !found {
		t.Fatal("an unreachable server must be reported as a failure")
	}
	// old Incus without NBD
	inc.Version.Extensions = nil
	found = false
	for _, c := range a.Doctor(context.Background()) {
		found = found || (c.Name == "incus" && c.Status == "fail")
	}
	if !found {
		t.Fatal("missing API extensions must fail")
	}
}

var _ = format.ID{}

func TestImportKeyRestoresTheSameDomainAndNeverOverwrites(t *testing.T) {
	master, _ := format.GenerateMaster()
	rec, _ := format.EncodeRecoveryKey(master)
	want, _ := format.DeriveKeys(master)
	path := filepath.Join(t.TempDir(), "etc", "key.json")
	dom, err := app.ImportKey(path, rec, "")
	if err != nil || dom != want.DomainID {
		t.Fatalf("%v %s", err, dom)
	}
	k, _, err := app.LoadKeys(path, nil)
	if err != nil || k.DomainID != want.DomainID {
		t.Fatalf("%v", err)
	}
	if _, err := app.ImportKey(path, rec, ""); err == nil {
		t.Fatal("overwrote an existing key file")
	}
	if _, err := app.ImportKey(filepath.Join(t.TempDir(), "k.json"), typo(rec), ""); err == nil {
		t.Fatal("a mistyped recovery key was accepted")
	}
}

// typo changes the last character of a recovery key to a different valid one.
func typo(rec string) string {
	// not the last character: its unused base32 bits decode to the same key
	i := len(rec) - 6
	repl := byte('A')
	if rec[i] == 'A' {
		repl = 'B'
	}
	return rec[:i] + string(repl) + rec[i+1:]
}
