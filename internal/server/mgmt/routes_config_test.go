package mgmt_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// newWebWithNotify is newWeb with a notification engine the test can drive (Emit).
func newWebWithNotify(t *testing.T) (*web, *notify.Engine) {
	t.Helper()
	n := testsupport.NewNode(t)
	st := auth.New(n.Svc.DB.Pool(), n.Svc.Sealer("test-totp"))
	ne := mgmt.NewNotifyEngine(n.Svc)
	s := &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Auth: st, Notify: ne,
		WebCert: func() any { return map[string]any{"mode": "self-signed", "days_left": 300} }}
	ts := httptest.NewServer(s.WebHandler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &web{t: t, ts: ts, auth: st, httpc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, ne
}

func (w *web) json(method, path string, body any, want int, out any) {
	w.t.Helper()
	code, b := w.req(method, path, body, w.h())
	if code != want {
		w.t.Fatalf("%s %s: %d, want %d: %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			w.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
}

// receiver is a webhook endpoint that records what it was sent.
type receiver struct {
	mu   sync.Mutex
	got  []map[string]any
	srv  *httptest.Server
	code int
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{code: 200}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		rc.mu.Lock()
		rc.got = append(rc.got, m)
		code := rc.code
		rc.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte("receiver says hello"))
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) kinds() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []string
	for _, m := range rc.got {
		k, _ := m["kind"].(string)
		out = append(out, k)
	}
	return out
}

func TestAboutAndSetupChecklist(t *testing.T) {
	ctx := context.Background()
	w, _ := newWebWithNotify(t)
	if _, err := w.auth.CreateUser(ctx, "boss", pw, auth.Admin, "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.auth.CreateUser(ctx, "viewer", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("boss")
	var a mgmt.About
	w.json("GET", "/api/v1/about", nil, 200, &a)
	if !a.DatabaseOK || !strings.HasPrefix(a.GoVersion, "go") || a.Started.IsZero() || a.DatastoreCount < 1 {
		t.Fatalf("about: %+v", a)
	}
	var cl struct {
		Items []struct {
			ID     string
			OK     bool
			Detail string
			Route  string
		}
		Done, Total int
		Complete    bool
	}
	w.json("GET", "/api/v1/setup-checklist", nil, 200, &cl)
	state := map[string]bool{}
	for _, it := range cl.Items {
		state[it.ID] = it.OK
		if it.Detail == "" || it.Route == "" {
			t.Errorf("item %s lacks detail or route", it.ID)
		}
	}
	if cl.Total != 8 || cl.Complete || state["base_url"] || state["channel"] || state["admin_2fa"] {
		t.Fatalf("fresh checklist: %+v", cl)
	}
	w.json("PUT", "/api/v1/settings", map[string]string{"base_url": "https://backup.example.com"}, 204, nil)
	w.json("GET", "/api/v1/setup-checklist", nil, 200, &cl)
	for _, it := range cl.Items {
		if it.ID == "base_url" && (!it.OK || it.Detail != "https://backup.example.com") {
			t.Fatalf("base_url item after setting it: %+v", it)
		}
	}
	// the web certificate is part of GET /certificates when the listener reports one
	var certs map[string]json.RawMessage
	w.json("GET", "/api/v1/certificates", nil, 200, &certs)
	if !strings.Contains(string(certs["web"]), `"self-signed"`) || len(certs["report"]) == 0 {
		t.Fatalf("certificates: %v", certs)
	}
	// a viewer may read /about but not the checklist
	v := &web{t: t, ts: w.ts, auth: w.auth, httpc: &http.Client{}}
	v.httpc.Jar, _ = cookiejar.New(nil)
	v.login("viewer")
	if code, _ := v.req("GET", "/api/v1/about", nil, nil); code != 200 {
		t.Fatalf("viewer about: %d", code)
	}
	if code, _ := v.req("GET", "/api/v1/setup-checklist", nil, nil); code != 403 {
		t.Fatalf("viewer checklist: %d", code)
	}
}

// The full notification path over the API: a webhook channel, its test message (the receiver's real
// answer), a rule, an alert, a manual tick that delivers it, and the simulator's verdicts.
func TestNotifyTickDeliversToWebhookAndSimulate(t *testing.T) {
	ctx := context.Background()
	w, ne := newWebWithNotify(t)
	if _, err := w.auth.CreateUser(ctx, "boss", pw, auth.Admin, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("boss")
	rc := newReceiver(t)

	var ch notify.Channel
	w.json("POST", "/api/v1/notifications/channels", map[string]any{"name": "hook", "kind": "webhook", "config": map[string]any{"format": "json"},
		"secret": map[string]any{"url": rc.srv.URL + "/hook?token=s3cret"}}, 201, &ch)
	if !ch.HasSecret || strings.Contains(ch.Target, "s3cret") {
		t.Fatalf("channel view: %+v", ch)
	}
	var tr struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	w.json("POST", "/api/v1/notifications/channels/"+ch.ID+"/test", nil, 200, &tr)
	if !tr.OK || len(rc.kinds()) != 1 || rc.kinds()[0] != "test" {
		t.Fatalf("test: %+v, receiver got %v", tr, rc.kinds())
	}
	// the receiver's refusal comes back as the answer
	rc.mu.Lock()
	rc.code = 401
	rc.mu.Unlock()
	w.json("POST", "/api/v1/notifications/channels/"+ch.ID+"/test", nil, 200, &tr)
	if tr.OK || !strings.Contains(tr.Error, "401") || strings.Contains(tr.Error, "s3cret") {
		t.Fatalf("refused test: %+v", tr)
	}
	rc.mu.Lock()
	rc.code = 200
	rc.mu.Unlock()

	var rule notify.Rule
	w.json("POST", "/api/v1/notifications/rules", map[string]any{"name": "page-me", "channel_ids": []string{ch.ID}, "event_types": []string{"job_*"}, "min_severity": "warning"}, 201, &rule)

	// simulator: same matching as the runner
	var sim notify.SimulateResult
	w.json("POST", "/api/v1/notifications/simulate", map[string]any{"type": "job_failed", "severity": "critical", "host": "node1"}, 200, &sim)
	if !sim.Deliver || len(sim.Rules) != 1 || !sim.Rules[0].Matched || sim.Rules[0].Channels[0].Name != "hook" {
		t.Fatalf("simulate match: %+v", sim)
	}
	w.json("POST", "/api/v1/notifications/simulate", map[string]any{"type": "job_failed", "severity": "info"}, 200, &sim)
	if sim.Deliver || sim.Rules[0].Matched || !strings.Contains(sim.Rules[0].Reason, "below") {
		t.Fatalf("simulate severity: %+v", sim)
	}
	w.json("POST", "/api/v1/notifications/simulate", map[string]any{"type": "backup_missed", "severity": "critical"}, 200, &sim)
	if sim.Rules[0].Matched || !strings.Contains(sim.Rules[0].Reason, "job_*") {
		t.Fatalf("simulate type: %+v", sim)
	}
	if code, _ := w.req("POST", "/api/v1/notifications/simulate", map[string]any{"severity": "warning"}, w.h()); code != 400 {
		t.Fatalf("simulate without type: %d", code)
	}

	// an alert, then a manual tick delivers it
	if err := ne.Emit(ctx, notify.Event{Type: "job_failed", Key: "restore-1", Severity: "warning", Title: "restore job failed", Host: "node1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !contains(rc.kinds(), "firing") {
		var res notify.TickResult
		w.json("POST", "/api/v1/notifications/tick", nil, 200, &res)
		if time.Now().After(deadline) {
			t.Fatalf("no firing message; receiver got %v, last tick %+v", rc.kinds(), res)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var dl []notify.Delivery
	w.json("GET", "/api/v1/notifications/deliveries?state=sent", nil, 200, &dl)
	if len(dl) < 2 {
		t.Fatalf("deliveries: %+v", dl)
	}
	// with a channel tested and a rule, those checklist items are done
	var cl struct {
		Items []struct {
			ID string
			OK bool
		}
	}
	w.json("GET", "/api/v1/setup-checklist", nil, 200, &cl)
	for _, it := range cl.Items {
		if (it.ID == "channel" || it.ID == "channel_tested" || it.ID == "rule") && !it.OK {
			t.Errorf("checklist %s not done: %+v", it.ID, cl.Items)
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestAdminTokenOverview(t *testing.T) {
	ctx := context.Background()
	w, _ := newWebWithNotify(t)
	if _, err := w.auth.CreateUser(ctx, "boss", pw, auth.Admin, "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.auth.CreateUser(ctx, "ops", pw, auth.Operator, "", "", false); err != nil {
		t.Fatal(err)
	}
	sec, _, err := w.auth.CreateToken(ctx, "ops", "ci", auth.Operator, 0)
	if err != nil {
		t.Fatal(err)
	}
	// demoting the owner limits the token
	ro := auth.ReadOnly
	if err := w.auth.UpdateUser(ctx, "ops", &ro, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	w.login("boss")
	var l []auth.AdminTokenInfo
	w.json("GET", "/api/v1/api-tokens", nil, 200, &l)
	if len(l) != 1 || l[0].Owner != "ops" || l[0].Role != auth.Operator || l[0].EffectiveRole != auth.ReadOnly {
		t.Fatalf("overview: %+v", l)
	}
	if code, b := w.req("GET", "/api/v1/api-tokens", nil, nil); strings.Contains(string(b), sec) || code != 200 {
		t.Fatal("token secret in the overview")
	}
	w.json("DELETE", "/api/v1/api-tokens/"+l[0].ID, nil, 204, nil)
	w.json("DELETE", "/api/v1/api-tokens/"+l[0].ID, nil, 404, nil)
	if _, err := w.auth.AuthenticateToken(ctx, sec); err == nil {
		t.Fatal("revoked token still authenticates")
	}
}
