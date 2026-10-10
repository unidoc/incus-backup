package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) {
	testdb.AtExit(s3test.Start())
	testdb.Main(m)
}

// run executes the real cobra tree and captures stdout.
func run(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	root := newRoot()
	root.SetArgs(append([]string{"--config", cfg}, args...))
	root.SetErr(io.Discard)
	err := root.ExecuteContext(context.Background())
	_ = w.Close()
	os.Stdout = old
	var b bytes.Buffer
	_, _ = io.Copy(&b, r)
	return b.String(), err
}

func TestServerCLIWalkthrough(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "server.yaml")
	url := testdb.New(t)
	out, err := run(t, cfg, "init", "--state-dir", filepath.Join(dir, "state"), "--database-url", url, "--repository", "custody", "--datastore", filepath.Join(dir, "ds"), "--san", "127.0.0.1")
	if err != nil || !strings.Contains(out, "CA fingerprint") {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if st, _ := os.Stat(cfg); st == nil || st.Mode().Perm() != 0o600 {
		t.Fatal("config must be 0600 (it holds the database URL)")
	}
	if _, err := run(t, cfg, "init", "--state-dir", filepath.Join(dir, "state"), "--database-url", url, "--datastore", filepath.Join(dir, "ds2")); err == nil {
		t.Fatal("init over an existing state directory must fail")
	}
	out, err = run(t, cfg, "token", "create", "benfica", "--scope", "backup")
	if err != nil || !strings.Contains(out, "ibt1.") {
		t.Fatalf("token: %v\n%s", err, out)
	}
	if _, err := run(t, cfg, "token", "create", "x", "--scope", "admin"); err == nil {
		t.Fatal("admin tokens must not exist")
	}
	if _, err := run(t, cfg, "policy", "set", "production", "--schedule", "every 4h", "--targets", "cikarang,amsterdam", "--keep-daily", "14", "--keep-weekly", "8"); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, cfg, "policy", "list")
	if !strings.Contains(out, "production") || !strings.Contains(out, "cikarang,amsterdam") || !strings.Contains(out, "0/0/14/8/0/0") {
		t.Fatalf("policy list:\n%s", out)
	}
	for _, args := range [][]string{{"status"}, {"capacity"}, {"prune", "--dry-run"}, {"verify"}, {"reconcile"}, {"host", "list"}, {"repo", "list"}, {"sync", "list"}, {"config", "export"}} {
		if out, err := run(t, cfg, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if out, err := run(t, cfg, "gc"); err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	out, _ = run(t, cfg, "config", "export")
	if !strings.Contains(out, "production") {
		t.Fatalf("export lacks policy:\n%s", out)
	}
	if _, err := run(t, cfg, "policy", "assign", "nobody", "production"); err == nil {
		t.Fatal("assigning to an unknown host must fail")
	}
	if out, err := run(t, cfg, "-r", "nope", "status"); err == nil {
		t.Fatalf("unknown repository accepted: %s", out)
	}
}

func TestServerCLIS3Repository(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "server.yaml")
	if _, err := run(t, cfg, "init", "--state-dir", filepath.Join(dir, "state"), "--database-url", testdb.New(t), "--repository", "custody", "--datastore", filepath.Join(dir, "ds"), "--san", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	svc, bucket := s3test.NewBucket(t)
	t.Setenv("INCUS_BACKUP_S3_SECRET_KEY", svc.SecretKey)
	args := []string{"repo", "create", "offsite", "--s3-endpoint", svc.Endpoint, "--s3-bucket", bucket, "--s3-prefix", "ib", "--s3-access-key", svc.AccessKey, "--s3-no-tls", "--s3-path-style"}
	if out, err := run(t, cfg, args...); err != nil || !strings.Contains(out, "offsite") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	out, err := run(t, cfg, "repo", "probe", "offsite")
	if err != nil || strings.Contains(out, "FAIL") || !strings.Contains(out, "ok    put if-absent") {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	out, _ = run(t, cfg, "repo", "list")
	if !strings.Contains(out, "s3") || !strings.Contains(out, "filesystem") || strings.Contains(out, svc.SecretKey) {
		t.Fatalf("list:\n%s", out)
	}
	for _, a := range [][]string{{"-r", "offsite", "status"}, {"-r", "offsite", "capacity"}, {"-r", "offsite", "verify"}, {"-r", "offsite", "gc"}, {"-r", "offsite", "reconcile"}} {
		if out, err := run(t, cfg, a...); err != nil {
			t.Fatalf("%v: %v\n%s", a, err, out)
		}
	}
	// both flavours at once, or a secret on the command line, are not accepted
	if _, err := run(t, cfg, "repo", "create", "x", "--datastore", dir, "--s3-bucket", "b", "--s3-endpoint", "e"); err == nil {
		t.Fatal("--datastore with --s3-* accepted")
	}
	if _, err := run(t, cfg, "repo", "create", "y", "--s3-endpoint", svc.Endpoint, "--s3-bucket", bucket, "--s3-secret-key", "x"); err == nil {
		t.Fatal("a secret key flag must not exist")
	}
}

// The operator surface for the PBS-class features, through the real cobra tree.
func TestServerCLIManagesSnapshotsMaintenanceVerificationAndCredentials(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "server.yaml")
	url := testdb.New(t)
	state := filepath.Join(dir, "state")
	if _, err := run(t, cfg, "init", "--state-dir", state, "--database-url", url, "--repository", "custody", "--datastore", filepath.Join(dir, "ds"), "--san", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	svc := service.New(d, state, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.MaxClockSkew = 100 * 365 * 24 * time.Hour
	tok, _ := svc.CreateToken(ctx, "custody", "benfica", api.ScopeBackup, time.Hour)
	_, csr, _ := pki.NewKeyAndCSR("benfica")
	er, err := svc.Enroll(ctx, api.EnrollRequest{Token: tok, CSR: string(csr)})
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := pki.ParseCertPEM([]byte(er.Certificate))
	h, err := svc.HostFromCert(ctx, cert, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	m := make([]byte, 32)
	m[0] = 9
	k, _ := format.DeriveKeys(m)
	const group = "0123456789abcdef0123456789abcdef"
	var snaps []string
	for i := 1; i <= 4; i++ {
		snap := fmt.Sprintf("2026100%d120000-%08x", i, i)
		snaps = append(snaps, snap)
		created := time.Date(2026, 10, i, 12, 0, 0, 0, time.UTC)
		resp, err := svc.BeginSession(ctx, h, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snap, Created: created, Project: "default", Object: "instance/web/root", Kind: "block", Policy: "short"})
		if err != nil {
			t.Fatal(err)
		}
		id, blob, _ := k.EncodeChunk([]byte(fmt.Sprintf("data-%d", i)), format.EncodeOpts{})
		if _, err := svc.SessionMissing(ctx, h, resp.Session, []string{id.String()}); err != nil {
			t.Fatal(err)
		}
		_ = svc.SessionPutChunk(ctx, h, resp.Session, id.String(), blob)
		idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: 6, Entries: []format.Entry{{End: 6, ID: id}}}).Marshal()
		_ = svc.SessionPutIndex(ctx, h, resp.Session, idx)
		man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created}, group, nil)
		_ = svc.SessionPutManifest(ctx, h, resp.Session, man)
		if _, err := svc.SessionCommit(ctx, h, resp.Session, api.CommitRequest{LogicalBytes: 6}); err != nil {
			t.Fatal(err)
		}
	}
	obj := "default/instance/web/root"
	out, err := run(t, cfg, "snapshot", "list", "--object", obj)
	if err != nil || strings.Count(out, "2026100") != 4 {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if _, err := run(t, cfg, "snapshot", "protect", "--object", obj, snaps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, cfg, "snapshot", "notes", "--object", obj, snaps[0], "before the upgrade"); err != nil {
		t.Fatal(err)
	}
	if out, _ = run(t, cfg, "snapshot", "list", "--object", obj); !strings.Contains(out, "yes") || !strings.Contains(out, "before the upgrade") {
		t.Fatalf("protection/notes not shown:\n%s", out)
	}
	if _, err := run(t, cfg, "snapshot", "forget", "--object", obj, snaps[0]); err == nil {
		t.Fatal("a protected snapshot was forgotten")
	}
	// retention preview shows the plan with reasons and zone, apply matches
	if _, err := run(t, cfg, "policy", "set", "short", "--keep-last", "2", "--timezone", "Atlantic/Reykjavik"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, cfg, "policy", "set", "bad", "--keep-last", "1", "--timezone", "Nowhere/Land"); err == nil {
		t.Fatal("unknown zone accepted")
	}
	out, err = run(t, cfg, "prune", "--dry-run")
	if err != nil || !strings.Contains(out, "protected") || !strings.Contains(out, "Atlantic/Reykjavik") || !strings.Contains(out, "(dry run)") {
		t.Fatalf("preview: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "prune"); err != nil || !strings.Contains(out, "kept 3 (1 protected), removed 1") {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	// verification modes and history
	if out, err = run(t, cfg, "verify", "--mode", "new"); err != nil || !strings.Contains(out, "checked 3 snapshots") {
		t.Fatalf("verify new: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "verify", "--mode", "new"); err != nil || !strings.Contains(out, "checked 0 snapshots (3 skipped)") {
		t.Fatalf("verify new again: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "snapshot", "history", "--object", obj, snaps[0]); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("history: %v\n%s", err, out)
	}
	if _, err = run(t, cfg, "verify", "--mode", "bogus"); err == nil {
		t.Fatal("bogus mode accepted")
	}
	if out, err = run(t, cfg, "corrupt"); err != nil || strings.Contains(out, "quarantined") {
		t.Fatalf("corrupt: %v\n%s", err, out)
	}
	// schedules live in PostgreSQL
	if _, err = run(t, cfg, "maintenance", "set", "--prune", "manual", "--gc-grace", "36h", "--reverify-days", "10"); err != nil {
		t.Fatal(err)
	}
	if out, err = run(t, cfg, "maintenance", "show"); err != nil || !strings.Contains(out, "prune      manual") || !strings.Contains(out, "36h0m0s") || !strings.Contains(out, "10d") {
		t.Fatalf("maintenance: %v\n%s", err, out)
	}
	if _, err = run(t, cfg, "maintenance", "set", "--gc-grace", "1h"); err == nil {
		t.Fatal("unsafe grace accepted")
	}
	// credentials are sealed, never listed with their secret, never deletable while in use
	t.Setenv("INCUS_BACKUP_S3_SECRET_KEY", "super-secret-value")
	if out, err = run(t, cfg, "credential", "create", "wasabi", "--access-key", "AKIA123"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err = run(t, cfg, "credential", "list"); err != nil || !strings.Contains(out, "AKIA123") || strings.Contains(out, "super-secret-value") {
		t.Fatalf("credential list: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "config", "export"); err != nil || strings.Contains(out, "super-secret-value") || !strings.Contains(out, "maintenance") {
		t.Fatalf("export: %v\n%s", err, out)
	}
	if _, err = run(t, cfg, "credential", "delete", "wasabi"); err != nil {
		t.Fatalf("an unused credential must be deletable: %v", err)
	}
	if out, err = run(t, cfg, "object", "notes", obj, "the shop server"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, _ = run(t, cfg, "object", "notes", obj); !strings.Contains(out, "the shop server") {
		t.Fatalf("object notes:\n%s", out)
	}
}

// With a daemon running, destructive CLI commands are forwarded to it over the admin socket.
func TestDestructiveCLICommandsAreForwardedToARunningDaemon(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "server.yaml")
	url := testdb.New(t)
	state := filepath.Join(dir, "state")
	if _, err := run(t, cfg, "init", "--state-dir", state, "--database-url", url, "--repository", "custody", "--datastore", filepath.Join(dir, "ds"), "--san", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	daemon := service.New(d, state, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	release, err := daemon.BecomeDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	sock := filepath.Join(state, "admin.sock")
	go func() { _ = (&mgmt.Server{Svc: daemon}).Serve(ctx, sock) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := run(t, cfg, "gc")
	if err != nil || !strings.Contains(out, "scanned") {
		t.Fatalf("gc through the daemon: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "prune"); err != nil || !strings.Contains(out, "kept") {
		t.Fatalf("prune through the daemon: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "verify"); err != nil || !strings.Contains(out, "checked") {
		t.Fatalf("verify through the daemon: %v\n%s", err, out)
	}
	// a second `serve` is refused while the first one runs
	if _, err := run(t, cfg, "serve"); err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("second serve: %v", err)
	}
	// without the socket the CLI says what to do instead of doing it unsafely
	cancel()
	time.Sleep(200 * time.Millisecond)
	if _, err := run(t, cfg, "gc"); err == nil {
		t.Fatal("GC ran from the CLI while the daemon still holds the lock and its socket is gone")
	}
}

func TestParseRateIsStrict(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "1048576": 1048576, "20MB": 20_000_000, "20mb": 20_000_000, "500KiB": 512_000, "1.5MiB": 1_572_864, "2gib": 2 << 30} {
		if got, err := parseRate(in); err != nil || got != want {
			t.Errorf("%q: %d %v (want %d)", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "0.5", "-5", "fast", "20 parsecs", "MB", "1e3"} {
		if v, err := parseRate(bad); err == nil {
			t.Errorf("%q accepted as %d", bad, v)
		}
	}
}
