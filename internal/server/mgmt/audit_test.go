package mgmt_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

type auditWorld struct {
	*web
	srv *mgmt.Server
	n   *testsupport.Node
}

func newAuditWorld(t *testing.T) *auditWorld {
	t.Helper()
	n := testsupport.NewNode(t)
	st := auth.New(n.Svc.DB.Pool(), n.Svc.Sealer("test-totp"))
	s := &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Auth: st}
	ts := httptest.NewServer(s.WebHandler())
	t.Cleanup(ts.Close)
	t.Cleanup(s.FlushAudit) // stops the summary flusher
	jar, _ := cookiejar.New(nil)
	return &auditWorld{web: &web{t: t, ts: ts, auth: st, httpc: &http.Client{Jar: jar}}, srv: s, n: n}
}

func (w *auditWorld) entries(action string) []auth.AuditEntry {
	w.t.Helper()
	l, err := w.auth.ListAudit(context.Background(), auth.AuditFilter{Action: action, Limit: 1000})
	if err != nil {
		w.t.Fatal(err)
	}
	return l
}

func detail(t *testing.T, e auth.AuditEntry) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(e.Detail, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func withWindow(t *testing.T, d time.Duration) {
	old := mgmt.AuditWindow
	mgmt.AuditWindow = d
	t.Cleanup(func() { mgmt.AuditWindow = old })
}

// Refused requests are audited: a role refusal (403), a CSRF failure, a cross-site login. Repeats
// within the window collapse into one summary entry instead of one row each.
func TestRefusedRequestsAreAuditedWithoutFlooding(t *testing.T) {
	withWindow(t, time.Hour) // summaries only on an explicit flush
	ctx := context.Background()
	w := newAuditWorld(t)
	if _, err := w.auth.CreateUser(ctx, "viewer", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("viewer")
	for i := 0; i < 5; i++ {
		if code, _ := w.req("DELETE", "/api/v1/policies/prod", nil, w.h()); code != http.StatusForbidden {
			t.Fatalf("readonly delete: %d", code)
		}
	}
	if code, _ := w.req("PUT", "/api/v1/assignments", map[string]string{}, nil); code != http.StatusForbidden {
		t.Fatalf("no CSRF: %d", code)
	}
	if code, _ := w.req("POST", "/api/v1/session", map[string]string{"name": "viewer", "password": pw}, map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("cross-site login: %d", code)
	}
	del := w.entries("delete /policies/{name}")
	if len(del) != 1 || del[0].Actor != "viewer" || del[0].OK || detail(t, del[0])["reason"] != "role readonly" {
		t.Fatalf("role refusal: %+v", del)
	}
	csrf := w.entries("put /assignments")
	if len(csrf) != 1 || detail(t, csrf[0])["reason"] != "csrf" || csrf[0].OK {
		t.Fatalf("csrf refusal: %+v", csrf)
	}
	xs := w.entries("post /session")
	if len(xs) != 1 || xs[0].Actor != "anonymous" || detail(t, xs[0])["reason"] != "cross-site request" {
		t.Fatalf("cross-site login: %+v", xs)
	}
	w.srv.FlushAudit()
	del = w.entries("delete /policies/{name}")
	if len(del) != 2 || detail(t, del[0])["repeated"] != float64(4) {
		t.Fatalf("summary of the repeats: %+v", del)
	}
}

// Raw repository material is audited as one summary per actor, repository and snapshot per
// window (counts and bytes), never one row per chunk; a delivery download is one entry each.
func TestRawReadsAndDeliveriesAreAudited(t *testing.T) {
	withWindow(t, 300*time.Millisecond)
	ctx := context.Background()
	w := newAuditWorld(t)
	if _, err := w.auth.CreateUser(ctx, "anna", pw, auth.Restore, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("anna")
	k := masterKeys()
	snap := "20261007100000-00000001"
	parts := []string{"alpha", "beta", "gamma"}
	backup(t, w.n, k, snap, w.n.Now, parts...)
	code, idx := w.req("GET", "/api/v1/snapshots/"+group+"/"+snap+"/index", nil, nil)
	if code != 200 {
		t.Fatalf("index: %d", code)
	}
	code, man := w.req("GET", "/api/v1/snapshots/"+group+"/"+snap+"/manifest", nil, nil)
	if code != 200 {
		t.Fatalf("manifest: %d", code)
	}
	x, err := format.ParseIndex(idx)
	if err != nil {
		t.Fatal(err)
	}
	total := int64(len(idx) + len(man))
	for i := 0; i < 20; i++ { // a browser reads the same chunks again and again
		for _, e := range x.Entries {
			code, b := w.req("GET", "/api/v1/chunks/"+e.ID.String(), nil, nil)
			if code != 200 {
				t.Fatalf("chunk: %d", code)
			}
			total += int64(len(b))
		}
	}
	if code, _ := w.req("GET", "/api/v1/jobs/00000000-0000-4000-8000-000000000001/delivery", nil, nil); code != http.StatusNotFound {
		t.Fatalf("delivery of an unknown job: %d", code)
	}
	dl := w.entries("download delivery")
	if len(dl) != 1 || dl[0].Actor != "anna" || dl[0].OK || detail(t, dl[0])["job"] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("delivery download: %+v", dl)
	}
	// the summary is written by itself when the window ends
	var raw []auth.AuditEntry
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if raw = w.entries("read repository material"); len(raw) > 0 {
			break
		}
	}
	if len(raw) != 1 {
		t.Fatalf("%d summaries for 62 requests: %+v", len(raw), raw)
	}
	d := detail(t, raw[0])
	if raw[0].Actor != "anna" || !strings.HasSuffix(raw[0].Target, group+"/"+snap) || d["index"] != float64(1) || d["manifest"] != float64(1) ||
		d["chunk"] != float64(60) || d["chunks_attributed"] != float64(60) || d["requests"] != float64(62) || d["bytes"] != float64(total) || !raw[0].OK {
		t.Fatalf("summary: %s %s", raw[0].Target, raw[0].Detail)
	}
}

// A token's audit actor names its owner: two users' tokens called "ci" stay apart.
func TestTokenActorNamesItsOwner(t *testing.T) {
	withWindow(t, time.Hour)
	ctx := context.Background()
	w := newAuditWorld(t)
	tokens := map[string]string{}
	for _, u := range []string{"anna", "bob"} {
		if _, err := w.auth.CreateUser(ctx, u, pw, auth.Admin, "", "", false); err != nil {
			t.Fatal(err)
		}
		w.login(u)
		code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "ci", "role": "readonly"}, w.h())
		if code != 201 {
			t.Fatalf("%d %s", code, b)
		}
		var tk struct{ Token string }
		_ = json.Unmarshal(b, &tk)
		tokens[u] = tk.Token
	}
	bare := &web{t: t, ts: w.ts, httpc: &http.Client{}}
	for _, u := range []string{"anna", "bob"} {
		if code, _ := bare.req("DELETE", "/api/v1/policies/x", nil, map[string]string{"Authorization": "Bearer " + tokens[u]}); code != 403 {
			t.Fatalf("readonly token delete: %d", code)
		}
	}
	got := map[string]bool{}
	for _, e := range w.entries("delete /policies/{name}") {
		got[e.Actor] = true
		if e.Kind != "token" {
			t.Fatalf("kind %q", e.Kind)
		}
	}
	if !got["token:ci (by anna)"] || !got["token:ci (by bob)"] {
		t.Fatalf("token actors: %v", got)
	}
}
