package maint_test

import (
	"context"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/maint"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/server/tasks"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// Scheduled jobs run as tasks started by "system", each linked from its maintenance run; a destructive
// job is skipped (not failed) while another destructive task holds the repository, and runs later.
func TestScheduledJobsRunAsTasksAndYieldToABusyRepository(t *testing.T) {
	n := testsupport.NewNode(t)
	ctx := context.Background()
	m := make([]byte, 32)
	m[0] = 1
	k, _ := format.DeriveKeys(m)
	_ = n.Svc.SetPolicy(ctx, "custody", api.Policy{Name: "short", KeepLast: 1})
	for i := 1; i <= 2; i++ {
		backup(t, n, k, i, n.Now.Add(time.Duration(i)*time.Hour))
	}
	ms := service.DefaultMaintenance()
	ms.GC, ms.Verify, ms.Scrub, ms.Reconcile = "manual", "manual", "manual", "manual"
	if err := n.Svc.SetMaintenance(ctx, "custody", ms); err != nil {
		t.Fatal(err)
	}
	tm := &tasks.Manager{Pool: n.Svc.DB.Pool(), Now: n.Svc.Now}
	t.Cleanup(tm.Close)
	r := &maint.Runner{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Tasks: tm}
	count := func(q string, a ...any) int {
		var c int
		_ = n.Svc.DB.Pool().QueryRow(ctx, q, a...).Scan(&c)
		return c
	}
	// another destructive task holds the repository: retention waits
	hold := make(chan struct{})
	h, err := tm.Start(ctx, tasks.Spec{Kind: "gc", RepositoryID: n.Repo.ID, Repository: "custody", Exclusive: true},
		func(ctx context.Context) (any, error) { <-hold; return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	n.Now = time.Date(2026, 10, 8, 5, 0, 0, 0, time.UTC)
	r.Tick(ctx)
	if count(`SELECT count(*) FROM maintenance_runs WHERE kind='retention'`) != 0 || count(`SELECT count(*) FROM snapshots`) != 2 {
		t.Fatal("retention ran while the repository was busy")
	}
	if count(`SELECT count(*) FROM tasks WHERE kind='capacity' AND state='succeeded' AND started_by='system' AND actor_kind='system'`) != 1 {
		t.Fatal("the non-destructive capacity job must still run as a task")
	}
	close(hold)
	if _, err := h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	n.Now = n.Now.Add(time.Minute)
	r.Tick(ctx)
	if count(`SELECT count(*) FROM snapshots`) != 1 {
		t.Fatal("retention did not run once the repository was free")
	}
	if count(`SELECT count(*) FROM maintenance_runs m JOIN tasks t ON t.id=m.task_id WHERE m.kind='retention' AND t.kind='retention' AND t.exclusive AND t.state='succeeded'`) != 1 {
		t.Fatal("the scheduled retention run is not linked to its task")
	}
}
