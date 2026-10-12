package maint_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/server/maint"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) { testdb.Main(m) }

const group = "0123456789abcdef0123456789abcdef"

var hosts = map[*testsupport.Node]*remote.Client{}

func hostFor(n *testsupport.Node, k *format.Keys) *remote.Client {
	if c := hosts[n]; c != nil {
		return c
	}
	c := n.Client(n.Enrol("benfica", api.ScopeBackup), k.DomainID)
	hosts[n] = c
	return c
}

func backup(t *testing.T, n *testsupport.Node, k *format.Keys, i int, created time.Time) {
	t.Helper()
	ctx := context.Background()
	cl := hostFor(n, k)
	snap := fmt.Sprintf("2026100%d120000-%08x", i, i)
	s, err := cl.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: snap, Created: created, Policy: "short", Project: "p", Object: "vm/root", Kind: "block"})
	if err != nil {
		t.Fatal(err)
	}
	id, blob, _ := k.EncodeChunk([]byte(fmt.Sprintf("data-%d", i)), format.EncodeOpts{})
	if err := s.PutChunk(ctx, id.String(), blob); err != nil {
		t.Fatal(err)
	}
	idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: 6, Entries: []format.Entry{{End: 6, ID: id}}}).Marshal()
	_ = s.PutIndex(ctx, idx)
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created}, group, nil)
	_ = s.PutManifest(ctx, man)
	if _, err := s.Commit(ctx, api.CommitRequest{LogicalBytes: 6}); err != nil {
		t.Fatal(err)
	}
}

func TestTickRunsHousekeepingOnSchedule(t *testing.T) {
	n := testsupport.NewNode(t)
	ctx := context.Background()
	m := make([]byte, 32)
	m[0] = 1
	k, _ := format.DeriveKeys(m)
	_ = n.Svc.SetPolicy(ctx, "custody", api.Policy{Name: "short", KeepLast: 1})
	for i := 1; i <= 3; i++ {
		backup(t, n, k, i, n.Now.Add(time.Duration(i)*time.Hour))
	}
	ms := service.DefaultMaintenance()
	ms.GC, ms.Verify, ms.Scrub, ms.Reconcile = "manual", "manual", "manual", "manual"
	if err := n.Svc.SetMaintenance(ctx, "custody", ms); err != nil {
		t.Fatal(err)
	}
	r := &maint.Runner{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}}

	n.Now = time.Date(2026, 10, 8, 5, 0, 0, 0, time.UTC)
	r.Tick(ctx)
	count := func(q string) int {
		var c int
		_ = n.Svc.DB.Pool().QueryRow(ctx, q).Scan(&c)
		return c
	}
	if count(`SELECT count(*) FROM capacity_samples`) != 1 {
		t.Fatal("capacity not sampled")
	}
	// prune ran at first tick (never ran before): short policy keeps 1
	if got := count(`SELECT count(*) FROM snapshots`); got != 1 {
		t.Fatalf("retention did not run: %d snapshots left", got)
	}
	// next minute: nothing is due again
	n.Now = n.Now.Add(time.Minute)
	r.Tick(ctx)
	if count(`SELECT count(*) FROM capacity_samples`) != 1 || count(`SELECT count(*) FROM maintenance_runs WHERE kind='retention'`) != 1 {
		t.Fatal("jobs ran twice")
	}
	// an hour later capacity is sampled again, retention not yet (daily)
	n.Now = n.Now.Add(time.Hour)
	r.Tick(ctx)
	if count(`SELECT count(*) FROM capacity_samples`) != 2 || count(`SELECT count(*) FROM maintenance_runs WHERE kind='retention'`) != 1 {
		t.Fatal("hourly capacity / daily retention schedule wrong")
	}
	n.Now = n.Now.Add(24 * time.Hour)
	r.Tick(ctx)
	if count(`SELECT count(*) FROM maintenance_runs WHERE kind='retention'`) != 2 {
		t.Fatal("retention did not run the next day")
	}
}
