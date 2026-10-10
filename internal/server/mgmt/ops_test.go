package mgmt_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/server/tasks"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// opsEnv is a management server on an admin socket whose Server value the test can reach.
type opsEnv struct {
	t *testing.T
	n *testsupport.Node
	s *mgmt.Server
	c *client
}

func newOps(t *testing.T, n *testsupport.Node, ops mgmt.Ops) *opsEnv {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Ops: ops, Hub: mgmt.NewHub()}
	t.Cleanup(s.Close)
	go func() { _ = s.Serve(ctx, sock) }()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &opsEnv{t: t, n: n, s: s, c: &client{t: t, http: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}}}
}

// start posts a task route and returns the accepted task.
func (e *opsEnv) start(method, path string, body any) tasks.Task {
	e.t.Helper()
	var out struct {
		Task tasks.Task `json:"task"`
	}
	code, b := e.c.do(method, path, body, &out)
	if code != http.StatusAccepted || out.Task.ID == "" {
		e.t.Fatalf("%s %s -> %d %s (want 202 with a task)", method, path, code, b)
	}
	return out.Task
}

// wait polls a task until it ended.
func (e *opsEnv) wait(id string) tasks.Detail {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var d tasks.Detail
		e.c.must("GET", "/api/v1/tasks/"+id, nil, &d)
		if d.Done() {
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("task %s did not end", id)
	return tasks.Detail{}
}

func chunkFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(root, "chunks"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// Manual maintenance runs as tasks: 202 + task id, a log, the maintenance run linked to it, ?wait=1
// keeps the synchronous answer, and the history is listed and filtered.
func TestMaintenanceRoutesRunAsTasks(t *testing.T) {
	n := testsupport.NewNode(t)
	e := newOps(t, n, mgmt.Ops{})
	events, stop := e.s.Hub.Subscribe()
	defer stop()
	k := masterKeys()
	e.c.must("PUT", "/api/v1/policies/p", api.Policy{KeepLast: 1}, nil)
	for i := 1; i <= 3; i++ {
		backup(t, n, k, fmt.Sprintf("2026100%d120000-%08x", i, i), n.Now.Add(time.Duration(i)*time.Hour), fmt.Sprintf("v%d", i))
	}

	tk := e.start("POST", "/api/v1/verify", map[string]any{"mode": "all"})
	if tk.Kind != "scrub" || tk.StartedBy != "local" || tk.ActorKind != "local" || tk.Repository != "custody" || !tk.Exclusive {
		t.Fatalf("accepted %+v", tk)
	}
	d := e.wait(tk.ID)
	if d.State != tasks.Succeeded || d.Progress != 100 || len(d.Log) == 0 || !strings.Contains(string(d.Result), `"ok":true`) {
		t.Fatalf("verify task %+v log %+v", d.Task, d.Log)
	}
	var linked int
	_ = n.Svc.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM maintenance_runs WHERE task_id=$1 AND kind='scrub' AND status='ok'`, tk.ID).Scan(&linked)
	if linked != 1 {
		t.Fatal("the maintenance run is not linked to its task")
	}

	// retention apply as a task, the preview stays synchronous
	var pre struct{ Removed int }
	e.c.must("POST", "/api/v1/prune?dry_run=1", nil, &pre)
	if pre.Removed != 2 {
		t.Fatalf("preview %+v", pre)
	}
	pt := e.start("POST", "/api/v1/prune", nil)
	if d := e.wait(pt.ID); d.State != tasks.Succeeded || !strings.Contains(string(d.Result), `"removed":2`) {
		t.Fatalf("prune %+v", d.Task)
	} else {
		removed := 0
		for _, l := range d.Log {
			if strings.HasPrefix(l.Line, "removed ") {
				removed++
			}
		}
		if removed != 2 {
			t.Fatalf("prune log %+v", d.Log)
		}
	}
	// ?wait=1: the old synchronous shape, still recorded as a task
	var gc struct {
		Scanned int  `json:"scanned"`
		DryRun  bool `json:"dry_run"`
	}
	e.c.must("POST", "/api/v1/gc?dry_run=1&wait=1", nil, &gc)
	if !gc.DryRun || gc.Scanned == 0 {
		t.Fatalf("gc dry run %+v", gc)
	}
	var rec struct{ Inserted, Removed int }
	e.c.must("POST", "/api/v1/reconcile?wait=1", nil, &rec)
	var all []tasks.Task
	e.c.must("GET", "/api/v1/tasks", nil, &all)
	if len(all) != 4 || all[0].Kind != "reconcile" || all[1].Kind != "gc" {
		t.Fatalf("history %+v", all)
	}
	e.c.must("GET", "/api/v1/tasks?kind=gc&state=succeeded&repository=custody", nil, &all)
	if len(all) != 1 || all[0].Params == nil || !strings.Contains(string(all[0].Params), `"dry_run":true`) {
		t.Fatalf("filtered %+v", all)
	}
	if code, _ := e.c.do("GET", "/api/v1/tasks?state=weird", nil, nil); code != 400 {
		t.Fatal("bad state filter")
	}
	if code, _ := e.c.do("GET", "/api/v1/tasks/00000000-0000-0000-0000-000000000000", nil, nil); code != 404 {
		t.Fatal("unknown task")
	}
	// pre-flight errors are immediate, not failed tasks
	if code, _ := e.c.do("POST", "/api/v1/gc?grace_hours=1", nil, nil); code != 400 {
		t.Fatal("short grace accepted")
	}
	if code, _ := e.c.do("POST", "/api/v1/verify", map[string]any{"mode": "sometimes"}, nil); code != 400 {
		t.Fatal("bad verify mode accepted")
	}
	if code, _ := e.c.do("POST", "/api/v1/sync/nope/run", nil, nil); code != 404 {
		t.Fatal("unknown sync job accepted")
	}
	// live events of type "task"
	seen := map[string]bool{}
	timeout := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev := <-events:
			if ev.Type == "task" && strings.Contains(string(ev.Data), tk.ID) {
				seen[tk.ID] = true
				break loop
			}
		case <-timeout:
			break loop
		}
	}
	if !seen[tk.ID] {
		t.Fatal("no SSE task event")
	}
}

// Cancelling a GC in the middle leaves the repository consistent: nothing is swept, every snapshot
// still verifies, the run is "cancelled" (not failed), and a later GC removes the garbage.
func TestCancelMidGCLeavesTheRepositoryConsistent(t *testing.T) {
	n := testsupport.NewNode(t)
	e := newOps(t, n, mgmt.Ops{})
	ctx := context.Background()
	k := masterKeys()
	for i := 1; i <= 3; i++ {
		backup(t, n, k, fmt.Sprintf("2026100%d120000-%08x", i, i), n.Now.Add(time.Duration(i)*time.Hour), fmt.Sprintf("unique-%d", i), "shared")
	}
	var snaps []api.SnapshotRef
	e.c.must("GET", "/api/v1/snapshots", nil, &snaps)
	// forget one: its unique chunk becomes garbage; age every chunk past the grace period
	e.c.must("DELETE", "/api/v1/snapshots/"+snaps[0].GroupID+"/"+snaps[0].SnapshotID, nil, nil)
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	before := chunkFiles(t, n.Repo.Path)
	for _, f := range before {
		_ = os.Chtimes(f, old, old)
	}
	st, err := n.Svc.Store(n.Repo)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	st.SetFault(func(p string) error {
		if p == "gc:after-mark" {
			close(reached)
			<-release
		}
		return nil
	})
	tk := e.start("POST", "/api/v1/gc?grace_hours=12", nil)
	<-reached
	// the repository is busy: a second destructive task is refused with 409 naming the GC
	if code, body := e.c.do("POST", "/api/v1/prune", nil, nil); code != 409 || !strings.Contains(body, tk.ID) {
		t.Fatalf("prune during GC -> %d %s", code, body)
	}
	var ct tasks.Task
	e.c.must("POST", "/api/v1/tasks/"+tk.ID+"/cancel", nil, &ct)
	if !ct.CancelRequested {
		t.Fatalf("%+v", ct)
	}
	close(release)
	d := e.wait(tk.ID)
	st.SetFault(nil)
	if d.State != tasks.Cancelled || d.Error != "cancelled by local" {
		t.Fatalf("cancelled GC %+v", d.Task)
	}
	if after := chunkFiles(t, n.Repo.Path); len(after) != len(before) {
		t.Fatalf("a cancelled GC deleted chunks: %d -> %d", len(before), len(after))
	}
	if err := st.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = n.Svc.DB.Pool().QueryRow(ctx, `SELECT status FROM maintenance_runs WHERE task_id=$1`, tk.ID).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("maintenance run status %q", status)
	}
	var vr struct {
		Ok    bool `json:"ok"`
		Stats struct{ Snapshots int }
	}
	e.c.must("POST", "/api/v1/verify?wait=1", map[string]any{"mode": "all"}, &vr)
	if !vr.Ok || vr.Stats.Snapshots != 2 {
		t.Fatalf("verify after the cancelled GC %+v", vr)
	}
	// the garbage was real: an uncancelled GC removes exactly the forgotten snapshot's unique chunk
	var gc struct{ Removed int }
	e.c.must("POST", "/api/v1/gc?grace_hours=12&wait=1", nil, &gc)
	if gc.Removed != 1 {
		t.Fatalf("GC removed %d", gc.Removed)
	}
	// cancelling an ended task is a conflict
	if code, _ := e.c.do("POST", "/api/v1/tasks/"+tk.ID+"/cancel", nil, nil); code != 409 {
		t.Fatal("cancel of an ended task")
	}
}

// Who may cancel what: the role that may start a task kind may cancel it.
func TestTaskCancelRoles(t *testing.T) {
	ctx := context.Background()
	w := newWeb(t)
	for _, r := range []string{"admin", "operator", "readonly"} {
		if _, err := w.auth.CreateUser(ctx, r, pw, roleOf(r), "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	srv := w.srv.TaskManager()
	park := func(kind string) string {
		h, err := srv.Start(ctx, tasks.Spec{Kind: kind}, func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() })
		if err != nil {
			t.Fatal(err)
		}
		return h.ID
	}
	gc, verify := park("gc"), park("verify")
	w.login("readonly")
	if code, _ := w.req("POST", "/api/v1/tasks/"+verify+"/cancel", nil, w.h()); code != 403 {
		t.Fatalf("readonly cancel -> %d", code)
	}
	if code, _ := w.req("GET", "/api/v1/tasks/"+verify, nil, nil); code != 200 {
		t.Fatalf("readonly read -> %d", code)
	}
	w.login("operator")
	if code, b := w.req("POST", "/api/v1/tasks/"+gc+"/cancel", nil, w.h()); code != 403 || !strings.Contains(string(b), "gc") {
		t.Fatalf("operator cancel gc -> %d %s", code, b)
	}
	if code, _ := w.req("POST", "/api/v1/tasks/"+verify+"/cancel", nil, w.h()); code != 200 {
		t.Fatalf("operator cancel verify -> %d", code)
	}
	w.login("admin")
	if code, _ := w.req("POST", "/api/v1/tasks/"+gc+"/cancel", nil, w.h()); code != 200 {
		t.Fatalf("admin cancel gc -> %d", code)
	}
	for _, id := range []string{gc, verify} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			d, _ := srv.Get(ctx, id, 0, 0)
			if d.Done() {
				if d.State != tasks.Cancelled || (id == gc && d.Error != "cancelled by admin") || (id == verify && d.Error != "cancelled by operator") {
					t.Fatalf("%+v", d.Task)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("not cancelled")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// F2: a repository in maintenance refuses a backup when the session opens (503, before the host reads
// anything), and a commit refused by maintenance is a 503 too, never a 400.
func TestMaintenanceModeRefusesBackupsWith503(t *testing.T) {
	n := testsupport.NewNode(t)
	e := newOps(t, n, mgmt.Ops{})
	ctx := context.Background()
	k := masterKeys()
	cl := hostClient(n, k)
	req := api.SessionRequest{GroupID: group, SnapshotID: "20261007120000-00000001", Created: n.Now, Policy: "p", Project: "default", Object: "vm/root", Kind: "block"}
	e.c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "read-only", "reason": "disk swap"}, nil)
	w, err := cl.Whoami(ctx)
	if err != nil || w.RepositoryMode != "read-only" {
		t.Fatalf("whoami %+v %v", w, err)
	}
	_, err = cl.Begin(ctx, req)
	var ae *remote.APIError
	if !errors.As(err, &ae) || ae.Status != 503 || !strings.Contains(ae.Msg, "read-only") {
		t.Fatalf("begin in read-only: %v", err)
	}
	e.c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "offline"}, nil)
	if _, err = cl.Begin(ctx, req); !remote.IsStatus(err, 503) {
		t.Fatalf("begin offline: %v", err)
	}
	// a session opened while online, frozen before its commit
	e.c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "online"}, nil)
	s, err := cl.Begin(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	id, blob, _ := k.EncodeChunk([]byte("payload"), formatOpts())
	if err := s.PutChunk(ctx, id.String(), blob); err != nil {
		t.Fatal(err)
	}
	idx, man := indexAndManifest(t, k, req.SnapshotID, id, 7)
	_ = s.PutIndex(ctx, idx)
	_ = s.PutManifest(ctx, man)
	e.c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "read-only"}, nil)
	_, err = s.Commit(ctx, api.CommitRequest{LogicalBytes: 7})
	if !errors.As(err, &ae) || ae.Status != 503 {
		t.Fatalf("commit in read-only: %v (want 503, never 400)", err)
	}
	var snaps []api.SnapshotRef
	e.c.must("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 0 {
		t.Fatal("a refused commit left a snapshot")
	}
}

// A deferred run (only maintenance refusals) is recorded as such and never raises backup_failed; it does
// not resolve an open backup_failed alert either (the failure was not fixed by it).
func TestDeferredRunsDoNotAlert(t *testing.T) {
	n := testsupport.NewNode(t)
	ctx := context.Background()
	k := masterKeys()
	cl := hostClient(n, k)
	ne := mgmt.NewNotifyEngine(n.Svc)
	firing := func() int {
		var c int
		_ = n.Svc.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM notify_alerts WHERE type='backup_failed' AND state='firing'`).Scan(&c)
		return c
	}
	run := func(status string, ok, failed, deferred int) {
		t.Helper()
		id, err := cl.StartRun(ctx, api.RunStart{Started: n.Now, ObjectsTotal: ok + failed + deferred})
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.FinishRun(ctx, id, api.RunFinish{Status: status, ObjectsOK: ok, ObjectsFailed: failed, ObjectsDeferred: deferred}); err != nil {
			t.Fatal(err)
		}
		n.Now = n.Now.Add(time.Minute)
		if _, err := ne.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	run("deferred", 0, 0, 3)
	if firing() != 0 {
		t.Fatal("a deferred run raised backup_failed")
	}
	var st string
	var def int
	_ = n.Svc.DB.Pool().QueryRow(ctx, `SELECT status, objects_deferred FROM backup_runs ORDER BY started_at DESC, finished_at DESC LIMIT 1`).Scan(&st, &def)
	if st != "deferred" || def != 3 {
		t.Fatalf("recorded %s %d", st, def)
	}
	run("failed", 0, 2, 0)
	if firing() != 1 {
		t.Fatal("a failed run must alert")
	}
	run("deferred", 0, 0, 2)
	if firing() != 1 {
		t.Fatal("a deferred run must not resolve the failure alert")
	}
	run("ok", 2, 0, 0)
	if firing() != 0 {
		t.Fatal("a good run resolves it")
	}
	if err := cl.FinishRun(ctx, "00000000-0000-0000-0000-000000000000", api.RunFinish{Status: "postponed"}); !remote.IsStatus(err, 400) {
		t.Fatalf("unknown status: %v", err)
	}
}

func roleOf(name string) auth.Role { return auth.Role(name) }

func formatOpts() format.EncodeOpts { return format.EncodeOpts{} }

func indexAndManifest(t *testing.T, k *format.Keys, snap string, id format.ID, size uint64) ([]byte, []byte) {
	t.Helper()
	x := &format.Index{Kind: format.IndexDynamic, Size: size, Entries: []format.Entry{{End: size, ID: id}}}
	idx, err := x.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	man, err := k.SealManifest(&format.Manifest{SnapshotID: snap, IndexDigest: format.IndexDigest(idx)}, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	return idx, man
}
