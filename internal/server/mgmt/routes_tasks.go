package mgmt

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/tasks"
)

func init() {
	perms["GET /api/v1/tasks"] = rd
	perms["GET /api/v1/tasks/{id}"] = rd
	perms["POST /api/v1/tasks/{id}/cancel"] = op
}

// taskCancelPerm is the role needed to cancel a task of a kind: the role that may start it.
var taskCancelPerm = map[string]auth.Perm{
	"verify": op, "scrub": op, "sync": op, "probe": op, "capacity": op,
	"gc": ad, "retention": ad, "reconcile": ad, "repository_destroy": ad, "group_delete": ad,
}

// tasks returns the task manager (created on first use for servers built without one, e.g. tests).
func (s *Server) tasks() *tasks.Manager {
	s.init()
	s.taskOnce.Do(func() {
		if s.Tasks == nil {
			s.Tasks = &tasks.Manager{Pool: s.Svc.DB.Pool(), Log: s.Log, Now: s.Svc.Now}
		}
		if s.Tasks.Publish == nil {
			hub := s.Hub
			s.Tasks.Publish = func(t tasks.Task) { hub.Publish("task", t) }
		}
	})
	return s.Tasks
}

// TaskManager returns the server's task manager (creating it when none was wired).
func (s *Server) TaskManager() *tasks.Manager { return s.tasks() }

// Close cancels the tasks this server started and waits until they recorded their end.
func (s *Server) Close() {
	s.init()
	if s.Tasks != nil {
		s.Tasks.Close()
	}
}

// actor is who a request acts as, in task terms.
func actor(r *http.Request) (string, string) {
	p := PrincipalFrom(r.Context())
	switch p.Kind {
	case "user":
		return p.Name(), tasks.ActorUser
	case "token":
		return p.Name(), tasks.ActorToken
	}
	return "local", tasks.ActorLocal
}

// runTask starts fn as a task. Without ?wait=1 it answers 202 with the task (the client follows it with
// GET /tasks/{id} or the SSE "task" event). With ?wait=1 (the CLI's forwarding and older clients) it waits
// and answers with sync, exactly as the synchronous route did; the task is recorded either way and the
// work continues if the caller goes away.
func (s *Server) runTask(w http.ResponseWriter, r *http.Request, sp tasks.Spec, fn tasks.Func, sync func(res any, err error)) {
	sp.Actor, sp.ActorKind = actor(r)
	h, err := s.tasks().Start(r.Context(), sp, fn)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("X-Task-Id", h.ID)
	if r.URL.Query().Get("wait") == "1" {
		res, err := h.Wait(r.Context())
		if r.Context().Err() != nil {
			return // the caller left; the task goes on
		}
		sync(res, err)
		return
	}
	d, err := s.tasks().Get(r.Context(), h.ID, 0, 1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": d.Task})
}

// ---- maintenance routes as tasks (registered in routes_core.go) --------------------------------

func (s *Server) pruneRoute(w http.ResponseWriter, r *http.Request, repo string) {
	if r.URL.Query().Get("dry_run") == "1" { // a plan: read-only and quick enough to answer directly
		res, err := s.Svc.Prune(r.Context(), repo, true)
		s.reply(w, r, res, err)
		return
	}
	rp, err := s.Svc.Preflight(r.Context(), repo, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.runTask(w, r, tasks.Spec{Kind: "retention", RepositoryID: rp.ID, Repository: repo, Exclusive: true},
		func(ctx context.Context) (any, error) { return s.Svc.Prune(ctx, repo, false) },
		func(res any, err error) { s.reply(w, r, res, err) })
}

func (s *Server) gcRoute(w http.ResponseWriter, r *http.Request, repo string) {
	grace := time.Duration(0)
	if g := r.URL.Query().Get("grace_hours"); g != "" {
		f, _ := strconv.ParseFloat(g, 64)
		grace = time.Duration(f * float64(time.Hour))
	}
	if grace == 0 {
		m, err := s.Svc.Maintenance(r.Context(), repo)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		grace = m.GCGrace
	}
	if grace < service.MinGCGrace {
		s.fail(w, r, fmt.Errorf("%w: gc grace must be at least %s (it protects uploads whose session was lost)", service.ErrBadRequest, service.MinGCGrace))
		return
	}
	dry := r.URL.Query().Get("dry_run") == "1"
	rp, err := s.Svc.Preflight(r.Context(), repo, !dry)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.runTask(w, r, tasks.Spec{Kind: "gc", RepositoryID: rp.ID, Repository: repo, Exclusive: !dry, Params: map[string]any{"dry_run": dry, "grace_hours": grace.Hours()}},
		func(ctx context.Context) (any, error) { return s.Svc.GC(ctx, repo, grace, dry) },
		func(res any, err error) { s.reply(w, r, res, err) })
}

func (s *Server) verifyRoute(w http.ResponseWriter, r *http.Request, repo string) {
	var req struct {
		Mode         string `json:"mode"`
		StaleDays    int    `json:"stale_days"`
		NoQuarantine bool   `json:"no_quarantine"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	kind := "verify"
	switch req.Mode {
	case "", "all":
		kind = "scrub"
	case "new", "stale":
	default:
		s.fail(w, r, fmt.Errorf("%w: unknown verify mode %q (all, new or stale)", service.ErrBadRequest, req.Mode))
		return
	}
	rp, err := s.Svc.Preflight(r.Context(), repo, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vr := service.VerifyRequest{Mode: req.Mode, StaleDays: req.StaleDays, NoQuarantine: req.NoQuarantine}
	s.runTask(w, r, tasks.Spec{Kind: kind, RepositoryID: rp.ID, Repository: repo, Exclusive: !req.NoQuarantine, Params: req},
		func(ctx context.Context) (any, error) {
			vs, err := s.Svc.VerifyJob(ctx, repo, vr)
			if vs != nil && err != nil && strings.HasPrefix(err.Error(), "verify found") {
				// a finding is a result: the task succeeds and its result says what was found
				return map[string]any{"ok": false, "error": err.Error(), "stats": vs}, nil
			}
			if err != nil {
				return vs, err
			}
			return map[string]any{"ok": true, "stats": vs}, nil
		},
		func(res any, err error) { s.reply(w, r, res, err) })
}

func (s *Server) reconcileRoute(w http.ResponseWriter, r *http.Request, repo string) {
	rp, err := s.Svc.Preflight(r.Context(), repo, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.runTask(w, r, tasks.Spec{Kind: "reconcile", RepositoryID: rp.ID, Repository: repo},
		func(ctx context.Context) (any, error) { return s.Svc.Reconcile(ctx, repo) },
		func(res any, err error) { s.reply(w, r, res, err) })
}

func (s *Server) syncRunRoute(w http.ResponseWriter, r *http.Request, repo string) {
	name := r.PathValue("name")
	rp, err := s.Svc.Preflight(r.Context(), repo, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.Sync.Status(r.Context(), repo, name); err != nil { // unknown job: 404 now, not a failed task
		s.fail(w, r, err)
		return
	}
	type out struct {
		Ok     bool   `json:"ok"`
		Error  string `json:"error"`
		Result any    `json:"result"`
	}
	s.runTask(w, r, tasks.Spec{Kind: "sync", RepositoryID: rp.ID, Repository: repo, Params: map[string]string{"job": name}},
		func(ctx context.Context) (any, error) {
			res, err := s.Sync.Run(ctx, repo, name)
			if res != nil && err != nil {
				return out{false, err.Error(), res}, err
			}
			return res, err
		},
		func(res any, err error) {
			if o, ok := res.(out); ok && err != nil {
				writeJSON(w, 200, o)
				return
			}
			s.reply(w, r, res, err)
		})
}

// probeRoute runs the repository probe as a task (history, log) but answers synchronously as before.
func (s *Server) probeRoute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rp, err := s.Svc.Repository(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	who, kind := actor(r)
	_, res, err := s.tasks().Run(r.Context(), tasks.Spec{Kind: "probe", RepositoryID: rp.ID, Repository: name, Actor: who, ActorKind: kind},
		func(ctx context.Context) (any, error) { return s.Svc.ProbeRepository(ctx, name) })
	s.reply(w, r, res, err)
}

// taskRoutes: the unified task list, detail with log tailing, and cancel.
func (s *Server) taskRoutes(h routeFn) {
	h("GET /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		l, err := s.tasks().List(r.Context(), tasks.Filter{State: q.Get("state"), Kind: q.Get("kind"), Repository: q.Get("repository"),
			Actor: q.Get("started_by"), Limit: limit, Before: before})
		s.reply(w, r, l, err)
	})
	h("GET /api/v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		d, err := s.tasks().Get(r.Context(), r.PathValue("id"), after, limit)
		s.reply(w, r, d, err)
	})
	h("POST /api/v1/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.tasks().Get(r.Context(), r.PathValue("id"), 0, 1)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		need, ok := taskCancelPerm[d.Kind]
		if !ok {
			need = ad
		}
		if p := PrincipalFrom(r.Context()); !p.Role.Allows(need) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "your role (" + string(p.Role) + ") may not cancel a " + d.Kind + " task"})
			return
		}
		who, _ := actor(r)
		t, err := s.tasks().Cancel(r.Context(), d.ID, who)
		s.reply(w, r, t, err)
	})
}

func tasksFilterRecent() tasks.Filter { return tasks.Filter{Limit: 200} }
