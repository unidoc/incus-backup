// Package mgmt is the local administration API: JSON over a unix socket, for the management
// UI and for scripts. It is a thin layer over service.Service, which is also what the
// incus-backup-server CLI calls, so every operation has exactly one implementation and the CLI, the
// API and the UI cannot disagree.
//
// The socket is reachable only through filesystem permissions (mode 0660, owned by the service
// account): there are no passwords and no network exposure. Secrets (S3 keys, enrolment tokens)
// are accepted or shown exactly once and are never returned again.
package mgmt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

const maxBody = 1 << 20

// ---- helpers ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := http.StatusInternalServerError, err.Error()
	switch {
	case errors.Is(err, service.ErrNotFound), errors.Is(err, datastore.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, service.ErrConflict), errors.Is(err, datastore.ErrExists), errors.Is(err, datastore.ErrProtected), errors.Is(err, datastore.ErrForeignOwner):
		code = http.StatusConflict
	case errors.Is(err, service.ErrBadRequest), errors.Is(err, datastore.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, datastore.ErrReadOnly), errors.Is(err, datastore.ErrOffline):
		code = http.StatusServiceUnavailable
	case errors.Is(err, service.ErrForbidden), errors.Is(err, service.ErrUnauthorized):
		code = http.StatusForbidden
	}
	if code == http.StatusInternalServerError && s.Log != nil {
		s.Log.Error("admin request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeJSON(w, code, api.Error{Error: msg})
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: %v", service.ErrBadRequest, err)
	}
	if len(b) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", service.ErrBadRequest, err)
	}
	return nil
}

// repo resolves ?repository=NAME, defaulting to the only repository.
func (s *Server) repo(r *http.Request) (string, error) {
	if n := r.URL.Query().Get("repository"); n != "" {
		return n, nil
	}
	rs, err := s.Svc.Repositories(r.Context())
	if err != nil {
		return "", err
	}
	if len(rs) == 1 {
		return rs[0].Name, nil
	}
	return "", fmt.Errorf("%w: several repositories exist: pass ?repository=NAME", service.ErrBadRequest)
}

type repoHandler func(w http.ResponseWriter, r *http.Request, repo string)

func (s *Server) withRepo(fn repoHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !repoBound(r) { // fail closed: scope enforcement only knows routes declared repository-bound
			s.fail(w, r, errors.New("route is not declared repository-bound in routes_meta.go"))
			return
		}
		repo, err := s.repo(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		fn(w, r, repo)
	}
}

// ---- views (what leaves the server) ------------------------------------------------------

// RepoView is a repository without any credential material.
type RepoView struct {
	Name       string              `json:"name"`
	Backend    string              `json:"backend"`
	Location   string              `json:"location"`
	DomainID   string              `json:"domain_id,omitempty"`
	S3         *service.S3Settings `json:"s3,omitempty"`
	Credential string              `json:"credential,omitempty"`
	Mode       string              `json:"mode"`
	ModeReason string              `json:"mode_reason,omitempty"`
}

func (s *Server) view(ctx context.Context, r service.Repo, creds map[string]string) RepoView {
	v := RepoView{Name: r.Name, Backend: r.Backend, Location: r.Path, DomainID: r.DomainID, Mode: r.Mode, ModeReason: r.ModeReason}
	if r.Backend == service.BackendS3 {
		var set service.S3Settings
		if json.Unmarshal(r.S3Raw, &set) == nil {
			v.S3 = &set
		}
		v.Credential = creds[r.CredentialID]
	}
	return v
}

type capacityView struct {
	*service.Capacity
	GrowthPerDay map[string]*float64 `json:"growth_per_day"` // null: not enough data
	Note         string              `json:"note,omitempty"`
}

// coreRoutes registers the repository-management routes ported from the original admin API.
func (s *Server) coreRoutes(h routeFn) {
	h("GET /api/v1/status", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		st, err := s.Svc.Status(r.Context(), repo)
		s.reply(w, r, st, err)
	}))
	h("GET /api/v1/capacity", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		if err := s.Svc.SampleCapacityIfStale(r.Context(), repo, 30*time.Minute); err != nil {
			s.fail(w, r, err)
			return
		}
		c, err := s.Svc.Capacity(r.Context(), repo)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v := capacityView{Capacity: c, GrowthPerDay: map[string]*float64{}}
		for d, g := range c.GrowthPerDay {
			if math.IsNaN(g) {
				v.GrowthPerDay[strconv.Itoa(d)] = nil
			} else {
				x := g
				v.GrowthPerDay[strconv.Itoa(d)] = &x
			}
		}
		if c.FSTotal == 0 {
			v.Note = "free space and a fill forecast are not available for this storage (object store)"
		}
		writeJSON(w, 200, v)
	}))

	// ---- repositories and credentials
	h("GET /api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		rs, err := s.Svc.Repositories(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		creds, _ := s.Svc.Credentials(r.Context())
		names := map[string]string{}
		for _, c := range creds {
			names[c.ID] = c.Name
		}
		out := make([]RepoView, 0, len(rs))
		for _, x := range rs {
			out = append(out, s.view(r.Context(), x, names))
		}
		writeJSON(w, 200, out)
	})
	type createRepo struct {
		Name       string              `json:"name"`
		Kind       string              `json:"kind"` // filesystem | s3
		Path       string              `json:"path,omitempty"`
		S3         *service.S3Settings `json:"s3,omitempty"`
		Credential string              `json:"credential,omitempty"`
		AccessKey  string              `json:"access_key,omitempty"`
		SecretKey  string              `json:"secret_key,omitempty"`
		Adopt      bool                `json:"adopt,omitempty"`
	}
	mk := func(attach bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req createRepo
			if err := decode(r, &req); err != nil {
				s.fail(w, r, err)
				return
			}
			var repo *service.Repo
			var err error
			switch req.Kind {
			case "filesystem":
				if attach {
					repo, err = s.Svc.AttachRepository(r.Context(), req.Name, req.Path, req.Adopt)
				} else {
					repo, err = s.Svc.CreateRepository(r.Context(), req.Name, req.Path)
				}
			case "s3":
				if req.S3 == nil {
					s.fail(w, r, fmt.Errorf("%w: s3 settings are required", service.ErrBadRequest))
					return
				}
				p := service.S3Params{S3Settings: *req.S3, Credential: req.Credential, AccessKey: req.AccessKey, SecretKey: req.SecretKey}
				if attach {
					repo, err = s.Svc.AttachS3Repository(r.Context(), req.Name, p, req.Adopt)
				} else {
					repo, err = s.Svc.AddS3Repository(r.Context(), req.Name, p)
				}
			default:
				err = fmt.Errorf("%w: kind must be filesystem or s3", service.ErrBadRequest)
			}
			if err != nil {
				s.fail(w, r, err)
				return
			}
			writeJSON(w, http.StatusCreated, s.view(r.Context(), *repo, nil))
		}
	}
	h("POST /api/v1/repositories", mk(false))
	h("POST /api/v1/repositories/attach", mk(true))
	h("POST /api/v1/repositories/{name}/probe", s.probeRoute)
	h("GET /api/v1/credentials", func(w http.ResponseWriter, r *http.Request) {
		cs, err := s.Svc.Credentials(r.Context())
		s.reply(w, r, cs, err)
	})
	type credReq struct {
		Name      string `json:"name"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	h("POST /api/v1/credentials", func(w http.ResponseWriter, r *http.Request) {
		var req credReq
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		c, err := s.Svc.CreateS3Credential(r.Context(), req.Name, service.S3Secret{AccessKey: req.AccessKey, SecretKey: req.SecretKey})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, c)
	})
	h("PUT /api/v1/credentials/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req credReq
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.RotateS3Credential(r.Context(), r.PathValue("name"), service.S3Secret{AccessKey: req.AccessKey, SecretKey: req.SecretKey}))
	})
	h("DELETE /api/v1/credentials/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.done(w, r, s.Svc.DeleteCredential(r.Context(), r.PathValue("name")))
	})

	// ---- hosts and tokens
	h("GET /api/v1/hosts", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		hs, err := s.Svc.Hosts(r.Context(), repo)
		s.reply(w, r, hs, err)
	}))
	h("POST /api/v1/hosts/{name}/{scope}/{action}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		switch a := r.PathValue("action"); a {
		case "disable", "enable":
			s.done(w, r, s.Svc.SetHostDisabled(r.Context(), repo, r.PathValue("name"), r.PathValue("scope"), a == "disable"))
		default:
			s.fail(w, r, fmt.Errorf("%w: action must be enable or disable", service.ErrBadRequest))
		}
	}))
	h("POST /api/v1/tokens", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			Host     string  `json:"host"`
			Scope    string  `json:"scope"`
			TTLHours float64 `json:"ttl_hours"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		ttl := time.Duration(req.TTLHours * float64(time.Hour))
		if ttl <= 0 {
			ttl = time.Hour
		}
		tok, err := s.Svc.CreateToken(r.Context(), repo, req.Host, req.Scope, ttl)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_in_hours": ttl.Hours(), "note": "shown only now"})
	}))

	// ---- policies and assignments
	h("GET /api/v1/policies", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		ps, err := s.Svc.Policies(r.Context(), repo)
		s.reply(w, r, ps, err)
	}))
	h("PUT /api/v1/policies/{name}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var p api.Policy
		if err := decode(r, &p); err != nil {
			s.fail(w, r, err)
			return
		}
		p.Name = r.PathValue("name")
		s.done(w, r, s.Svc.SetPolicy(r.Context(), repo, p))
	}))
	h("DELETE /api/v1/policies/{name}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		s.done(w, r, s.Svc.DeletePolicy(r.Context(), repo, r.PathValue("name")))
	}))
	h("GET /api/v1/assignments", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		as, err := s.Svc.Assignments(r.Context(), repo)
		s.reply(w, r, as, err)
	}))
	h("PUT /api/v1/assignments", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var a service.AssignmentRow
		if err := decode(r, &a); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.Assign(r.Context(), repo, a.Host, a.Project, a.Object, a.Policy))
	}))
	h("DELETE /api/v1/assignments", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var a service.AssignmentRow
		if err := decode(r, &a); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.Unassign(r.Context(), repo, a.Host, a.Project, a.Object))
	}))

	// ---- objects and snapshots
	h("GET /api/v1/objects", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		os, err := s.Svc.ObjectRows(r.Context(), repo)
		s.reply(w, r, os, err)
	}))
	h("PUT /api/v1/objects/{group}/retention", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			Policy string `json:"policy"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.SetObjectRetentionByGroup(r.Context(), repo, r.PathValue("group"), req.Policy))
	}))
	h("POST /api/v1/objects/{group}/transfer", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			Host string `json:"host"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.TransferObject(r.Context(), repo, r.PathValue("group"), req.Host))
	}))
	h("PUT /api/v1/objects/{group}/notes", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			Notes string `json:"notes"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.SetGroupNotes(r.Context(), repo, r.PathValue("group"), req.Notes))
	}))
	h("GET /api/v1/snapshots", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var groups []string
		if g := r.URL.Query().Get("group"); g != "" {
			groups = []string{g}
		}
		l, err := s.Svc.AdminSnapshots(r.Context(), repo, groups)
		s.reply(w, r, l, err)
	}))
	h("PUT /api/v1/snapshots/{group}/{snap}/protected", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			Protected bool `json:"protected"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.SetSnapshotProtected(r.Context(), repo, r.PathValue("group"), r.PathValue("snap"), req.Protected))
	}))
	h("PUT /api/v1/snapshots/{group}/{snap}/notes", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			Notes string `json:"notes"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.SetSnapshotNotes(r.Context(), repo, r.PathValue("group"), r.PathValue("snap"), req.Notes))
	}))
	h("GET /api/v1/snapshots/{group}/{snap}/verifications", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		hs, err := s.Svc.VerificationHistory(r.Context(), repo, r.PathValue("group"), r.PathValue("snap"))
		s.reply(w, r, hs, err)
	}))
	h("DELETE /api/v1/snapshots/{group}/{snap}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		s.done(w, r, s.Svc.ForgetSnapshot(r.Context(), repo, r.PathValue("group"), r.PathValue("snap")))
	}))

	// ---- housekeeping
	h("GET /api/v1/maintenance", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		m, err := s.Svc.Maintenance(r.Context(), repo)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, 200, struct {
			*service.MaintenanceSettings
			GCGraceHours float64 `json:"gc_grace_hours"`
		}{m, m.GCGrace.Hours()})
	}))
	h("PUT /api/v1/maintenance", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		cur, err := s.Svc.Maintenance(r.Context(), repo)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		var req struct {
			Capacity, Reconcile, Prune, GC, Verify, Scrub *string
			GCGraceHours                                  *float64 `json:"gc_grace_hours"`
			DefaultReverifyDays                           *int     `json:"default_reverify_days"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = *v
			}
		}
		set(&cur.Capacity, req.Capacity)
		set(&cur.Reconcile, req.Reconcile)
		set(&cur.Prune, req.Prune)
		set(&cur.GC, req.GC)
		set(&cur.Verify, req.Verify)
		set(&cur.Scrub, req.Scrub)
		if req.GCGraceHours != nil {
			cur.GCGrace = time.Duration(*req.GCGraceHours * float64(time.Hour))
		}
		if req.DefaultReverifyDays != nil {
			cur.DefaultReverifyDays = *req.DefaultReverifyDays
		}
		s.done(w, r, s.Svc.SetMaintenance(r.Context(), repo, *cur))
	}))
	h("GET /api/v1/runs", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rs, err := s.Svc.MaintenanceRuns(r.Context(), repo, n)
		s.reply(w, r, rs, err)
	}))
	h("POST /api/v1/prune", s.withRepo(s.pruneRoute))
	h("POST /api/v1/gc", s.withRepo(s.gcRoute))
	h("POST /api/v1/verify", s.withRepo(s.verifyRoute))
	h("POST /api/v1/reconcile", s.withRepo(s.reconcileRoute))
	h("GET /api/v1/corrupt", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		cs, err := s.Svc.CorruptChunks(r.Context(), repo, r.URL.Query().Get("healed") == "1")
		s.reply(w, r, cs, err)
	}))
	h("DELETE /api/v1/escrow", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		s.done(w, r, s.Svc.ClearEscrow(r.Context(), repo))
	}))

	// ---- sync
	if s.Sync != nil {
		h("GET /api/v1/sync", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
			js, err := s.Sync.Jobs(r.Context(), repo)
			s.reply(w, r, js, err)
		}))
		h("POST /api/v1/sync", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
			var req struct {
				syncer.Job
				Token string `json:"token"`
			}
			if err := decode(r, &req); err != nil {
				s.fail(w, r, err)
				return
			}
			s.done(w, r, s.Sync.AddJob(r.Context(), repo, req.Job, req.Token))
		}))
		h("PUT /api/v1/sync/{name}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
			var req syncer.JobOptions
			if err := decode(r, &req); err != nil {
				s.fail(w, r, err)
				return
			}
			s.done(w, r, s.Sync.SetOptions(r.Context(), repo, r.PathValue("name"), req))
		}))
		h("POST /api/v1/sync/{name}/run", s.withRepo(s.syncRunRoute))
		h("GET /api/v1/sync/{name}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
			st, err := s.Sync.Status(r.Context(), repo, r.PathValue("name"))
			s.reply(w, r, st, err)
		}))
		h("DELETE /api/v1/sync/{name}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
			s.done(w, r, s.Sync.RemoveJob(r.Context(), repo, r.PathValue("name")))
		}))
	}
}

// long returns a context for work that must finish even if the caller disconnects (a half-aborted GC,
// verify or sync leaves a run stuck as "running"): it keeps the values of the request but not its cancellation.
func long(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

func (s *Server) reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) done(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
