package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

// The operations commands against a running daemon (admin socket): repo mode, task list/show/cancel,
// jobs list/create/cancel, alerts list/ack, audit list, repo detach, object delete's checks.
func TestOperationsCLIOverTheAdminSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "server.yaml")
	url := testdb.New(t)
	state := filepath.Join(dir, "state")
	if _, err := run(t, cfg, "init", "--state-dir", state, "--database-url", url, "--repository", "custody", "--datastore", filepath.Join(dir, "ds"), "--san", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// without a daemon the API commands say so
	if _, err := run(t, cfg, "task", "list"); err == nil || !strings.Contains(err.Error(), "admin socket") {
		t.Fatalf("task list without a daemon: %v", err)
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
	srv := &mgmt.Server{Svc: daemon, Auth: auth.New(d.Pool(), daemon.Sealer("user-totp"))}
	defer srv.Close()
	go func() { _ = srv.Serve(ctx, sock) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// repo mode (direct: any process may set it)
	if out, err := run(t, cfg, "repo", "mode", "custody", "read-only", "--reason", "disk swap"); err != nil || !strings.Contains(out, "read-only") {
		t.Fatalf("repo mode: %v\n%s", err, out)
	}
	if r, _ := daemon.Repository(ctx, "custody"); r.Mode != "read-only" || r.ModeReason != "disk swap" {
		t.Fatalf("%+v", r)
	}
	if _, err := run(t, cfg, "repo", "mode", "custody", "sideways"); err == nil {
		t.Fatal("bad mode accepted")
	}
	if _, err := run(t, cfg, "gc"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("gc in read-only: %v", err)
	}
	if _, err := run(t, cfg, "repo", "mode", "custody", "online"); err != nil {
		t.Fatal(err)
	}

	// tasks: the forwarded GC is a task; list, show (with log), cancel of an ended task
	if out, err := run(t, cfg, "gc"); err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	out, err := run(t, cfg, "task", "list", "--kind", "gc")
	if err != nil || !strings.Contains(out, "succeeded") || !strings.Contains(out, "local") {
		t.Fatalf("task list: %v\n%s", err, out)
	}
	id := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`).FindString(out)
	if out, err = run(t, cfg, "task", "show", id); err != nil || !strings.Contains(out, "State: succeeded") || !strings.Contains(out, "mark:") {
		t.Fatalf("task show: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "task", "show", "--json", id); err != nil || !strings.Contains(out, `"kind": "gc"`) {
		t.Fatalf("task show --json: %v\n%s", err, out)
	}
	if _, err = run(t, cfg, "task", "cancel", id); err == nil || !strings.Contains(err.Error(), "already ended") {
		t.Fatalf("cancel an ended task: %v", err)
	}

	// jobs: a host row (enrolment needs the agent listener, not under test here)
	if _, err := d.Pool().Exec(ctx, `INSERT INTO hosts (repository_id, name, scope, cert_fingerprint) SELECT id, 'benfica', 'backup', 'fp-cli-test' FROM repositories WHERE name='custody'`); err != nil {
		t.Fatal(err)
	}
	if out, err = run(t, cfg, "jobs", "create", "backup_now", "benfica", "--params", `{"project":"default"}`); err != nil || !strings.Contains(out, "queued") {
		t.Fatalf("jobs create: %v\n%s", err, out)
	}
	jid := regexp.MustCompile(`Job (\S+) queued`).FindStringSubmatch(out)[1]
	if out, err = run(t, cfg, "jobs", "list"); err != nil || !strings.Contains(out, jid) || !strings.Contains(out, "backup_now") {
		t.Fatalf("jobs list: %v\n%s", err, out)
	}
	if _, err = run(t, cfg, "jobs", "cancel", jid); err != nil {
		t.Fatal(err)
	}
	if out, _ = run(t, cfg, "jobs", "list", "--state", "cancelled"); !strings.Contains(out, jid) {
		t.Fatalf("cancelled job not listed:\n%s", out)
	}
	if _, err = run(t, cfg, "jobs", "create", "launch_rockets", "benfica"); err == nil {
		t.Fatal("unknown job type accepted")
	}

	// alerts
	if _, err := d.Pool().Exec(ctx, `INSERT INTO notify_alerts (key, type, severity, title, state, first_seen, last_seen, last_change)
		VALUES ('t:1','host_offline','warning','benfica is silent','firing',now(),now(),now())`); err != nil {
		t.Fatal(err)
	}
	if out, err = run(t, cfg, "alerts", "list"); err != nil || !strings.Contains(out, "benfica is silent") {
		t.Fatalf("alerts list: %v\n%s", err, out)
	}
	aid := strings.Fields(strings.Split(out, "\n")[1])[0]
	if out, err = run(t, cfg, "alerts", "ack", aid, "--comment", "on it"); err != nil || !strings.Contains(out, "acknowledged") {
		t.Fatalf("alerts ack: %v\n%s", err, out)
	}

	// object delete: typed confirmation and a clear error for an unknown object
	if _, err = run(t, cfg, "object", "delete", "default/vm1"); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("object delete without confirm: %v", err)
	}
	if _, err = run(t, cfg, "object", "delete", "default/vm1", "--confirm", "default/vm1"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("object delete of an unknown object: %v", err)
	}

	// repo detach: offline first, every identity named, then forwarded to the daemon
	if _, err = run(t, cfg, "repo", "detach", "custody"); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("detach online: %v", err)
	}
	_, _ = run(t, cfg, "repo", "mode", "custody", "offline")
	if _, err = run(t, cfg, "repo", "detach", "custody"); err == nil || !strings.Contains(err.Error(), "benfica") {
		t.Fatalf("detach with an enrolled host: %v", err)
	}
	if _, err = run(t, cfg, "repo", "destroy", "custody", "--confirm", "nope", "--force-hosts", "benfica"); err == nil || !strings.Contains(err.Error(), "type the repository name") {
		t.Fatalf("destroy confirm: %v", err)
	}
	if out, err = run(t, cfg, "repo", "detach", "custody", "--force-hosts", "benfica"); err != nil || !strings.Contains(out, "detached") {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	if out, _ = run(t, cfg, "repo", "list"); strings.Contains(out, "custody") {
		t.Fatalf("still listed:\n%s", out)
	}

	// audit: the API calls above are recorded
	if out, err = run(t, cfg, "audit", "list", "--limit", "200"); err != nil || !strings.Contains(out, "post /jobs") || !strings.Contains(out, "delete /repositories/{name}") {
		t.Fatalf("audit list: %v\n%s", err, out)
	}
	if out, err = run(t, cfg, "audit", "list", "--action", "repository detach"); err != nil || !strings.Contains(out, "custody") {
		t.Fatalf("audit list --action: %v\n%s", err, out)
	}
}
