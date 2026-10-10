package service_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
)

// EffectivePolicies must name, for every object, exactly the policy Prune applies to it, and say why.
func TestEffectivePoliciesAgreeWithPrune(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	for _, p := range []api.Policy{{Name: "nightly", KeepDaily: 7}, {Name: "short", KeepLast: 1}, {Name: "hourly", KeepHourly: 24}, {Name: "weekly", KeepWeekly: 4}} {
		if err := e.svc.SetPolicy(ctx, "custody", p); err != nil {
			t.Fatal(err)
		}
	}
	objs := []api.ObjectInfo{
		{Project: "prod", Name: "instance/web", Kind: "instance", GroupID: "00000000000000000000000000000001", Policy: "nightly"},
		{Project: "prod", Name: "instance/web/root", Kind: "block", GroupID: "00000000000000000000000000000002", Policy: "nightly"},
		{Project: "prod", Name: "instance/db/root", Kind: "block", GroupID: "00000000000000000000000000000003", Policy: "nightly"},
		{Project: "dev", Name: "volume/scratch", Kind: "filesystem", GroupID: "00000000000000000000000000000004", Policy: "nightly"},
		{Project: "other", Name: "instance/x/root", Kind: "block", GroupID: "00000000000000000000000000000005", Policy: "ghost"},
	}
	if err := e.svc.UpsertInventory(ctx, h, api.Inventory{Objects: objs}); err != nil {
		t.Fatal(err)
	}
	start := e.now
	for i, o := range objs {
		for d := 0; d < 3; d++ {
			ts := start.Add(time.Duration(d*24+i) * time.Hour)
			if err := e.upload(h, k, o.GroupID, snapID(ts, i*10+d), ts, o.Policy, fmt.Sprintf("%s-%d", o.Name, d)); err != nil {
				t.Fatal(err)
			}
		}
	}
	check := func(stage string, want map[string][2]string) {
		t.Helper()
		es, err := e.svc.EffectivePolicies(ctx, "custody")
		if err != nil {
			t.Fatal(err)
		}
		plan, err := e.svc.PrunePreview(ctx, "custody")
		if err != nil {
			t.Fatal(err)
		}
		applied := map[string]string{}
		for _, p := range plan.Plan {
			applied[p.Group] = p.Policy
		}
		if len(es) != len(objs) {
			t.Fatalf("%s: %d objects, want %d", stage, len(es), len(objs))
		}
		for _, x := range es {
			if applied[x.GroupID] != x.Policy {
				t.Errorf("%s: %s/%s effective %q but prune applies %q", stage, x.Project, x.Name, x.Policy, applied[x.GroupID])
			}
			if w, ok := want[x.GroupID[30:]]; ok && (x.Policy != w[0] || x.Source != w[1]) {
				t.Errorf("%s: %s/%s = %s via %s, want %s via %s", stage, x.Project, x.Name, x.Policy, x.Source, w[0], w[1])
			}
			if x.Snapshots != 3 {
				t.Errorf("%s: %s has %d snapshots", stage, x.Name, x.Snapshots)
			}
		}
	}
	// nothing assigned: the pinned (first declared) policy, an unknown one keeps everything
	check("pinned", map[string][2]string{"01": {"nightly", "pinned"}, "05": {"ghost", "pinned"}})
	es, _ := e.svc.EffectivePolicies(ctx, "custody")
	for _, x := range es {
		if x.Name == "instance/x/root" && x.Known {
			t.Error("an unknown policy is reported as known")
		}
	}
	// host default < project < object; "instance/web" and "instance/web/root" both match object "web"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.svc.Assign(ctx, "custody", "benfica", "", "", "weekly"))
	must(e.svc.Assign(ctx, "custody", "benfica", "prod", "", "short"))
	must(e.svc.Assign(ctx, "custody", "benfica", "prod", "web", "hourly"))
	check("assigned", map[string][2]string{
		"01": {"hourly", "object"}, "02": {"hourly", "object"}, "03": {"short", "project"}, "04": {"weekly", "host"}, "05": {"weekly", "host"},
	})
	must(e.svc.Unassign(ctx, "custody", "benfica", "prod", "web"))
	check("unassigned", map[string][2]string{"01": {"short", "project"}, "02": {"short", "project"}})
}
