package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/progress"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/tasks"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func manager(t *testing.T) (*tasks.Manager, *testsupport.Node) {
	t.Helper()
	n := testsupport.NewNode(t)
	m := &tasks.Manager{Pool: n.Svc.DB.Pool(), MaxLogLines: 50}
	t.Cleanup(m.Close)
	return m, n
}

func TestTaskLifecycleLogProgressAndResult(t *testing.T) {
	m, n := manager(t)
	ctx := context.Background()
	var mu sync.Mutex
	var events []tasks.Task
	m.Publish = func(t tasks.Task) { mu.Lock(); events = append(events, t); mu.Unlock() }
	id, res, err := m.Run(ctx, tasks.Spec{Kind: "gc", RepositoryID: n.Repo.ID, Repository: "custody", Actor: "anna", ActorKind: tasks.ActorUser, Params: map[string]int{"x": 1}},
		func(ctx context.Context) (any, error) {
			if progress.TaskID(ctx) == "" {
				return nil, errors.New("no task id in the context")
			}
			for i := 0; i < 80; i++ {
				progress.Logf(ctx, progress.Info, "line %d", i)
			}
			progress.Set(ctx, 50, "half")
			return map[string]int{"removed": 7}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]int)["removed"] != 7 {
		t.Fatalf("result %v", res)
	}
	d, err := m.Get(ctx, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != tasks.Succeeded || d.Progress != 100 || d.StartedBy != "anna" || d.ActorKind != "user" || d.Repository != "custody" || string(d.Result) != `{"removed": 7}` {
		t.Fatalf("task %+v", d.Task)
	}
	// the log is capped (50 lines kept, 30 dropped) and says so
	if len(d.Log) != 50 || d.LogLines != 50 || d.LogDropped != 30 || d.Log[0].Line != "line 0" {
		t.Fatalf("log: %d lines, counted %d, dropped %d", len(d.Log), d.LogLines, d.LogDropped)
	}
	// tailing: only lines after a seq
	tail, _ := m.Get(ctx, id, 45, 0)
	if len(tail.Log) != 5 || tail.Log[0].Seq != 46 || tail.NextSeq != 50 {
		t.Fatalf("tail %d lines from %d next %d", len(tail.Log), tail.Log[0].Seq, tail.NextSeq)
	}
	mu.Lock()
	last := events[len(events)-1]
	mu.Unlock()
	if last.ID != id || last.State != tasks.Succeeded {
		t.Fatalf("last event %+v", last)
	}
	// failure keeps the error
	_, _, err = m.Run(ctx, tasks.Spec{Kind: "verify"}, func(context.Context) (any, error) { return nil, errors.New("boom") })
	if err == nil {
		t.Fatal("want the error")
	}
	l, _ := m.List(ctx, tasks.Filter{State: tasks.Failed})
	if len(l) != 1 || l[0].Error != "boom" || l[0].Kind != "verify" || l[0].StartedBy != "system" {
		t.Fatalf("failed list %+v", l)
	}
	if _, err := m.List(ctx, tasks.Filter{State: "nope"}); !errors.Is(err, service.ErrBadRequest) {
		t.Fatalf("bad state filter: %v", err)
	}
	if _, err := m.Get(ctx, "not-an-id", 0, 0); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestCancelStopsTheTaskAndRecordsWho(t *testing.T) {
	m, n := manager(t)
	ctx := context.Background()
	started := make(chan struct{})
	h, err := m.Start(ctx, tasks.Spec{Kind: "gc", RepositoryID: n.Repo.ID, Repository: "custody", Exclusive: true}, func(ctx context.Context) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	// a second destructive task on the same repository is refused while the first runs
	if _, err := m.Start(ctx, tasks.Spec{Kind: "retention", RepositoryID: n.Repo.ID, Repository: "custody", Exclusive: true},
		func(context.Context) (any, error) { return nil, nil }); !errors.Is(err, tasks.ErrBusy) || !errors.Is(err, service.ErrConflict) {
		t.Fatalf("second exclusive task: %v", err)
	}
	// a non-destructive one is fine
	if _, _, err := m.Run(ctx, tasks.Spec{Kind: "reconcile", RepositoryID: n.Repo.ID, Repository: "custody"}, func(context.Context) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cancel(ctx, h.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome %v", err)
	}
	d, _ := m.Get(ctx, h.ID, 0, 0)
	if d.State != tasks.Cancelled || d.Error != "cancelled by bob" || !d.CancelRequested || d.Finished == nil {
		t.Fatalf("cancelled task %+v", d.Task)
	}
	// cancelling a finished task is a conflict
	if _, err := m.Cancel(ctx, h.ID, "bob"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("cancel twice: %v", err)
	}
	// the slot is free again
	if _, _, err := m.Run(ctx, tasks.Spec{Kind: "retention", RepositoryID: n.Repo.ID, Repository: "custody", Exclusive: true}, func(context.Context) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverFailsTasksOfADeadDaemonAndPurgeBoundsHistory(t *testing.T) {
	m, n := manager(t)
	ctx := context.Background()
	p := n.Svc.DB.Pool()
	// a task row left running by a daemon that died
	var id string
	if err := p.QueryRow(ctx, `INSERT INTO tasks (kind, repository_id, repository_name, state, exclusive, created_at, started_at) VALUES ('gc',$1,'custody','running',true,now(),now()) RETURNING id`, n.Repo.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// while it is "running" it holds the repository's slot
	if _, err := m.Start(ctx, tasks.Spec{Kind: "gc", RepositoryID: n.Repo.ID, Exclusive: true}, func(context.Context) (any, error) { return nil, nil }); !errors.Is(err, tasks.ErrBusy) {
		t.Fatalf("slot held by the stale task: %v", err)
	}
	// a fresh daemon (new manager) recovers
	m2 := &tasks.Manager{Pool: p}
	t.Cleanup(m2.Close)
	if k, err := m2.Recover(ctx); err != nil || k != 1 {
		t.Fatalf("recover %d %v", k, err)
	}
	d, _ := m2.Get(ctx, id, 0, 0)
	if d.State != tasks.Failed || d.Error != "server restarted while the task was running" {
		t.Fatalf("recovered %+v", d.Task)
	}
	if _, _, err := m2.Run(ctx, tasks.Spec{Kind: "gc", RepositoryID: n.Repo.ID, Exclusive: true}, func(context.Context) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	// purge: by age and by count
	for i := 0; i < 5; i++ {
		if _, _, err := m2.Run(ctx, tasks.Spec{Kind: fmt.Sprint("k", i)}, func(context.Context) (any, error) { return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = p.Exec(ctx, `UPDATE tasks SET finished_at=now() - interval '40 days', created_at=now() - interval '40 days' WHERE id=$1`, id)
	m3 := &tasks.Manager{Pool: p, KeepMax: 3}
	t.Cleanup(m3.Close)
	if k, err := m3.Purge(ctx); err != nil || k != 4 { // 1 too old + 3 beyond the newest 3 of the other 6
		t.Fatalf("purge %d %v", k, err)
	}
	if l, _ := m3.List(ctx, tasks.Filter{}); len(l) != 3 {
		t.Fatalf("left %d", len(l))
	}
}

func TestCloseCancelsRunningTasks(t *testing.T) {
	m, _ := manager(t)
	ctx := context.Background()
	h, err := m.Start(ctx, tasks.Spec{Kind: "verify"}, func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	d, _ := m.Get(ctx, h.ID, 0, 0)
	if d.State != tasks.Cancelled || d.Error != "cancelled by the server shutting down" {
		t.Fatalf("%+v", d.Task)
	}
	if _, err := m.Start(ctx, tasks.Spec{Kind: "x"}, func(context.Context) (any, error) { return nil, nil }); err == nil {
		t.Fatal("a closed manager must refuse new tasks")
	}
}
