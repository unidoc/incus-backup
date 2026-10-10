package mgmt_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// The metadata table (routes_meta.go) must describe exactly the registered routes: the scope
// enforcement and the OpenAPI document are generated from it.
func TestEveryRouteHasMetadata(t *testing.T) {
	n := testsupport.NewNode(t)
	s := &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Auth: auth.New(n.Svc.DB.Pool(), n.Svc.Sealer("x"))}
	reg := map[string]bool{}
	for _, p := range mgmt.RegisteredRoutes(s) {
		reg[p] = true
	}
	docs := mgmt.RouteDocs()
	for p := range reg {
		d, ok := docs[p]
		if !ok {
			t.Errorf("route %s has no entry in routes_meta.go (summary, tag, repository binding, types)", p)
			continue
		}
		if d[0] == "" || d[1] == "" {
			t.Errorf("route %s: summary and tag are required", p)
		}
		if _, ok := mgmt.Perms()[p]; !ok {
			t.Errorf("route %s has no permission", p)
		}
	}
	for p := range docs {
		if !reg[p] {
			t.Errorf("routes_meta.go documents %s, which is not registered", p)
		}
	}
	if len(reg) < 125 {
		t.Fatalf("only %d routes registered", len(reg))
	}
	// path-bound {name} must really be a repository
	for p, b := range mgmt.RouteBindings() {
		if b == "path" && !strings.Contains(p, "/repositories/{name}") {
			t.Errorf("%s is path-bound but {name} is not a repository", p)
		}
		if strings.Contains(p, "/repositories/{name}") && b != "path" {
			t.Errorf("%s: {name} is a repository but the binding is %s", p, b)
		}
	}
}

type scopeEnv struct {
	t    *testing.T
	n    *testsupport.Node
	s    *mgmt.Server
	ts   *httptest.Server
	auth *auth.Store
}

func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	n := testsupport.NewNode(t)
	st := auth.New(n.Svc.DB.Pool(), n.Svc.Sealer("test-totp"))
	s := &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Auth: st, Hub: mgmt.NewHub()}
	ts := httptest.NewServer(s.WebHandler())
	t.Cleanup(ts.Close)
	if _, err := n.Svc.CreateRepository(context.Background(), "second", filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatal(err)
	}
	return &scopeEnv{t: t, n: n, s: s, ts: ts, auth: st}
}

// user creates a user with a global role and (when scopes != nil) per-repository roles, signed in.
func (e *scopeEnv) user(name string, global auth.Role, scopes map[string]auth.Role) *web {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.auth.CreateUser(ctx, name, pw, global, "", "", false); err != nil {
		e.t.Fatal(err)
	}
	if scopes != nil {
		var rr []auth.RepoRole
		for r, role := range scopes {
			rr = append(rr, auth.RepoRole{Repository: r, Role: role})
		}
		if err := e.auth.SetUserAccess(ctx, name, true, rr); err != nil {
			e.t.Fatal(err)
		}
	}
	jar, _ := cookiejar.New(nil)
	w := &web{t: e.t, ts: e.ts, auth: e.auth, httpc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	w.login(name)
	return w
}

var placeholders = strings.NewReplacer("{scope}", "backup", "{action}", "enable", "{group}", "0123456789abcdef0123456789abcdef", "{snap}", "s",
	"{id}", "00000000-0000-0000-0000-000000000000", "{type}", "stale_backup")

// Matrix: a user with global role none and one role on "custody" may do on custody exactly what the role
// allows, and nothing at all on "second" or on global routes.
func TestScopedRoleMatrix(t *testing.T) {
	e := newScopeEnv(t)
	bind := mgmt.RouteBindings()
	for _, role := range []auth.Role{auth.ReadOnly, auth.Restore, auth.Operator, auth.Admin} {
		w := e.user("scoped-"+string(role), auth.None, map[string]auth.Role{"custody": role})
		for pat, perm := range mgmt.Perms() {
			b := bind[pat]
			if mgmt.PublicRoutes()[pat] || b == "self" || b == "job" || b == "alert" || pat == "GET /api/v1/events" {
				continue
			}
			method, path, _ := strings.Cut(pat, " ")
			path = placeholders.Replace(path)
			for _, repo := range []string{"custody", "second"} {
				p := strings.ReplaceAll(path, "{name}", map[bool]string{true: repo, false: "nobody"}[b == "path"])
				if b == "query" || b == "body" {
					p += "?repository=" + repo
				}
				code, body := w.req(method, p, nil, w.h())
				var want bool // forbidden?
				switch b {
				case "global":
					want = true // global role none
				case "list":
					want = !role.Allows(perm)
				default:
					want = repo == "second" || !role.Allows(perm)
				}
				if (code == 403) != want {
					t.Errorf("%s as %s on %s: %d %s, forbidden want %v", pat, role, repo, code, body, want)
				}
				if code == 500 && strings.Contains(string(body), "not declared repository-bound") {
					t.Errorf("%s: registered with withRepo but declared %s", pat, b)
				}
			}
		}
	}
}

// Every withRepo route must be declared bindQuery: as an unscoped admin no route may hit the fail-closed guard.
func TestNoUndeclaredRepositoryRoutes(t *testing.T) {
	e := newScopeEnv(t)
	w := e.user("root", auth.Admin, nil)
	for pat := range mgmt.Perms() {
		method, path, _ := strings.Cut(pat, " ")
		path = strings.ReplaceAll(placeholders.Replace(path), "{name}", "custody")
		if strings.Contains(pat, "/me/") || pat == "GET /api/v1/events" || pat == "DELETE /api/v1/session" || strings.HasPrefix(pat, "DELETE /api/v1/users/") {
			continue
		}
		code, body := w.req(method, path+"?repository=custody", nil, w.h())
		if code == 500 && strings.Contains(string(body), "not declared repository-bound") {
			t.Errorf("%s: %s", pat, body)
		}
	}
}

func names(t *testing.T, b []byte, key string) []string {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(b, &items); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var out []string
	for _, it := range items {
		out = append(out, it[key].(string))
	}
	sort.Strings(out)
	return out
}

func TestScopedListsJobsAlertsAndIDOR(t *testing.T) {
	e := newScopeEnv(t)
	ctx := context.Background()
	pool := e.n.Svc.DB.Pool()
	// one job in each repository
	jobIn := map[string]string{}
	for _, repo := range []string{"custody", "second"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO host_jobs (repository_id, host_name, type, params, created_by, created_at, queue_until, max_attempts)
			SELECT id, 'h1', 'backup_now', '{}', 'test', now(), now() + interval '1 hour', 1 FROM repositories WHERE name=$1 RETURNING id::text`, repo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		jobIn[repo] = id
	}
	// one alert in each repository and one global alert
	eng := mgmt.NewNotifyEngine(e.n.Svc)
	for _, repo := range []string{"custody", "second", ""} {
		if err := eng.Emit(ctx, notify.Event{Type: "job_failed", Key: "k-" + repo, Severity: "warning", Repository: repo, Title: "t " + repo}); err != nil {
			t.Fatal(err)
		}
	}
	var alertSecond int64
	if err := pool.QueryRow(ctx, `SELECT id FROM notify_alerts WHERE repository='second'`).Scan(&alertSecond); err != nil {
		t.Fatal(err)
	}

	w := e.user("ops", auth.None, map[string]auth.Role{"custody": auth.Operator})
	admin := e.user("root", auth.Admin, nil)

	code, b := w.req("GET", "/api/v1/repositories", nil, nil)
	if code != 200 || strings.Join(names(t, b, "name"), ",") != "custody" {
		t.Fatalf("scoped repositories: %d %s", code, b)
	}
	_, b = admin.req("GET", "/api/v1/repositories", nil, nil)
	if strings.Join(names(t, b, "name"), ",") != "custody,second" {
		t.Fatalf("admin repositories: %s", b)
	}
	// the overview contains only custody, also in the totals
	code, b = w.req("GET", "/api/v1/overview", nil, nil)
	var ov struct {
		Repositories []struct{ Name string } `json:"repositories"`
		Problems     []struct{ Repository string }
	}
	if code != 200 || json.Unmarshal(b, &ov) != nil || len(ov.Repositories) != 1 || ov.Repositories[0].Name != "custody" {
		t.Fatalf("scoped overview: %d %s", code, b)
	}
	for _, p := range ov.Problems {
		if p.Repository != "custody" {
			t.Fatalf("overview problem of %q leaked", p.Repository)
		}
	}
	// jobs: list filtered, other repository's ids are "not found", including with ?repository=second
	for _, q := range []string{"", "?repository=second", "?repository=custody"} {
		_, b = w.req("GET", "/api/v1/jobs"+q, nil, nil)
		for _, r := range names(t, b, "repository") {
			if r != "custody" {
				t.Fatalf("jobs%s leaked %s: %s", q, r, b)
			}
		}
	}
	if code, b := w.req("GET", "/api/v1/jobs/"+jobIn["second"], nil, nil); code != 404 {
		t.Fatalf("IDOR job of second: %d %s", code, b)
	}
	if code, b := w.req("POST", "/api/v1/jobs/"+jobIn["second"]+"/cancel", nil, w.h()); code != 404 {
		t.Fatalf("IDOR cancel job of second: %d %s", code, b)
	}
	if code, b := w.req("GET", "/api/v1/jobs/"+jobIn["custody"], nil, nil); code != 200 {
		t.Fatalf("own job: %d %s", code, b)
	}
	// creating a job in another repository through the body
	if code, b := w.req("POST", "/api/v1/jobs", map[string]any{"type": "backup_now", "host": "h1", "repository": "second", "params": map[string]any{}}, w.h()); code != 403 {
		t.Fatalf("job in second via body: %d %s", code, b)
	}
	if code, b := w.req("POST", "/api/v1/restore-tests/run", map[string]any{"host": "h1", "repository": "second", "project": "default", "object": "x"}, w.h()); code != 403 {
		t.Fatalf("restore test in second via body: %d %s", code, b)
	}
	// alerts: only custody (global alerts need a global role)
	_, b = w.req("GET", "/api/v1/alerts", nil, nil)
	if got := strings.Join(names(t, b, "repository"), ","); got != "custody" {
		t.Fatalf("scoped alerts: %q", got)
	}
	if code, b := w.req("GET", "/api/v1/alerts/"+itoa(alertSecond), nil, nil); code != 404 {
		t.Fatalf("IDOR alert of second: %d %s", code, b)
	}
	if code, b := w.req("POST", "/api/v1/alerts/"+itoa(alertSecond)+"/ack", map[string]string{"comment": "x"}, w.h()); code != 404 {
		t.Fatalf("IDOR ack alert of second: %d %s", code, b)
	}
	// raw material of another repository (restore role exfiltration)
	for _, p := range []string{"/api/v1/chunks/" + strings.Repeat("ab", 32) + "?repository=second", "/api/v1/snapshots/" + group + "/s/index?repository=second", "/api/v1/snapshots/" + group + "/s/manifest?repository=second"} {
		if code, b := w.req("GET", p, nil, nil); code != 403 {
			t.Fatalf("%s: %d %s", p, code, b)
		}
	}
	// chunks are stored per repository: a chunk of custody is "not found" through ?repository=second,
	// even for an administrator, and found through custody
	k := masterKeys()
	backup(t, e.n, k, "20261007120000-00000001", e.n.Now, "alpha-chunk")
	id, _, _ := k.EncodeChunk([]byte("alpha-chunk"), format.EncodeOpts{})
	if code, _ := admin.req("GET", "/api/v1/chunks/"+id.String()+"?repository=custody", nil, nil); code != 200 {
		t.Fatalf("own chunk: %d", code)
	}
	if code, _ := admin.req("GET", "/api/v1/chunks/"+id.String()+"?repository=second", nil, nil); code != 404 {
		t.Fatalf("custody chunk through second: %d", code)
	}
	// the default repository is resolved and checked too: with two repositories the caller must name one
	if code, _ := w.req("GET", "/api/v1/objects", nil, nil); code != 400 {
		t.Fatalf("objects without ?repository= with two repositories: %d", code)
	}
	// session shows the access
	_, b = w.req("GET", "/api/v1/session", nil, nil)
	if !strings.Contains(string(b), `"access":{"global":"none","repositories":{"custody":"operator"}}`) {
		t.Fatalf("session access: %s", b)
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }

func TestScopedTokens(t *testing.T) {
	e := newScopeEnv(t)
	ctx := context.Background()
	admin := e.user("root", auth.Admin, nil)
	// an admin's token restricted to custody with role operator: nothing global, operator on custody only
	code, b := admin.req("POST", "/api/v1/me/tokens", map[string]any{"name": "ci", "role": "operator", "repositories": []string{"custody"}}, admin.h())
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	var tok struct {
		Token string
		Info  auth.TokenInfo
	}
	_ = json.Unmarshal(b, &tok)
	if len(tok.Info.Repositories) != 1 || tok.Info.Repositories[0] != "custody" {
		t.Fatalf("token info: %s", b)
	}
	bearer := map[string]string{"Authorization": "Bearer " + tok.Token}
	tw := &web{t: t, ts: e.ts, httpc: &http.Client{}}
	check := func(method, path string, want int) {
		t.Helper()
		if code, b := tw.req(method, path, nil, bearer); code != want {
			t.Fatalf("token %s %s: %d %s, want %d", method, path, code, b, want)
		}
	}
	check("GET", "/api/v1/users", 403)                            // global admin route
	check("GET", "/api/v1/settings", 403)                         // global
	check("GET", "/api/v1/objects?repository=custody", 200)       // read on custody
	check("GET", "/api/v1/objects?repository=second", 403)        // not listed
	check("POST", "/api/v1/gc?repository=custody&dry_run=1", 403) // admin route, token is operator
	check("PUT", "/api/v1/repositories/custody/mode", 403)        // admin
	_, b = tw.req("GET", "/api/v1/repositories", nil, bearer)
	if strings.Join(names(t, b, "name"), ",") != "custody" {
		t.Fatalf("token repositories: %s", b)
	}
	// a token cannot widen: a scoped user (none + restore on custody) cannot mint an operator token,
	// and a restricted token naming "second" gets nothing there
	w := e.user("rest", auth.None, map[string]auth.Role{"custody": auth.Restore})
	if code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "x", "role": "operator"}, w.h()); code != 400 {
		t.Fatalf("operator token for a restore user: %d %s", code, b)
	}
	code, b = w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "y", "role": "restore", "repositories": []string{"custody", "second"}}, w.h())
	if code != 201 {
		t.Fatalf("restricted token: %d %s", code, b)
	}
	_ = json.Unmarshal(b, &tok)
	bearer = map[string]string{"Authorization": "Bearer " + tok.Token}
	check("GET", "/api/v1/chunks/"+strings.Repeat("ab", 32)+"?repository=second", 403)
	check("GET", "/api/v1/objects?repository=custody", 200)
	// the administrator's token overview shows what the restricted token can do at most (restore), not
	// the owner's global "none"
	_, b = admin.req("GET", "/api/v1/api-tokens", nil, nil)
	var all []auth.AdminTokenInfo
	_ = json.Unmarshal(b, &all)
	for _, x := range all {
		if x.Name == "y" && (x.EffectiveRole != auth.Restore || strings.Join(x.Repositories, ",") != "custody,second") {
			t.Fatalf("token overview: %+v", x)
		}
	}
	// deleting the only listed repository's rows must not widen the token (fail closed)
	if _, err := e.n.Svc.DB.Pool().Exec(ctx, `DELETE FROM api_token_repos`); err != nil {
		t.Fatal(err)
	}
	check("GET", "/api/v1/objects?repository=custody", 403)
	// a user whose scope rows vanished keeps NO repository access (flag, not "has rows")
	if _, err := e.n.Svc.DB.Pool().Exec(ctx, `DELETE FROM user_repo_scopes`); err != nil {
		t.Fatal(err)
	}
	if code, _ := w.req("GET", "/api/v1/objects?repository=custody", nil, nil); code != 403 {
		t.Fatalf("scoped user without rows: %d", code)
	}
	// access editor: admins cannot be scoped; unknown repositories and role none are refused
	if code, b := admin.req("PUT", "/api/v1/users/root/access", map[string]any{"scoped": true, "scopes": []any{}}, admin.h()); code != 400 {
		t.Fatalf("scoping an admin: %d %s", code, b)
	}
	if code, b := admin.req("PUT", "/api/v1/users/rest/access", map[string]any{"scoped": true, "scopes": []any{map[string]string{"repository": "nope", "role": "readonly"}}}, admin.h()); code != 400 {
		t.Fatalf("unknown repository: %d %s", code, b)
	}
	code, b = admin.req("PUT", "/api/v1/users/rest/access", map[string]any{"scoped": true, "scopes": []any{map[string]string{"repository": "second", "role": "readonly"}}}, admin.h())
	if code != 200 || !strings.Contains(string(b), `"repository":"second"`) {
		t.Fatalf("set access: %d %s", code, b)
	}
	if code, _ := w.req("GET", "/api/v1/objects?repository=second", nil, nil); code != 200 {
		t.Fatalf("after grant: %d", code)
	}
}

// Live events are filtered per subscriber: a scoped user only gets events of its repositories.
func TestScopedEventStream(t *testing.T) {
	e := newScopeEnv(t)
	w := e.user("viewer", auth.None, map[string]auth.Role{"custody": auth.ReadOnly})
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := w.httpc.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	if l, _ := rd.ReadString('\n'); !strings.HasPrefix(l, ": connected") {
		t.Fatalf("first line %q", l)
	}
	time.Sleep(100 * time.Millisecond) // subscribed
	e.s.Hub.Publish("job", map[string]string{"repository": "second", "job_id": "leak"})
	e.s.Hub.Publish("repository", map[string]string{"name": "second", "mode": "offline"})
	e.s.Hub.Publish("settings", map[string]string{"x": "global"}) // global: needs a global role
	e.s.Hub.Publish("job", map[string]string{"repository": "custody", "job_id": "mine"})
	for {
		l, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(l, "data:") {
			if !strings.Contains(l, "mine") {
				t.Fatalf("scoped subscriber got %s", l)
			}
			return
		}
	}
}
