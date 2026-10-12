package mgmt

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/traffic"
)

// Traffic control (docs/traffic.md): rate-limit rules of the agent API and the live throughput per host.
func init() {
	perms["GET /api/v1/traffic/rules"] = rd
	perms["POST /api/v1/traffic/rules"] = ad
	perms["PUT /api/v1/traffic/rules/{name}"] = ad
	perms["DELETE /api/v1/traffic/rules/{name}"] = ad
	perms["GET /api/v1/traffic/current"] = rd
	apiRoutes["GET /api/v1/traffic/rules"] = apiRoute{Summary: "Rate-limit rules of the agent API", Tag: "Traffic", Resp: []traffic.Rule{}}
	apiRoutes["POST /api/v1/traffic/rules"] = apiRoute{Summary: "Add a rate-limit rule", Tag: "Traffic", Body: trafficRuleInput{}, Resp: traffic.Rule{}, Status: 201}
	apiRoutes["PUT /api/v1/traffic/rules/{name}"] = apiRoute{Summary: "Replace a rate-limit rule (the body may rename it)", Tag: "Traffic", Body: trafficRuleInput{}, Resp: traffic.Rule{}}
	apiRoutes["DELETE /api/v1/traffic/rules/{name}"] = apiRoute{Summary: "Delete a rate-limit rule", Tag: "Traffic"}
	apiRoutes["GET /api/v1/traffic/current"] = apiRoute{Summary: "Live throughput per host (5-second average) and which rules are active now", Tag: "Traffic", Resp: TrafficCurrent{}}
}

type trafficRuleInput struct {
	Name      string `json:"name"`
	Enabled   *bool  `json:"enabled,omitempty" doc:"default true"`
	Scope     string `json:"scope" doc:"host or global"`
	Hosts     string `json:"hosts,omitempty" doc:"comma-separated globs (default *)"`
	Direction string `json:"direction" doc:"upload or download"`
	RateBps   int64  `json:"rate_bps"`
	Burst     int64  `json:"burst_bytes,omitempty"`
	Schedule  string `json:"schedule,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

func (in trafficRuleInput) rule() traffic.Rule {
	en := in.Enabled == nil || *in.Enabled
	return traffic.Rule{Name: in.Name, Enabled: en, Scope: in.Scope, Hosts: in.Hosts, Direction: in.Direction, RateBps: in.RateBps,
		Burst: in.Burst, Schedule: in.Schedule, Timezone: in.Timezone, Comment: in.Comment}
}

// TrafficCurrent is GET /traffic/current.
type TrafficCurrent struct {
	At    time.Time            `json:"at"`
	Hosts []traffic.HostRate   `json:"hosts"`
	Rules []traffic.ActiveRule `json:"rules"`
}

func trafficErr(err error) error {
	switch {
	case errors.Is(err, traffic.ErrBad):
		return fmt.Errorf("%w: %v", service.ErrBadRequest, err)
	case errors.Is(err, traffic.ErrNotFound):
		return fmt.Errorf("%w: traffic rule", service.ErrNotFound)
	case errors.Is(err, traffic.ErrExists):
		return fmt.Errorf("%w: %v", service.ErrConflict, err)
	}
	return err
}

func (s *Server) trafficRoutes(h routeFn) {
	store := func() traffic.Store { return traffic.Store{Pool: s.Svc.DB.Pool()} }
	lim := func() *traffic.Limiter { return traffic.For(s.Svc.DB.Pool()) }
	h("GET /api/v1/traffic/rules", func(w http.ResponseWriter, r *http.Request) {
		l, err := store().List(r.Context())
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/traffic/rules", func(w http.ResponseWriter, r *http.Request) {
		var in trafficRuleInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		x, err := store().Create(r.Context(), in.rule())
		if err != nil {
			s.fail(w, r, trafficErr(err))
			return
		}
		lim().Invalidate()
		writeJSON(w, http.StatusCreated, x)
	})
	h("PUT /api/v1/traffic/rules/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in trafficRuleInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		x, err := store().Update(r.Context(), r.PathValue("name"), in.rule())
		if err != nil {
			s.fail(w, r, trafficErr(err))
			return
		}
		lim().Invalidate()
		writeJSON(w, 200, x)
	})
	h("DELETE /api/v1/traffic/rules/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := store().Delete(r.Context(), r.PathValue("name")); err != nil {
			s.fail(w, r, trafficErr(err))
			return
		}
		lim().Invalidate()
		w.WriteHeader(http.StatusNoContent)
	})
	h("GET /api/v1/traffic/current", func(w http.ResponseWriter, r *http.Request) {
		l := lim()
		writeJSON(w, 200, TrafficCurrent{At: l.Now().UTC(), Hosts: l.Current(), Rules: l.Rules(r.Context())})
	})
}
