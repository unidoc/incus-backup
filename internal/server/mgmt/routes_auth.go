package mgmt

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/service"
)

func (s *Server) authFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: err.Error()})
	case errors.Is(err, auth.ErrNeedTOTP):
		// the password was right; a second factor is needed. second_factor says which ones the user has;
		// ticket starts the security-key step (POST /session/webauthn/begin) without the password again.
		v := map[string]any{"error": err.Error(), "totp_required": true}
		var sf *auth.SecondFactorNeeded
		if errors.As(err, &sf) {
			v["second_factor"] = map[string]bool{"totp": sf.TOTP, "webauthn": sf.WebAuthn}
			v["ticket"] = sf.Ticket
		}
		writeJSON(w, http.StatusUnauthorized, v)
	case errors.Is(err, auth.ErrLocked):
		writeJSON(w, http.StatusTooManyRequests, api.Error{Error: err.Error()})
	case errors.Is(err, auth.ErrLocalLoginOff):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "local_login_disabled": true})
	case errors.Is(err, auth.ErrReauth):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "password_required": true})
	case errors.Is(err, auth.ErrNoSession):
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: err.Error()})
	case errors.Is(err, auth.ErrNotFound):
		s.fail(w, r, fmt.Errorf("%w: %v", service.ErrNotFound, err))
	case errors.Is(err, auth.ErrExists):
		s.fail(w, r, fmt.Errorf("%w: %v", service.ErrConflict, err))
	case errors.Is(err, auth.ErrBad), errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrSetup):
		s.fail(w, r, fmt.Errorf("%w: %v", service.ErrBadRequest, err))
	default:
		s.fail(w, r, err)
	}
}

type sessionView struct {
	User      auth.User `json:"user"`
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// Access: per-repository roles (absent: the user's role applies to every repository).
	Access *auth.Access `json:"access,omitempty"`
	// Method: how this session was opened (password, passkey, oidc).
	Method string `json:"method,omitempty"`
	// Enrol2FA: the sign-in policy requires a second factor that is not set up yet; only the
	// enrolment routes answer until it is (docs/authentication.md).
	Enrol2FA bool `json:"enrol_2fa_required,omitempty"`
}

func (s *Server) setCookie(w http.ResponseWriter, tok string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure,
		SameSite: http.SameSiteStrictMode, Expires: exp})
}

// selfName returns the signed-in user's name; the local admin and tokens have no "me".
func (s *Server) self(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p := PrincipalFrom(r.Context())
	if p.Kind != "user" {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "this needs a signed-in user (not the local socket or an API token)"})
		return p, false
	}
	return p, true
}

func (s *Server) authRoutes(h routeFn) {
	if s.Auth == nil {
		return
	}
	h("GET /api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.Svc.Version})
	})
	h("GET /api/v1/events", s.Hub.serveEvents)

	h("POST /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		if !s.throttle.allow(clientIP(r), s.Auth.Now()) {
			writeJSON(w, http.StatusTooManyRequests, api.Error{Error: "too many attempts from this address: wait a minute"})
			return
		}
		var req struct{ Name, Password, Code string }
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		ip := clientIP(r)
		sess, err := s.Auth.Login(r.Context(), req.Name, req.Password, req.Code, ip, r.UserAgent())
		entry := auth.AuditEntry{Actor: req.Name, Action: "login", IP: ip, OK: err == nil}
		if err != nil && !errors.Is(err, auth.ErrNeedTOTP) {
			entry.Detail = mustJSON(map[string]any{"method": "password", "reason": err.Error()})
			_ = s.Auth.Audit(r.Context(), entry)
			if errors.Is(err, auth.ErrLocked) { // someone keeps guessing: tell the administrators
				_ = s.Notify.Emit(r.Context(), notify.Event{Type: "login_security", Key: ip + "/" + req.Name, Severity: "warning",
					Title: "Repeated failed sign-ins for " + req.Name, Detail: "from " + ip + "; the account is locked for a few minutes"})
			}
			s.authFail(w, r, err)
			return
		}
		if err != nil {
			s.authFail(w, r, err) // password right, code still needed: not an event
			return
		}
		method := "password"
		if req.Code != "" {
			method = "password+totp"
		}
		entry.Detail = mustJSON(map[string]any{"method": method})
		_ = s.Auth.Audit(r.Context(), entry)
		writeJSON(w, 200, s.startSession(w, r, sess)) // per-repository roles, so the console can follow them from the start
	})
	h("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if p.Kind != "user" {
			writeJSON(w, 200, map[string]any{"user": map[string]any{"name": p.User, "role": p.Role}, "kind": p.Kind, "access": p.Access})
			return
		}
		u, err := s.Auth.User(r.Context(), p.User)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, sessionView{User: *u, CSRF: p.CSRF, Access: p.Access, Method: p.Method, Enrol2FA: p.Enrol2FA})
	})
	h("DELETE /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieName); err == nil {
			_ = s.Auth.Logout(r.Context(), c.Value)
		}
		s.setCookie(w, "", time.Unix(0, 0))
		w.WriteHeader(http.StatusNoContent)
	})

	h("GET /api/v1/setup", func(w http.ResponseWriter, r *http.Request) {
		need, err := s.Auth.SetupNeeded(r.Context())
		s.reply(w, r, map[string]bool{"needed": need}, err)
	})
	h("POST /api/v1/setup", func(w http.ResponseWriter, r *http.Request) {
		if !s.throttle.allow(clientIP(r), s.Auth.Now()) {
			writeJSON(w, http.StatusTooManyRequests, api.Error{Error: "too many attempts from this address: wait a minute"})
			return
		}
		var req struct{ Token, Name, Password string }
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		u, err := s.Auth.CompleteSetup(r.Context(), req.Token, req.Name, req.Password)
		_ = s.Auth.Audit(r.Context(), auth.AuditEntry{Actor: truncName(req.Name), Action: "setup", IP: clientIP(r), OK: err == nil})
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, u)
	})

	// ---- users (admin)
	h("GET /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		us, err := s.Auth.Users(r.Context())
		s.reply(w, r, us, err)
	})
	h("POST /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name, Password, Display, Email string
			Role                           auth.Role
			MustChange                     bool `json:"must_change"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		u, err := s.Auth.CreateUser(r.Context(), req.Name, req.Password, req.Role, req.Display, req.Email, req.MustChange)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, u)
	})
	h("PUT /api/v1/users/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Role     *auth.Role `json:"role"`
			Display  *string    `json:"display_name"`
			Email    *string    `json:"email"`
			Disabled *bool      `json:"disabled"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.UpdateUser(r.Context(), r.PathValue("name"), req.Role, req.Display, req.Email, req.Disabled); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("DELETE /api/v1/users/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Auth.DeleteUser(r.Context(), r.PathValue("name")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("POST /api/v1/users/{name}/password", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password   string `json:"password"`
			MustChange bool   `json:"must_change"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.SetPassword(r.Context(), r.PathValue("name"), req.Password, req.MustChange, ""); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("DELETE /api/v1/users/{name}/totp", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Auth.DisableTOTP(r.Context(), r.PathValue("name")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- me
	h("POST /api/v1/me/password", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req struct{ Current, New string }
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		c, _ := r.Cookie(cookieName)
		keep := ""
		if c != nil {
			keep = c.Value
		}
		if err := s.Auth.ChangeOwnPassword(r.Context(), p.User, req.Current, req.New, keep); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("POST /api/v1/me/totp", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		en, err := s.Auth.BeginTOTP(r.Context(), p.User)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, en)
	})
	h("POST /api/v1/me/totp/confirm", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req struct{ Code string }
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		codes, err := s.Auth.ConfirmTOTP(r.Context(), p.User, req.Code)
		if err != nil {
			s.fail(w, r, fmt.Errorf("%w: %v", service.ErrBadRequest, err))
			return
		}
		writeJSON(w, 200, map[string]any{"recovery_codes": codes, "note": "shown only now"})
	})
	h("DELETE /api/v1/me/totp", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req struct{ Password string }
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.VerifyPassword(r.Context(), p.User, req.Password); err != nil {
			s.authFail(w, r, err)
			return
		}
		if err := s.Auth.DisableOwnTOTP(r.Context(), p.User); err != nil { // not the last factor the policy requires
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("GET /api/v1/me/sessions", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		c, _ := r.Cookie(cookieName)
		cur := ""
		if c != nil {
			cur = c.Value
		}
		l, err := s.Auth.Sessions(r.Context(), p.User, cur)
		s.reply(w, r, l, err)
	})
	h("DELETE /api/v1/me/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		if err := s.Auth.RevokeSession(r.Context(), p.User, r.PathValue("id")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("GET /api/v1/me/tokens", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		l, err := s.Auth.Tokens(r.Context(), p.User)
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/me/tokens", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		var req struct {
			Name     string
			Role     auth.Role
			TTLHours float64 `json:"ttl_hours"`
			// Repositories restricts the token (docs/permissions.md); absent = not restricted.
			Repositories []string `json:"repositories"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if req.Role == "" {
			req.Role = p.MaxRole()
		}
		sec, info, err := s.Auth.CreateTokenFor(r.Context(), p.User, req.Name, req.Role, time.Duration(req.TTLHours*float64(time.Hour)), req.Repositories)
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": sec, "info": info, "note": "shown only now"})
	})
	h("DELETE /api/v1/me/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.self(w, r)
		if !ok {
			return
		}
		if err := s.Auth.RevokeToken(r.Context(), p.User, r.PathValue("id")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var f auth.AuditFilter
		f.Actor, f.Action = q.Get("actor"), q.Get("action")
		fmt.Sscan(q.Get("before"), &f.Before)
		fmt.Sscan(q.Get("limit"), &f.Limit)
		l, err := s.Auth.ListAudit(r.Context(), f)
		s.reply(w, r, l, err)
	})
}

// truncName bounds an unauthenticated, caller-chosen name before it is written to the audit log (valid user
// names are at most 64 bytes; the request body may be 1 MiB).
func truncName(n string) string {
	if len(n) > 64 {
		return strings.ToValidUTF8(n[:64], "") + "…"
	}
	return n
}
