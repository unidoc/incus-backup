// Package schedule parses the small schedule language used in policies,
// sync jobs and maintenance settings:
//
//	"03:00"            every day at 03:00 (local time of the evaluating host)
//	"every 4h"         every 4 hours, aligned to the hour grid since midnight
//	"every 30m"
//	"hourly"           at minute 0 of every hour
//	"daily"            03:00 every day
//	"weekly"           Sunday 03:00
//	"monthly"          1st of the month 03:00
//	"mon 02:30"        weekly on that weekday and time
//	""  or "manual"    never automatically
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed schedule.
type Spec struct {
	manual   bool
	every    time.Duration
	hour     int
	min      int
	weekday  int // -1: any
	monthday int // 0: any
}

// Parse parses a schedule expression.
func Parse(s string) (Spec, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "manual", "never":
		return Spec{manual: true}, nil
	case "hourly":
		return Spec{every: time.Hour}, nil
	case "daily":
		return Spec{hour: 3, weekday: -1}, nil
	case "weekly":
		return Spec{hour: 3, weekday: int(time.Sunday)}, nil
	case "monthly":
		return Spec{hour: 3, weekday: -1, monthday: 1}, nil
	}
	if strings.HasPrefix(s, "every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(s, "every ")))
		if err != nil || d < time.Minute || d > 24*time.Hour {
			return Spec{}, fmt.Errorf("invalid interval %q (use 1m..24h, e.g. \"every 4h\")", s)
		}
		return Spec{every: d}, nil
	}
	sp := Spec{weekday: -1}
	fields := strings.Fields(s)
	if len(fields) == 2 {
		days := map[string]time.Weekday{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
		d, ok := days[fields[0]]
		if !ok {
			return Spec{}, fmt.Errorf("invalid weekday %q", fields[0])
		}
		sp.weekday = int(d)
		fields = fields[1:]
	}
	if len(fields) != 1 {
		return Spec{}, fmt.Errorf("invalid schedule %q", s)
	}
	hm := strings.Split(fields[0], ":")
	if len(hm) != 2 {
		return Spec{}, fmt.Errorf("invalid schedule %q (want HH:MM)", s)
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return Spec{}, fmt.Errorf("invalid time in %q", s)
	}
	sp.hour, sp.min = h, m
	return sp, nil
}

// Manual reports whether the schedule never fires by itself.
func (s Spec) Manual() bool { return s.manual }

// Next returns the first firing strictly after t, in t's location.
func (s Spec) Next(t time.Time) time.Time {
	if s.manual {
		return time.Time{}
	}
	if s.every > 0 {
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		n := t.Sub(day)/s.every + 1
		next := day.Add(n * s.every)
		if next.Day() != day.Day() && s.every < 24*time.Hour {
			// crossing midnight: the grid restarts at 00:00
			return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		}
		return next
	}
	c := time.Date(t.Year(), t.Month(), t.Day(), s.hour, s.min, 0, 0, t.Location())
	for i := 0; i < 400; i++ {
		if c.After(t) && (s.weekday < 0 || int(c.Weekday()) == s.weekday) && (s.monthday == 0 || c.Day() == s.monthday) {
			return c
		}
		c = time.Date(c.Year(), c.Month(), c.Day()+1, s.hour, s.min, 0, 0, t.Location())
	}
	return time.Time{}
}

// Due reports whether a job last run at last (zero = never) is due at now.
// A schedule that fired while the process was down is due once, not many times.
func (s Spec) Due(last, now time.Time) bool {
	if s.manual {
		return false
	}
	if last.IsZero() {
		return true
	}
	n := s.Next(last)
	return !n.IsZero() && !n.After(now)
}
