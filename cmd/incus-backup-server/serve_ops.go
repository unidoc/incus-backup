package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/unidoc/incus-backup/internal/server/logring"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/sysinfo"
	"github.com/unidoc/incus-backup/internal/server/tasks"
	"github.com/unidoc/incus-backup/internal/server/updatecheck"
)

// installLogRing tees the daemon's log into an in-memory ring (GET /system/logs, support reports).
// Called before the service is built so every component logs through it.
func (a *app) installLogRing() *logring.Ring {
	ring := logring.New(5000)
	a.log = slog.New(logring.NewHandler(ring, a.log.Handler()))
	slog.SetDefault(a.log)
	return ring
}

// startTasks creates the daemon's task manager and fails the tasks a previous daemon left running.
// Only the daemon calls it, after taking the daemon lock.
func (a *app) startTasks(ctx context.Context, hub *mgmt.Hub) (*tasks.Manager, error) {
	// live "task" events are wired before anything (the scheduler) can start a task
	tm := &tasks.Manager{Pool: a.db.Pool(), Log: a.log, Now: a.svc.Now, Publish: func(t tasks.Task) { hub.Publish("task", t) }}
	n, err := tm.Recover(ctx)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		a.log.Warn("tasks interrupted by the previous shutdown were marked failed", "count", n)
	}
	go tm.Loop(ctx)
	return tm, nil
}

// updateCheck starts the opt-in release check and wires the update_available condition (call it before
// the notification engine runs).
func (a *app) updateCheck(ctx context.Context, ne *notify.Engine) *updatecheck.Checker {
	upd := &updatecheck.Checker{Current: version, Settings: a.svc.UpdateCheckSettings}
	go upd.Loop(ctx)
	ne.UpdateStatus = func(ctx context.Context) (string, string, string, bool) {
		st := upd.Status(ctx)
		return st.Latest, st.Current, st.ReleaseURL, st.Enabled && st.UpdateAvailable
	}
	return upd
}

// wireOps connects the operations extras (before any listener serves): system samples, the status page's
// listener list, the log ring, the update checker and the optional metrics listener.
func (a *app) wireOps(ctx context.Context, ms *mgmt.Server, upd *updatecheck.Checker, ring *logring.Ring) {
	cfg := a.cfg
	ls := []mgmt.Listener{{Name: "agent", Addr: cfg.Listen, Detail: "hosts and sync peers (mutual TLS)"}}
	if w := cfg.WebAddr(); w != "" {
		ls = append(ls, mgmt.Listener{Name: "web", Addr: w, Detail: "web UI and /api/v1 (HTTPS)"})
	}
	if s := cfg.AdminSocketPath(); s != "" {
		ls = append(ls, mgmt.Listener{Name: "admin", Addr: s, Detail: "local admin API (unix socket)"})
	}
	if cfg.MetricsListen != "" {
		d := "Prometheus /metrics (API token required)"
		if cfg.MetricsUnauthenticated {
			d = "Prometheus /metrics (no authentication)"
		}
		ls = append(ls, mgmt.Listener{Name: "metrics", Addr: cfg.MetricsListen, Detail: d})
	}
	ms.Ops = mgmt.Ops{Logs: ring, Listeners: ls, Config: cfg.Redacted, Update: upd}
	go (&sysinfo.Sampler{Pool: a.db.Pool(), StateDir: cfg.StateDir, Now: a.svc.Now}).Loop(ctx)
	if cfg.MetricsListen != "" {
		go a.serveMetrics(ctx, ms, cfg.MetricsListen, cfg.MetricsUnauthenticated)
	}
}

func (a *app) serveMetrics(ctx context.Context, ms *mgmt.Server, addr string, unauth bool) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		a.log.Error("metrics listener", "addr", addr, "err", err)
		return
	}
	srv := &http.Server{Handler: ms.MetricsHandler(unauth), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	a.log.Info("metrics listening", "addr", addr, "authenticated", !unauth)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Error("metrics listener stopped", "err", err)
	}
}
