package mgmt

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/server/tasks"
)

// Server is the management API (/api/v1). The same routes are served on two listeners:
//
//   - the unix admin socket (LocalHandler): whoever can open the socket is the server administrator
//     (filesystem permissions), no login. The server CLI uses this.
//   - the HTTPS web listener (WebHandler): session cookie + CSRF for the browser, Bearer API tokens for
//     automation; every route is checked against the caller's role.
//
// Secrets (S3 keys, enrolment tokens, API tokens) are accepted or shown exactly once and never returned.
type Server struct {
	Svc  *service.Service
	Sync *syncer.Manager
	Auth *auth.Store
	Log  *slog.Logger
	Hub  *Hub
	// UI serves the embedded web application for everything outside /api/ (nil = API only).
	UI http.Handler
	// AgentListen is the agent API listen address (for the onboarding command).
	AgentListen string
	// Notify is the alert/notification engine (created on first use when nil).
	Notify *notify.Engine
	// WebCert reports the web listener certificate (nil = unknown).
	WebCert func() any
	// Secure marks cookies Secure and adds HSTS (the web listener is TLS).
	Secure bool
	// Tasks runs long operations as tasks (created on first use when nil; the daemon shares one with the scheduler).
	Tasks *tasks.Manager
	// Ops holds the operations extras (log ring, listeners, metrics, update check; see routes_system.go).
	Ops Ops

	taskOnce  sync.Once
	statsOnce sync.Once
	updOnce   sync.Once

	throttle *ipLimiter
	audit    *auditAgg // security audit: refusals, deliveries, raw reads (audit.go)
	once     sync.Once
	oidcOnce sync.Once // single sign-on (routes_authn.go)
	oidcRP   *auth.OIDC
}

func (s *Server) init() {
	s.once.Do(func() {
		if s.Hub == nil {
			s.Hub = NewHub()
		}
		if s.Hub.Resolve == nil {
			s.Hub.Resolve = s.eventRepo
		}
		if s.Log == nil {
			s.Log = slog.Default()
		}
		s.throttle = newIPLimiter(20, time.Minute)
		s.audit = newAuditAgg(s.writeAudit)
		if s.Notify == nil {
			s.Notify = NewNotifyEngine(s.Svc)
		}
	})
}

// routeFn registers one route; its permission comes from the table in perms.go.
type routeFn func(pattern string, fn http.HandlerFunc)

type ctxKey struct{}

// PrincipalFrom returns who the request acts as.
func PrincipalFrom(ctx context.Context) auth.Principal {
	p, _ := ctx.Value(ctxKey{}).(auth.Principal)
	return p
}

// LocalHandler is the API for the unix admin socket.
func (s *Server) LocalHandler() http.Handler {
	s.init()
	return s.build(func(next http.Handler, _ string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, auth.Local)))
		})
	}, false)
}

// WebHandler is the API plus the UI for the HTTPS listener.
func (s *Server) WebHandler() http.Handler {
	s.init()
	api := s.build(s.authn, true)
	root := http.NewServeMux()
	root.Handle("/api/", api)
	if s.UI != nil {
		root.Handle("/", s.UI)
	}
	return s.secureHeaders(root)
}

// Serve listens on a unix socket (mode 0660) until ctx ends.
func (s *Server) Serve(ctx context.Context, path string) error {
	s.init()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.LocalHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		_ = os.Remove(path)
	}()
	err = srv.Serve(ln)
	s.FlushAudit() // pending security summaries are written before the daemon ends
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler is kept for callers that want the local API as a plain handler.
func (s *Server) Handler() http.Handler { return s.LocalHandler() }

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.Secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

const cookieName = "ib_session"

// publicRoutes need no login (they carry their own checks).
var publicRoutes = map[string]bool{
	"POST /api/v1/session": true,
	"GET /api/v1/setup":    true,
	"POST /api/v1/setup":   true,
	"GET /api/v1/ping":     true,
}

type authnFn func(next http.Handler, pattern string) http.Handler

// authn resolves the caller of a web request: Bearer token first, then the session cookie.
func (s *Server) authn(next http.Handler, pattern string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicRoutes[pattern] {
			// login and setup carry no CSRF token: refuse what a cross-site page can send without a
			// preflight (a foreign Origin, or a form body: text/plain, urlencoded, multipart), so another site
			// cannot sign a browser in to an account of its choosing (login CSRF)
			if unsafeMethod(r.Method) && (!sameOrigin(r) || !jsonOrNone(r)) {
				s.auditRefused(r, auth.Principal{}, pattern, "cross-site request")
				writeJSON(w, http.StatusForbidden, api.Error{Error: "cross-site request refused: send JSON from this origin"})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		var p *auth.Principal
		var err error
		bearer := false
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			bearer = true
			p, err = s.Auth.AuthenticateToken(r.Context(), strings.TrimPrefix(h, "Bearer "))
		} else if c, cerr := r.Cookie(cookieName); cerr == nil {
			p, err = s.Auth.Authenticate(r.Context(), c.Value)
		} else {
			err = auth.ErrNoSession
		}
		if err != nil {
			if errors.Is(err, auth.ErrNoSession) {
				writeJSON(w, http.StatusUnauthorized, api.Error{Error: "not signed in"})
				return
			}
			s.fail(w, r, err)
			return
		}
		if !bearer && unsafeMethod(r.Method) {
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(p.CSRF)) != 1 {
				s.auditRefused(r, *p, pattern, "csrf")
				writeJSON(w, http.StatusForbidden, api.Error{Error: "missing or wrong CSRF token"})
				return
			}
		}
		if !s.enrolGate(w, r, p, pattern) { // a session that must first set up a second factor (routes_authn.go)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, *p)
		ctx = context.WithValue(ctx, recheckKey{}, func(c context.Context) bool { // long-lived streams re-check the caller
			var err error
			if bearer {
				_, err = s.Auth.AuthenticateToken(c, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			} else if ck, cerr := r.Cookie(cookieName); cerr == nil {
				_, err = s.Auth.Recheck(c, ck.Value) // not activity: an open stream must not renew the idle timeout
			} else {
				err = auth.ErrNoSession
			}
			return err == nil
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recheckKey holds a function that re-validates the caller of a web request (nil on the admin socket).
type recheckKey struct{}

// jsonOrNone reports whether the body is declared as JSON (or not declared at all, which no browser form does).
func jsonOrNone(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// sameOrigin rejects cross-site browser requests: when Origin is present its host must be ours.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

type statusWriter struct {
	http.ResponseWriter
	code int
	n    int64 // body bytes written
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// build assembles the mux. Every route must have an entry in perms (a missing one panics at start-up,
// so a new route can never ship without a role decision). Mutating requests are written to the audit log.
func (s *Server) build(authn authnFn, web bool) http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn http.HandlerFunc) {
		perm, ok := perms[pattern]
		if !ok {
			panic("mgmt: route without a permission entry: " + pattern)
		}
		pat := pattern
		public := web && publicRoutes[pat]
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !public {
				var ok bool
				orig := r
				gate := &statusWriter{ResponseWriter: w}
				if r, ok = s.authorize(gate, r, pat, perm); !ok { // repository scopes: scope.go
					if gate.code == http.StatusForbidden {
						pp := PrincipalFrom(orig.Context())
						s.auditRefused(orig, pp, pat, "role "+string(pp.Role))
					}
					return
				}
			}
			p := PrincipalFrom(r.Context())
			sw := &statusWriter{ResponseWriter: w}
			t0 := time.Now()
			fn(sw, r)
			s.httpStats().Observe(pat, r.Method, sw.status(), time.Since(t0))
			if r.Method == http.MethodGet {
				s.auditRead(r, p, pat, sw) // deliveries and raw repository material only
			}
			if unsafeMethod(r.Method) && !strings.HasPrefix(pat, "POST /api/v1/session") && s.Auth != nil {
				_ = s.Auth.Audit(context.WithoutCancel(r.Context()), auth.AuditEntry{Actor: p.Name(), Kind: p.Kind, Action: auditAction(pat),
					Target: r.URL.Path, IP: clientIP(r), OK: sw.code < 400})
			}
		})
		var handler http.Handler = inner
		if m := apiRoutes[pattern]; m.Bind == bindList && m.ListKey != "" {
			handler = filterList(inner, m.ListKey, perm)
		}
		mux.Handle(pattern, authn(handler, pattern))
	}
	s.coreRoutes(h)
	s.authRoutes(h)
	s.authnRoutes(h)
	s.dashRoutes(h)
	s.moreRoutes(h)
	s.jobRoutes(h)
	s.certRoutes(h)
	s.notifyRoutes(h)
	s.configRoutes(h)
	s.infraRoutes(h)
	s.storageRoutes(h)
	s.protectRoutes(h)
	s.accessRoutes(h)
	s.trafficRoutes(h)
	s.taskRoutes(h)
	s.systemRoutes(h)
	s.lifecycleRoutes(h)
	return mux
}

func auditAction(pattern string) string {
	_, path, _ := strings.Cut(pattern, " ")
	method, _, _ := strings.Cut(pattern, " ")
	return strings.ToLower(method) + " " + strings.TrimPrefix(path, "/api/v1")
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// ---- per-IP limiter for login and setup ---------------------------------------------------

type ipLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newIPLimiter(max int, window time.Duration) *ipLimiter {
	return &ipLimiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	keep := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.max {
		l.hits[ip] = keep
		return false
	}
	l.hits[ip] = append(keep, now)
	if len(l.hits) > 10000 { // bound memory
		for k, v := range l.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > l.window {
				delete(l.hits, k)
			}
		}
	}
	return true
}
