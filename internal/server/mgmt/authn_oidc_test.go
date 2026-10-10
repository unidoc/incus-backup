package mgmt_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/oauth2-proxy/mockoidc"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

// Single sign-on end to end: the server talks to a REAL OpenID provider over HTTP
// (github.com/oauth2-proxy/mockoidc: discovery, JWKS with an RSA key, an authorize endpoint that
// approves at once, a token endpoint that checks the client secret and PKCE and signs ID tokens).
// The browser is a cookie jar following the redirects. Claims are changed per test through the
// provider's user (iss, aud, exp, nonce, email_verified, groups); a middleware breaks signatures or
// PKCE verifiers where a test needs it.

type idp struct {
	m *mockoidc.MockOIDC

	mu          sync.Mutex
	authQuery   url.Values // last authorize request
	tokenForm   url.Values // last token request
	breakSig    bool       // corrupt the next ID token's signature
	breakVerify bool       // replace the next code_verifier
}

func startIdP(t *testing.T) *idp {
	t.Helper()
	m, err := mockoidc.NewServer(nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{m: m}
	_ = m.AddMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			p.mu.Lock()
			defer p.mu.Unlock()
			switch r.URL.Path {
			case mockoidc.AuthorizationEndpoint:
				p.authQuery = r.Form
				if r.Form.Get("code_challenge_method") != "S256" || r.Form.Get("code_challenge") == "" || r.Form.Get("nonce") == "" {
					http.Error(w, "PKCE S256 and a nonce are required here", 400)
					return
				}
			case mockoidc.TokenEndpoint:
				if r.Form.Get("client_secret") == "" { // x/oauth2 tries HTTP basic first; mockoidc wants the form
					next.ServeHTTP(w, r)
					return
				}
				p.tokenForm = r.Form
				if p.breakVerify {
					p.breakVerify = false
					r.Form.Set("code_verifier", strings.Repeat("A", 43))
				}
				if p.breakSig {
					p.breakSig = false
					rec := httptest.NewRecorder()
					next.ServeHTTP(rec, r)
					var tr map[string]any
					_ = json.Unmarshal(rec.Body.Bytes(), &tr)
					tok := tr["id_token"].(string)
					parts := strings.Split(tok, ".")
					sig := []byte(parts[2])
					mid := len(sig) / 2
					if sig[mid] == 'A' { // change a character in the middle: every bit there is signature
						sig[mid] = 'B'
					} else {
						sig[mid] = 'A'
					}
					tr["id_token"] = parts[0] + "." + parts[1] + "." + string(sig)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(tr)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ln, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	return p
}

// person is who signs in at the identity provider next; edit changes the ID token's claims.
type person struct {
	*mockoidc.MockUser
	edit func(*mockoidc.IDTokenClaims)
}

func (u *person) Claims(scope []string, c *mockoidc.IDTokenClaims) (jwt.Claims, error) {
	if u.edit != nil {
		u.edit(c)
	}
	return u.MockUser.Claims(scope, c)
}

func (p *idp) next(sub, email string, verified bool, groups ...string) *person {
	u := &person{MockUser: &mockoidc.MockUser{Subject: sub, Email: email, EmailVerified: verified, PreferredUsername: strings.Split(email, "@")[0], Groups: groups}}
	p.m.QueueUser(u)
	return u
}

// provider registers the IdP as "corp" (through the admin API) and returns its input for changes.
func (e *authEnv) provider(admin *browser, p *idp, mod func(*auth.OIDCProviderInput)) auth.OIDCProviderInput {
	e.t.Helper()
	cfg := p.m.Config()
	in := auth.OIDCProviderInput{Name: "corp", DisplayName: "Corp SSO", Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		Scopes: []string{"openid", "email", "profile", "groups"}, DefaultRole: auth.ReadOnly, AutoCreate: true, Enabled: true}
	if mod != nil {
		mod(&in)
	}
	code, out := admin.do("POST", "/api/v1/oidc/providers", in)
	if code == 409 {
		code, out = admin.do("PUT", "/api/v1/oidc/providers/corp", in)
	}
	if code >= 300 {
		e.t.Fatalf("provider: %d %s", code, out)
	}
	return in
}

// sso walks the browser through the redirects: our login route, the identity provider, our
// callback. It returns where the callback sent the browser and the callback's Set-Cookie.
func (b *browser) sso(next string) (string, string) {
	b.e.t.Helper()
	b.e.s.ResetThrottleForTest()
	code, out, h := b.doH("GET", "/api/v1/auth/oidc/corp/login?next="+url.QueryEscape(next), nil, nil)
	if code != 302 {
		b.e.t.Fatalf("login: %d %s", code, out)
	}
	loc := h.Get("Location")
	if strings.HasPrefix(loc, "/login?") {
		return loc, ""
	}
	code, out, h = b.doH("GET", loc, nil, nil) // the identity provider
	if code != 302 {
		b.e.t.Fatalf("identity provider: %d %s", code, out)
	}
	cb := h.Get("Location")
	if !strings.HasPrefix(cb, b.e.base+"/api/v1/auth/oidc/corp/callback?") {
		b.e.t.Fatalf("redirect back to %s", cb)
	}
	code, out, h = b.doH("GET", cb, nil, nil)
	if code != 302 {
		b.e.t.Fatalf("callback: %d %s", code, out)
	}
	return h.Get("Location"), strings.Join(h.Values("Set-Cookie"), "\n")
}

func (b *browser) ssoOK(next string) {
	b.e.t.Helper()
	loc, _ := b.sso(next)
	if loc != next {
		b.e.t.Fatalf("single sign-on ended at %s, want %s", loc, next)
	}
	var sv struct{ CSRF string }
	_ = json.Unmarshal(b.must("GET", "/api/v1/session", nil, 200), &sv)
	b.csrf = sv.CSRF
}

func (b *browser) ssoFails(code string) {
	b.e.t.Helper()
	loc, _ := b.sso("/")
	if loc != "/login?sso_error="+code {
		b.e.t.Fatalf("single sign-on ended at %s, want error %s", loc, code)
	}
	if c, _ := b.do("GET", "/api/v1/session", nil); c != 401 {
		b.e.t.Fatalf("a failed single sign-on left a session: %d", c)
	}
}

type sessionAnswer struct {
	User   auth.User    `json:"user"`
	Access *auth.Access `json:"access"`
	Method string       `json:"method"`
	Enrol  bool         `json:"enrol_2fa_required"`
}

func (b *browser) session() sessionAnswer {
	var s sessionAnswer
	_ = json.Unmarshal(b.must("GET", "/api/v1/session", nil, 200), &s)
	return s
}

func TestOIDCSignInRolesAndScopes(t *testing.T) {
	e := newAuthEnv(t)
	if _, err := e.n.Svc.CreateRepository(context.Background(), "second", filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatal(err)
	}
	p := startIdP(t)
	e.user("root", auth.Admin)
	admin := e.browser()
	admin.passwordLogin("root")
	e.provider(admin, p, func(in *auth.OIDCProviderInput) {
		in.RoleMappings = []auth.RoleMapping{{Group: "backup-admins", Role: auth.Admin}}
		in.ScopeMappings = []auth.ScopeMapping{{Group: "team-a", Repository: "custody", Role: auth.Operator}}
	})
	// the sign-in page lists the provider (names only)
	opts := string(e.browser().must("GET", "/api/v1/auth/oidc/providers", nil, 200))
	if !strings.Contains(opts, `{"name":"corp","label":"Corp SSO"}`) || strings.Contains(opts, "issuer") || strings.Contains(opts, p.m.ClientID) {
		t.Fatalf("public provider list: %s", opts)
	}

	// 1. first sign-in creates the user with the default role, restricted to the mapped repository
	jane := e.browser()
	p.next("sub-jane", "jane@corp.example", true, "team-a")
	loc, cookies := jane.sso("/repos")
	if loc != "/repos" {
		t.Fatalf("ended at %s", loc)
	}
	if !strings.Contains(cookies, "ib_session=") || !strings.Contains(cookies, "HttpOnly") || !strings.Contains(cookies, "SameSite=Strict") {
		t.Fatalf("session cookie: %s", cookies)
	}
	s := jane.session()
	if s.User.Name != "jane" || s.User.Role != auth.ReadOnly || s.User.HasPassword || s.Method != "oidc" || len(s.User.Identities) != 1 {
		t.Fatalf("jane: %+v", s)
	}
	if s.Access == nil || s.Access.Global != auth.ReadOnly || len(s.Access.Repos) != 1 || s.Access.Repos["custody"] != auth.Operator {
		t.Fatalf("jane's access: %+v", s.Access)
	}
	jane.adopt(jane.must("GET", "/api/v1/session", nil, 200))
	// repository scopes hold everywhere: lists, the overview, raw chunks, the event stream
	if got := names(t, jane.must("GET", "/api/v1/repositories", nil, 200), "name"); strings.Join(got, ",") != "custody" {
		t.Fatalf("jane's repositories: %v", got)
	}
	var ov struct {
		Repositories []struct{ Name string } `json:"repositories"`
	}
	_ = json.Unmarshal(jane.must("GET", "/api/v1/overview", nil, 200), &ov)
	if len(ov.Repositories) != 1 || ov.Repositories[0].Name != "custody" {
		t.Fatalf("jane's overview: %+v", ov)
	}
	if code, _ := jane.do("GET", "/api/v1/chunks/"+strings.Repeat("ab", 32)+"?repository=second", nil); code != 403 {
		t.Fatalf("raw chunk of another repository: %d", code)
	}
	if code, _ := jane.do("GET", "/api/v1/snapshots/"+group+"/s/index?repository=second", nil); code != 403 {
		t.Fatalf("index of another repository: %d", code)
	}
	if code, _ := jane.do("GET", "/api/v1/users", nil); code != 403 {
		t.Fatalf("users as readonly: %d", code)
	}
	checkScopedEvents(t, e, jane)
	// no local password: the password route fails without counting, and cannot be set by the user
	e.s.ResetThrottleForTest()
	if code, _ := e.browser().do("POST", "/api/v1/session", map[string]string{"name": "jane", "password": ""}); code != 401 {
		t.Fatalf("password sign-in of an SSO user: %d", code)
	}
	if f := failedLogins(t, e, "jane"); f != 0 {
		t.Fatalf("failed logins of an SSO user: %d", f)
	}
	if code, out := jane.do("POST", "/api/v1/me/password", map[string]string{"current": "", "new": "a new long password"}); code != 400 || !strings.Contains(string(out), "single sign-on") {
		t.Fatalf("SSO user setting a password: %d %s", code, out)
	}
	if code, out := jane.do("POST", "/api/v1/me/webauthn/register/begin", map[string]any{}); code != 400 || !strings.Contains(string(out), "single sign-on") {
		t.Fatalf("SSO user registering a key: %d %s", code, out)
	}
	// require_2fa does not limit single sign-on sessions (the identity provider owns the second factor)
	if err := e.st.SetPolicy(context.Background(), auth.PolicyUpdate{Require2FA: ptr("all")}); err != nil {
		t.Fatal(err)
	}
	if jane.session().Enrol {
		t.Fatal("SSO session limited by require_2fa")
	}
	_ = e.st.SetPolicy(context.Background(), auth.PolicyUpdate{Require2FA: ptr("none")})

	// 2. the role is re-evaluated at every sign-in: now in backup-admins
	jane2 := e.browser()
	p.next("sub-jane", "jane@corp.example", true, "backup-admins")
	jane2.ssoOK("/")
	if s := jane2.session(); s.User.Role != auth.Admin || s.Access != nil {
		t.Fatalf("jane as backup-admin: %+v", s)
	}
	// 3. and again: no group at all: default role, restricted to nothing
	jane3 := e.browser()
	p.next("sub-jane", "jane@corp.example", true)
	jane3.ssoOK("/")
	if s := jane3.session(); s.User.Role != auth.ReadOnly || s.Access == nil || len(s.Access.Repos) != 0 {
		t.Fatalf("jane without groups: %+v", s)
	}
	if got := string(jane3.must("GET", "/api/v1/repositories", nil, 200)); got != "[]\n" {
		t.Fatalf("repositories without a group: %s", got)
	}
	// no second user was created; the audit log has the sign-ins
	us := string(admin.must("GET", "/api/v1/users", nil, 200))
	if strings.Count(us, `"identities":["corp"]`) != 1 {
		t.Fatalf("users: %s", us)
	}
	aud := string(admin.must("GET", "/api/v1/audit?action=login", nil, 200))
	if strings.Count(aud, `"provider": "corp"`)+strings.Count(aud, `"provider":"corp"`) < 3 {
		t.Fatalf("audit: %s", aud)
	}
}

// checkScopedEvents: jane (restricted to custody) receives custody's events only.
func checkScopedEvents(t *testing.T, e *authEnv, b *browser) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.base+"/api/v1/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := b.c.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	if l, _ := rd.ReadString('\n'); !strings.HasPrefix(l, ": connected") {
		t.Fatalf("first line %q", l)
	}
	time.Sleep(100 * time.Millisecond)
	e.s.Hub.Publish("job", map[string]string{"repository": "second", "job_id": "leak"})
	e.s.Hub.Publish("job", map[string]string{"repository": "custody", "job_id": "mine"})
	for {
		l, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(l, "data:") {
			if !strings.Contains(l, "mine") {
				t.Fatalf("SSO user got another repository's event: %s", l)
			}
			return
		}
	}
}

func TestOIDCRefusals(t *testing.T) {
	e := newAuthEnv(t)
	p := startIdP(t)
	e.user("root", auth.Admin)
	admin := e.browser()
	admin.passwordLogin("root")
	in := e.provider(admin, p, nil)

	// token problems: each is refused by go-oidc or the nonce check, and no session results
	cases := []struct {
		name string
		edit func(*mockoidc.IDTokenClaims)
	}{
		{"nonce", func(c *mockoidc.IDTokenClaims) { c.Nonce = "a-different-nonce" }},
		{"expired", func(c *mockoidc.IDTokenClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) }},
		{"audience", func(c *mockoidc.IDTokenClaims) { c.Audience = jwt.ClaimStrings{"another-client"} }},
		{"issuer", func(c *mockoidc.IDTokenClaims) { c.Issuer = "http://127.0.0.1:1/evil" }},
	}
	for _, c := range cases {
		u := p.next("sub-x", "x@corp.example", true)
		u.edit = c.edit
		e.browser().ssoFails("failed")
	}
	p.mu.Lock()
	p.breakSig = true
	p.mu.Unlock()
	p.next("sub-x", "x@corp.example", true)
	e.browser().ssoFails("failed")
	// e-mail not verified
	p.next("sub-x", "x@corp.example", false)
	e.browser().ssoFails("unverified")
	// domain not allowed
	in.AllowedDomains = []string{"example.org"}
	e.provider(admin, p, func(x *auth.OIDCProviderInput) { *x = in })
	p.next("sub-x", "x@corp.example", true)
	e.browser().ssoFails("domain")
	in.AllowedDomains = nil
	e.provider(admin, p, func(x *auth.OIDCProviderInput) { *x = in })
	// the identity provider refused (or the person cancelled)
	b := e.browser()
	e.s.ResetThrottleForTest()
	_, _, h := b.doH("GET", "/api/v1/auth/oidc/corp/login", nil, nil)
	au, _ := url.Parse(h.Get("Location"))
	state := au.Query().Get("state")
	_, _, h = b.doH("GET", "/api/v1/auth/oidc/corp/callback?error=access_denied&state="+url.QueryEscape(state), nil, nil)
	if h.Get("Location") != "/login?sso_error=denied" {
		t.Fatalf("idp error: %s", h.Get("Location"))
	}
	// state: wrong, replayed, or completed in another browser
	b = e.browser()
	_, _, h = b.doH("GET", "/api/v1/auth/oidc/corp/login", nil, nil)
	p.next("sub-y", "y@corp.example", true)
	_, _, h = b.doH("GET", h.Get("Location"), nil, nil)
	cb, _ := url.Parse(h.Get("Location"))
	q := cb.Query()
	q.Set("state", "forged")
	_, _, h2 := b.doH("GET", e.base+cb.Path+"?"+q.Encode(), nil, nil)
	if h2.Get("Location") != "/login?sso_error=state" {
		t.Fatalf("forged state: %s", h2.Get("Location"))
	}
	other := e.browser() // the victim's browser gets the attacker's callback URL (login CSRF)
	_, _, h2 = other.doH("GET", cb.String(), nil, nil)
	if h2.Get("Location") != "/login?sso_error=state" {
		t.Fatalf("callback in another browser: %s", h2.Get("Location"))
	}
	_, _, h2 = b.doH("GET", cb.String(), nil, nil) // the state was consumed by the attempt above
	if h2.Get("Location") != "/login?sso_error=state" {
		t.Fatalf("replayed state: %s", h2.Get("Location"))
	}
	// automatic creation off: an unknown subject has no account
	in.AutoCreate = false
	e.provider(admin, p, func(x *auth.OIDCProviderInput) { *x = in })
	p.next("sub-new", "new@corp.example", true)
	e.browser().ssoFails("no_account")
	if code, _ := admin.do("GET", "/api/v1/users/new/identities", nil); code != 200 {
		t.Fatal("identities route")
	}
	var users []auth.User
	_ = json.Unmarshal(admin.must("GET", "/api/v1/users", nil, 200), &users)
	if len(users) != 1 {
		t.Fatalf("users were created: %+v", users)
	}
	// an administrator's explicit link by e-mail: the first verified sign-in binds the subject
	e.user("bob", auth.Operator)
	admin.must("POST", "/api/v1/users/bob/identities", map[string]string{"provider": "corp", "email": "Bob@Corp.example"}, 201)
	bob := e.browser()
	p.next("sub-bob", "bob@corp.example", true)
	bob.ssoOK("/")
	if s := bob.session(); s.User.Name != "bob" || s.User.Role != auth.ReadOnly {
		t.Fatalf("bob: %+v", s) // the provider's mapping is authoritative: operator -> readonly (default role)
	}
	// account takeover by e-mail: another subject with bob's (verified) address does NOT become bob
	p.next("sub-mallory", "bob@corp.example", true)
	e.browser().ssoFails("no_account")
	in.AutoCreate = true
	e.provider(admin, p, func(x *auth.OIDCProviderInput) { *x = in })
	p.next("sub-mallory", "bob@corp.example", true)
	e.browser().ssoFails("exists") // the name "bob" is taken: never linked by name either
	// a disabled user cannot sign in
	admin.must("PUT", "/api/v1/users/bob", map[string]any{"disabled": true}, 204)
	p.next("sub-bob", "bob@corp.example", true)
	e.browser().ssoFails("disabled")
	admin.must("PUT", "/api/v1/users/bob", map[string]any{"disabled": false}, 204)
	// a disabled provider is not offered and cannot be used
	in.Enabled = false
	e.provider(admin, p, func(x *auth.OIDCProviderInput) { *x = in })
	if strings.Contains(string(e.browser().must("GET", "/api/v1/auth/oidc/providers", nil, 200)), "corp") {
		t.Fatal("disabled provider listed")
	}
	if loc, _ := e.browser().sso("/"); loc != "/login?sso_error=provider" {
		t.Fatalf("disabled provider: %s", loc)
	}
	aud := string(admin.must("GET", "/api/v1/audit?action=login&limit=500", nil, 200))
	for _, want := range []string{"unverified", "no_account", "exists", "disabled", "nonce", "state"} {
		if !strings.Contains(aud, want) {
			t.Errorf("audit lacks a failed sign-in with %q", want)
		}
	}
}

func TestOIDCUsesPKCE(t *testing.T) {
	e := newAuthEnv(t)
	p := startIdP(t)
	e.user("root", auth.Admin)
	admin := e.browser()
	admin.passwordLogin("root")
	e.provider(admin, p, nil)
	p.next("sub-a", "a@corp.example", true)
	e.browser().ssoOK("/")
	p.mu.Lock()
	challenge, verifier := p.authQuery.Get("code_challenge"), p.tokenForm.Get("code_verifier")
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(verifier))
	if verifier == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		t.Fatalf("PKCE: challenge %q, verifier %q", challenge, verifier)
	}
	// the token endpoint refuses a wrong verifier: the code alone is worthless
	p.mu.Lock()
	p.breakVerify = true
	p.mu.Unlock()
	p.next("sub-a", "a@corp.example", true)
	e.browser().ssoFails("failed")
}

func TestOIDCClientSecretIsNeverReturned(t *testing.T) {
	e := newAuthEnv(t)
	p := startIdP(t)
	e.user("root", auth.Admin)
	admin := e.browser()
	admin.passwordLogin("root")
	secret := p.m.ClientSecret
	in := e.provider(admin, p, nil)
	var bodies [][]byte
	get := func(path string) {
		_, out := admin.do("GET", path, nil)
		bodies = append(bodies, out)
	}
	get("/api/v1/oidc/providers")
	in.ClientSecret = "" // keeps the stored one
	_, out := admin.do("PUT", "/api/v1/oidc/providers/corp", in)
	bodies = append(bodies, out)
	_, out = admin.do("POST", "/api/v1/oidc/providers/corp/test", nil)
	bodies = append(bodies, out)
	var rep auth.DiscoveryReport
	_ = json.Unmarshal(out, &rep)
	if !rep.OK || rep.JWKSKeys < 1 || rep.Issuer != p.m.Issuer() || !strings.HasSuffix(rep.RedirectURI, "/api/v1/auth/oidc/corp/callback") {
		t.Fatalf("discovery test: %s", out)
	}
	for _, path := range []string{"/api/v1/credentials", "/api/v1/audit?limit=500", "/api/v1/auth/settings", "/api/v1/openapi.json", "/api/v1/settings", "/api/v1/auth/oidc/providers", "/api/v1/users"} {
		get(path)
	}
	p.next("sub-a", "a@corp.example", true)
	e.browser().ssoOK("/") // the stored secret still works after the PUT without one
	for i, b := range bodies {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("response %d contains the client secret: %s", i, b)
		}
	}
	var providers []auth.OIDCProvider
	_ = json.Unmarshal(bodies[0], &providers)
	if len(providers) != 1 || !providers[0].HasSecret {
		t.Fatalf("providers: %s", bodies[0])
	}
	// stored sealed, never in clear text
	var sealed []byte
	var kind string
	if err := e.n.Svc.DB.Pool().QueryRow(context.Background(), `SELECT kind, sealed FROM credentials WHERE name LIKE 'oidc-corp-%'`).Scan(&kind, &sealed); err != nil {
		t.Fatal(err)
	}
	if kind != "oidc" || bytes.Contains(sealed, []byte(secret)) {
		t.Fatalf("secret not sealed: kind %s", kind)
	}
	// validation: never admin by default, https issuers
	bad := in
	bad.Name, bad.DefaultRole = "other", auth.Admin
	if code, out := admin.do("POST", "/api/v1/oidc/providers", bad); code != 400 || !strings.Contains(string(out), "cannot be admin") {
		t.Fatalf("default role admin: %d %s", code, out)
	}
	bad.DefaultRole, bad.Issuer = auth.ReadOnly, "http://id.example.com"
	if code, _ := admin.do("POST", "/api/v1/oidc/providers", bad); code != 400 {
		t.Fatal("plain-http issuer accepted")
	}
	// clearing the secret makes it a public client (this provider then refuses the code exchange); setting
	// it again stores a new sealed secret that works
	clear := in
	clear.ClearSecret = true
	var pv auth.OIDCProvider
	_ = json.Unmarshal(admin.must("PUT", "/api/v1/oidc/providers/corp", clear, 200), &pv)
	if pv.HasSecret {
		t.Fatal("secret not cleared")
	}
	p.next("sub-a", "a@corp.example", true)
	e.browser().ssoFails("failed")
	again := in
	again.ClientSecret = secret
	_ = json.Unmarshal(admin.must("PUT", "/api/v1/oidc/providers/corp", again, 200), &pv)
	if !pv.HasSecret {
		t.Fatal("secret not stored again")
	}
	p.next("sub-a", "a@corp.example", true)
	e.browser().ssoOK("/")
	// deleting the provider deletes its secret
	admin.must("DELETE", "/api/v1/oidc/providers/corp", nil, 204)
	var n int
	_ = e.n.Svc.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM credentials WHERE kind='oidc'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d oidc secrets left", n)
	}
}

func TestLocalLoginLockoutGuard(t *testing.T) {
	e := newAuthEnv(t)
	p := startIdP(t)
	e.user("root", auth.Admin)
	admin := e.browser()
	admin.passwordLogin("root")
	in := e.provider(admin, p, func(in *auth.OIDCProviderInput) {
		in.RoleMappings = []auth.RoleMapping{{Group: "backup-admins", Role: auth.Admin}}
	})
	// nobody could administer the server through single sign-on yet: refused
	code, out := admin.do("PUT", "/api/v1/auth/settings", map[string]any{"local_login": false})
	if code != 400 || !strings.Contains(string(out), "no enabled administrator") {
		t.Fatalf("local_login off without an SSO admin: %d %s", code, out)
	}
	// an administrator signs in through the provider: now allowed
	p.next("sub-ann", "ann@corp.example", true, "backup-admins")
	sso := e.browser()
	sso.ssoOK("/")
	var st struct {
		SSOAdmins  []string `json:"sso_admins"`
		LocalLogin bool     `json:"local_login"`
	}
	_ = json.Unmarshal(admin.must("PUT", "/api/v1/auth/settings", map[string]any{"local_login": false}, 200), &st)
	if st.LocalLogin || len(st.SSOAdmins) != 1 || st.SSOAdmins[0] != "ann" {
		t.Fatalf("settings: %+v", st)
	}
	// password sign-in is refused (also for the administrator); sessions already open keep working
	e.s.ResetThrottleForTest()
	if code, out := e.browser().do("POST", "/api/v1/session", map[string]string{"name": "root", "password": pw}); code != 403 || !strings.Contains(string(out), "single sign-on") {
		t.Fatalf("password sign-in with local_login off: %d %s", code, out)
	}
	sso.must("GET", "/api/v1/users", nil, 200)
	// what would lock everyone out is refused: disabling or deleting the provider, unlinking the last SSO admin
	in.Enabled = false
	if code, out := sso.do("PUT", "/api/v1/oidc/providers/corp", in); code != 400 {
		t.Fatalf("disabling the only provider: %d %s", code, out)
	}
	if code, _ := sso.do("DELETE", "/api/v1/oidc/providers/corp", nil); code != 400 {
		t.Fatal("deleting the only provider")
	}
	if code, _ := sso.do("DELETE", "/api/v1/users/ann/identities/corp", nil); code != 400 {
		t.Fatal("unlinking the only SSO administrator")
	}
	// a refused update changes nothing, not even the secret: the provider still signs in with the old one
	in.Enabled, in.ClientSecret = false, "a-new-secret-that-must-not-be-stored"
	if code, _ := sso.do("PUT", "/api/v1/oidc/providers/corp", in); code != 400 {
		t.Fatal("refused update")
	}
	var nSecrets int
	_ = e.n.Svc.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM credentials WHERE kind='oidc'`).Scan(&nSecrets)
	if nSecrets != 1 {
		t.Fatalf("%d oidc secrets after a refused update", nSecrets)
	}
	p.next("sub-ann", "ann@corp.example", true, "backup-admins")
	e.browser().ssoOK("/")
	// break-glass: the server CLI works on the database directly (user local-login on)
	on := true
	if err := e.st.SetPolicy(context.Background(), auth.PolicyUpdate{LocalLogin: &on}); err != nil {
		t.Fatal(err)
	}
	e.browser().passwordLogin("root")
}

// The admin socket (local-admin, implicit administrator) has the same routes, without CSRF or sign-in.
func TestAuthSettingsOverTheSocket(t *testing.T) {
	e := newAuthEnv(t)
	ts := httptest.NewServer(e.s.LocalHandler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/api/v1/auth/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &v)
	if resp.StatusCode != 200 || v["local_login"] != true || v["require_2fa"] != "none" || v["webauthn_rp_id_effective"] != "localhost" {
		t.Fatalf("auth settings: %d %s", resp.StatusCode, b)
	}
}
