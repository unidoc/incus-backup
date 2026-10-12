package traffic

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RefreshEvery is how old the cached rules may get before they are re-read.
const RefreshEvery = 30 * time.Second

// piece bounds one bucket reservation, so concurrent transfers interleave (fairness) and no single
// wait ever exceeds what a bucket can hold.
const piece = 32 << 10

// Limiter applies the rules to transfers. The zero value is not usable; use New or For.
type Limiter struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Now is the clock for windows and buckets (tests inject one).
	Now func() time.Time

	mu        sync.Mutex
	loaded    time.Time
	rules     []compiled
	repoNames map[string]string
	buckets   map[string]*bucket
	meters    map[meterKey]*meter
}

type compiled struct {
	Rule
	sched Schedule
	loc   *time.Location
	burst float64
}

// New returns a limiter reading rules from pool.
func New(pool *pgxpool.Pool, log *slog.Logger) *Limiter {
	if log == nil {
		log = slog.Default()
	}
	return &Limiter{Pool: pool, Log: log, Now: time.Now, buckets: map[string]*bucket{}, meters: map[meterKey]*meter{}}
}

var (
	regMu    sync.Mutex
	registry = map[*pgxpool.Pool]*Limiter{}
)

// For returns THE limiter of a database (created on first use), so the agent API and the management
// API share buckets and live counters without wiring.
func For(pool *pgxpool.Pool) *Limiter {
	regMu.Lock()
	defer regMu.Unlock()
	if l, ok := registry[pool]; ok {
		return l
	}
	l := New(pool, nil)
	registry[pool] = l
	return l
}

// Invalidate makes the next transfer re-read the rules (after a change through the API).
func (l *Limiter) Invalidate() {
	l.mu.Lock()
	l.loaded = time.Time{}
	l.mu.Unlock()
}

// refresh re-reads rules and repository names when they are older than RefreshEvery. On a database
// error the previous rules stay in force (and with none loaded yet, nothing is limited).
func (l *Limiter) refresh(ctx context.Context) {
	l.mu.Lock()
	stale := l.Now().Sub(l.loaded) >= RefreshEvery
	l.mu.Unlock()
	if !stale || l.Pool == nil {
		return
	}
	rules, err := Store{Pool: l.Pool}.List(ctx)
	names := map[string]string{}
	if err == nil {
		rows, qerr := l.Pool.Query(ctx, `SELECT id::text, name FROM repositories`)
		if qerr == nil {
			for rows.Next() {
				var id, n string
				if rows.Scan(&id, &n) == nil {
					names[id] = n
				}
			}
			rows.Close()
		}
		err = qerr
	}
	if err != nil {
		l.Log.Warn("traffic rules: keeping the previous rules", "err", err)
		l.mu.Lock()
		l.loaded = l.Now() // do not hammer a failing database
		l.mu.Unlock()
		return
	}
	var cs []compiled
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		sc, err := ParseSchedule(r.Schedule)
		loc, lerr := time.LoadLocation(r.Timezone)
		if err != nil || lerr != nil {
			continue // Validate prevents this; a rule that cannot be evaluated is skipped
		}
		b := float64(r.Burst)
		if b <= 0 {
			b = float64(r.RateBps)
		}
		if b < piece {
			b = piece
		}
		cs = append(cs, compiled{Rule: r, sched: sc, loc: loc, burst: b})
	}
	l.mu.Lock()
	l.rules, l.repoNames, l.loaded = cs, names, l.Now()
	l.mu.Unlock()
}

// Rules returns the rules in force and whether each is active now.
func (l *Limiter) Rules(ctx context.Context) []ActiveRule {
	l.refresh(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	out := []ActiveRule{}
	for _, r := range l.rules {
		out = append(out, ActiveRule{Name: r.Name, Scope: r.Scope, Direction: r.Direction, RateBps: r.RateBps, Active: r.sched.Active(now, r.loc)})
	}
	return out
}

// ActiveRule is a rule as GET /traffic/current shows it.
type ActiveRule struct {
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Direction string `json:"direction"`
	RateBps   int64  `json:"rate_bps"`
	Active    bool   `json:"active" doc:"inside its time window now"`
}

// bucketsFor returns the buckets that apply to a transfer now (strictest wins: a transfer waits for
// every one of them) and the repository name.
func (l *Limiter) bucketsFor(ctx context.Context, repoID, host, dir string) ([]*bucket, string) {
	l.refresh(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	repo := l.repoNames[repoID]
	now := l.Now()
	var out []*bucket
	for _, r := range l.rules {
		if r.Direction != dir || !r.Matches(repo, host) || !r.sched.Active(now, r.loc) {
			continue
		}
		key := r.ID + "|" + strconv.FormatInt(r.Updated.UnixNano(), 10)
		if r.Scope == ScopeHost {
			key += "|" + repoID + "/" + host
		}
		b := l.buckets[key]
		if b == nil {
			if len(l.buckets) > 10000 { // bound memory: idle buckets refill to full anyway
				l.buckets = map[string]*bucket{}
			}
			b = &bucket{rate: float64(r.RateBps), burst: r.burst, tokens: r.burst, last: now, now: l.Now}
			l.buckets[key] = b
		}
		out = append(out, b)
	}
	return out, repo
}

// ---- token bucket ---------------------------------------------------------------------------

type bucket struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// reserve takes n bytes and returns how long the caller must wait before sending them. Reservations
// queue up in arrival order (tokens go negative), which makes a shared bucket fair between hosts.
func (b *bucket) reserve(n float64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = now
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

func wait(ctx context.Context, bs []*bucket, n int) error {
	var d time.Duration
	for _, b := range bs {
		if x := b.reserve(float64(n)); x > d {
			d = x
		}
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- readers and writers --------------------------------------------------------------------

type transfer struct {
	l   *Limiter
	ctx context.Context
	bs  []*bucket
	m   *meter
}

func (l *Limiter) transfer(ctx context.Context, repoID, host, dir string) transfer {
	bs, repo := l.bucketsFor(ctx, repoID, host, dir)
	return transfer{l: l, ctx: ctx, bs: bs, m: l.meter(repo, host, dir)}
}

// Reader limits a request body (upload) of a host. The host is identified by its repository id and name.
func (l *Limiter) Reader(ctx context.Context, repoID, host string, r io.Reader) io.Reader {
	return &reader{transfer: l.transfer(ctx, repoID, host, Upload), r: r}
}

type reader struct {
	transfer
	r io.Reader
}

func (x *reader) Read(p []byte) (int, error) {
	if len(p) > piece {
		p = p[:piece]
	}
	n, err := x.r.Read(p)
	if n > 0 {
		x.m.add(n, x.l.Now())
		if len(x.bs) > 0 {
			if werr := wait(x.ctx, x.bs, n); werr != nil {
				return n, werr
			}
		}
	}
	return n, err
}

// Writer limits a response body (download) to a host.
func (l *Limiter) Writer(ctx context.Context, repoID, host string, w io.Writer) io.Writer {
	return &writer{transfer: l.transfer(ctx, repoID, host, Download), w: w}
}

type writer struct {
	transfer
	w io.Writer
}

func (x *writer) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		n := len(p) - done
		if n > piece {
			n = piece
		}
		if len(x.bs) > 0 {
			if err := wait(x.ctx, x.bs, n); err != nil {
				return done, err
			}
		}
		k, err := x.w.Write(p[done : done+n])
		done += k
		x.m.add(k, x.l.Now())
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// ---- live counters --------------------------------------------------------------------------

type meterKey struct{ repo, host, dir string }

const rateWindow = 5 // seconds averaged for the live rate

type meter struct {
	mu    sync.Mutex
	secs  [rateWindow + 2]int64 // unix second of each slot
	bytes [rateWindow + 2]int64
	total int64
	seen  time.Time
}

func (m *meter) add(n int, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := now.Unix()
	i := int(s % int64(len(m.secs)))
	if m.secs[i] != s {
		m.secs[i], m.bytes[i] = s, 0
	}
	m.bytes[i] += int64(n)
	m.total += int64(n)
	m.seen = now
}

// rate is the average over the last rateWindow complete seconds.
func (m *meter) rate(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := now.Unix()
	var sum int64
	for i, s := range m.secs {
		if s < cur && s >= cur-rateWindow {
			sum += m.bytes[i]
		}
	}
	return float64(sum) / rateWindow
}

func (l *Limiter) meter(repo, host, dir string) *meter {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := meterKey{repo, host, dir}
	m := l.meters[k]
	if m == nil {
		if len(l.meters) > 20000 {
			l.meters = map[meterKey]*meter{}
		}
		m = &meter{}
		l.meters[k] = m
	}
	return m
}

// HostRate is one host's live throughput (GET /traffic/current).
type HostRate struct {
	Repository    string    `json:"repository"`
	Host          string    `json:"host"`
	UploadBps     float64   `json:"upload_bps" doc:"average over the last 5 seconds"`
	DownloadBps   float64   `json:"download_bps"`
	UploadTotal   int64     `json:"upload_bytes_total" doc:"since the server started"`
	DownloadTotal int64     `json:"download_bytes_total"`
	LastSeen      time.Time `json:"last_seen"`
}

// Current returns every host seen since the server started, busiest first.
func (l *Limiter) Current() []HostRate {
	now := l.Now()
	l.mu.Lock()
	keys := make([]meterKey, 0, len(l.meters))
	ms := make([]*meter, 0, len(l.meters))
	for k, m := range l.meters {
		keys = append(keys, k)
		ms = append(ms, m)
	}
	l.mu.Unlock()
	by := map[[2]string]*HostRate{}
	for i, k := range keys {
		hk := [2]string{k.repo, k.host}
		h := by[hk]
		if h == nil {
			h = &HostRate{Repository: k.repo, Host: k.host}
			by[hk] = h
		}
		m := ms[i]
		r := m.rate(now)
		m.mu.Lock()
		tot, seen := m.total, m.seen
		m.mu.Unlock()
		if k.dir == Upload {
			h.UploadBps, h.UploadTotal = r, tot
		} else {
			h.DownloadBps, h.DownloadTotal = r, tot
		}
		if seen.After(h.LastSeen) {
			h.LastSeen = seen
		}
	}
	out := make([]HostRate, 0, len(by))
	for _, h := range by {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].UploadBps+out[i].DownloadBps, out[j].UploadBps+out[j].DownloadBps
		if a != b {
			return a > b
		}
		return out[i].Repository+"/"+out[i].Host < out[j].Repository+"/"+out[j].Host
	})
	return out
}
