package mgmt

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/unidoc/incus-backup/internal/server/notify"
)

// processStarted is when this server process started (GET /about reports the uptime from it).
var processStarted = time.Now().UTC()

func init() {
	perms["GET /api/v1/about"] = rd
	perms["GET /api/v1/setup-checklist"] = ad
	perms["POST /api/v1/notifications/tick"] = ad
	perms["POST /api/v1/notifications/simulate"] = op
	perms["GET /api/v1/api-tokens"] = ad
	perms["DELETE /api/v1/api-tokens/{id}"] = ad
}

// About is GET /about: what runs and whether its dependencies answer.
type About struct {
	Version        string    `json:"version"`
	Started        time.Time `json:"started"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	GoVersion      string    `json:"go_version"`
	DatabaseOK     bool      `json:"database_ok"`
	DatabaseError  string    `json:"database_error,omitempty"`
	DatastoreCount int       `json:"datastore_count"`
	InstanceName   string    `json:"instance_name"`
}

// configRoutes: server information, the setup checklist, the notification engine's manual tick and
// simulator, and the administrator's overview of every API token.
func (s *Server) configRoutes(h routeFn) {
	h("GET /api/v1/about", func(w http.ResponseWriter, r *http.Request) {
		a := About{Version: s.Svc.Version, Started: processStarted, GoVersion: runtime.Version()}
		a.UptimeSeconds = int64(time.Since(processStarted).Seconds())
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.Svc.DB.Pool().Ping(ctx); err != nil {
			a.DatabaseError = err.Error()
		} else {
			a.DatabaseOK = true
			if repos, err := s.Svc.Repositories(ctx); err == nil {
				a.DatastoreCount = len(repos)
			}
			a.InstanceName = s.Svc.Setting(ctx, "instance_name")
		}
		writeJSON(w, 200, a)
	})
	h("GET /api/v1/setup-checklist", func(w http.ResponseWriter, r *http.Request) {
		c, err := s.Svc.SetupChecklist(r.Context())
		s.reply(w, r, c, err)
	})
	// One evaluation + delivery round now (the daemon does this every minute). For operations and tests:
	// "skipped" means another tick held the lock; nothing is evaluated twice.
	h("POST /api/v1/notifications/tick", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(long(r), 2*time.Minute)
		defer cancel()
		res, err := s.Notify.Tick(ctx)
		s.reply(w, r, res, err)
	})
	h("POST /api/v1/notifications/simulate", func(w http.ResponseWriter, r *http.Request) {
		var in notify.SimulateInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		res, err := s.Notify.Simulate(r.Context(), in)
		s.reply(w, r, res, err)
	})
	h("GET /api/v1/api-tokens", func(w http.ResponseWriter, r *http.Request) {
		l, err := s.Auth.AllTokens(r.Context())
		if err != nil {
			s.authFail(w, r, err)
			return
		}
		writeJSON(w, 200, l)
	})
	h("DELETE /api/v1/api-tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Auth.RevokeAnyToken(r.Context(), r.PathValue("id")); err != nil {
			s.authFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
