package mgmt

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/auth"
)

// Passkeys / security keys (WebAuthn) and single sign-on (OIDC): docs/authentication.md. The services
// are in internal/server/auth (webauthn.go, oidc.go, policy.go); every way in ends in the same session
// (auth.Store.openSession) and the same answer (startSession).

func init() {
	for pat, p := range map[string]auth.Perm{
		// public: sign-in (they carry their own checks: throttle, cross-site refusal, ceremony state)
		"POST /api/v1/session/webauthn/begin":       rd,
		"POST /api/v1/session/webauthn/finish":      rd,
		"GET /api/v1/auth/oidc/providers":           rd,
		"GET /api/v1/auth/oidc/{provider}/login":    rd,
		"GET /api/v1/auth/oidc/{provider}/callback": rd,
		// one's own keys
		"GET /api/v1/me/webauthn":                  rd,
		"POST /api/v1/me/webauthn/register/begin":  rd,
		"POST /api/v1/me/webauthn/register/finish": rd,
		"PATCH /api/v1/me/webauthn/{id}":           rd,
		"DELETE /api/v1/me/webauthn/{id}":          rd,
		// administration
		"DELETE /api/v1/users/{name}/webauthn":              ad,
		"GET /api/v1/users/{name}/identities":               ad,
		"POST /api/v1/users/{name}/identities":              ad,
		"DELETE /api/v1/users/{name}/identities/{provider}": ad,
		"GET /api/v1/oidc/providers":                        ad,
		"POST /api/v1/oidc/providers":                       ad,
		"PUT /api/v1/oidc/providers/{name}":                 ad,
		"DELETE /api/v1/oidc/providers/{name}":              ad,
		"POST /api/v1/oidc/providers/{name}/test":           ad,
		"GET /api/v1/auth/settings":                         ad,
		"PUT /api/v1/auth/settings":                         ad,
	} {
		perms[pat] = p
	}
	for _, pat := range []string{"POST /api/v1/session/webauthn/begin", "POST /api/v1/session/webauthn/finish",
		"GET /api/v1/auth/oidc/providers", "GET /api/v1/auth/oidc/{provider}/login", "GET /api/v1/auth/oidc/{provider}/callback"} {
		publicRoutes[pat] = true
	}
	for pat, m := range map[string]apiRoute{
		"POST /api/v1/session/webauthn/begin": {Summary: "Start a passkey sign-in (no body) or the security-key second factor ({ticket} from POST /session); returns navigator.credentials.get() options", Tag: "Auth", Bind: bindSelf,
			Body: docWebAuthnBegin{}, Resp: auth.CeremonyStart{}},
		"POST /api/v1/session/webauthn/finish": {Summary: "Finish a passkey or second-factor sign-in (sets the session cookie like POST /session)", Tag: "Auth", Bind: bindSelf,
			Body: docWebAuthnFinish{}, Resp: sessionView{}},
		"GET /api/v1/auth/oidc/providers":                   {Summary: "Sign-in options for the sign-in page: identity providers (name, label), whether password and passkey sign-in are on (no login)", Tag: "Auth", Bind: bindSelf, Resp: docSignInOptions{}},
		"GET /api/v1/auth/oidc/{provider}/login":            {Summary: "Redirect the browser to the identity provider (?next=/path after sign-in)", Tag: "Auth", Bind: bindSelf, Query: []param{qp("next", "string", "console path to open after sign-in")}, Status: 302},
		"GET /api/v1/auth/oidc/{provider}/callback":         {Summary: "Identity provider's redirect back: sets the session cookie and redirects to the console", Tag: "Auth", Bind: bindSelf, Status: 302},
		"GET /api/v1/me/webauthn":                           {Summary: "My passkeys and security keys", Tag: "Me", Bind: bindSelf, Resp: []auth.WebAuthnKey{}},
		"POST /api/v1/me/webauthn/register/begin":           {Summary: "Start registering a passkey or security key (password needed when the sign-in is older than 10 minutes); returns navigator.credentials.create() options", Tag: "Me", Bind: bindSelf, Body: docPasswordOpt{}, Resp: auth.CeremonyStart{}},
		"POST /api/v1/me/webauthn/register/finish":          {Summary: "Finish registering: the browser's credential and a name", Tag: "Me", Bind: bindSelf, Body: docWebAuthnRegister{}, Resp: auth.WebAuthnKey{}, Status: 201},
		"PATCH /api/v1/me/webauthn/{id}":                    {Summary: "Rename one of my keys", Tag: "Me", Bind: bindSelf, Body: docKeyName{}},
		"DELETE /api/v1/me/webauthn/{id}":                   {Summary: "Remove one of my keys (password needed when the sign-in is older than 10 minutes; the last second factor the policy requires is refused)", Tag: "Me", Bind: bindSelf, Body: docPasswordOpt{}},
		"DELETE /api/v1/users/{name}/webauthn":              {Summary: "Remove every passkey and security key of a user (lost key)", Tag: "Users", Resp: docRemoved{}},
		"GET /api/v1/users/{name}/identities":               {Summary: "A user's links to identity providers (pending: linked by e-mail, not signed in yet)", Tag: "Users", Resp: []auth.Identity{}},
		"POST /api/v1/users/{name}/identities":              {Summary: "Link a user to an identity provider by e-mail: the first sign-in with that verified address binds the account", Tag: "Users", Body: docLink{}, Status: 201},
		"DELETE /api/v1/users/{name}/identities/{provider}": {Summary: "Remove a user's link to an identity provider", Tag: "Users"},
		"GET /api/v1/oidc/providers":                        {Summary: "Identity providers (never the client secret)", Tag: "Authentication", Resp: []auth.OIDCProvider{}},
		"POST /api/v1/oidc/providers":                       {Summary: "Add an identity provider (the client secret is sealed and never returned)", Tag: "Authentication", Body: auth.OIDCProviderInput{}, Resp: auth.OIDCProvider{}, Status: 201},
		"PUT /api/v1/oidc/providers/{name}":                 {Summary: "Replace an identity provider's settings (an empty client_secret keeps the stored one)", Tag: "Authentication", Body: auth.OIDCProviderInput{}, Resp: auth.OIDCProvider{}},
		"DELETE /api/v1/oidc/providers/{name}":              {Summary: "Remove an identity provider and its user links", Tag: "Authentication"},
		"POST /api/v1/oidc/providers/{name}/test":           {Summary: "Run discovery against the issuer and fetch its key set", Tag: "Authentication", Resp: auth.DiscoveryReport{}},
		"GET /api/v1/auth/settings":                         {Summary: "Sign-in policy: require_2fa, local_login, passkey relying party (and what is in effect)", Tag: "Authentication", Resp: docAuthSettings{}},
		"PUT /api/v1/auth/settings":                         {Summary: "Change the sign-in policy (local_login=false needs an administrator who signs in through an identity provider)", Tag: "Authentication", Body: auth.PolicyUpdate{}, Resp: docAuthSettings{}},
	} {
		apiRoutes[pat] = m
	}
}

type docWebAuthnBegin struct {
	Ticket string `json:"ticket,omitempty" doc:"from the 401 of POST /session: the security-key second factor; absent: passkey sign-in"`
}
type docWebAuthnFinish struct {
	Ceremony   string          `json:"ceremony"`
	Credential json.RawMessage `json:"credential" doc:"the PublicKeyCredential as JSON (base64url binary fields)"`
}
type docWebAuthnRegister struct {
	Ceremony   string          `json:"ceremony"`
	Name       string          `json:"name"`
	Credential json.RawMessage `json:"credential"`
}
type docPasswordOpt struct {
	Password string `json:"password,omitempty"`
}
type docKeyName struct {
	Name string `json:"name"`
}
type docRemoved struct {
	Removed int `json:"removed"`
}
type docLink struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
}
type docSignInOptions struct {
	Providers  []auth.OIDCButton `json:"providers"`
	LocalLogin bool              `json:"local_login"`
	Passkeys   bool              `json:"passkeys"`
}
type docAuthSettings struct {
	auth.Policy
	// in effect
	BaseURL         string   `json:"base_url"`
	WebAuthnRPIDEff string   `json:"webauthn_rp_id_effective"`
	OriginsEff      []string `json:"webauthn_origins_effective"`
	WebAuthnError   string   `json:"webauthn_error,omitempty"`
	SSOAdmins       []string `json:"sso_admins"`
	RedirectBase    string   `json:"oidc_redirect_base"`
}

// enrolRoutes are what a session that must first set up a second factor may reach (auth.Principal.Enrol2FA).
var enrolRoutes = map[string]bool{
	"GET /api/v1/session": true, "DELETE /api/v1/session": true, "GET /api/v1/ping": true,
	"POST /api/v1/me/totp": true, "POST /api/v1/me/totp/confirm": true,
	"GET /api/v1/me/webauthn": true, "POST /api/v1/me/webauthn/register/begin": true, "POST /api/v1/me/webauthn/register/finish": true,
}

// enrolGate refuses everything but the enrolment routes for a session that must set up a second factor.
func (s *Server) enrolGate(w http.ResponseWriter, r *http.Request, p *auth.Principal, pattern string) bool {
	if !p.Enrol2FA || enrolRoutes[pattern] {
		return true
	}
	s.auditRefused(r, *p, pattern, "second factor not set up")
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "your account must use two-factor authentication: set up a security key or an authenticator app first",
		"enrol_2fa_required": true})
	return false
}

func (s *Server) oidcSvc() *auth.OIDC {
	s.oidcOnce.Do(func() { s.oidcRP = &auth.OIDC{Store: s.Auth, Secrets: s.Svc} })
	return s.oidcRP
}

// startSession answers a successful sign-in, whatever the method: the session cookie, the CSRF token,
// the user and its repository access (exactly what POST /session answers).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, sess *auth.Session) sessionView {
	s.setCookie(w, sess.Token, sess.Expires)
	v := sessionView{User: sess.User, CSRF: sess.CSRF, ExpiresAt: sess.Expires}
	if p, err := s.Auth.Authenticate(r.Context(), sess.Token); err == nil {
		v.Access, v.Method, v.Enrol2FA = p.Access, p.Method, p.Enrol2FA
	}
	return v
}

// cookieValue is the caller's session cookie ("" without one).
func cookieValue(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) redirectBase(r *http.Request) string {
	return strings.TrimRight(s.Svc.Setting(r.Context(), "base_url"), "/")
}

func (s *Server) loginAudit(r *http.Request, actor string, ok bool, detail map[string]any) {
	if actor == "" {
		actor = "anonymous"
	}
	_ = s.Auth.Audit(r.Context(), auth.AuditEntry{Actor: truncName(actor), Action: "login", IP: clientIP(r), OK: ok, Detail: mustJSON(detail)})
}

func (s *Server) authnRoutes(h routeFn) {
	if s.Auth == nil {
		return
	}
	throttled := func(w http.ResponseWriter, r *http.Request) bool {
		if !s.throttle.allow(clientIP(r), s.Auth.Now()) {
			writeJSON(w, http.StatusTooManyRequests, api.Error{Error: "too many attempts from this address: wait a minute"})
			return true
		}
		return false
	}

	// ---- sign-in with a passkey, or a security key as the second factor
	h("POST /api/v1/session/webauthn/begin", func(w http.ResponseWriter, r *http.Request) {
		if throttled(w, r) {
			return
		}
		var req docWebAuthnBegin
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		var start *auth.CeremonyStart
		var err error
		if req.Ticket != "" {
			start, err = s.Auth.BeginWebAuthnSecondFactor(r.Context(), req.Ticket, r.Header.Get("Origin"))
		} else {
			start, err = s.Auth.BeginPasskeyLogin(r.Context(), r.Header.Get("Origin"))
		}
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, start)
	})
	h("POST /api/v1/session/webauthn/finish", func(w http.ResponseWriter, r *http.Request) {
		if throttled(w, r) {
			return
		}
		var req docWebAuthnFinish
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		sess, name, method, err := s.Auth.FinishWebAuthnLogin(r.Context(), req.Ceremony, req.Credential, clientIP(r), r.UserAgent())
		if err != nil {
			s.loginAudit(r, name, false, map[string]any{"method": method, "reason": err.Error()})
			if errors.Is(err, auth.ErrCloned) {
				writeJSON(w, http.StatusUnauthorized, api.Error{Error: err.Error()})
				return
			}
			if errors.Is(err, auth.ErrInvalidCredentials) {
				writeJSON(w, http.StatusUnauthorized, api.Error{Error: "the security key was not accepted: use a key registered for this account, or sign in another way"})
				return
			}
			s.authFail(w, r, err)
			return
		}
		s.loginAudit(r, sess.User.Name, true, map[string]any{"method": method})
		writeJSON(w, 200, s.startSession(w, r, sess))
	})

	// ---- single sign-on
	h("GET /api/v1/auth/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
		bs, err := s.oidcSvc().Buttons(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		pol, err := s.Auth.Policy(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		_, _, rpErr := s.Auth.WebAuthnRP(r.Context())
		writeJSON(w, 200, docSignInOptions{Providers: bs, LocalLogin: pol.LocalLogin, Passkeys: pol.LocalLogin && rpErr == nil})
	})
	ssoFail := func(w http.ResponseWriter, r *http.Request, code string) {
		http.Redirect(w, r, "/login?sso_error="+url.QueryEscape(code), http.StatusFound)
	}
	h("GET /api/v1/auth/oidc/{provider}/login", func(w http.ResponseWriter, r *http.Request) {
		if throttled(w, r) {
			return
		}
		base := s.redirectBase(r)
		if base == "" {
			ssoFail(w, r, "config")
			return
		}
		name := r.PathValue("provider")
		u, browser, err := s.oidcSvc().StartLogin(r.Context(), name, base+"/api/v1/auth/oidc/"+name+"/callback", r.URL.Query().Get("next"))
		if err != nil {
			s.loginAudit(r, "", false, map[string]any{"method": "oidc", "provider": truncName(name), "reason": err.Error()})
			var se *auth.SSOError
			if errors.As(err, &se) {
				ssoFail(w, r, se.Code)
				return
			}
			ssoFail(w, r, "failed")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ib_oidc", Value: browser, Path: "/api/v1/auth/oidc/", HttpOnly: true, Secure: s.Secure,
			SameSite: http.SameSiteLaxMode, MaxAge: int(auth.LoginTTL / time.Second)}) // Lax: it must come back on the IdP's redirect
		http.Redirect(w, r, u, http.StatusFound)
	})
	h("GET /api/v1/auth/oidc/{provider}/callback", func(w http.ResponseWriter, r *http.Request) {
		if throttled(w, r) {
			return
		}
		name := r.PathValue("provider")
		q := r.URL.Query()
		browser := ""
		if c, err := r.Cookie("ib_oidc"); err == nil {
			browser = c.Value
		}
		http.SetCookie(w, &http.Cookie{Name: "ib_oidc", Value: "", Path: "/api/v1/auth/oidc/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
		sess, next, err := s.oidcSvc().FinishLogin(r.Context(), name, q.Get("state"), q.Get("code"), q.Get("error"), browser,
			s.redirectBase(r)+"/api/v1/auth/oidc/"+name+"/callback", clientIP(r), r.UserAgent())
		if err != nil {
			var se *auth.SSOError
			code, actor := "failed", ""
			if errors.As(err, &se) {
				code, actor = se.Code, se.User
			}
			s.loginAudit(r, actor, false, map[string]any{"method": "oidc", "provider": truncName(name), "error": code, "reason": err.Error()})
			ssoFail(w, r, code)
			return
		}
		s.loginAudit(r, sess.User.Name, true, map[string]any{"method": "oidc", "provider": name})
		s.startSession(w, r, sess)
		http.Redirect(w, r, next, http.StatusFound)
	})

	// ---- my keys
	h("GET /api/v1/me/webauthn", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		l, err := s.Auth.WebAuthnKeys(r.Context(), p.User)
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/me/webauthn/register/begin", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req docPasswordOpt
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.Reauthenticate(r.Context(), p.User, cookieValue(r), req.Password); err != nil {
			s.authFail(w, r, err)
			return
		}
		start, err := s.Auth.BeginWebAuthnRegistration(r.Context(), p.User, r.Header.Get("Origin"))
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, start)
	})
	h("POST /api/v1/me/webauthn/register/finish", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req docWebAuthnRegister
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		k, err := s.Auth.FinishWebAuthnRegistration(r.Context(), p.User, req.Ceremony, req.Name, req.Credential)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, k)
	})
	h("PATCH /api/v1/me/webauthn/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req docKeyName
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.RenameWebAuthnKey(r.Context(), p.User, r.PathValue("id"), req.Name); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("DELETE /api/v1/me/webauthn/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req docPasswordOpt
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.Reauthenticate(r.Context(), p.User, cookieValue(r), req.Password); err != nil {
			s.authFail(w, r, err)
			return
		}
		if err := s.Auth.DeleteWebAuthnKey(r.Context(), p.User, r.PathValue("id")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- administration
	h("DELETE /api/v1/users/{name}/webauthn", func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Auth.ResetWebAuthn(r.Context(), r.PathValue("name"))
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, docRemoved{Removed: n})
	})
	h("GET /api/v1/users/{name}/identities", func(w http.ResponseWriter, r *http.Request) {
		l, err := s.oidcSvc().Identities(r.Context(), r.PathValue("name"))
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/users/{name}/identities", func(w http.ResponseWriter, r *http.Request) {
		var req docLink
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.oidcSvc().LinkIdentity(r.Context(), r.PathValue("name"), req.Provider, req.Email); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	h("DELETE /api/v1/users/{name}/identities/{provider}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.oidcSvc().UnlinkIdentity(r.Context(), r.PathValue("name"), r.PathValue("provider")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("GET /api/v1/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
		l, err := s.oidcSvc().Providers(r.Context())
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
		var in auth.OIDCProviderInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		p, err := s.oidcSvc().CreateProvider(r.Context(), in)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, p)
	})
	h("PUT /api/v1/oidc/providers/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in auth.OIDCProviderInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		if in.Name != "" && in.Name != r.PathValue("name") {
			writeJSON(w, http.StatusBadRequest, api.Error{Error: "a provider cannot be renamed (the name is part of its redirect URL): add a new one"})
			return
		}
		p, err := s.oidcSvc().UpdateProvider(r.Context(), r.PathValue("name"), in)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, p)
	})
	h("DELETE /api/v1/oidc/providers/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.oidcSvc().DeleteProvider(r.Context(), r.PathValue("name")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("POST /api/v1/oidc/providers/{name}/test", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		redirect := ""
		if base := s.redirectBase(r); base != "" {
			redirect = base + "/api/v1/auth/oidc/" + name + "/callback"
		}
		rep, err := s.oidcSvc().Test(r.Context(), name, redirect)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		if redirect == "" {
			rep.Warnings = append(rep.Warnings, "the console address (base_url) is not set: the redirect URI cannot be built and sign-in will fail")
			rep.OK = false
		}
		writeJSON(w, 200, rep)
	})
	authSettings := func(w http.ResponseWriter, r *http.Request) {
		pol, err := s.Auth.Policy(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v := docAuthSettings{Policy: pol, BaseURL: s.redirectBase(r), OriginsEff: []string{}, SSOAdmins: []string{}}
		if v.BaseURL != "" {
			v.RedirectBase = v.BaseURL + "/api/v1/auth/oidc/"
		}
		if id, origins, err := s.Auth.WebAuthnRP(r.Context()); err != nil {
			v.WebAuthnError = err.Error()
		} else {
			v.WebAuthnRPIDEff, v.OriginsEff = id, origins
		}
		if l, err := s.Auth.SSOAdmins(r.Context()); err == nil {
			v.SSOAdmins = l
		}
		writeJSON(w, 200, v)
	}
	h("GET /api/v1/auth/settings", authSettings)
	h("PUT /api/v1/auth/settings", func(w http.ResponseWriter, r *http.Request) {
		var u auth.PolicyUpdate
		if err := decode(r, &u); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.SetPolicy(r.Context(), u); err != nil {
			s.authFail(w, r, err)
			return
		}
		authSettings(w, r)
	})
}
