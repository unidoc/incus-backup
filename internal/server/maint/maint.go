// Package maint runs the server's automatic housekeeping: session reaping,
// capacity sampling, retention, garbage collection, verification, reconcile and
// scheduled synchronisation. Tick is pure with respect to time (it takes "now"
// from the service), so tests drive it with a fake clock.
package maint

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/schedule"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/server/tasks"
)

// Runner executes due jobs.
type Runner struct {
	Svc  *service.Service
	Sync *syncer.Manager
	// Tasks, when set, runs every scheduled job as a task (log, progress, cancel; see internal/server/tasks).
	Tasks *tasks.Manager

	mu      sync.Mutex
	running map[string]bool
}

func (r *Runner) last(ctx context.Context, repoID, kind string) time.Time {
	var t *time.Time
	_ = r.Svc.DB.Pool().QueryRow(ctx, `SELECT max(started_at) FROM maintenance_runs WHERE repository_id=$1 AND kind=$2 AND status IN ('ok','running','cancelled')`, repoID, kind).Scan(&t)
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Tick runs everything that is due at the service clock's "now".
func (r *Runner) Tick(ctx context.Context) {
	log := r.Svc.Log
	now := r.Svc.Now()
	r.Svc.ReapSessions(ctx)
	if _, err := jobs.For(r.Svc).Maintain(ctx); err != nil {
		log.Error("job housekeeping", "err", err)
	}
	repos, err := r.Svc.Repositories(ctx)
	if err != nil {
		log.Error("listing repositories", "err", err)
		return
	}
	for _, repo := range repos {
		type job struct {
			kind, spec string
			run        func(ctx context.Context) error
		}
		ms, err := r.Svc.Maintenance(ctx, repo.Name)
		if err != nil {
			log.Error("reading maintenance settings", "repo", repo.Name, "err", err)
			continue
		}
		jobs := []job{
			{"capacity", ms.Capacity, func(ctx context.Context) error { return r.Svc.SampleCapacity(ctx, repo.Name) }},
			{"reconcile", ms.Reconcile, func(ctx context.Context) error { _, err := r.Svc.Reconcile(ctx, repo.Name); return err }},
			{"retention", ms.Prune, func(ctx context.Context) error { _, err := r.Svc.Prune(ctx, repo.Name, false); return err }},
			{"gc", ms.GC, func(ctx context.Context) error { _, err := r.Svc.GC(ctx, repo.Name, ms.GCGrace, false); return err }},
			// verify new snapshots and re-verify the stale ones; "scrub" reads every chunk object
			{"verify", ms.Verify, func(ctx context.Context) error {
				_, err := r.Svc.VerifyJob(ctx, repo.Name, service.VerifyRequest{Mode: "stale"})
				return err
			}},
			{"scrub", ms.Scrub, func(ctx context.Context) error {
				_, err := r.Svc.VerifyJob(ctx, repo.Name, service.VerifyRequest{Mode: "all"})
				return err
			}},
		}
		for _, j := range jobs {
			sp, err := schedule.Parse(j.spec)
			if err != nil {
				continue
			}
			var last time.Time
			if j.kind == "capacity" {
				last = r.lastCapacity(ctx, repo.ID)
			} else {
				last = r.last(ctx, repo.ID, j.kind)
			}
			if !sp.Due(last, now) {
				continue
			}
			if err := r.runTask(ctx, repo, j.kind, j.run); err != nil {
				if errors.Is(err, tasks.ErrBusy) {
					log.Info("maintenance job skipped: the repository runs another destructive task", "repo", repo.Name, "job", j.kind, "err", err)
					continue
				}
				log.Error("maintenance job failed", "repo", repo.Name, "job", j.kind, "err", err)
			}
		}
		due, err := r.Sync.Due(ctx, repo.Name)
		if err != nil {
			log.Error("listing due sync jobs", "err", err)
			continue
		}
		for _, j := range due {
			r.startSync(ctx, repo.Name, j.Name)
		}
	}
}

// exclusiveKinds are the destructive scheduled jobs: at most one per repository at a time.
var exclusiveKinds = map[string]bool{"retention": true, "gc": true, "verify": true, "scrub": true}

// runTask runs one due job, as a task when a task manager is wired (the job's work then reports
// progress and can be cancelled from the UI); the run itself stays in maintenance_runs, linked to it.
func (r *Runner) runTask(ctx context.Context, repo service.Repo, kind string, run func(context.Context) error) error {
	if r.Tasks == nil {
		return run(ctx)
	}
	_, _, err := r.Tasks.Run(ctx, tasks.Spec{Kind: kind, RepositoryID: repo.ID, Repository: repo.Name, Exclusive: exclusiveKinds[kind],
		Params: map[string]string{"scheduled": "true"}}, func(tctx context.Context) (any, error) { return nil, run(tctx) })
	return err
}

// startSync runs a due sync job in its own goroutine: one slow or hung peer must not stall
// the housekeeping of every repository, and a job never runs twice at once.
func (r *Runner) startSync(ctx context.Context, repo, name string) {
	key := repo + "/" + name
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	if r.running[key] {
		r.mu.Unlock()
		return
	}
	r.running[key] = true
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.running, key)
			r.mu.Unlock()
		}()
		jctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
		defer cancel()
		var res *syncer.Result
		run := func(ctx context.Context) error {
			var err error
			res, err = r.Sync.Run(ctx, repo, name)
			return err
		}
		var err error
		if r.Tasks != nil {
			var rid string
			if rp, rerr := r.Svc.Repository(jctx, repo); rerr == nil {
				rid = rp.ID
			}
			_, _, err = r.Tasks.Run(jctx, tasks.Spec{Kind: "sync", RepositoryID: rid, Repository: repo, Params: map[string]string{"job": name}},
				func(ctx context.Context) (any, error) { err := run(ctx); return res, err })
		} else {
			err = run(jctx)
		}
		if err != nil {
			r.Svc.Log.Error("sync failed", "repo", repo, "job", name, "err", err, "result", res)
		}
	}()
}

func (r *Runner) lastCapacity(ctx context.Context, repoID string) time.Time {
	var t *time.Time
	_ = r.Svc.DB.Pool().QueryRow(ctx, `SELECT max(taken_at) FROM capacity_samples WHERE repository_id=$1`, repoID).Scan(&t)
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Loop ticks once a minute until ctx ends.
func (r *Runner) Loop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	r.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}
