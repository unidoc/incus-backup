package retention_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/retention"
)

// The web console previews "what this policy keeps" with a TypeScript port of Plan
// (web/src/lib/retention.ts). Its vitest checks the port against these vectors; this test checks
// that the vectors still are what Plan computes, so neither side can drift silently.
//
// Regenerate (only when Plan changes on purpose): IB_WRITE_WEB_VECTORS=1 go test ./internal/retention -run WebConsole
func TestWebConsoleRetentionVectorsMatchPlan(t *testing.T) {
	if os.Getenv("IB_WRITE_WEB_VECTORS") != "" {
		writeWebVectors(t)
	}
	raw, err := os.ReadFile("../../web/src/lib/retention.vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vecs []struct {
		Rules [6]int   `json:"rules"`
		TZ    string   `json:"tz"`
		Snaps [][3]any `json:"snaps"`
		Plan  [][3]any `json:"plan"`
	}
	if err := json.Unmarshal(raw, &vecs); err != nil {
		t.Fatal(err)
	}
	if len(vecs) < 100 {
		t.Fatalf("only %d vectors", len(vecs))
	}
	for i, v := range vecs {
		loc, err := time.LoadLocation(v.TZ)
		if err != nil {
			t.Fatal(err)
		}
		var ss []retention.Snapshot
		for _, s := range v.Snaps {
			ss = append(ss, retention.Snapshot{ID: s[0].(string), Time: time.Unix(int64(s[1].(float64)), 0), Protected: s[2].(float64) == 1})
		}
		r := retention.Rules{Last: v.Rules[0], Hourly: v.Rules[1], Daily: v.Rules[2], Weekly: v.Rules[3], Monthly: v.Rules[4], Yearly: v.Rules[5]}
		plan := retention.Plan(ss, r, loc)
		if len(plan) != len(v.Plan) {
			t.Fatalf("vector %d: %d decisions, file has %d", i, len(plan), len(v.Plan))
		}
		for j, d := range plan {
			w := v.Plan[j]
			if d.ID != w[0].(string) || d.Keep != (w[1].(float64) == 1) || d.Reason != w[2].(string) {
				t.Fatalf("vector %d (%s, rules %v) decision %d: Plan says %s keep=%v %q, file says %v", i, v.TZ, v.Rules, j, d.ID, d.Keep, d.Reason, w)
			}
		}
	}
}

func writeWebVectors(t *testing.T) {
	type vec struct {
		Rules [6]int   `json:"rules"` // last, hourly, daily, weekly, monthly, yearly
		TZ    string   `json:"tz"`
		Snaps [][3]any `json:"snaps"` // [id, unix seconds, protected 0|1], shuffled
		Plan  [][3]any `json:"plan"`  // newest first: [id, keep 0|1, reason]
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	r := rand.New(rand.NewSource(42))
	zones := []string{"UTC", "Atlantic/Reykjavik", "Europe/Berlin", "America/New_York", "Asia/Kolkata", "Australia/Lord_Howe", "Pacific/Chatham"}
	var out []vec
	for i := 0; i < 160; i++ {
		tz := zones[r.Intn(len(zones))]
		loc, _ := time.LoadLocation(tz)
		pick := func(max int) int {
			if r.Intn(3) == 0 {
				return 0
			}
			return r.Intn(max) + 1
		}
		rules := retention.Rules{Last: pick(4), Hourly: pick(30), Daily: pick(10), Weekly: pick(6), Monthly: pick(14), Yearly: pick(3)}
		if i%17 == 0 {
			rules = retention.Rules{}
		}
		// a schedule: interval between 20 min and 3 days, with jitter, across DST changes
		start := time.Date(2024+r.Intn(3), time.Month(1+r.Intn(12)), 1+r.Intn(28), r.Intn(24), r.Intn(60), 0, 0, time.UTC)
		step := time.Duration(20+r.Intn(4300)) * time.Minute
		n := 1 + r.Intn(70)
		var ss []retention.Snapshot
		var vs [][3]any
		tm := start
		for j := 0; j < n; j++ {
			tm = tm.Add(step + time.Duration(r.Intn(600))*time.Second)
			s := retention.Snapshot{ID: fmt.Sprintf("s%d", j), Time: tm, Protected: r.Intn(15) == 0}
			ss = append(ss, s)
			vs = append(vs, [3]any{s.ID, tm.Unix(), b2i(s.Protected)})
		}
		r.Shuffle(len(vs), func(a, b int) { vs[a], vs[b] = vs[b], vs[a] })
		var ds [][3]any
		for _, d := range retention.Plan(ss, rules, loc) {
			ds = append(ds, [3]any{d.ID, b2i(d.Keep), d.Reason})
		}
		out = append(out, vec{[6]int{rules.Last, rules.Hourly, rules.Daily, rules.Weekly, rules.Monthly, rules.Yearly}, tz, vs, ds})
	}
	// Boundary vectors: one or a few backups a day across month and year ends, so ISO weeks
	// straddle them and a weekly rule walks past snapshots a later monthly/yearly rule would
	// otherwise take (the PBS "remove" mark; see pbsref_test.go).
	for i := 0; i < 60; i++ {
		tz := zones[r.Intn(len(zones))]
		loc, _ := time.LoadLocation(tz)
		rules := retention.Rules{Daily: r.Intn(4), Weekly: 1 + r.Intn(4), Monthly: 1 + r.Intn(6), Yearly: r.Intn(3)}
		if r.Intn(2) == 0 {
			rules.Last = r.Intn(4)
		}
		end := time.Date(2024+r.Intn(3), time.Month(1+r.Intn(12)), 1, 0, 0, 0, 0, loc).AddDate(0, 0, r.Intn(10))
		perDay := 1 + r.Intn(3)
		n := 20 + r.Intn(60)
		var ss []retention.Snapshot
		var vs [][3]any
		for j := 0; j < n; j++ {
			tm := end.Add(-time.Duration(j) * 24 * time.Hour / time.Duration(perDay)).Add(time.Duration(r.Intn(3600)) * time.Second)
			s := retention.Snapshot{ID: fmt.Sprintf("b%d", j), Time: tm, Protected: r.Intn(20) == 0}
			ss = append(ss, s)
			vs = append(vs, [3]any{s.ID, tm.Unix(), b2i(s.Protected)})
		}
		r.Shuffle(len(vs), func(a, b int) { vs[a], vs[b] = vs[b], vs[a] })
		var ds [][3]any
		for _, d := range retention.Plan(ss, rules, loc) {
			ds = append(ds, [3]any{d.ID, b2i(d.Keep), d.Reason})
		}
		out = append(out, vec{[6]int{rules.Last, rules.Hourly, rules.Daily, rules.Weekly, rules.Monthly, rules.Yearly}, tz, vs, ds})
	}
	b, _ := json.Marshal(out)
	if err := os.WriteFile("../../web/src/lib/retention.vectors.json", append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
