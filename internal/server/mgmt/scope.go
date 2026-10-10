package mgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// Per-repository access (docs/permissions.md), enforced in ONE place: authorize runs for every route
// before its handler, finds the repository the request touches the way the route's metadata says
// (routes_meta.go), and checks the principal's role ON THAT repository. List routes are filtered per
// item afterwards (filterList), the event stream per subscriber (hub.go).
//
// Unscoped principals (no auth.Access: the local socket, users without repository roles and their
// unrestricted tokens) are decided exactly as before scopes existed: their role against the route.

func init() {
	perms["GET /api/v1/users/{name}/access"] = ad
	perms["PUT /api/v1/users/{name}/access"] = ad
	perms["GET /api/v1/openapi.json"] = rd
}

func forbidden(w http.ResponseWriter, role auth.Role) {
	writeJSON(w, http.StatusForbidden, api.Error{Error: "your role (" + string(role) + ") may not do this"})
}

// authorize decides a non-public request. It returns the request to hand to the handler (with the
// effective role in the principal and, for repository-bound routes, the resolved repository pinned in
// ?repository= so the handler resolves exactly the repository that was checked), or false after
// writing the refusal.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, pattern string, perm auth.Perm) (*http.Request, bool) {
	p := PrincipalFrom(r.Context())
	meta, ok := apiRoutes[pattern]
	if !ok {
		// cannot happen (TestEveryRouteHasMetadata); fail closed for scoped principals anyway
		meta.Bind = bindGlobal
		if p.Scoped() {
			forbidden(w, auth.None)
			return nil, false
		}
	}
	if meta.Bind == bindQuery {
		r = r.WithContext(context.WithValue(r.Context(), repoBoundKey{}, true))
	}
	if !p.Scoped() {
		if meta.Bind == bindSelf && perm == auth.PermRead {
			return r, true
		}
		if !p.Role.Allows(perm) {
			forbidden(w, p.Role)
			return nil, false
		}
		return r, true
	}
	withRole := func(r *http.Request, role auth.Role) *http.Request {
		q := p
		q.Role = role
		return r.WithContext(context.WithValue(r.Context(), ctxKey{}, q))
	}
	switch meta.Bind {
	case bindSelf:
		if perm == auth.PermRead || p.Role.Allows(perm) {
			return r, true
		}
		forbidden(w, p.Role)
		return nil, false
	case bindGlobal:
		if !p.Role.Allows(perm) {
			forbidden(w, p.Role)
			return nil, false
		}
		return r, true
	case bindList:
		if !p.MaxRole().Allows(perm) {
			forbidden(w, p.MaxRole())
			return nil, false
		}
		return r, true
	case bindQuery, bindPath, bindBody:
		var repo string
		var err error
		switch meta.Bind {
		case bindPath:
			repo = r.PathValue("name")
		case bindBody:
			repo, err = s.bodyRepo(r)
		default:
			repo, err = s.repo(r)
		}
		if err != nil {
			s.fail(w, r, err)
			return nil, false
		}
		role := p.RoleFor(repo)
		if !role.Allows(perm) {
			writeJSON(w, http.StatusForbidden, api.Error{Error: "you have no " + permName(perm) + " access to repository " + strconv.Quote(repo)})
			return nil, false
		}
		if meta.Bind != bindPath {
			r = pinRepo(r, repo)
		}
		return withRole(r, role), true
	case bindJob:
		j, err := jobs.For(s.Svc).GetJob(r.Context(), r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return nil, false
		}
		role := p.RoleFor(j.Repository)
		if !role.Allows(auth.PermRead) {
			writeJSON(w, http.StatusNotFound, api.Error{Error: "not found: job " + strconv.Quote(r.PathValue("id"))})
			return nil, false
		}
		if !role.Allows(perm) {
			forbidden(w, role)
			return nil, false
		}
		return withRole(r, role), true
	case bindAlert:
		id, err := pathID(r)
		if err != nil {
			s.fail(w, r, err)
			return nil, false
		}
		a, err := s.Notify.GetAlert(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return nil, false
		}
		role := p.Role // an alert without a repository is global
		if a.Alert.Repository != "" {
			role = p.RoleFor(a.Alert.Repository)
		}
		if !role.Allows(auth.PermRead) {
			writeJSON(w, http.StatusNotFound, api.Error{Error: "not found: alert " + strconv.Quote(r.PathValue("id"))})
			return nil, false
		}
		if !role.Allows(perm) {
			forbidden(w, role)
			return nil, false
		}
		return withRole(r, role), true
	}
	forbidden(w, auth.None)
	return nil, false
}

type repoBoundKey struct{}

// repoBound reports whether authorize treated the request as bound to ?repository= (withRepo refuses
// to run otherwise, so a route registered with withRepo but declared global cannot bypass scopes).
func repoBound(r *http.Request) bool { b, _ := r.Context().Value(repoBoundKey{}).(bool); return b }

func permName(p auth.Perm) string {
	switch p {
	case auth.PermRead:
		return "read"
	case auth.PermRestore:
		return "restore"
	case auth.PermOperate:
		return "operator"
	}
	return "admin"
}

// pinRepo sets ?repository= to the checked repository.
func pinRepo(r *http.Request, repo string) *http.Request {
	r2 := r.Clone(r.Context())
	u := *r.URL
	q := u.Query()
	q.Set("repository", repo)
	u.RawQuery = q.Encode()
	r2.URL = &u
	r2.Body = r.Body
	return r2
}

// bodyRepo reads the JSON body's "repository" field (else the query/default repository) and puts the
// body back for the handler.
func (s *Server) bodyRepo(r *http.Request) (string, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return "", errors.Join(service.ErrBadRequest, err)
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	var v struct {
		Repository string `json:"repository"`
	}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &v); err != nil {
			return "", errors.Join(service.ErrBadRequest, err)
		}
	}
	if v.Repository != "" {
		return v.Repository, nil
	}
	return s.repo(r)
}

// listWriter buffers a list response so items of repositories the principal may not see can be removed.
type listWriter struct {
	http.ResponseWriter
	code int
	buf  bytes.Buffer
}

func (l *listWriter) WriteHeader(c int)           { l.code = c }
func (l *listWriter) Write(b []byte) (int, error) { return l.buf.Write(b) }

// filterList wraps a bindList handler with a ListKey for scoped principals: the JSON array answer keeps
// only items whose repository (ListKey field) the principal may read with the route's permission; an
// item without a repository is kept only when the global role allows it.
func filterList(next http.Handler, key string, perm auth.Perm) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if !p.Scoped() || key == "" {
			next.ServeHTTP(w, r)
			return
		}
		lw := &listWriter{ResponseWriter: w}
		next.ServeHTTP(lw, r)
		if lw.code == 0 {
			lw.code = 200
		}
		var items []map[string]json.RawMessage
		if lw.code != 200 || json.Unmarshal(lw.buf.Bytes(), &items) != nil {
			if lw.code == 200 { // not an array: refuse rather than leak
				writeJSON(w, http.StatusInternalServerError, api.Error{Error: "list filter: unexpected answer shape"})
				return
			}
			w.WriteHeader(lw.code)
			_, _ = w.Write(lw.buf.Bytes())
			return
		}
		out := make([]map[string]json.RawMessage, 0, len(items))
		for _, it := range items {
			var repo string
			_ = json.Unmarshal(it[key], &repo)
			if visible(p, repo, perm) {
				out = append(out, it)
			}
		}
		writeJSON(w, 200, out)
	})
}

// visible: may p see an item of repository repo ("" = not repository-bound) with permission perm?
func visible(p auth.Principal, repo string, perm auth.Perm) bool {
	if repo == "" {
		return p.Role.Allows(perm)
	}
	return p.RoleFor(repo).Allows(perm)
}

// repoFilter is the overview's filter: nil for unscoped principals.
func repoFilter(r *http.Request) func(string) bool {
	p := PrincipalFrom(r.Context())
	if !p.Scoped() {
		return nil
	}
	return func(repo string) bool { return p.RoleFor(repo).Allows(auth.PermRead) }
}

// accessRoutes: the administrator's "Repository access" panel and the OpenAPI document.
func (s *Server) accessRoutes(h routeFn) {
	h("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		doc := OpenAPI(s.Svc.Version)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	if s.Auth == nil {
		return
	}
	h("GET /api/v1/users/{name}/access", func(w http.ResponseWriter, r *http.Request) {
		a, err := s.Auth.UserAccessOf(r.Context(), r.PathValue("name"))
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, a)
	})
	h("PUT /api/v1/users/{name}/access", func(w http.ResponseWriter, r *http.Request) {
		var req docAccessPut
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Auth.SetUserAccess(r.Context(), r.PathValue("name"), req.Scoped, req.Scopes); err != nil {
			s.authFail(w, r, err)
			return
		}
		a, err := s.Auth.UserAccessOf(r.Context(), r.PathValue("name"))
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, a)
	})
}

// eventRepo names the repository an SSE event belongs to ("" = global). Alerts carry only an id, so it
// is looked up (only when a scoped subscriber is connected).
func (s *Server) eventRepo(typ string, data json.RawMessage) string {
	var v struct {
		Repository string `json:"repository"`
		Name       string `json:"name"`
		AlertID    int64  `json:"alert_id"`
	}
	_ = json.Unmarshal(data, &v)
	switch typ {
	case "repository":
		return v.Name
	case "alert":
		if v.AlertID > 0 && s.Notify != nil {
			if a, err := s.Notify.GetAlert(context.Background(), v.AlertID); err == nil {
				return a.Alert.Repository
			}
			return "\x00unknown" // cannot tell: no scoped subscriber gets it
		}
	}
	return v.Repository
}
