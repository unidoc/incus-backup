package mgmt

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/schedule"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

// Routes for the storage and protection-configuration pages of the web console (docs/api-storage.md):
// connection checks before a repository is created, a repository's identity, the effective retention
// policy of every object, the sync run history and schedule, a sync peer check that does not spend
// the token, and a schedule preview evaluated by the server's own clock and parser.
func init() {
	perms["POST /api/v1/repositories/check"] = ad // takes an inline secret and reveals server paths
	perms["POST /api/v1/repositories/{name}/check"] = op
	perms["GET /api/v1/repositories/{name}/info"] = rd
	perms["GET /api/v1/policies/effective"] = rd
	perms["GET /api/v1/sync/{name}/runs"] = rd
	perms["PUT /api/v1/sync/{name}/schedule"] = op
	perms["POST /api/v1/sync/check-remote"] = ad
	perms["GET /api/v1/schedule/preview"] = rd
}

// SchedulePreview is the answer of GET /schedule/preview.
type SchedulePreview struct {
	Spec   string `json:"spec"`
	Valid  bool   `json:"valid"`
	Error  string `json:"error,omitempty"`
	Manual bool   `json:"manual"`
	// Next firings in the server's local time zone (RFC 3339 with offset).
	Next []time.Time `json:"next"`
	// Zone is the server's time zone name (schedules of maintenance and sync jobs use it).
	Zone string `json:"zone"`
	// DueNow: with ?after=, the schedule fired since then and the job runs at the scheduler's next tick.
	DueNow bool `json:"due_now"`
}

func (s *Server) storageRoutes(h routeFn) {
	h("POST /api/v1/repositories/check", func(w http.ResponseWriter, r *http.Request) {
		var req service.RepoCheckRequest
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		rep, err := s.Svc.CheckRepositorySettings(long(r), req)
		s.reply(w, r, rep, err)
	})
	h("POST /api/v1/repositories/{name}/check", func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.Svc.CheckRepository(long(r), r.PathValue("name"))
		s.reply(w, r, rep, err)
	})
	h("GET /api/v1/repositories/{name}/info", func(w http.ResponseWriter, r *http.Request) {
		info, err := s.Svc.RepositoryInfo(r.Context(), r.PathValue("name"))
		s.reply(w, r, info, err)
	})
	h("GET /api/v1/policies/effective", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		es, err := s.Svc.EffectivePolicies(r.Context(), repo)
		s.reply(w, r, es, err)
	}))
	h("GET /api/v1/schedule/preview", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		n, _ := strconv.Atoi(q.Get("count"))
		if n <= 0 || n > 20 {
			n = 5
		}
		now := time.Now()
		if s.Svc.Now != nil {
			now = s.Svc.Now()
		}
		now = now.In(time.Local)
		out := SchedulePreview{Spec: q.Get("spec"), Zone: time.Local.String(), Next: []time.Time{}}
		if out.Zone == "Local" {
			out.Zone, _ = now.Zone()
		}
		sp, err := schedule.Parse(out.Spec)
		if err != nil {
			out.Error = err.Error()
			writeJSON(w, 200, out)
			return
		}
		out.Valid, out.Manual = true, sp.Manual()
		if !sp.Manual() {
			if a := q.Get("after"); a != "" {
				last, err := time.Parse(time.RFC3339, a)
				if err != nil {
					s.fail(w, r, fmt.Errorf("%w: after must be an RFC 3339 time", service.ErrBadRequest))
					return
				}
				out.DueNow = sp.Due(last, now)
			}
			t := now
			for i := 0; i < n; i++ {
				t = sp.Next(t)
				if t.IsZero() {
					break
				}
				out.Next = append(out.Next, t)
			}
		}
		writeJSON(w, 200, out)
	})
	if s.Sync == nil {
		return
	}
	h("GET /api/v1/sync/{name}/runs", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rs, err := s.Sync.Runs(r.Context(), repo, r.PathValue("name"), n)
		s.reply(w, r, rs, err)
	}))
	h("PUT /api/v1/sync/{name}/schedule", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req syncer.ScheduleUpdate
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Sync.SetSchedule(r.Context(), repo, r.PathValue("name"), req))
	}))
	h("POST /api/v1/sync/check-remote", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Remote string `json:"remote"`
			Token  string `json:"token"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if strings.TrimSpace(req.Remote) == "" {
			s.fail(w, r, fmt.Errorf("%w: remote is required (host:port of the other server's agent API)", service.ErrBadRequest))
			return
		}
		writeJSON(w, 200, syncer.CheckRemote(r.Context(), req.Remote, req.Token))
	})
}
