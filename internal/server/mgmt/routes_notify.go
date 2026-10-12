package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// ServiceCertStatus adapts the certificate report to the alert engine's cert_expiry condition.
func ServiceCertStatus(svc *service.Service) func(context.Context) ([]notify.CertItem, error) {
	return func(ctx context.Context) ([]notify.CertItem, error) {
		r, err := svc.CertificateStatus(ctx)
		if err != nil {
			return nil, err
		}
		out := []notify.CertItem{{Name: "ca", Subject: r.CA.Subject, NotAfter: r.CA.NotAfter}, {Name: "server", Subject: r.Server.Subject, NotAfter: r.Server.NotAfter}}
		if r.Rotation != nil {
			out = append(out, notify.CertItem{Name: "ca-next", Subject: r.Rotation.Next.Subject, NotAfter: r.Rotation.Next.NotAfter})
		}
		for _, p := range r.SyncPeers {
			out = append(out, notify.CertItem{Name: "sync:" + p.Repository + "/" + p.Job, Subject: p.Subject, NotAfter: p.NotAfter, Err: p.Error})
		}
		return out, nil
	}
}

// NewNotifyEngine builds the engine with its hooks wired (the daemon runs it; the API manages it).
func NewNotifyEngine(svc *service.Service) *notify.Engine {
	e := notify.New(svc)
	e.CertStatus = ServiceCertStatus(svc)
	e.RestoreTests = notify.DBRestoreTests{Pool: svc.DB.Pool()}
	return e
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: id must be a number", service.ErrBadRequest)
	}
	return id, nil
}

func (s *Server) notifyRoutes(h routeFn) {
	n := s.Notify

	// ---- channels
	h("GET /api/v1/notifications/channels", func(w http.ResponseWriter, r *http.Request) {
		l, err := n.ListChannels(r.Context())
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/notifications/channels", func(w http.ResponseWriter, r *http.Request) {
		var in notify.ChannelInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		c, err := n.CreateChannel(r.Context(), in)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, c)
	})
	h("PUT /api/v1/notifications/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in notify.ChannelInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		c, err := n.UpdateChannel(r.Context(), r.PathValue("id"), in)
		s.reply(w, r, c, err)
	})
	h("DELETE /api/v1/notifications/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.done(w, r, n.DeleteChannel(r.Context(), r.PathValue("id")))
	})
	h("POST /api/v1/notifications/channels/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		d, err := n.TestChannel(r.Context(), r.PathValue("id"))
		if err != nil { // the receiver's answer is the result the UI shows
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "delivery": d})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "delivery": d})
	})

	// ---- rules
	h("GET /api/v1/notifications/rules", func(w http.ResponseWriter, r *http.Request) {
		l, err := n.ListRules(r.Context())
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/notifications/rules", func(w http.ResponseWriter, r *http.Request) {
		in := notify.DefaultRule("")
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		x, err := n.CreateRule(r.Context(), in)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, x)
	})
	h("PUT /api/v1/notifications/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		in := notify.DefaultRule("")
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		x, err := n.UpdateRule(r.Context(), r.PathValue("id"), in)
		s.reply(w, r, x, err)
	})
	h("DELETE /api/v1/notifications/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.done(w, r, n.DeleteRule(r.Context(), r.PathValue("id")))
	})

	// ---- alerts, silences, deliveries
	h("GET /api/v1/alerts", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := notify.AlertFilter{Type: q.Get("type"), Severity: q.Get("severity"), Host: q.Get("host"), Active: q.Get("active") == "1"}
		if st := q.Get("state"); st != "" {
			f.States = splitComma(st)
		}
		f.Limit, _ = strconv.Atoi(q.Get("limit"))
		l, err := n.ListAlerts(r.Context(), f)
		s.reply(w, r, l, err)
	})
	h("GET /api/v1/alerts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		a, err := n.GetAlert(r.Context(), id)
		s.reply(w, r, a, err)
	})
	h("POST /api/v1/alerts/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		var req struct {
			Comment string `json:"comment"`
			Undo    bool   `json:"undo"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		who := PrincipalFrom(r.Context()).Name()
		var a *notify.Alert
		if req.Undo {
			a, err = n.Unacknowledge(r.Context(), id, who)
		} else {
			a, err = n.Acknowledge(r.Context(), id, who, req.Comment)
		}
		s.reply(w, r, a, err)
	})
	h("GET /api/v1/silences", func(w http.ResponseWriter, r *http.Request) {
		l, err := n.ListSilences(r.Context(), r.URL.Query().Get("all") == "1")
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/silences", func(w http.ResponseWriter, r *http.Request) {
		var in notify.Silence
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		in.Author = PrincipalFrom(r.Context()).Name()
		x, err := n.CreateSilence(r.Context(), in)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, x)
	})
	h("DELETE /api/v1/silences/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, n.Unsilence(r.Context(), id))
	})
	h("GET /api/v1/notifications/deliveries", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := notify.DeliveryFilter{ChannelID: q.Get("channel"), State: q.Get("state")}
		f.AlertID, _ = strconv.ParseInt(q.Get("alert"), 10, 64)
		f.Limit, _ = strconv.Atoi(q.Get("limit"))
		l, err := n.ListDeliveries(r.Context(), f)
		s.reply(w, r, l, err)
	})
	h("POST /api/v1/notifications/deliveries/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, n.RetryDelivery(r.Context(), id))
	})

	// ---- engine settings and thresholds
	h("GET /api/v1/notifications/settings", func(w http.ResponseWriter, r *http.Request) {
		st, err := n.GetSettings(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		cs, err := n.ListConditions(r.Context())
		s.reply(w, r, map[string]any{"settings": st, "conditions": cs}, err)
	})
	h("PUT /api/v1/notifications/settings", func(w http.ResponseWriter, r *http.Request) {
		var st notify.Settings
		if err := decode(r, &st); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, n.SetSettings(r.Context(), st))
	})
	h("PUT /api/v1/notifications/conditions/{type}", func(w http.ResponseWriter, r *http.Request) {
		var c struct {
			Enabled  bool            `json:"enabled"`
			Severity string          `json:"severity"`
			Params   json.RawMessage `json:"params"`
		}
		if err := decode(r, &c); err != nil {
			s.fail(w, r, err)
			return
		}
		s.done(w, r, n.SetCondition(r.Context(), notify.Condition{Type: r.PathValue("type"), Enabled: c.Enabled, Severity: c.Severity, Params: c.Params}))
	})
}

// BridgeNotify forwards alert changes to the SSE hub until ctx ends.
func (s *Server) BridgeNotify(ctx context.Context) {
	s.init()
	ch := s.Notify.Subscribe()
	go func() {
		defer s.Notify.Unsubscribe(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-ch:
				if !ok {
					return
				}
				s.Hub.Publish("alert", c)
			}
		}
	}()
}
