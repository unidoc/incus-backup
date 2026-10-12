package app_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/jobspec"
	srvjobs "github.com/unidoc/incus-backup/internal/server/jobs"
)

func TestBackupNowJobDoesNotMoveThePolicySchedule(t *testing.T) {
	a, inc, n := newApp(t)
	ctx := context.Background()
	inc.AddVM("default", "bosch", map[string]int{"root": 4 << 20}, true)
	inc.SetConfig("default", "bosch", engine.KeyPolicy, "nightly")
	inc.GuestWrite("default", "bosch", "root", 0, []byte("hello"))
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	a.Eng.Now = func() time.Time { return now }
	js := srvjobs.For(n.Svc)
	raw, _ := json.Marshal(jobspec.BackupNow{Object: "bosch"})
	j, err := js.CreateJob(ctx, "alice", "custody", "benfica", "backup_now", raw)
	if err != nil {
		t.Fatal(err)
	}
	x, err := a.Executor()
	if err != nil {
		t.Fatal(err)
	}
	x.Heartbeat = 50 * time.Millisecond
	if ran, err := x.Poll(ctx, "cikarang", 2*time.Second); err != nil || !ran {
		t.Fatal(ran, err)
	}
	got, _ := js.GetJob(ctx, j.ID)
	if got.State != "succeeded" {
		t.Fatalf("%s: %s", got.State, got.Message)
	}
	var out struct {
		Objects []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"objects"`
	}
	_ = json.Unmarshal(got.Result, &out)
	if len(out.Objects) != 2 || !out.Objects[0].OK {
		t.Fatalf("%s", got.Result)
	}
	if st := a.State.Object("default/block/instance/bosch/root"); st.LastStatus != "ok" || st.LastSnap == "" {
		t.Fatalf("%+v", st)
	}
	if !a.State.Runs["nightly"].IsZero() {
		t.Fatal("a backup_now job moved the policy's schedule: tonight's run would be skipped")
	}
	// tonight's scheduled run still happens
	now = time.Date(2026, 10, 9, 3, 5, 0, 0, time.UTC)
	if rep, err := a.Tick(ctx); err != nil || rep == nil {
		t.Fatalf("scheduled run suppressed: %v %v", err, rep)
	}
	// types that are not allowed by default are refused by this host
	raw, _ = json.Marshal(jobspec.RestoreInstance{Project: "default", Instance: "bosch", AsName: "copy"})
	j2, _ := js.CreateJob(ctx, "alice", "custody", "benfica", "restore_instance", raw)
	x.Poll(ctx, "cikarang", 2*time.Second)
	if g, _ := js.GetJob(ctx, j2.ID); g.State != "failed" || !strings.Contains(g.Message, "not enabled") {
		t.Fatalf("%s %s", g.State, g.Message)
	}
}

func TestTickRunsScheduledRestoreTestsAndReportsThem(t *testing.T) {
	a, inc, n := newApp(t)
	ctx := context.Background()
	if err := n.Svc.SetPolicy(ctx, "custody", api.Policy{Name: "nightly", Schedule: "03:00", Targets: []string{"cikarang"}, RestoreTestSchedule: "daily"}); err != nil {
		t.Fatal(err)
	}
	inc.AddVM("default", "bosch", map[string]int{"root": 4 << 20}, true)
	inc.SetConfig("default", "bosch", engine.KeyPolicy, "nightly")
	inc.GuestWrite("default", "bosch", "root", 0, []byte("hello"))
	docs := t.TempDir()
	if err := os.WriteFile(docs+"/a.txt", []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	inc.AddFSVolume("default", "docs", docs)
	inc.Vols["default/docs"].Config[engine.KeyPolicy] = "nightly"
	now := time.Date(2026, 10, 8, 3, 5, 0, 0, time.UTC)
	a.Eng.Now = func() time.Time { return now }
	if rep, err := a.Tick(ctx); err != nil || rep == nil || rep.Failed() != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	// "daily" = one slot per day testing everything once: 3 objects (root disk, instance, docs)
	js := srvjobs.For(n.Svc)
	tests, err := js.ListRestoreTests(ctx, srvjobs.RestoreTestFilter{Repository: "custody"})
	if err != nil || len(tests) != 3 {
		t.Fatalf("%v %+v", err, tests)
	}
	methods := map[string]string{}
	for _, rt := range tests {
		if rt.State != "passed" || rt.Host != "benfica" || rt.JobID != nil {
			t.Fatalf("%+v", rt)
		}
		methods[rt.Object] = rt.Method
	}
	if methods["default/volume/docs"] != "extract" || methods["default/instance/bosch/root"] != "deep" || methods["default/instance/bosch"] != "deep" {
		t.Fatalf("%v", methods)
	}
	// later the same day: nothing new
	now = now.Add(2 * time.Hour)
	a.Tick(ctx)
	if tests, _ = js.ListRestoreTests(ctx, srvjobs.RestoreTestFilter{Repository: "custody"}); len(tests) != 3 {
		t.Fatalf("tested again within the slot: %d", len(tests))
	}
	h, _ := a.History()
	recs, _ := h.Snapshot()
	if r := recs["cikarang|default/volume/docs"]; r.Count != 1 || !r.Passed {
		t.Fatalf("%+v", recs)
	}
	if _, err := os.Stat(a.RestoreTestHistoryPath()); err != nil {
		t.Fatal(err)
	}
}
