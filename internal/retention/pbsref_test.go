package retention

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

// pbsReference is a line-by-line port of Proxmox Backup Server's prune decision
// (pbs-datastore/src/prune.rs: compute_prune_info + mark_selections, finished snapshots only).
// It is the oracle Plan is checked against; keep it literal, do not "improve" it.
//
// The one input difference: PBS snapshot times within a group are unique (they name the
// directory); we sort equal times by ID descending, so the port does the same.
//
// It returns, per snapshot ID, the PBS mark: "keep:<rule>", "protected", "remove" or "" (unmarked,
// which PBS also removes).
func pbsReference(snaps []Snapshot, r Rules, loc *time.Location) map[string]string {
	list := append([]Snapshot(nil), snaps...)
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Time.Equal(list[j].Time) {
			return list[i].Time.After(list[j].Time)
		}
		return list[i].ID > list[j].ID
	})
	mark := map[string]string{}
	markSelections := func(keep int, rule string, selectID func(Snapshot) string) {
		includeHash := map[string]bool{}
		alreadyIncluded := map[string]bool{}
		for _, info := range list {
			if strings.HasPrefix(mark[info.ID], "keep") {
				alreadyIncluded[selectID(info)] = true
			}
		}
		for _, info := range list {
			if _, ok := mark[info.ID]; ok {
				continue
			}
			if info.Protected {
				mark[info.ID] = "protected"
				continue
			}
			selID := selectID(info)
			if alreadyIncluded[selID] {
				continue
			}
			if !includeHash[selID] {
				if len(includeHash) >= keep {
					break
				}
				includeHash[selID] = true
				mark[info.ID] = "keep:" + rule
			} else {
				mark[info.ID] = "remove"
			}
		}
	}
	local := func(t time.Time) time.Time { return t.In(loc) }
	if r.Last > 0 {
		markSelections(r.Last, "last", func(x Snapshot) string { return x.ID })
	}
	if r.Hourly > 0 {
		markSelections(r.Hourly, "hourly", func(x Snapshot) string { return local(x.Time).Format("2006/01/02/15") })
	}
	if r.Daily > 0 {
		markSelections(r.Daily, "daily", func(x Snapshot) string { return local(x.Time).Format("2006/01/02") })
	}
	if r.Weekly > 0 {
		markSelections(r.Weekly, "weekly", func(x Snapshot) string {
			y, w := local(x.Time).ISOWeek()
			return fmt.Sprintf("%d/%d", y, w)
		})
	}
	if r.Monthly > 0 {
		markSelections(r.Monthly, "monthly", func(x Snapshot) string { return local(x.Time).Format("2006/01") })
	}
	if r.Yearly > 0 {
		markSelections(r.Yearly, "yearly", func(x Snapshot) string { return local(x.Time).Format("2006") })
	}
	// PBS: with no keep option at all, everything is kept; protected snapshots are always kept.
	for _, info := range list {
		if info.Protected {
			mark[info.ID] = "protected"
		}
	}
	return mark
}

// expectFromPBS turns a PBS mark into what Plan must say. The only deliberate differences
// (docs/retention.md): no rule keeps everything ("retention disabled"), and the newest unprotected
// snapshot is never removed. The latter cannot change any decision while a rule is active,
// because the first active rule always keeps the newest unprotected snapshot; checkPlan
// proves that on every random configuration by requiring Reason never to be "newest".
func checkPlan(t *testing.T, snaps []Snapshot, r Rules, loc *time.Location, label string) (decisions int) {
	t.Helper()
	ref := pbsReference(snaps, r, loc)
	got := Plan(snaps, r, loc)
	if len(got) != len(snaps) {
		t.Fatalf("%s: %d decisions for %d snapshots", label, len(got), len(snaps))
	}
	for _, d := range got {
		m := ref[d.ID]
		var wantKeep bool
		var wantReason string
		switch {
		case m == "protected":
			wantKeep, wantReason = true, "protected"
		case !r.Any():
			wantKeep, wantReason = true, "retention disabled"
		case strings.HasPrefix(m, "keep:"):
			wantKeep, wantReason = true, strings.TrimPrefix(m, "keep:")
		default:
			wantKeep, wantReason = false, ""
		}
		if d.Keep != wantKeep || d.Reason != wantReason {
			var b strings.Builder
			for _, x := range got {
				fmt.Fprintf(&b, "\n  %s %s ours=%v %-9q pbs=%q", x.ID, x.Time.In(loc).Format("2006-01-02 Mon 15:04 MST"), x.Keep, x.Reason, ref[x.ID])
			}
			t.Fatalf("%s rules=%+v tz=%s: %s ours keep=%v reason=%q, PBS mark %q (want keep=%v reason=%q)%s",
				label, r, loc, d.ID, d.Keep, d.Reason, m, wantKeep, wantReason, b.String())
		}
	}
	return len(got)
}

func zones(t *testing.T) []*time.Location {
	t.Helper()
	var out []*time.Location
	for _, n := range []string{"UTC", "Atlantic/Reykjavik", "America/New_York", "Europe/Berlin", "Asia/Kolkata", "Australia/Lord_Howe", "Pacific/Chatham"} {
		z, err := time.LoadLocation(n)
		if err != nil {
			t.Fatalf("time zone %s: %v (the test needs the zone database)", n, err)
		}
		out = append(out, z)
	}
	return out
}

// TestPlanEqualsPBSRandom: 24 000 random configurations, several snapshot layouts, 7 time zones
// (including half-hour and 45-minute DST zones), protected snapshots. Every single decision and
// reason must equal the PBS port.
func TestPlanEqualsPBSRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	locs := zones(t)
	total := 0
	for it := 0; it < 24000; it++ {
		loc := locs[rng.Intn(len(locs))]
		var snaps []Snapshot
		n := 1 + rng.Intn(80)
		switch it % 4 {
		case 0: // uniformly random over 400 days
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < n; i++ {
				snaps = append(snaps, Snapshot{ID: fmt.Sprintf("s%03d", i), Time: base.Add(time.Duration(rng.Int63n(int64(400 * 24 * time.Hour))))})
			}
		case 1: // a regular schedule with jitter (20 min .. 3 days)
			tm := time.Date(2024+rng.Intn(3), time.Month(1+rng.Intn(12)), 1+rng.Intn(28), rng.Intn(24), rng.Intn(60), 0, 0, time.UTC)
			step := time.Duration(20+rng.Intn(4300)) * time.Minute
			for i := 0; i < n; i++ {
				tm = tm.Add(step + time.Duration(rng.Intn(600))*time.Second)
				snaps = append(snaps, Snapshot{ID: fmt.Sprintf("s%03d", i), Time: tm})
			}
		case 2: // clustered around month and year ends, so ISO weeks straddle the boundary
			for i := 0; i < n; i++ {
				y, m := 2024+rng.Intn(3), time.Month(1+rng.Intn(12))
				end := time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
				off := time.Duration(rng.Int63n(int64(8*24*time.Hour))) - 4*24*time.Hour
				snaps = append(snaps, Snapshot{ID: fmt.Sprintf("s%03d", i), Time: end.Add(off)})
			}
		case 3: // dense: many per hour/day, some exactly equal times (tie-break)
			base := time.Date(2026, time.Month(1+rng.Intn(12)), 1+rng.Intn(28), 0, 0, 0, 0, loc)
			for i := 0; i < n; i++ {
				tm := base.Add(time.Duration(rng.Intn(20*24*6)) * 10 * time.Minute)
				if i > 0 && rng.Intn(10) == 0 {
					tm = snaps[rng.Intn(i)].Time
				}
				snaps = append(snaps, Snapshot{ID: fmt.Sprintf("s%03d", i), Time: tm})
			}
		}
		for i := range snaps {
			snaps[i].Protected = rng.Intn(15) == 0
		}
		pick := func(max int) int {
			if rng.Intn(2) == 0 {
				return 0
			}
			return rng.Intn(max)
		}
		r := Rules{Last: pick(8), Hourly: pick(30), Daily: pick(10), Weekly: pick(8), Monthly: pick(14), Yearly: pick(4)}
		total += checkPlan(t, snaps, r, loc, fmt.Sprintf("config %d", it))
	}
	t.Logf("%d decisions equal to the PBS port", total)
}

// Hand-built cases where the old "skip only within the rule" code differed from PBS.
func TestPlanEqualsPBSBoundaries(t *testing.T) {
	utc := time.UTC
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, utc) }
	daily := func(from, to time.Time) []Snapshot {
		var s []Snapshot
		for tm := from; !tm.After(to); tm = tm.AddDate(0, 0, 1) {
			s = append(s, Snapshot{ID: tm.Format("2006-01-02"), Time: tm})
		}
		return s
	}
	// ISO week 2026-W27 runs Mon 29 June .. Sun 5 July. With one backup a day up to 9 July,
	// keep-weekly 2 keeps 9 July (W28) and 5 July (W27) and walks past 30 June (also W27): PBS
	// marks it removed, so keep-monthly must take 31 May for May.. never 30 June for June.
	s := daily(day(2026, 4, 1), day(2026, 7, 9))
	checkPlan(t, s, Rules{Weekly: 2, Monthly: 3}, utc, "W27 straddles June/July")
	k := kept(Plan(s, Rules{Weekly: 2, Monthly: 3}, utc))
	if k["2026-06-30"] {
		t.Fatal("2026-06-30 was walked past by keep-weekly (same ISO week as 5 July) and must not be kept as monthly")
	}
	// the audit's example: {Last:3 Daily:3 Weekly:3 Monthly:6}
	checkPlan(t, daily(day(2026, 1, 1), day(2026, 7, 20)), Rules{Last: 3, Daily: 3, Weekly: 3, Monthly: 6}, utc, "audit example")
	// year end: 2026-W53 runs 28 Dec 2026 .. 3 Jan 2027
	checkPlan(t, daily(day(2026, 11, 20), day(2027, 1, 2)), Rules{Weekly: 1, Monthly: 2, Yearly: 2}, utc, "W53 straddles the year")
	checkPlan(t, daily(day(2025, 12, 1), day(2026, 1, 1)), Rules{Daily: 1, Weekly: 1, Yearly: 3}, utc, "2026-W01 starts in 2025")
	// DST: Lord Howe moves by 30 minutes, Chatham by an hour at 2:45
	for _, z := range []string{"Australia/Lord_Howe", "Pacific/Chatham", "America/New_York"} {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Fatal(err)
		}
		var hs []Snapshot
		for tm := time.Date(2026, 3, 25, 0, 0, 0, 0, utc); tm.Before(time.Date(2026, 4, 12, 0, 0, 0, 0, utc)); tm = tm.Add(25 * time.Minute) {
			hs = append(hs, Snapshot{ID: tm.Format(time.RFC3339), Time: tm})
		}
		for _, r := range []Rules{{Hourly: 48, Daily: 7, Weekly: 2, Monthly: 2}, {Last: 5, Hourly: 400}, {Daily: 30, Monthly: 1}} {
			checkPlan(t, hs, r, loc, "DST "+z)
		}
	}
}
