package mgmt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func init() { auth.Argon2Memory, auth.Argon2Time = 1024, 1 }

const pw = "correct horse battery"

type web struct {
	t     *testing.T
	ts    *httptest.Server
	auth  *auth.Store
	csrf  string
	httpc *http.Client
	srv   *mgmt.Server
	node  *testsupport.Node
}

func newWeb(t *testing.T) *web {
	t.Helper()
	n := testsupport.NewNode(t)
	st := auth.New(n.Svc.DB.Pool(), n.Svc.Sealer("test-totp"))
	s := &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Auth: st}
	ts := httptest.NewServer(s.WebHandler())
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close) // background tasks end before the database goes away
	jar, _ := cookiejar.New(nil)
	return &web{t: t, ts: ts, auth: st, srv: s, node: n, httpc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (w *web) req(method, path string, body any, hdr map[string]string) (int, []byte) {
	w.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, w.ts.URL+path, rd)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := w.httpc.Do(r)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (w *web) login(name string) {
	w.t.Helper()
	code, b := w.req("POST", "/api/v1/session", map[string]string{"name": name, "password": pw}, nil)
	if code != 200 {
		w.t.Fatalf("login %s: %d %s", name, code, b)
	}
	var v struct{ CSRF string }
	_ = json.Unmarshal(b, &v)
	w.csrf = v.CSRF
}

func (w *web) h() map[string]string { return map[string]string{"X-CSRF-Token": w.csrf} }

func TestEveryRouteRequiresLoginAndRoleMatrix(t *testing.T) {
	ctx := context.Background()
	w := newWeb(t)
	roles := []auth.Role{auth.ReadOnly, auth.Restore, auth.Operator, auth.Admin}
	for _, r := range roles {
		if _, err := w.auth.CreateUser(ctx, string(r), pw, r, "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	routes := mgmt.Perms()
	// 1. anonymous: everything but the public routes is 401
	for pat := range routes {
		if mgmt.PublicRoutes()[pat] {
			continue
		}
		method, path, _ := strings.Cut(pat, " ")
		path = strings.NewReplacer("{name}", "x", "{scope}", "backup", "{action}", "enable", "{group}", "0123456789abcdef0123456789abcdef", "{snap}", "s", "{id}", "0123456789abcdef").Replace(path)
		if code, _ := w.req(method, path, nil, nil); code != 401 {
			t.Errorf("anonymous %s: %d, want 401", pat, code)
		}
	}
	// 2. each role: 403 exactly when the role lacks the permission
	for _, role := range roles {
		w = &web{t: t, ts: w.ts, auth: w.auth, httpc: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		jar, _ := cookiejar.New(nil)
		w.httpc.Jar = jar
		w.login(string(role))
		for pat, perm := range routes {
			if mgmt.PublicRoutes()[pat] || pat == "GET /api/v1/events" || pat == "DELETE /api/v1/session" || strings.Contains(pat, "/me/") {
				continue
			}
			method, path, _ := strings.Cut(pat, " ")
			path = strings.NewReplacer("{name}", "nobody", "{scope}", "backup", "{action}", "enable", "{group}", "0123456789abcdef0123456789abcdef", "{snap}", "s", "{id}", "0123456789abcdef").Replace(path)
			code, body := w.req(method, path, nil, w.h())
			if want := !role.Allows(perm); (code == 403) != want {
				t.Errorf("%s as %s: %d (%s), forbidden want %v", pat, role, code, body, want)
			}
		}
	}
}

func TestCSRFAndBearer(t *testing.T) {
	ctx := context.Background()
	w := newWeb(t)
	_, _ = w.auth.CreateUser(ctx, "anna", pw, auth.Admin, "", "", false)
	w.login("anna")
	// cookie without CSRF header: refused
	if code, _ := w.req("PUT", "/api/v1/assignments", map[string]string{}, nil); code != 403 {
		t.Fatalf("no CSRF: %d", code)
	}
	// wrong header
	if code, _ := w.req("PUT", "/api/v1/assignments", map[string]string{}, map[string]string{"X-CSRF-Token": "x"}); code != 403 {
		t.Fatalf("wrong CSRF: %d", code)
	}
	// foreign origin even with the right token
	hh := w.h()
	hh["Origin"] = "https://evil.example"
	if code, _ := w.req("PUT", "/api/v1/assignments", map[string]string{}, hh); code != 403 {
		t.Fatalf("cross-origin: %d", code)
	}
	// right token passes the gate (the empty assignment is then a 400 from the service)
	if code, b := w.req("PUT", "/api/v1/assignments", map[string]string{}, w.h()); code == 403 || code == 401 {
		t.Fatalf("valid CSRF refused: %d %s", code, b)
	}
	// bearer token: no cookie, no CSRF, same role limits
	code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "ci", "role": "readonly"}, w.h())
	if code != 201 {
		t.Fatalf("%d %s", code, b)
	}
	var tk struct{ Token string }
	_ = json.Unmarshal(b, &tk)
	bare := &web{t: t, ts: w.ts, httpc: &http.Client{}}
	if code, _ := bare.req("GET", "/api/v1/repositories", nil, map[string]string{"Authorization": "Bearer " + tk.Token}); code != 200 {
		t.Fatalf("bearer read: %d", code)
	}
	if code, _ := bare.req("DELETE", "/api/v1/policies/x", nil, map[string]string{"Authorization": "Bearer " + tk.Token}); code != 403 {
		t.Fatalf("readonly token must not delete: %d", code)
	}
	if code, _ := bare.req("GET", "/api/v1/repositories", nil, map[string]string{"Authorization": "Bearer ibt_nope"}); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	// the secret S3 material is never in a credentials listing; token secrets never in the token listing
	code, b = w.req("GET", "/api/v1/me/tokens", nil, nil)
	if code != 200 || strings.Contains(string(b), tk.Token) {
		t.Fatalf("token secret leaked in listing: %d %s", code, b)
	}
}

func TestSetupFlowAndAudit(t *testing.T) {
	ctx := context.Background()
	w := newWeb(t)
	code, b := w.req("GET", "/api/v1/setup", nil, nil)
	if code != 200 || !strings.Contains(string(b), `"needed":true`) {
		t.Fatalf("%d %s", code, b)
	}
	tok, _ := w.auth.SetupToken(ctx)
	if code, _ := w.req("POST", "/api/v1/setup", map[string]string{"token": "wrong", "name": "boss", "password": pw}, nil); code != 400 {
		t.Fatalf("wrong setup token: %d", code)
	}
	if code, b := w.req("POST", "/api/v1/setup", map[string]string{"token": tok, "name": "boss", "password": pw}, nil); code != 201 {
		t.Fatalf("%d %s", code, b)
	}
	w.login("boss")
	// a mutating call is audited, a failed login too
	w.req("PUT", "/api/v1/assignments", map[string]string{}, w.h())
	w.req("POST", "/api/v1/session", map[string]string{"name": "boss", "password": "wrong wrong wrong"}, nil)
	code, b = w.req("GET", "/api/v1/audit", nil, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	for _, want := range []string{`"action":"setup"`, `"action":"login"`, `"action":"put /assignments"`, `"ok":false`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("audit lacks %s:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), pw) {
		t.Fatal("password in audit log")
	}
}

func TestSecurityHeadersAndLoginThrottle(t *testing.T) {
	w := newWeb(t)
	resp, err := http.Get(w.ts.URL + "/api/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Cache-Control"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	got429 := false
	for i := 0; i < 30; i++ {
		if code, _ := w.req("POST", "/api/v1/session", map[string]string{"name": "x", "password": "y"}, nil); code == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("login attempts from one address are not rate limited")
	}
}
