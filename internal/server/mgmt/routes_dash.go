package mgmt

import (
	"net/http"
	"strconv"
)

// dashRoutes registers the aggregated read models for the dashboard and the maintenance-mode switch.
func (s *Server) dashRoutes(h routeFn) {
	h("GET /api/v1/overview", func(w http.ResponseWriter, r *http.Request) {
		ov, err := s.Svc.OverviewFor(r.Context(), repoFilter(r))
		s.reply(w, r, ov, err)
	})
	h("GET /api/v1/capacity/series", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		pts, err := s.Svc.CapacitySeries(r.Context(), repo, days)
		s.reply(w, r, pts, err)
	}))
	h("PUT /api/v1/repositories/{name}/mode", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode   string `json:"mode"`
			Reason string `json:"reason"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, s.Svc.SetRepositoryMode(r.Context(), r.PathValue("name"), req.Mode, req.Reason))
		s.Hub.Publish("repository", map[string]string{"name": r.PathValue("name"), "mode": req.Mode})
	})
}
