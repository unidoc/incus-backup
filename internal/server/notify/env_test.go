package notify_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) {
	testdb.AtExit(s3test.Start())
	testdb.Main(m)
}

// syncBuf is a goroutine-safe log sink (the engine logs from delivery goroutines).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	t    *testing.T
	ctx  context.Context
	d    *db.DB
	url  string
	svc  *service.Service
	eng  *notify.Engine
	repo *service.Repo
	logs *syncBuf

	mu  sync.Mutex
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	u := testdb.New(t)
	d, err := db.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, d: d, url: u, logs: &syncBuf{}, now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	e.svc = service.New(d, filepath.Join(dir, "state"), "test", slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	e.svc.Now = e.clock
	e.repo, err = e.svc.CreateRepository(ctx, "custody", filepath.Join(dir, "ds"))
	if err != nil {
		t.Fatal(err)
	}
	e.eng = notify.New(e.svc)
	return e
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}
func (e *env) set(t time.Time) { e.mu.Lock(); e.now = t; e.mu.Unlock() }

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.d.Pool().Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) tick() *notify.TickResult {
	e.t.Helper()
	r, err := e.eng.Tick(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if r.Skipped {
		e.t.Fatal("tick skipped")
	}
	if len(r.Errors) > 0 {
		e.t.Fatalf("condition errors: %v", r.Errors)
	}
	return r
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// host enrols a backup host directly in the database; returns its id.
func (e *env) host(name string, lastSeen *time.Time) string {
	e.t.Helper()
	var id string
	if err := e.d.Pool().QueryRow(e.ctx, `INSERT INTO hosts (repository_id, name, scope, cert_fingerprint, enrolled_at, last_seen_at) VALUES ($1,$2,'backup',$3,$4,$5) RETURNING id`,
		e.repo.ID, name, randHex(32), e.clock().Add(-30*24*time.Hour), lastSeen).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) policy(name, sched string) {
	e.t.Helper()
	e.exec(`INSERT INTO policies (repository_id, name, schedule) VALUES ($1,$2,$3)`, e.repo.ID, name, sched)
}

// object registers an instance object of host; returns its group id.
func (e *env) object(hostID, hostName, project, inst, policy string, firstSeen time.Time) string {
	e.t.Helper()
	g := randHex(16)
	e.exec(`INSERT INTO storage_objects (repository_id, host_id, host_name, project, name, kind, group_id, policy_name, retention_policy, first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,'instance',$6,$7,$7,$8,$9)`, e.repo.ID, hostID, hostName, project, "instance/"+inst+"/root", g, policy, firstSeen, e.clock())
	return g
}

func (e *env) snapshot(group string, committed time.Time) string {
	e.t.Helper()
	id := randHex(8)
	e.exec(`INSERT INTO snapshots (repository_id, group_id, snapshot_id, created_at, committed_at, index_digest) VALUES ($1,$2,$3,$4,$4,'x')`,
		e.repo.ID, group, id, committed)
	return id
}

func (e *env) alerts(f notify.AlertFilter) []notify.Alert {
	e.t.Helper()
	as, err := e.eng.ListAlerts(e.ctx, f)
	if err != nil {
		e.t.Fatal(err)
	}
	return as
}

func (e *env) alertByKey(key string) *notify.Alert {
	e.t.Helper()
	for _, a := range e.alerts(notify.AlertFilter{Limit: 1000}) {
		if a.Key == key {
			return &a
		}
	}
	return nil
}

func (e *env) deliveries(f notify.DeliveryFilter) []notify.Delivery {
	e.t.Helper()
	ds, err := e.eng.ListDeliveries(e.ctx, f)
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

// ---- an HTTP receiver ---------------------------------------------------------------------

type rcvReq struct {
	Path   string
	Header http.Header
	Body   []byte
	At     time.Time
}

type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []rcvReq
	status []int // status per request (last one repeats); default 200
	body   string
	echo   bool // answer with what the request carried (a verbose receiver echoing credentials)
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, rcvReq{Path: req.URL.Path, Header: req.Header.Clone(), Body: b, At: time.Now()})
		code := 200
		if len(r.status) > 0 {
			code = r.status[0]
			if len(r.status) > 1 {
				r.status = r.status[1:]
			}
		}
		body := r.body
		if r.echo {
			body += " auth=" + req.Header.Get("Authorization") + " key=" + req.Header.Get("X-Gotify-Key") + " path=" + req.URL.Path
		}
		r.mu.Unlock()
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) setStatus(body string, codes ...int) {
	r.mu.Lock()
	r.status, r.body = codes, body
	r.mu.Unlock()
}

func (r *receiver) requests() []rcvReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rcvReq(nil), r.reqs...)
}

// webhookChannel creates a JSON webhook channel to r with a rule that hears everything (warning+).
func (e *env) webhookChannel(r *receiver, name string, mut func(*notify.Rule)) (*notify.Channel, *notify.Rule) {
	e.t.Helper()
	ch, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: name, Kind: notify.KindWebhook, Config: json.RawMessage(`{"format":"json"}`),
		Secret: &notify.ChannelSecret{URL: r.URL + "/hook/" + name, HMACSecret: "hmac-" + name}})
	if err != nil {
		e.t.Fatal(err)
	}
	rule := notify.DefaultRule("rule-"+name, ch.ID)
	if mut != nil {
		mut(&rule)
	}
	rr, err := e.eng.CreateRule(e.ctx, rule)
	if err != nil {
		e.t.Fatal(err)
	}
	return ch, rr
}

type hookMsg struct {
	Kind     string           `json:"kind"`
	Severity string           `json:"severity"`
	Subject  string           `json:"subject"`
	Text     string           `json:"text"`
	Link     string           `json:"link"`
	Alert    notify.AlertView `json:"alert"`
}

func (r *receiver) messages(t *testing.T) []hookMsg {
	t.Helper()
	var out []hookMsg
	for _, q := range r.requests() {
		var m hookMsg
		if err := json.Unmarshal(q.Body, &m); err != nil {
			t.Fatalf("webhook body is not JSON: %v: %s", err, q.Body)
		}
		out = append(out, m)
	}
	return out
}

func kinds(ms []hookMsg) []string {
	var k []string
	for _, m := range ms {
		k = append(k, m.Kind)
	}
	return k
}
