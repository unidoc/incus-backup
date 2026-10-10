package schedule

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	cases := []struct{ spec, from, want string }{
		{"03:00", "2026-10-07 02:00", "2026-10-07 03:00"},
		{"03:00", "2026-10-07 03:00", "2026-10-08 03:00"},
		{"03:00", "2026-10-07 23:59", "2026-10-08 03:00"},
		{"every 4h", "2026-10-07 02:10", "2026-10-07 04:00"},
		{"every 4h", "2026-10-07 21:00", "2026-10-08 00:00"},
		{"every 30m", "2026-10-07 10:10", "2026-10-07 10:30"},
		{"hourly", "2026-10-07 10:10", "2026-10-07 11:00"},
		{"weekly", "2026-10-07 10:00", "2026-10-11 03:00"}, // Wed -> Sun
		{"mon 02:30", "2026-10-07 10:00", "2026-10-12 02:30"},
		{"monthly", "2026-10-07 10:00", "2026-11-01 03:00"},
		{"daily", "2026-12-31 23:00", "2027-01-01 03:00"},
	}
	for _, c := range cases {
		sp, err := Parse(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := sp.Next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%q after %s: got %s want %s", c.spec, c.from, got, c.want)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, s := range []string{"25:00", "3", "every", "every 10s", "every 48h", "xyz 03:00", "03:00 04:00 05:00", "03:61", "-1:00"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	for _, s := range []string{"", "manual", "never"} {
		sp, err := Parse(s)
		if err != nil || !sp.Manual() || !sp.Next(time.Now()).IsZero() || sp.Due(time.Time{}, time.Now()) {
			t.Errorf("%q should be manual: %v", s, err)
		}
	}
}

func TestDue(t *testing.T) {
	sp, _ := Parse("03:00")
	if !sp.Due(time.Time{}, at("2026-10-07 01:00")) {
		t.Fatal("never run must be due")
	}
	if sp.Due(at("2026-10-07 03:00"), at("2026-10-07 12:00")) {
		t.Fatal("already ran today")
	}
	if !sp.Due(at("2026-10-07 03:00"), at("2026-10-08 03:00")) {
		t.Fatal("next day must be due")
	}
	// missed several days while down: due once, then not again until the next slot
	if !sp.Due(at("2026-10-01 03:00"), at("2026-10-07 12:00")) {
		t.Fatal("overdue must be due")
	}
	if sp.Due(at("2026-10-07 12:00"), at("2026-10-07 12:01")) {
		t.Fatal("must not fire again immediately after catching up")
	}
}
