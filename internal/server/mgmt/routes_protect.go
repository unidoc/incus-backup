package mgmt

import (
	"net/http"
	"strings"
)

// Routes of the "Protection & recovery" section of the web console (docs/api-protect.md).

func init() {
	perms["GET /api/v1/snapshot-stats"] = rd
}

func (s *Server) protectRoutes(h routeFn) {
	// The recorded statistics of every snapshot of a repository (or ?group=a,b): dirty/read/uploaded
	// bytes, full-read flag, duration, base snapshot and the latest verification. One query, so a
	// list of thousands of snapshots needs no per-snapshot call.
	h("GET /api/v1/snapshot-stats", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var groups []string
		for _, g := range strings.Split(r.URL.Query().Get("group"), ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}
		l, err := s.Svc.SnapshotStatsList(r.Context(), repo, groups)
		s.reply(w, r, l, err)
	}))
}
