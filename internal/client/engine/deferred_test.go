package engine_test

import (
	"context"
	"testing"

	"github.com/unidoc/incus-backup/internal/client/engine"
)

// A repository in maintenance defers the backup: nothing is read for a target set that would refuse
// it, the run is recorded as "deferred" (not failed), and the CLI does not count it as a failure.
func TestMaintenanceDefersInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "cikarang", "amsterdam")
	w.policy("prod", []string{"cikarang"}, "all")
	w.policy("both", []string{"cikarang", "amsterdam"}, "all")
	setupVM(w, 1)
	w.inc.AddVM("default", "porto", map[string]int{"root": 4 * MiB}, true)
	w.inc.SetConfig("default", "porto", engine.KeyPolicy, "both")
	w.inc.GuestWrite("default", "porto", "root", 0, rnd(4*MiB, 2))
	if err := w.nodes["cikarang"].Svc.SetRepositoryMode(ctx, "custody", "read-only", "disk swap"); err != nil {
		t.Fatal(err)
	}
	rep := w.eng.Run(ctx, w.objects(), engine.RunOptions{})
	if rep.Failed() != 0 || rep.Deferred() == 0 {
		t.Fatalf("failed %d deferred %d", rep.Failed(), rep.Deferred())
	}
	for _, r := range rep.Results {
		if !r.Deferred() {
			t.Fatalf("%s: not deferred: err=%v targets=%+v", r.Object.Name, r.Err, r.Targets)
		}
		// bosch goes only to the frozen repository: it must not have been read at all
		if r.Object.Instance != nil && r.Object.Instance.Name == "bosch" && (r.Read != 0 || !engine.IsDeferred(r.Err)) {
			t.Fatalf("bosch was read (%d bytes) for a repository that refuses it", r.Read)
		}
	}
	// porto also goes to amsterdam (online): it was backed up there, deferred for cikarang
	var porto *engine.Result
	for _, r := range rep.Results {
		if r.Object.Instance != nil && r.Object.Instance.Name == "porto" && r.Object.Kind == engine.KindBlock {
			porto = r
		}
	}
	if porto == nil || porto.Read == 0 {
		t.Fatal("porto was not backed up to the online target")
	}
	committed := map[string]bool{}
	for _, tr := range porto.Targets {
		committed[tr.Target] = tr.Committed
	}
	if !committed["amsterdam"] || committed["cikarang"] {
		t.Fatalf("porto targets %+v", porto.Targets)
	}
	status := func(target string) (string, int, int) {
		var st string
		var failed, deferred int
		_ = w.nodes[target].Svc.DB.Pool().QueryRow(ctx, `SELECT status, objects_failed, objects_deferred FROM backup_runs ORDER BY started_at DESC LIMIT 1`).Scan(&st, &failed, &deferred)
		return st, failed, deferred
	}
	if st, f, d := status("cikarang"); st != "deferred" || f != 0 || d == 0 {
		t.Fatalf("cikarang run %s failed=%d deferred=%d", st, f, d)
	}
	if st, _, _ := status("amsterdam"); st != "ok" {
		t.Fatalf("amsterdam run %s", st)
	}
	// online again: the next run backs everything up
	if err := w.nodes["cikarang"].Svc.SetRepositoryMode(ctx, "custody", "online", ""); err != nil {
		t.Fatal(err)
	}
	if rep := w.eng.Run(ctx, w.objects(), engine.RunOptions{}); rep.Failed() != 0 || rep.Deferred() != 0 {
		t.Fatalf("after maintenance: failed %d deferred %d", rep.Failed(), rep.Deferred())
	}
}
