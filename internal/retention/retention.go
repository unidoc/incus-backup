// Package retention decides which snapshots of one object a policy keeps. It is
// a pure function over snapshot times, so the server can apply it without
// decrypting anything.
//
// Semantics are those of Proxmox Backup Server's prune (pbs-datastore/src/prune.rs,
// compute_prune_info + mark_selections), verified against its source:
//
//   - Rules run in a fixed order: last, hourly, daily, weekly, monthly, yearly.
//   - Each rule walks the snapshots newest first and keeps the newest snapshot of each
//     of the N most recent distinct periods.
//   - Rules are CUMULATIVE: a period that an earlier rule already kept a snapshot for
//     does not use up a slot of a later rule. keep-last 3 + keep-daily 7 on one backup a
//     day therefore keeps 10 snapshots, not 7.
//   - When a rule keeps the newest snapshot of a period, the OLDER snapshots of that period
//     that the rule walks past are decided "remove" for good: later rules skip them, exactly
//     as PBS marks them PruneMark::Remove. (So a weekly rule that walked past 2026-06-30 in
//     ISO week 27 stops a later monthly rule from keeping it for June.)
//   - Periods are calendar periods in the policy's time zone (daily = local calendar
//     day, weekly = ISO week), never UTC unless the policy says so.
//   - Protected snapshots are never removed and consume no slot.
//
// Every decision equals PBS for the same snapshots, rules and time zone; pbsref_test.go checks
// that against a literal port of prune.rs on 24 000 random configurations. What differs is the
// input, not the algorithm (docs/retention.md): periods are cut in the policy's time zone (PBS uses
// the server's local time), snapshot time is the server's commit time, keep-last counts snapshots
// even when two share a time, and a safety net keeps the newest unprotected snapshot. That net
// never changes a decision while a rule is active (the first rule always keeps it); with no rule
// at all everything is kept, as in PBS.
package retention

import (
	"fmt"
	"sort"
	"time"
)

// Rules are the keep counts. A zero field means the rule is not used. If no rule is
// used at all, everything is kept (retention disabled).
type Rules struct {
	Last, Hourly, Daily, Weekly, Monthly, Yearly int
}

// Any reports whether at least one rule is active.
func (r Rules) Any() bool {
	return r.Last > 0 || r.Hourly > 0 || r.Daily > 0 || r.Weekly > 0 || r.Monthly > 0 || r.Yearly > 0
}

// Snapshot is one restore point.
type Snapshot struct {
	ID        string
	Time      time.Time
	Protected bool
}

// Decision is the verdict for one snapshot.
type Decision struct {
	Snapshot
	Keep   bool
	Reason string // "newest", "last", "daily", ..., "protected", "retention disabled", "" for removals
}

// Plan returns a decision for every snapshot, newest first. loc is the policy's
// time zone (nil = UTC).
func Plan(snaps []Snapshot, r Rules, loc *time.Location) []Decision {
	if loc == nil {
		loc = time.UTC
	}
	s := append([]Snapshot(nil), snaps...)
	sort.Slice(s, func(i, j int) bool {
		if !s[i].Time.Equal(s[j].Time) {
			return s[i].Time.After(s[j].Time)
		}
		return s[i].ID > s[j].ID
	})
	out := make([]Decision, len(s))
	for i := range s {
		out[i] = Decision{Snapshot: s[i]}
		switch {
		case s[i].Protected:
			out[i].Keep, out[i].Reason = true, "protected"
		case !r.Any():
			out[i].Keep, out[i].Reason = true, "retention disabled"
		}
	}
	if len(s) == 0 || !r.Any() {
		return out
	}
	mark := func(i int, why string) {
		if s[i].Protected {
			return
		}
		if out[i].Reason == "" {
			out[i].Reason = why
		} else {
			out[i].Reason += "," + why
		}
		out[i].Keep = true
	}
	// kept tracks snapshots kept by a RULE; protected ones never enter a bucket (PBS skips
	// them before they can occupy one).
	kept := make([]bool, len(s))
	// removed: older snapshots of a period a rule already kept (PBS PruneMark::Remove); every later
	// rule skips them.
	removed := make([]bool, len(s))
	selection := func(n int, name string, period func(time.Time) string) {
		key := func(i int) string {
			if period == nil { // keep-last: every snapshot is its own period (PBS keys it by snapshot name)
				return "\x00" + s[i].ID
			}
			return period(s[i].Time.In(loc))
		}
		if n <= 0 {
			return
		}
		already := map[string]bool{}
		for i := range s {
			if kept[i] {
				already[key(i)] = true
			}
		}
		used := map[string]bool{}
		var newlyKept []int
		for i := range s {
			if s[i].Protected || kept[i] || removed[i] {
				continue
			}
			k := key(i)
			if already[k] {
				continue
			}
			if used[k] {
				removed[i] = true
				continue
			}
			if len(used) >= n {
				break
			}
			used[k] = true
			newlyKept = append(newlyKept, i)
		}
		for _, i := range newlyKept {
			mark(i, name)
			kept[i] = true
		}
	}
	selection(r.Last, "last", nil)
	selection(r.Hourly, "hourly", func(t time.Time) string { return t.Format("2006010215") })
	selection(r.Daily, "daily", func(t time.Time) string { return t.Format("20060102") })
	selection(r.Weekly, "weekly", func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-%02d", y, w) })
	selection(r.Monthly, "monthly", func(t time.Time) string { return t.Format("200601") })
	selection(r.Yearly, "yearly", func(t time.Time) string { return t.Format("2006") })
	// Safety net, not a rule: whatever the counts say, the newest unprotected snapshot survives.
	for i := range s {
		if !s[i].Protected {
			if !out[i].Keep {
				mark(i, "newest")
			}
			break
		}
	}
	return out
}
