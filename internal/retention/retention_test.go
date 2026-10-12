package retention

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func daily(n int, start time.Time) []Snapshot {
	var s []Snapshot
	for i := 0; i < n; i++ {
		t := start.AddDate(0, 0, -i)
		s = append(s, Snapshot{ID: fmt.Sprintf("s%03d", i), Time: t})
	}
	return s
}

func kept(d []Decision) map[string]bool {
	m := map[string]bool{}
	for _, x := range d {
		if x.Keep {
			m[x.ID] = true
		}
	}
	return m
}

func TestDisabledKeepsEverything(t *testing.T) {
	d := Plan(daily(10, time.Now()), Rules{}, nil)
	if len(kept(d)) != 10 {
		t.Fatal("rules empty must keep all")
	}
}

func TestKeepLastAndDaily(t *testing.T) {
	now := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	d := Plan(daily(30, now), Rules{Daily: 7}, nil)
	if got := len(kept(d)); got != 7 {
		t.Fatalf("daily=7 kept %d", got)
	}
	if !kept(d)["s000"] || !kept(d)["s006"] || kept(d)["s007"] {
		t.Fatal("wrong days kept")
	}
	d = Plan(daily(30, now), Rules{Last: 3}, nil)
	if len(kept(d)) != 3 {
		t.Fatal("last=3")
	}
}

func TestMultipleSnapshotsPerDayKeepNewest(t *testing.T) {
	base := time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC)
	s := []Snapshot{
		{ID: "a", Time: base}, {ID: "b", Time: base.Add(-2 * time.Hour)}, {ID: "c", Time: base.Add(-5 * time.Hour)}, {ID: "d", Time: base.AddDate(0, 0, -1)},
	}
	k := kept(Plan(s, Rules{Daily: 2}, nil))
	if !k["a"] || !k["d"] || k["b"] || k["c"] {
		t.Fatalf("kept %v", k)
	}
}

func TestWeeklyMonthlyYearlyUnion(t *testing.T) {
	now := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	d := Plan(daily(800, now), Rules{Daily: 14, Weekly: 8, Monthly: 12, Yearly: 3}, nil)
	n := len(kept(d))
	if n < 14 || n > 14+8+12+3 {
		t.Fatalf("kept %d", n)
	}
}

func TestNewestAlwaysKept(t *testing.T) {
	now := time.Now()
	// Rules that would otherwise select nothing useful still keep the newest.
	d := Plan(daily(5, now), Rules{Yearly: 1}, nil)
	if !d[0].Keep {
		t.Fatal("newest dropped")
	}
	if len(Plan(nil, Rules{Daily: 1}, nil)) != 0 {
		t.Fatal("empty input")
	}
}

// Property test over random timelines: newest kept, never more than the sum of
// the limits, idempotent (re-applying to the survivors removes nothing).
func TestProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		n := 1 + rng.Intn(120)
		now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
		var s []Snapshot
		for i := 0; i < n; i++ {
			s = append(s, Snapshot{ID: fmt.Sprintf("x%d-%d", iter, i), Time: now.Add(-time.Duration(rng.Intn(24*400)) * time.Hour)})
		}
		r := Rules{Last: rng.Intn(4), Hourly: rng.Intn(4), Daily: rng.Intn(10), Weekly: rng.Intn(6), Monthly: rng.Intn(8), Yearly: rng.Intn(3)}
		d := Plan(s, r, nil)
		if len(d) != n {
			t.Fatal("decision count")
		}
		k := 0
		for _, x := range d {
			if x.Keep {
				k++
			}
		}
		if r.Any() {
			max := 1 + r.Last + r.Hourly + r.Daily + r.Weekly + r.Monthly + r.Yearly
			if k > max {
				t.Fatalf("kept %d > bound %d for %+v", k, max, r)
			}
			if !d[0].Keep {
				t.Fatal("newest not kept")
			}
		}
		var survivors []Snapshot
		for _, x := range d {
			if x.Keep {
				survivors = append(survivors, x.Snapshot)
			}
		}
		d2 := Plan(survivors, r, nil)
		for _, x := range d2 {
			if !x.Keep && r.Any() {
				// A snapshot that was kept for a period may lose its period slot
				// only if an older kept one outranks it; the rules are stable on
				// survivors for the same set of period winners.
				t.Fatalf("not idempotent: %v dropped on second pass (rules %+v)", x.ID, r)
			}
		}
	}
}

// PBS semantics: periods already covered by an earlier rule do not use up a later rule's slots.
func TestRulesAreCumulativeLikePBS(t *testing.T) {
	now := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	d := Plan(daily(30, now), Rules{Last: 3, Daily: 7}, nil)
	if got := len(kept(d)); got != 10 {
		t.Fatalf("keep-last 3 + keep-daily 7 with one backup a day keeps 10 in PBS, got %d", got)
	}
	d = Plan(daily(60, now), Rules{Daily: 3, Weekly: 2}, nil)
	k := kept(d)
	// 3 dailies (days 0-2) + the next 2 distinct ISO weeks not already covered
	if len(k) != 5 {
		t.Fatalf("daily 3 + weekly 2 kept %d: %v", len(k), k)
	}
}

func TestProtectedSnapshotsAreNeverRemovedAndUseNoSlot(t *testing.T) {
	now := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	s := daily(10, now)
	s[2].Protected, s[5].Protected = true, true
	d := Plan(s, Rules{Daily: 3}, nil)
	k := kept(d)
	for _, id := range []string{"s000", "s001", "s003", "s002", "s005"} {
		if !k[id] {
			t.Fatalf("%s should be kept: %v", id, k)
		}
	}
	// 3 dailies among the unprotected (s000 s001 s003) + 2 protected
	if len(k) != 5 {
		t.Fatalf("kept %d: %v", len(k), k)
	}
	for _, x := range d {
		if x.ID == "s002" && x.Reason != "protected" {
			t.Fatalf("reason %q", x.Reason)
		}
	}
}

// Days are calendar days in the policy's zone: 23:30 UTC on Oct 6 is Oct 7 in Reykjavik+2, so the
// two evening backups below are different days in UTC but the same day in Berlin.
func TestPeriodsFollowThePolicyTimeZone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	a := Snapshot{ID: "a", Time: time.Date(2026, 10, 6, 23, 30, 0, 0, time.UTC)} // 01:30 Oct 7 Berlin
	b := Snapshot{ID: "b", Time: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)}  // 12:00 Oct 7 Berlin
	c := Snapshot{ID: "c", Time: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	utc := kept(Plan([]Snapshot{a, b, c}, Rules{Daily: 2}, time.UTC))
	ber := kept(Plan([]Snapshot{a, b, c}, Rules{Daily: 2}, berlin))
	if !utc["b"] || !utc["a"] || utc["c"] {
		t.Fatalf("UTC: %v", utc)
	}
	if !ber["b"] || ber["a"] || !ber["c"] {
		t.Fatalf("Berlin (a and b are the same local day): %v", ber)
	}
}
