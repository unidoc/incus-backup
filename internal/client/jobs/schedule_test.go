package jobs_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/client/jobs"
	"github.com/unidoc/incus-backup/internal/jobspec"
)

// sim drives the planner with a fake clock the way the daemon does (a tick every hour).
type sim struct {
	objs  []jobs.TestObject
	hist  map[string]jobs.TestRecord
	slots map[string]time.Time
	tests map[string][]time.Time
	modes map[string][]string
	load  []int
}

func newSim(n int, schedule string) *sim {
	s := &sim{hist: map[string]jobs.TestRecord{}, slots: map[string]time.Time{}, tests: map[string][]time.Time{}, modes: map[string][]string{}}
	for i := 0; i < n; i++ {
		kind := []string{"block", "filesystem", "instance"}[i%3]
		s.objs = append(s.objs, jobs.TestObject{Target: "cikarang", Project: "default", Name: fmt.Sprintf("volume/v%03d", i), Kind: kind, Policy: "prod", Schedule: schedule})
	}
	return s
}

func (s *sim) tick(now time.Time) int {
	planned, ran := jobs.Plan(s.objs, s.hist, s.slots, now)
	for _, p := range planned {
		r := s.hist[p.Key()]
		r.Last, r.Count, r.Mode = now, r.Count+1, p.Mode
		s.hist[p.Key()] = r
		s.tests[p.Key()] = append(s.tests[p.Key()], now)
		s.modes[p.Key()] = append(s.modes[p.Key()], p.Mode)
	}
	for _, pol := range ran {
		s.slots[pol] = now
	}
	if len(ran) > 0 {
		s.load = append(s.load, len(planned))
	}
	return len(planned)
}

func TestRestoreTestScheduleCoversEveryObjectEveryPeriodAndSpreadsTheLoad(t *testing.T) {
	s := newSim(60, "monthly")
	start := time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)
	end := start.AddDate(0, 4, 0)
	for now := start; now.Before(end); now = now.Add(time.Hour) {
		s.tick(now)
	}
	maxGap := 31*24*time.Hour + 24*time.Hour // one (longest) month plus one daily slot of slack
	for _, o := range s.objs {
		ts := s.tests[o.Key()]
		if len(ts) < 3 {
			t.Fatalf("%s tested only %d times in 4 months", o.Key(), len(ts))
		}
		if ts[0].Sub(start) > maxGap {
			t.Fatalf("%s first tested after %s", o.Key(), ts[0].Sub(start))
		}
		for i := 1; i < len(ts); i++ {
			if gap := ts[i].Sub(ts[i-1]); gap > maxGap {
				t.Fatalf("%s went %s without a restore test", o.Key(), gap)
			}
		}
		if gap := end.Sub(ts[len(ts)-1]); gap > maxGap {
			t.Fatalf("%s untested for the last %s", o.Key(), gap)
		}
	}
	// spread: never everything at once (2 per daily slot for 60 objects in ~30 slots)
	maxLoad, maxGapSeen := 0, time.Duration(0)
	for _, n := range s.load {
		if n > maxLoad {
			maxLoad = n
		}
	}
	for _, ts := range s.tests {
		for i := 1; i < len(ts); i++ {
			if g := ts[i].Sub(ts[i-1]); g > maxGapSeen {
				maxGapSeen = g
			}
		}
	}
	t.Logf("60 objects, monthly, 4 months: %d slots, at most %d tests per slot, longest gap between tests %s", len(s.load), maxLoad, maxGapSeen)
	for i, n := range s.load {
		if n > 4 {
			t.Fatalf("slot %d tested %d objects at once", i, n)
		}
	}
	if len(s.load) < 100 || len(s.load) > 125 {
		t.Fatalf("expected about one slot a day, got %d slots", len(s.load))
	}
	// methods: filesystem extract, block deep with a full every 4th, instance deep
	for _, o := range s.objs {
		for i, m := range s.modes[o.Key()] {
			want := jobspec.ModeDeep
			switch {
			case o.Kind == "filesystem":
				want = jobspec.ModeExtract
			case o.Kind == "block" && i%4 == 3:
				want = jobspec.ModeFull
			}
			if m != want {
				t.Fatalf("%s (%s) test %d used %s, want %s", o.Key(), o.Kind, i, m, want)
			}
		}
	}
}

func TestRestoreTestScheduleCatchesUpAfterDowntimeAndHonoursManual(t *testing.T) {
	s := newSim(30, "weekly") // period 7 days, 7 daily slots, quota 5
	start := time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)
	now := start
	for ; now.Before(start.AddDate(0, 0, 14)); now = now.Add(time.Hour) {
		s.tick(now)
	}
	// once a slot ran, ticks later the same day plan nothing
	if n := s.tick(now.Add(time.Minute)); n != 0 {
		// a slot may be due exactly now; then the very next tick must be empty
		if m := s.tick(now.Add(2 * time.Minute)); m != 0 {
			t.Fatalf("two slots within minutes: %d then %d", n, m)
		}
	}
	// the daemon is down for 10 days: everything is overdue and is tested at the first slot back
	back := now.AddDate(0, 0, 10)
	if n := s.tick(back); n != 30 {
		t.Fatalf("after downtime %d of 30 overdue objects were tested", n)
	}
	// a manual (or invalid) schedule never tests automatically
	m := newSim(5, "manual")
	for now := start; now.Before(start.AddDate(0, 1, 0)); now = now.Add(time.Hour) {
		if n := m.tick(now); n != 0 {
			t.Fatal("a manual schedule planned a test")
		}
	}
	bad := newSim(5, "every other tuesday")
	if n := bad.tick(start); n != 0 {
		t.Fatal("an invalid schedule planned a test")
	}
	if p := jobs.Period("monthly", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); p != 28*24*time.Hour {
		t.Fatalf("period of monthly from mid-January (Feb 1 -> Mar 1): %s", p)
	}
}

func TestRestoreTestHistoryPersists(t *testing.T) {
	dir := t.TempDir()
	h, err := jobs.LoadHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if err := h.Record("cikarang", &jobspec.RestoreTestResult{Project: "default", Object: "volume/docs", Method: "extract", Passed: false, Snapshot: "s",
		Details: map[string]any{"error": "boom"}}, at); err != nil {
		t.Fatal(err)
	}
	if err := h.MarkSlot("prod", at); err != nil {
		t.Fatal(err)
	}
	h2, err := jobs.LoadHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	recs, slots := h2.Snapshot()
	r := recs["cikarang|default/volume/docs"]
	if r.Count != 1 || r.Passed || r.Error != "boom" || !r.Last.Equal(at) || !slots["prod"].Equal(at) {
		t.Fatalf("%+v %v", r, slots)
	}
}
