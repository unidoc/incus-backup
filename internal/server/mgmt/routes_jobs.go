package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// jobPerm maps the job service's permission hint to a role.
func jobPerm(hint string) auth.Perm {
	switch hint {
	case "jobs.restore", "jobs.files":
		return auth.PermRestore
	}
	return auth.PermOperate
}

// StartBridges forwards the job service's live events to the SSE hub until ctx ends.
func (s *Server) StartBridges(ctx context.Context) {
	s.init()
	ch, cancel := jobs.For(s.Svc).Subscribe()
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				s.Hub.Publish("job", e)
				if e.State == "failed" {
					_ = s.Notify.Emit(ctx, notify.Event{Type: "job_failed", Key: e.JobID, Severity: "warning", Host: e.Host, Repository: e.Repository,
						Title: "Job " + e.Type + " failed on " + e.Host, Detail: e.Message})
				}
			}
		}
	}()
}

func (s *Server) jobRoutes(h routeFn) {
	js := func() *jobs.Service { return jobs.For(s.Svc) }

	h("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := jobs.JobFilter{Repository: q.Get("repository"), Host: q.Get("host"), Type: q.Get("type")}
		if st := q.Get("state"); st != "" {
			f.States = splitComma(st)
		}
		f.Limit, _ = strconv.Atoi(q.Get("limit"))
		l, err := js().ListJobs(r.Context(), f)
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type       string          `json:"type"`
			Host       string          `json:"host"`
			Repository string          `json:"repository"`
			Params     json.RawMessage `json:"params"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		p := PrincipalFrom(r.Context())
		hint := jobs.RequiredPermission(req.Type)
		if hint == "" {
			s.fail(w, r, fmt.Errorf("%w: unknown job type %q", service.ErrBadRequest, req.Type))
			return
		}
		if !p.Role.Allows(jobPerm(hint)) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "your role (" + string(p.Role) + ") may not start " + req.Type + " jobs"})
			return
		}
		repo := req.Repository
		if repo == "" {
			var err error
			if repo, err = s.repo(r); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		j, err := js().CreateJob(r.Context(), p.Name(), repo, req.Host, req.Type, req.Params)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, j)
	})
	h("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := js().GetJob(r.Context(), r.PathValue("id"))
		s.reply(w, r, j, err)
	})
	h("POST /api/v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		j, err := js().Cancel(r.Context(), PrincipalFrom(r.Context()).Name(), r.PathValue("id"))
		s.reply(w, r, j, err)
	})
	h("GET /api/v1/jobs/{id}/delivery", func(w http.ResponseWriter, r *http.Request) {
		rc, size, sum, err := js().OpenDelivery(r.Context(), r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("X-Content-SHA256", sum)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, rc)
	})

	h("GET /api/v1/restore-tests", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("latest") == "1" {
			repo, err := s.repo(r)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			l, err := js().LatestRestoreTestPerObject(r.Context(), repo)
			s.reply(w, r, l, err)
			return
		}
		f := jobs.RestoreTestFilter{Repository: q.Get("repository"), Host: q.Get("host"), Object: q.Get("object"), State: q.Get("state")}
		f.Limit, _ = strconv.Atoi(q.Get("limit"))
		l, err := js().ListRestoreTests(r.Context(), f)
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/restore-tests/run", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host       string `json:"host"`
			Repository string `json:"repository"`
			Project    string `json:"project"`
			Object     string `json:"object"`
			Snapshot   string `json:"snapshot"`
			Mode       string `json:"mode"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		repo := req.Repository
		if repo == "" {
			var err error
			if repo, err = s.repo(r); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		params, _ := json.Marshal(map[string]string{"project": req.Project, "object": req.Object, "snapshot": req.Snapshot, "mode": req.Mode})
		j, err := js().CreateJob(r.Context(), PrincipalFrom(r.Context()).Name(), repo, req.Host, "restore_test", params)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, j)
	})
}

// splitComma splits a comma list, dropping empty items. Linear: the input is a query parameter of any
// signed-in user (the old rune-by-rune string concatenation was quadratic, minutes of CPU for 1 MB).
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) certRoutes(h routeFn) {
	h("GET /api/v1/certificates", func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.Svc.CertificateStatus(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out := map[string]any{"report": rep}
		if s.WebCert != nil {
			out["web"] = s.WebCert()
		}
		writeJSON(w, 200, out)
	})
	h("PUT /api/v1/certificates/sans", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SANs []string `json:"sans"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.SetServerSANs(r.Context(), req.SANs))
	})
	h("POST /api/v1/certificates/ca/rotate", func(w http.ResponseWriter, r *http.Request) {
		fp, err := s.Svc.RotateCA(r.Context())
		s.reply(w, r, map[string]string{"new_ca_fingerprint": fp}, err)
	})
	h("POST /api/v1/certificates/ca/promote", func(w http.ResponseWriter, r *http.Request) {
		force := r.URL.Query().Get("force") == "1"
		s.done(w, r, s.Svc.PromoteCA(r.Context(), force))
	})
}
