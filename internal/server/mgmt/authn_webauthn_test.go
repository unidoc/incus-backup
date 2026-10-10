package mgmt_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// WebAuthn end to end through the real HTTP routes, real PostgreSQL and go-webauthn, with a software
// authenticator (github.com/descope/virtualwebauthn: real ECDSA P-256 keys, packed attestation, real
// signatures over the server's challenges). Nothing of the protocol is mocked on the server side.

type authEnv struct {
	t      *testing.T
	n      *testsupport.Node
	s      *mgmt.Server
	st     *auth.Store
	ts     *httptest.Server
	base   string // http://localhost:PORT (an RP ID must be a domain name, not 127.0.0.1)
	offset atomic.Int64
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	n := testsupport.NewNode(t)
	st := auth.New(n.Svc.DB.Pool(), n.Svc.Sealer("test-totp"))
	e := &authEnv{t: t, n: n, st: st}
	st.Now = func() time.Time { return time.Now().Add(time.Duration(e.offset.Load())) }
	e.s = &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}, Auth: st, Hub: mgmt.NewHub()}
	e.ts = httptest.NewServer(e.s.WebHandler())
	t.Cleanup(e.ts.Close)
	t.Cleanup(e.s.Close)
	u, _ := url.Parse(e.ts.URL)
	e.base = "http://localhost:" + u.Port()
	if err := n.Svc.SetSettings(context.Background(), map[string]string{"base_url": e.base}); err != nil {
		t.Fatal(err)
	}
	return e
}

// advance moves the auth store's clock.
func (e *authEnv) advance(d time.Duration) { e.offset.Add(int64(d)) }

func (e *authEnv) user(name string, role auth.Role) {
	e.t.Helper()
	if _, err := e.st.CreateUser(context.Background(), name, pw, role, "", "", false); err != nil {
		e.t.Fatal(err)
	}
}

// browser is one cookie jar talking to the server like a same-origin page.
type browser struct {
	e    *authEnv
	jar  *cookiejar.Jar
	c    *http.Client
	csrf string
}

func (e *authEnv) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, jar: jar, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(method, path string, body any) (int, []byte) {
	b.e.t.Helper()
	code, out, _ := b.doH(method, path, body, nil)
	return code, out
}

func (b *browser) doH(method, path string, body any, hdr map[string]string) (int, []byte, http.Header) {
	b.e.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	u := path
	if strings.HasPrefix(path, "/") {
		u = b.e.base + path
	}
	r, _ := http.NewRequest(method, u, rd)
	if method != "GET" {
		r.Header.Set("Origin", b.e.base)
		r.Header.Set("Content-Type", "application/json")
		if b.csrf != "" {
			r.Header.Set("X-CSRF-Token", b.csrf)
		}
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := b.c.Do(r)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp.Header
}

func (b *browser) must(method, path string, body any, want int) []byte {
	b.e.t.Helper()
	code, out := b.do(method, path, body)
	if code != want {
		b.e.t.Fatalf("%s %s: %d %s (want %d)", method, path, code, out, want)
	}
	return out
}

func (b *browser) adopt(out []byte) {
	var v struct{ CSRF string }
	if err := json.Unmarshal(out, &v); err != nil || v.CSRF == "" {
		b.e.t.Fatalf("no session answer: %s", out)
	}
	b.csrf = v.CSRF
}

// passwordLogin signs in with the password (no second factor) and returns the session answer.
func (b *browser) passwordLogin(name string) {
	b.e.t.Helper()
	b.adopt(b.must("POST", "/api/v1/session", map[string]string{"name": name, "password": pw}, 200))
}

// secondFactorTicket: the password is right and a second factor is needed.
func (b *browser) secondFactorTicket(name string) (string, map[string]bool) {
	b.e.t.Helper()
	code, out := b.do("POST", "/api/v1/session", map[string]string{"name": name, "password": pw})
	var v struct {
		Ticket       string          `json:"ticket"`
		TOTPRequired bool            `json:"totp_required"`
		SecondFactor map[string]bool `json:"second_factor"`
	}
	if code != 401 || json.Unmarshal(out, &v) != nil || v.Ticket == "" || !v.TOTPRequired {
		b.e.t.Fatalf("password of a user with a key: %d %s", code, out)
	}
	return v.Ticket, v.SecondFactor
}

// key is a software authenticator with one credential.
type key struct {
	rp   virtualwebauthn.RelyingParty
	a    virtualwebauthn.Authenticator
	cred virtualwebauthn.Credential
	id   string // server's id of the registered credential
}

func (e *authEnv) newKey() *key {
	return &key{
		rp:   virtualwebauthn.RelyingParty{ID: "localhost", Name: "incus-backup", Origin: e.base},
		a:    virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{Transports: []virtualwebauthn.Transport{virtualwebauthn.TransportUSB}}),
		cred: virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2),
	}
}

type ceremony struct {
	Ceremony string          `json:"ceremony"`
	Options  json.RawMessage `json:"options"`
}

// register runs the whole registration through the routes (the browser must be signed in).
func (b *browser) register(k *key, name string) {
	b.e.t.Helper()
	var c ceremony
	_ = json.Unmarshal(b.must("POST", "/api/v1/me/webauthn/register/begin", map[string]any{}, 200), &c)
	opts, err := virtualwebauthn.ParseAttestationOptions(string(c.Options))
	if err != nil {
		b.e.t.Fatalf("%v: %s", err, c.Options)
	}
	resp := virtualwebauthn.CreateAttestationResponse(k.rp, k.a, k.cred, *opts)
	var got auth.WebAuthnKey
	_ = json.Unmarshal(b.must("POST", "/api/v1/me/webauthn/register/finish", map[string]any{"ceremony": c.Ceremony, "name": name, "credential": json.RawMessage(resp)}, 201), &got)
	if got.Name != name || got.ID == "" {
		b.e.t.Fatalf("registered key: %+v", got)
	}
	k.id = got.ID
	k.a.Options.UserHandle = []byte(opts.UserID) // the authenticator stores the user handle (discoverable)
	k.a.AddCredential(k.cred)
}

// assert answers a begin with the key (counter advanced like a real authenticator).
func (k *key) assert(t *testing.T, options json.RawMessage) string {
	t.Helper()
	opts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("%v: %s", err, options)
	}
	k.cred.Counter++
	return virtualwebauthn.CreateAssertionResponse(k.rp, k.a, k.cred, *opts)
}

func (b *browser) begin(body any) ceremony {
	b.e.t.Helper()
	var c ceremony
	_ = json.Unmarshal(b.must("POST", "/api/v1/session/webauthn/begin", body, 200), &c)
	return c
}

func (b *browser) finish(c ceremony, resp string) (int, []byte) {
	b.e.t.Helper()
	return b.do("POST", "/api/v1/session/webauthn/finish", map[string]any{"ceremony": c.Ceremony, "credential": json.RawMessage(resp)})
}

// keyLogin: password, then the key as the second factor.
func (b *browser) keyLogin(name string, k *key) {
	b.e.t.Helper()
	ticket, _ := b.secondFactorTicket(name)
	c := b.begin(map[string]string{"ticket": ticket})
	code, out := b.finish(c, k.assert(b.e.t, c.Options))
	if code != 200 {
		b.e.t.Fatalf("second factor: %d %s", code, out)
	}
	b.adopt(out)
}

func failedLogins(t *testing.T, e *authEnv, name string) int {
	var n int
	if err := e.n.Svc.DB.Pool().QueryRow(context.Background(), `SELECT failed_logins FROM users WHERE name=$1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWebAuthnRegistrationAndSecondFactor(t *testing.T) {
	e := newAuthEnv(t)
	e.user("anna", auth.Operator)
	b := e.browser()
	b.passwordLogin("anna")
	k := e.newKey()
	b.register(k, "YubiKey 5")
	var keys []auth.WebAuthnKey
	_ = json.Unmarshal(b.must("GET", "/api/v1/me/webauthn", nil, 200), &keys)
	if len(keys) != 1 || keys[0].Name != "YubiKey 5" || keys[0].SignCount != 0 || len(keys[0].Transports) != 1 {
		t.Fatalf("keys: %+v", keys)
	}
	// rename
	b.must("PATCH", "/api/v1/me/webauthn/"+k.id, map[string]string{"name": "Desk key"}, 204)

	// the password alone opens no session any more: a ticket for the second factor, no cookie
	n := e.browser()
	ticket, methods := n.secondFactorTicket("anna")
	if !methods["webauthn"] || methods["totp"] {
		t.Fatalf("methods %v", methods)
	}
	if code, _ := n.do("GET", "/api/v1/session", nil); code != 401 {
		t.Fatalf("a session after the password alone: %d", code)
	}
	// a TOTP-looking code cannot stand in for the key (and counts as a failure)
	if code, out := n.do("POST", "/api/v1/session", map[string]string{"name": "anna", "password": pw, "code": "123456"}); code != 401 || strings.Contains(string(out), "ticket") {
		t.Fatalf("code instead of key: %d %s", code, out)
	}
	if f := failedLogins(t, e, "anna"); f != 1 {
		t.Fatalf("failed logins %d", f)
	}
	// the key: a real session, the same shape as a password sign-in
	c := n.begin(map[string]string{"ticket": ticket})
	if !strings.Contains(string(c.Options), `"allowCredentials"`) || !strings.Contains(string(c.Options), `"rpId":"localhost"`) {
		t.Fatalf("second-factor options: %s", c.Options)
	}
	code, out := n.finish(c, k.assert(t, c.Options))
	if code != 200 {
		t.Fatalf("finish: %d %s", code, out)
	}
	var sv struct {
		CSRF   string    `json:"csrf"`
		User   auth.User `json:"user"`
		Method string    `json:"method"`
	}
	_ = json.Unmarshal(out, &sv)
	if sv.CSRF == "" || sv.User.Name != "anna" || sv.User.WebAuthnKeys != 1 || sv.Method != "password" {
		t.Fatalf("session view %s", out)
	}
	n.csrf = sv.CSRF
	n.must("GET", "/api/v1/repositories", nil, 200)
	if f := failedLogins(t, e, "anna"); f != 0 {
		t.Fatalf("failed logins not reset: %d", f)
	}
	// the cookie is the ordinary session cookie (HttpOnly, SameSite=Strict)
	_, _, hdr := e.browser().doH("POST", "/api/v1/session", map[string]string{"name": "anna", "password": pw}, nil)
	if hdr.Get("Set-Cookie") != "" {
		t.Fatal("a cookie was set before the second factor")
	}
	// state of the key: counter and last use recorded
	_ = json.Unmarshal(n.must("GET", "/api/v1/me/webauthn", nil, 200), &keys)
	if keys[0].Name != "Desk key" || keys[0].SignCount != 1 || keys[0].LastUsed == nil {
		t.Fatalf("after use: %+v", keys[0])
	}
	// audit: registration, rename and the sign-in by key
	admin := e.browser()
	e.user("root", auth.Admin)
	admin.passwordLogin("root")
	aud := string(admin.must("GET", "/api/v1/audit?limit=100", nil, 200))
	for _, want := range []string{`"action":"post /me/webauthn/register/finish"`, `"action":"patch /me/webauthn/{id}"`, `"method": "webauthn"`, `"actor":"anna"`} {
		if !strings.Contains(strings.ReplaceAll(aud, `"method":"webauthn"`, `"method": "webauthn"`), want) {
			t.Errorf("audit lacks %s:\n%s", want, aud)
		}
	}
}

func TestWebAuthnTicketIsSingleUse(t *testing.T) {
	e := newAuthEnv(t)
	e.user("anna", auth.Operator)
	b := e.browser()
	b.passwordLogin("anna")
	k := e.newKey()
	b.register(k, "k")
	n := e.browser()
	ticket, _ := n.secondFactorTicket("anna")
	c := n.begin(map[string]string{"ticket": ticket})
	if code, out := n.finish(c, k.assert(t, c.Options)); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	// the same ticket again: refused at begin
	if code, out := e.browser().do("POST", "/api/v1/session/webauthn/begin", map[string]string{"ticket": ticket}); code != 401 {
		t.Fatalf("spent ticket: %d %s", code, out)
	}
	// a made-up ticket names nobody
	if code, _ := e.browser().do("POST", "/api/v1/session/webauthn/begin", map[string]string{"ticket": "nope"}); code != 401 {
		t.Fatalf("unknown ticket: %d", code)
	}
}

func TestWebAuthnRefusals(t *testing.T) {
	e := newAuthEnv(t)
	e.user("anna", auth.Operator)
	e.user("bert", auth.Operator)
	ka, kb := e.newKey(), e.newKey()
	for name, k := range map[string]*key{"anna": ka, "bert": kb} {
		b := e.browser()
		b.passwordLogin(name)
		b.register(k, name+"'s key")
	}
	try := func(name string, mutate func(k *key) *key, k *key) (int, string) {
		t.Helper()
		e.s.ResetThrottleForTest()
		n := e.browser()
		ticket, _ := n.secondFactorTicket(name)
		c := n.begin(map[string]string{"ticket": ticket})
		use := k
		if mutate != nil {
			use = mutate(k)
		}
		code, out := n.finish(c, use.assert(t, c.Options))
		return code, string(out)
	}
	// wrong origin (a phishing page relaying the challenge)
	code, out := try("anna", func(k *key) *key { x := *k; x.rp.Origin = "http://localhost.evil.example:1"; return &x }, ka)
	if code != 401 || !strings.Contains(out, "not accepted") {
		t.Fatalf("wrong origin: %d %s", code, out)
	}
	// wrong relying party id (the authenticator signed for another site)
	code, out = try("anna", func(k *key) *key { x := *k; x.rp.ID = "evil.example"; return &x }, ka)
	if code != 401 {
		t.Fatalf("wrong RP ID: %d %s", code, out)
	}
	// cross-user: bert's ticket, anna's key
	before := failedLogins(t, e, "bert")
	if code, out = try("bert", nil, ka); code != 401 {
		t.Fatalf("anna's key for bert: %d %s", code, out)
	}
	if failedLogins(t, e, "bert") != before+1 {
		t.Fatal("a wrong key does not count as a failed sign-in")
	}
	// a replayed challenge: the same ceremony twice
	e.s.ResetThrottleForTest()
	n := e.browser()
	ticket, _ := n.secondFactorTicket("anna")
	c := n.begin(map[string]string{"ticket": ticket})
	resp := ka.assert(t, c.Options)
	if code, out := n.finish(c, resp); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := e.browser().finish(c, resp); code != 400 || !strings.Contains(string(out), "already used") {
		t.Fatalf("replayed ceremony: %d %s", code, out)
	}
	// the same signed answer under a fresh ceremony: the challenge does not match
	n2 := e.browser()
	t2, _ := n2.secondFactorTicket("anna")
	c2 := n2.begin(map[string]string{"ticket": t2})
	if code, out := n2.finish(c2, resp); code != 401 {
		t.Fatalf("replayed assertion: %d %s", code, out)
	}
	// expired ceremony state (5 minutes)
	e.s.ResetThrottleForTest()
	n3 := e.browser()
	t3, _ := n3.secondFactorTicket("anna")
	c3 := n3.begin(map[string]string{"ticket": t3})
	e.advance(6 * time.Minute)
	if code, out := n3.finish(c3, ka.assert(t, c3.Options)); code != 400 || !strings.Contains(string(out), "expired") {
		t.Fatalf("expired ceremony: %d %s", code, out)
	}
	e.advance(-6 * time.Minute)
	// counter regression: a cloned key replays an old counter. go-webauthn only flags it; the server refuses
	// and blocks the key from then on, also for answers with a higher counter.
	e.s.ResetThrottleForTest()
	ka.cred.Counter -= 3
	code, out = try("anna", nil, ka)
	if code != 401 || !strings.Contains(out, "may have been copied") {
		t.Fatalf("counter regression: %d %s", code, out)
	}
	ka.cred.Counter += 100
	if code, out = try("anna", nil, ka); code != 401 || !strings.Contains(out, "copied") {
		t.Fatalf("cloned key used again: %d %s", code, out)
	}
	// every refused assertion since the last success counted against anna (the growing lock of lockFor)
	if f := failedLogins(t, e, "anna"); f != 3 {
		t.Fatalf("failed logins after three refused assertions: %d", f)
	}
	admin := e.browser()
	e.user("root", auth.Admin)
	admin.passwordLogin("root")
	var users []auth.User
	_ = json.Unmarshal(admin.must("GET", "/api/v1/users", nil, 200), &users)
	for _, u := range users {
		if u.Name == "anna" && (u.WebAuthnKeys != 1 || !u.HasPassword) {
			t.Fatalf("users list: %+v", u)
		}
	}
	keys, _ := e.st.WebAuthnKeys(context.Background(), "anna")
	if len(keys) != 1 || !keys[0].CloneWarning {
		t.Fatalf("clone warning not recorded: %+v", keys)
	}
	// an administrator's reset removes the keys (lost or cloned key); the user signs in with the password again
	var rm struct{ Removed int }
	_ = json.Unmarshal(admin.must("DELETE", "/api/v1/users/anna/webauthn", nil, 200), &rm)
	if rm.Removed != 1 {
		t.Fatalf("reset removed %d", rm.Removed)
	}
	e.s.ResetThrottleForTest()
	e.browser().passwordLogin("anna")
	if !strings.Contains(string(admin.must("GET", "/api/v1/audit?action=delete%20/users", nil, 200)), `/api/v1/users/anna/webauthn`) {
		t.Fatal("reset not audited")
	}
}

func TestPasskeySignIn(t *testing.T) {
	e := newAuthEnv(t)
	e.user("anna", auth.Restore)
	b := e.browser()
	b.passwordLogin("anna")
	k := e.newKey()
	b.register(k, "Phone")

	// begin names no user and allows any credential: the same answer for everybody (no enumeration)
	n := e.browser()
	c := n.begin(map[string]any{})
	if strings.Contains(string(c.Options), "allowCredentials") || strings.Contains(string(c.Options), "anna") || !strings.Contains(string(c.Options), `"userVerification":"required"`) {
		t.Fatalf("passkey options: %s", c.Options)
	}
	code, out := n.finish(c, k.assert(t, c.Options))
	if code != 200 {
		t.Fatalf("passkey: %d %s", code, out)
	}
	n.adopt(out)
	var sv struct {
		Method string    `json:"method"`
		User   auth.User `json:"user"`
	}
	_ = json.Unmarshal(n.must("GET", "/api/v1/session", nil, 200), &sv)
	if sv.Method != "passkey" || sv.User.Name != "anna" {
		t.Fatalf("passkey session: %+v", sv)
	}
	// the role is the user's role: restore may not run verify
	if code, _ := n.do("POST", "/api/v1/verify", map[string]any{"mode": "new"}); code != 403 {
		t.Fatalf("restore role via passkey ran verify: %d", code)
	}
	// user verification is required for a passkey sign-in
	k.a.Options.UserNotVerified = true
	c = e.browser().begin(map[string]any{})
	if code, out := e.browser().finish(c, k.assert(t, c.Options)); code != 401 {
		t.Fatalf("passkey without user verification: %d %s", code, out)
	}
	k.a.Options.UserNotVerified = false
	// an unknown user handle: refused, nobody's counter changes
	other := e.newKey()
	other.a.Options.UserHandle = []byte("not-a-user-handle-of-this-server")
	other.a.AddCredential(other.cred)
	e.s.ResetThrottleForTest()
	c = e.browser().begin(map[string]any{})
	if code, _ := e.browser().finish(c, other.assert(t, c.Options)); code != 401 {
		t.Fatalf("unknown passkey: %d", code)
	}
	// disabled users cannot sign in with a passkey
	admin := e.browser()
	e.user("root", auth.Admin)
	admin.passwordLogin("root")
	admin.must("PUT", "/api/v1/users/anna", map[string]any{"disabled": true}, 204)
	c = e.browser().begin(map[string]any{})
	if code, _ := e.browser().finish(c, k.assert(t, c.Options)); code != 401 {
		t.Fatalf("disabled user's passkey: %d", code)
	}
	admin.must("PUT", "/api/v1/users/anna", map[string]any{"disabled": false}, 204)
	// with local sign-in off, passkeys are refused too (single sign-on only)
	if _, err := e.n.Svc.DB.Pool().Exec(context.Background(), `INSERT INTO server_settings (key, value) VALUES ('local_login','false')`); err != nil {
		t.Fatal(err)
	}
	if code, out := e.browser().do("POST", "/api/v1/session/webauthn/begin", map[string]any{}); code != 403 || !strings.Contains(string(out), "local_login_disabled") {
		t.Fatalf("passkey with local_login=false: %d %s", code, out)
	}
	if code, _ := e.browser().do("POST", "/api/v1/session", map[string]string{"name": "root", "password": pw}); code != 403 {
		t.Fatalf("password with local_login=false: %d", code)
	}
}

func TestWebAuthnRegistrationNeedsRecentSignIn(t *testing.T) {
	e := newAuthEnv(t)
	e.user("anna", auth.Operator)
	b := e.browser()
	b.passwordLogin("anna")
	e.advance(11 * time.Minute) // an old (but still active) session
	b.do("GET", "/api/v1/session", nil)
	code, out := b.do("POST", "/api/v1/me/webauthn/register/begin", map[string]any{})
	if code != 400 || !strings.Contains(string(out), "password_required") {
		t.Fatalf("old session registering: %d %s", code, out)
	}
	if code, _ := b.do("POST", "/api/v1/me/webauthn/register/begin", map[string]any{"password": "wrong wrong wrong"}); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	b.must("POST", "/api/v1/me/webauthn/register/begin", map[string]any{"password": pw}, 200)
	// a ceremony is bound to its user: bert cannot finish anna's registration
	e.user("bert", auth.Operator)
	bb := e.browser()
	bb.passwordLogin("bert")
	var c ceremony
	_ = json.Unmarshal(b.must("POST", "/api/v1/me/webauthn/register/begin", map[string]any{"password": pw}, 200), &c)
	opts, _ := virtualwebauthn.ParseAttestationOptions(string(c.Options))
	k := e.newKey()
	resp := virtualwebauthn.CreateAttestationResponse(k.rp, k.a, k.cred, *opts)
	if code, out := bb.do("POST", "/api/v1/me/webauthn/register/finish", map[string]any{"ceremony": c.Ceremony, "name": "x", "credential": json.RawMessage(resp)}); code != 400 {
		t.Fatalf("another user's ceremony: %d %s", code, out)
	}
	// API tokens have no "me" for sign-in methods
	code, out = b.do("POST", "/api/v1/me/tokens", map[string]any{"name": "ci", "role": "readonly"})
	var tk struct{ Token string }
	_ = json.Unmarshal(out, &tk)
	if code != 201 {
		t.Fatalf("%d %s", code, out)
	}
	r, _ := http.NewRequest("POST", e.base+"/api/v1/me/webauthn/register/begin", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+tk.Token)
	resp2, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == 200 {
		t.Fatal("an API token started a key registration")
	}
}

func TestPasskeysNeedAConsoleAddress(t *testing.T) {
	e := newAuthEnv(t)
	if err := e.n.Svc.SetSettings(context.Background(), map[string]string{"base_url": ""}); err != nil {
		t.Fatal(err)
	}
	code, out := e.browser().do("POST", "/api/v1/session/webauthn/begin", map[string]any{})
	if code != 400 || !strings.Contains(string(out), "base_url") {
		t.Fatalf("no RP configured: %d %s", code, out)
	}
	var opts struct {
		Passkeys   bool `json:"passkeys"`
		LocalLogin bool `json:"local_login"`
	}
	_ = json.Unmarshal(e.browser().must("GET", "/api/v1/auth/oidc/providers", nil, 200), &opts)
	if opts.Passkeys || !opts.LocalLogin {
		t.Fatalf("sign-in options without RP: %+v", opts)
	}
	// the RP ID must cover the origins; an explicit RP ID / origin list overrides base_url
	ctx := context.Background()
	if err := e.st.SetPolicy(ctx, auth.PolicyUpdate{WebAuthnRPID: ptr("example.com"), WebAuthnOrigins: ptr([]string{"https://backup.example.com"})}); err != nil {
		t.Fatal(err)
	}
	if id, origins, err := e.st.WebAuthnRP(ctx); err != nil || id != "example.com" || len(origins) != 1 {
		t.Fatalf("explicit RP: %s %v %v", id, origins, err)
	}
	if err := e.st.SetPolicy(ctx, auth.PolicyUpdate{WebAuthnRPID: ptr("other.example")}); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("RP ID not covering the origin: %v", err)
	}
	if err := e.st.SetPolicy(ctx, auth.PolicyUpdate{WebAuthnOrigins: ptr([]string{"http://backup.example.com"})}); err == nil {
		t.Fatal("plain http origin accepted")
	}
	_ = e.st.SetPolicy(ctx, auth.PolicyUpdate{WebAuthnRPID: ptr(""), WebAuthnOrigins: ptr([]string{})})
	// an IP address cannot be a relying party
	_ = e.n.Svc.SetSettings(context.Background(), map[string]string{"base_url": "https://192.0.2.7"})
	if code, out := e.browser().do("POST", "/api/v1/session/webauthn/begin", map[string]any{}); code != 400 || !strings.Contains(string(out), "IP address") {
		t.Fatalf("IP RP: %d %s", code, out)
	}
}

func TestRequire2FAPolicy(t *testing.T) {
	e := newAuthEnv(t)
	e.user("root", auth.Admin)
	e.user("ops", auth.Operator)
	admin := e.browser()
	admin.passwordLogin("root")
	if code, _ := admin.do("PUT", "/api/v1/auth/settings", map[string]any{"require_2fa": "sometimes"}); code != 400 {
		t.Fatal("invalid require_2fa accepted")
	}
	// the policy cannot be set through the generic settings route (it has no guard)
	if code, _ := admin.do("PUT", "/api/v1/settings", map[string]string{"require_2fa": "all"}); code != 400 {
		t.Fatal("require_2fa through PUT /settings")
	}
	admin.must("PUT", "/api/v1/auth/settings", map[string]any{"require_2fa": "admin"}, 200)
	// the same session is limited at once (decided per request)
	code, out := admin.do("GET", "/api/v1/repositories", nil)
	if code != 403 || !strings.Contains(string(out), "enrol_2fa_required") {
		t.Fatalf("admin without second factor: %d %s", code, out)
	}
	var sv struct {
		Enrol bool `json:"enrol_2fa_required"`
	}
	_ = json.Unmarshal(admin.must("GET", "/api/v1/session", nil, 200), &sv)
	if !sv.Enrol {
		t.Fatal("GET /session does not report the enrolment requirement")
	}
	// an operator is not covered by "admin"
	op := e.browser()
	op.passwordLogin("ops")
	op.must("GET", "/api/v1/repositories", nil, 200)
	// enrolment works in the limited session, and lifts the limit
	k := e.newKey()
	admin.register(k, "admin key")
	admin.must("GET", "/api/v1/repositories", nil, 200)
	// the last factor cannot be removed while the policy covers the user
	code, out = admin.do("DELETE", "/api/v1/me/webauthn/"+k.id, map[string]any{})
	if code != 400 || !strings.Contains(string(out), "last one") {
		t.Fatalf("removing the last factor: %d %s", code, out)
	}
	k2 := e.newKey()
	admin.register(k2, "spare")
	admin.must("DELETE", "/api/v1/me/webauthn/"+k.id, map[string]any{}, 204)
	// "all": the operator's session is limited now; the TOTP route is open to enrol
	if err := e.st.SetPolicy(context.Background(), auth.PolicyUpdate{Require2FA: ptr("all")}); err != nil {
		t.Fatal(err)
	}
	if code, _ := op.do("GET", "/api/v1/repositories", nil); code != 403 {
		t.Fatalf("operator under all: %d", code)
	}
	op.must("POST", "/api/v1/me/totp", nil, 200)
	// a TOTP-only user cannot remove TOTP under the policy
	var enr auth.TOTPEnrolment
	_ = json.Unmarshal(op.must("POST", "/api/v1/me/totp", nil, 200), &enr)
	totpConfirm(t, op, enr.Secret)
	op.must("GET", "/api/v1/repositories", nil, 200)
	if code, out := op.do("DELETE", "/api/v1/me/totp", map[string]string{"password": pw}); code != 400 || !strings.Contains(string(out), "last one") {
		t.Fatalf("removing TOTP, the last factor: %d %s", code, out)
	}
	// single sign-on sessions are not limited (the identity provider owns the second factor): tested in
	// the OIDC tests. Administrators can still reset a user's factors (lost device); the user is then
	// limited to enrolment again.
	admin.must("DELETE", "/api/v1/users/ops/totp", nil, 204)
	if code, _ := op.do("GET", "/api/v1/repositories", nil); code != 403 {
		t.Fatalf("after the reset: %d", code)
	}
}

func ptr[T any](v T) *T { return &v }

// totpConfirm finishes a TOTP enrolment with a code computed here (RFC 6238, SHA-1, 30 s, 6 digits).
func totpConfirm(t *testing.T, b *browser, secret string) {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], uint64(time.Now().Unix()/30))
	m := hmac.New(sha1.New, key)
	m.Write(c[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1_000_000)
	b.must("POST", "/api/v1/me/totp/confirm", map[string]string{"code": code}, 200)
}
