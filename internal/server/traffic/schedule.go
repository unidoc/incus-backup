package traffic

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is when a rule is active: a list of weekly windows in a time zone. The text form is
//
//	""                         always
//	"mon-fri 08:00-18:00"      weekdays, 8 to 18 local time
//	"sat,sun"                  whole days
//	"22:00-06:00"              every night (a window may cross midnight: it then runs into the next day)
//	"mon-fri 08:00-12:00; mon-fri 13:00-18:00"   several windows, separated by ";"
//
// Days are mon tue wed thu fri sat sun, ranges ("mon-fri", "fri-mon" wraps), lists ("sat,sun") or
// "*"/"daily". Times are local wall-clock times in the rule's IANA zone; "24:00" ends at midnight. On a
// DST change the wall clock decides (a window 02:00-03:00 is shorter or empty on the day the clock jumps).
type Schedule struct {
	windows []window
}

type window struct {
	days     [7]bool // indexed by time.Weekday
	from, to int     // minutes since local midnight; to may be 1440
}

var dayNames = map[string]time.Weekday{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// ParseSchedule parses the text form.
func ParseSchedule(s string) (Schedule, error) {
	var out Schedule
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "always" {
		return out, nil
	}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		w := window{from: 0, to: 1440}
		fields := strings.Fields(part)
		if len(fields) > 2 {
			return out, fmt.Errorf("window %q: want \"DAYS HH:MM-HH:MM\"", part)
		}
		dayText, timeText := "*", ""
		for _, f := range fields {
			if strings.Contains(f, ":") {
				timeText = f
			} else {
				dayText = f
			}
		}
		if err := parseDays(dayText, &w.days); err != nil {
			return out, fmt.Errorf("window %q: %v", part, err)
		}
		if timeText != "" {
			a, b, ok := strings.Cut(timeText, "-")
			if !ok {
				return out, fmt.Errorf("window %q: time range must be HH:MM-HH:MM", part)
			}
			var err error
			if w.from, err = parseClock(a, false); err != nil {
				return out, fmt.Errorf("window %q: %v", part, err)
			}
			if w.to, err = parseClock(b, true); err != nil {
				return out, fmt.Errorf("window %q: %v", part, err)
			}
			if w.from == w.to {
				return out, fmt.Errorf("window %q: empty time range", part)
			}
		}
		out.windows = append(out.windows, w)
	}
	return out, nil
}

func parseDays(s string, days *[7]bool) error {
	if s == "*" || s == "daily" || s == "all" {
		for i := range days {
			days[i] = true
		}
		return nil
	}
	for _, item := range strings.Split(s, ",") {
		a, b, isRange := strings.Cut(item, "-")
		da, ok := dayNames[a]
		if !ok {
			return fmt.Errorf("unknown day %q (use mon..sun)", a)
		}
		if !isRange {
			days[da] = true
			continue
		}
		db, ok := dayNames[b]
		if !ok {
			return fmt.Errorf("unknown day %q (use mon..sun)", b)
		}
		for d := da; ; d = (d + 1) % 7 {
			days[d] = true
			if d == db {
				break
			}
		}
	}
	return nil
}

func parseClock(s string, end bool) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || mm < 0 || mm > 59 || hh < 0 || hh > 24 || (hh == 24 && (mm != 0 || !end)) {
		return 0, fmt.Errorf("time %q must be HH:MM between 00:00 and 24:00", s)
	}
	return hh*60 + mm, nil
}

// Always reports whether the schedule has no windows (the rule is always active).
func (s Schedule) Always() bool { return len(s.windows) == 0 }

// Active reports whether t (any zone) falls in a window, judged by the wall clock in loc.
func (s Schedule) Active(t time.Time, loc *time.Location) bool {
	if len(s.windows) == 0 {
		return true
	}
	lt := t.In(loc)
	tod := lt.Hour()*60 + lt.Minute()
	day := lt.Weekday()
	prev := (day + 6) % 7
	for _, w := range s.windows {
		if w.from < w.to {
			if w.days[day] && tod >= w.from && tod < w.to {
				return true
			}
			continue
		}
		// crosses midnight: from the start day's evening into the next morning
		if (w.days[day] && tod >= w.from) || (w.days[prev] && tod < w.to) {
			return true
		}
	}
	return false
}
