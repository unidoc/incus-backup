package traffic_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/traffic"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func mustLoc(t *testing.T, n string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(n)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestScheduleWindows(t *testing.T) {
	ber := mustLoc(t, "Europe/Berlin")
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, ber)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		spec string
		when string // 2026-10-05 is a Monday
		want bool
	}{
		{"", "2026-10-11 03:00", true},
		{"mon-fri 08:00-18:00", "2026-10-05 08:00", true},
		{"mon-fri 08:00-18:00", "2026-10-05 17:59", true},
		{"mon-fri 08:00-18:00", "2026-10-05 18:00", false},
		{"mon-fri 08:00-18:00", "2026-10-10 10:00", false}, // Saturday
		{"sat,sun", "2026-10-11 23:59", true},
		{"sat,sun", "2026-10-12 00:00", false},
		{"fri-mon", "2026-10-07 12:00", false}, // Wednesday
		{"fri-mon", "2026-10-12 12:00", true},  // Monday (range wraps)
		{"22:00-06:00", "2026-10-05 23:30", true},
		{"22:00-06:00", "2026-10-06 05:59", true},
		{"22:00-06:00", "2026-10-06 06:00", false},
		{"fri 22:00-06:00", "2026-10-10 03:00", true},  // Saturday morning belongs to Friday's window
		{"fri 22:00-06:00", "2026-10-09 03:00", false}, // Friday morning does not
		{"mon-fri 08:00-12:00; mon-fri 13:00-18:00", "2026-10-05 12:30", false},
		{"mon-fri 08:00-12:00; mon-fri 13:00-18:00", "2026-10-05 13:30", true},
		{"daily 00:00-24:00", "2026-10-05 23:59", true},
		// DST: on 29 March 2026 Berlin jumps from 02:00 to 03:00; the wall clock decides
		{"sun 01:00-04:00", "2026-03-29 03:30", true},
	}
	for _, c := range cases {
		s, err := traffic.ParseSchedule(c.spec)
		if err != nil {
			t.Fatalf("%q: %v", c.spec, err)
		}
		if got := s.Active(at(c.when), ber); got != c.want {
			t.Errorf("%q at %s: %v, want %v", c.spec, c.when, got, c.want)
		}
	}
	// the zone matters: 07:30 UTC on a Monday is 09:30 in Berlin (summer)
	s, _ := traffic.ParseSchedule("mon 09:00-10:00")
	if !s.Active(time.Date(2026, 10, 5, 7, 30, 0, 0, time.UTC), ber) || s.Active(time.Date(2026, 10, 5, 7, 30, 0, 0, time.UTC), time.UTC) {
		t.Fatal("zone not applied")
	}
	for _, bad := range []string{"monday", "mon 8-18", "mon 08:00-08:00", "mon 25:00-26:00", "mon 08:00", "mon tue 08:00-09:00", "xyz-fri"} {
		if _, err := traffic.ParseSchedule(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestRuleMatchesAndValidates(t *testing.T) {
	r := traffic.Rule{Name: "x", Scope: "host", Direction: "upload", RateBps: 64 << 10, Hosts: "web-*, custody/db-1"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if !r.Matches("any", "web-3") || r.Matches("other", "db-1") || !r.Matches("custody", "db-1") || r.Matches("custody", "mail") {
		t.Fatal("glob matching")
	}
	for _, b := range []traffic.Rule{
		{Name: "", Scope: "host", Direction: "upload", RateBps: 64 << 10},
		{Name: "a", Scope: "everyone", Direction: "upload", RateBps: 64 << 10},
		{Name: "a", Scope: "host", Direction: "sideways", RateBps: 64 << 10},
		{Name: "a", Scope: "host", Direction: "upload", RateBps: 10},
		{Name: "a", Scope: "host", Direction: "upload", RateBps: 64 << 10, Timezone: "Mars/Olympus"},
		{Name: "a", Scope: "host", Direction: "upload", RateBps: 64 << 10, Schedule: "someday"},
		{Name: "a", Scope: "host", Direction: "upload", RateBps: 64 << 10, Hosts: "[x"},
	} {
		if err := b.Validate(); !errors.Is(err, traffic.ErrBad) {
			t.Errorf("%+v: %v", b, err)
		}
	}
}

type env struct {
	t     *testing.T
	store traffic.Store
	lim   *traffic.Limiter
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, store: traffic.Store{Pool: d.Pool()}, lim: traffic.New(d.Pool(), nil)}
}

func (e *env) rule(r traffic.Rule) {
	e.t.Helper()
	r.Enabled = true
	if _, err := e.store.Create(context.Background(), r); err != nil {
		e.t.Fatal(err)
	}
	e.lim.Invalidate()
}

const MiB = 1 << 20

// upload pushes n bytes through the limiter as host and returns how long it took.
func (e *env) upload(ctx context.Context, host string, n int) (time.Duration, error) {
	start := time.Now()
	_, err := io.Copy(io.Discard, e.lim.Reader(ctx, "", host, bytes.NewReader(make([]byte, n))))
	return time.Since(start), err
}

func within(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got < want*80/100 || got > want*120/100 {
		t.Fatalf("%s took %v, want %v ±20%%", what, got.Round(time.Millisecond), want)
	}
	t.Logf("%s took %v (expected %v)", what, got.Round(time.Millisecond), want)
}

func TestUploadAndDownloadRates(t *testing.T) {
	e := newEnv(t)
	e.rule(traffic.Rule{Name: "up", Scope: "host", Hosts: "*", Direction: "upload", RateBps: MiB, Burst: 64 << 10})
	e.rule(traffic.Rule{Name: "down", Scope: "host", Hosts: "*", Direction: "download", RateBps: 2 * MiB, Burst: 64 << 10})
	// 3 MiB at 1 MiB/s with a 64 KiB burst: (3 MiB - 64 KiB) / 1 MiB/s
	d, err := e.upload(context.Background(), "h1", 3*MiB)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "3 MiB upload at 1 MiB/s", d, time.Duration(float64(3*MiB-64<<10)/MiB*float64(time.Second)))
	start := time.Now()
	var sink bytes.Buffer
	if _, err := e.lim.Writer(context.Background(), "", "h1", &sink).Write(make([]byte, 5*MiB)); err != nil || sink.Len() != 5*MiB {
		t.Fatal(err, sink.Len())
	}
	within(t, "5 MiB download at 2 MiB/s", time.Since(start), time.Duration(float64(5*MiB-64<<10)/(2*MiB)*float64(time.Second)))
	// live counters saw both directions for h1
	cur := e.lim.Current()
	if len(cur) != 1 || cur[0].Host != "h1" || cur[0].UploadTotal != 3*MiB || cur[0].DownloadTotal != 5*MiB {
		t.Fatalf("current: %+v", cur)
	}
	// the live rate is the average of the last 5 complete seconds: sample it during a 7 s upload
	done := make(chan struct{})
	go func() { defer close(done); _, _ = e.upload(context.Background(), "h2", 7*MiB) }()
	time.Sleep(6200 * time.Millisecond)
	var r float64
	for _, h := range e.lim.Current() {
		if h.Host == "h2" {
			r = h.UploadBps
		}
	}
	<-done
	if r < 0.8*MiB || r > 1.2*MiB {
		t.Fatalf("live upload rate %.0f B/s, want 1 MiB/s ±20%%", r)
	}
	t.Logf("live upload rate %.0f B/s at 1 MiB/s", r)
}

func TestHostBucketsAreIndependentAndGlobalIsShared(t *testing.T) {
	e := newEnv(t)
	e.rule(traffic.Rule{Name: "per-host", Scope: "host", Hosts: "a*", Direction: "upload", RateBps: MiB, Burst: 64 << 10})
	e.rule(traffic.Rule{Name: "shared", Scope: "global", Hosts: "b*", Direction: "upload", RateBps: 2 * MiB, Burst: 64 << 10})
	run := func(hosts ...string) map[string]time.Duration {
		var mu sync.Mutex
		out := map[string]time.Duration{}
		var wg sync.WaitGroup
		for _, h := range hosts {
			wg.Add(1)
			go func(h string) {
				defer wg.Done()
				d, err := e.upload(context.Background(), h, 3*MiB)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				out[h] = d
				mu.Unlock()
			}(h)
		}
		wg.Wait()
		return out
	}
	// two hosts under the per-host rule: each gets its own 1 MiB/s, so both finish in ~3 s
	got := run("a1", "a2")
	for h, d := range got {
		within(t, "per-host "+h, d, time.Duration(float64(3*MiB-64<<10)/MiB*float64(time.Second)))
	}
	// two hosts sharing 2 MiB/s: 6 MiB in ~3 s in total, and fairly (both finish within 20 % of each other)
	got = run("b1", "b2")
	for h, d := range got {
		within(t, "shared "+h, d, time.Duration(float64(6*MiB-64<<10)/(2*MiB)*float64(time.Second)))
	}
	// a host not matched by any rule is not limited
	d, _ := e.upload(context.Background(), "c1", 8*MiB)
	if d > 500*time.Millisecond {
		t.Fatalf("unlimited host took %v", d)
	}
}

func TestStrictestRuleWinsAndChangesApply(t *testing.T) {
	e := newEnv(t)
	e.rule(traffic.Rule{Name: "wide", Scope: "global", Hosts: "*", Direction: "upload", RateBps: 8 * MiB, Burst: 64 << 10})
	e.rule(traffic.Rule{Name: "narrow", Scope: "host", Hosts: "slow", Direction: "upload", RateBps: MiB, Burst: 64 << 10})
	d, _ := e.upload(context.Background(), "slow", 2*MiB)
	within(t, "strictest of 8 and 1 MiB/s", d, time.Duration(float64(2*MiB-64<<10)/MiB*float64(time.Second)))
	// raise the narrow rule: the change applies after Invalidate (and within RefreshEvery without it)
	if _, err := e.store.Update(context.Background(), "narrow", traffic.Rule{Name: "narrow", Enabled: true, Scope: "host", Hosts: "slow", Direction: "upload", RateBps: 4 * MiB, Burst: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	e.lim.Invalidate()
	d, _ = e.upload(context.Background(), "slow", 4*MiB)
	within(t, "after raising to 4 MiB/s", d, time.Duration(float64(4*MiB-64<<10)/(4*MiB)*float64(time.Second)))
	// disabled rules do nothing
	if _, err := e.store.Update(context.Background(), "narrow", traffic.Rule{Name: "narrow", Enabled: false, Scope: "host", Hosts: "slow", Direction: "upload", RateBps: 4 * MiB}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(context.Background(), "wide"); err != nil {
		t.Fatal(err)
	}
	e.lim.Invalidate()
	if d, _ := e.upload(context.Background(), "slow", 8*MiB); d > 500*time.Millisecond {
		t.Fatalf("no active rule but took %v", d)
	}
	if err := e.store.Delete(context.Background(), "wide"); !errors.Is(err, traffic.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := e.store.Create(context.Background(), traffic.Rule{Name: "narrow", Scope: "host", Direction: "upload", RateBps: MiB}); !errors.Is(err, traffic.ErrExists) {
		t.Fatal(err)
	}
}

func TestTimeWindowDecidesWhetherARuleApplies(t *testing.T) {
	e := newEnv(t)
	e.rule(traffic.Rule{Name: "office", Scope: "host", Hosts: "*", Direction: "upload", RateBps: MiB, Burst: 64 << 10, Schedule: "mon-fri 08:00-18:00", Timezone: "Europe/Berlin"})
	ber := mustLoc(t, "Europe/Berlin")
	// a running clock shifted to a chosen local time (buckets need time to pass)
	shift := func(local time.Time) {
		off := time.Until(local)
		e.lim.Now = func() time.Time { return time.Now().Add(off) }
		e.lim.Invalidate()
	}
	shift(time.Date(2026, 10, 11, 10, 0, 0, 0, ber)) // Sunday 10:00: outside
	if d, _ := e.upload(context.Background(), "h", 8*MiB); d > 500*time.Millisecond {
		t.Fatalf("Sunday: limited (%v)", d)
	}
	shift(time.Date(2026, 10, 7, 10, 0, 0, 0, ber)) // Wednesday 10:00: inside
	d, _ := e.upload(context.Background(), "h", 2*MiB)
	within(t, "Wednesday 10:00", d, time.Duration(float64(2*MiB-64<<10)/MiB*float64(time.Second)))
	rs := e.lim.Rules(context.Background())
	if len(rs) != 1 || !rs[0].Active {
		t.Fatalf("rules: %+v", rs)
	}
}

func TestCancelStopsWaitingWithoutLeaks(t *testing.T) {
	e := newEnv(t)
	e.rule(traffic.Rule{Name: "slow", Scope: "global", Hosts: "*", Direction: "upload", RateBps: 64 << 10, Burst: 64 << 10})
	_, _ = e.upload(context.Background(), "warm", 1) // load rules and the pool before counting
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := e.upload(ctx, "h", 8*MiB) // would take 2 minutes
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
				t.Errorf("err %v after %v", err, time.Since(start))
			}
		}()
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}
