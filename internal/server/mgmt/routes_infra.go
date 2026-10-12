package mgmt

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/unidoc/incus-backup/internal/server/service"
)

func init() {
	perms["GET /api/v1/runs/backups"] = rd
	perms["GET /api/v1/objects/stats"] = rd
	perms["GET /api/v1/onboarding/pending"] = ad
	perms["DELETE /api/v1/onboarding/pending/{id}"] = ad
}

// infraRoutes: read models for the hosts, objects and capacity pages (docs/api-infra.md).
func (s *Server) infraRoutes(h routeFn) {
	// Backup runs the hosts reported, newest first: ?host=&status=&before=<RFC 3339>&limit=
	h("GET /api/v1/runs/backups", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		q := r.URL.Query()
		f := service.BackupRunFilter{Host: q.Get("host"), Status: q.Get("status")}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				s.fail(w, r, fmt.Errorf("%w: limit must be a number", service.ErrBadRequest))
				return
			}
			f.Limit = n
		}
		if v := q.Get("before"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				s.fail(w, r, fmt.Errorf("%w: before must be an RFC 3339 time", service.ErrBadRequest))
				return
			}
			f.Before = t
		}
		l, err := s.Svc.BackupRuns(r.Context(), repo, f)
		s.reply(w, r, l, err)
	}))

	// Every storage object with freshness (the overview's rule), latest snapshot and size history: ?host=
	h("GET /api/v1/objects/stats", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		l, err := s.Svc.ObjectStats(r.Context(), repo, r.URL.Query().Get("host"))
		s.reply(w, r, l, err)
	}))

	// Enrolment tokens that were issued and not used yet (metadata only; the token was shown once).
	h("GET /api/v1/onboarding/pending", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		l, err := s.Svc.PendingEnrolments(r.Context(), repo)
		s.reply(w, r, l, err)
	}))
	h("DELETE /api/v1/onboarding/pending/{id}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		s.done(w, r, s.Svc.RevokeEnrolment(r.Context(), repo, r.PathValue("id")))
	}))
}
