package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/jobs"
	"github.com/unidoc/incus-backup/internal/client/state"
	"github.com/unidoc/incus-backup/internal/jobspec"
)

// Host jobs and automatic restore tests (docs/jobs.md).

type restoreTests struct {
	exec *jobs.Executor
	hist *jobs.History
}

// Executor returns the host-job executor of this App (built once from the configuration).
func (a *App) Executor() (*jobs.Executor, error) {
	if a.rt != nil {
		return a.rt.exec, nil
	}
	var allow map[string]bool
	if len(a.Cfg.Jobs.Allow) > 0 {
		allow = map[string]bool{}
		for _, t := range a.Cfg.Jobs.Allow {
			if _, ok := jobspec.Lookup(t); !ok {
				return nil, fmt.Errorf("jobs.allow: unknown job type %q (known: %v)", t, jobspec.Types)
			}
			allow[t] = true
		}
	}
	hist, err := jobs.LoadHistory(a.Cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("reading the restore-test history: %w", err)
	}
	x := &jobs.Executor{Eng: a.Eng, Log: a.Log, StateDir: a.Cfg.StateDir, Allow: allow,
		RecoveryImage: a.Cfg.Recovery.Image, RecoveryPool: a.Cfg.Recovery.Pool, RecoveryMemory: a.Cfg.Recovery.Memory, RecoveryCPU: a.Cfg.Recovery.CPU,
		PoolFor: a.poolFor, BackupNow: a.backupNow, Heavy: &a.heavy,
		OnRestoreTest: func(target string, res *jobspec.RestoreTestResult, _, _ time.Time) {
			if err := hist.Record(target, res, a.Eng.Now()); err != nil { // the engine clock, as the planner uses
				a.Log.Warn("could not record a restore test locally", "err", err)
			}
		}}
	a.rt = &restoreTests{exec: x, hist: hist}
	return x, nil
}

// StartJobs starts pulling jobs from every server in the background (unless jobs.disabled).
func (a *App) StartJobs(ctx context.Context) {
	if a.Cfg.Jobs.Disabled {
		a.Log.Info("host jobs are disabled (jobs.disabled in the configuration)")
		return
	}
	x, err := a.Executor()
	if err != nil {
		a.Log.Error("host jobs NOT started", "err", err)
		return
	}
	go x.Run(ctx)
}

func (a *App) poolFor(ctx context.Context, project, name string) string {
	objs, err := a.Plan(ctx)
	if err != nil {
		return ""
	}
	for _, o := range objs {
		if o.Project == project && o.Name == name {
			if o.Ref.Pool != "" {
				return o.Ref.Pool
			}
			if o.Instance != nil {
				return o.Instance.RootPool()
			}
		}
	}
	return ""
}

// backupNow runs a backup of a selection for a job. Unlike a scheduled run it does not move the
// policies' schedule (a one-object backup must not suppress the policy's next scheduled run).
func (a *App) backupNow(ctx context.Context, p jobspec.BackupNow) (any, error) {
	a.Refresh(ctx)
	objs, err := a.Plan(ctx)
	if err != nil {
		return nil, &engine.PublicError{Msg: "discovering the Incus objects failed", Err: err}
	}
	var sel []*engine.Object
	n := 0
	for _, o := range objs {
		if p.Project != "" && o.Project != p.Project {
			continue
		}
		sel = append(sel, o)
		if o.Skip != "" || (p.Policy != "" && o.Policy != p.Policy) || (p.Object != "" && o.Name != p.Object && o.OwnerName() != p.Object) {
			continue
		}
		n++
	}
	if n == 0 {
		return nil, &engine.PublicError{Msg: "nothing on this host matches the selection (unknown, excluded or without a policy)"}
	}
	opt := engine.RunOptions{}
	if p.Policy != "" {
		opt.Policies = []string{p.Policy}
	}
	if p.Object != "" {
		opt.Names = []string{p.Object}
	}
	rep := a.Eng.Run(ctx, sel, opt)
	a.Eng.ReportInventory(ctx, objs)
	now := a.Eng.Now()
	type objOut struct {
		Project  string `json:"project"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		OK       bool   `json:"ok"`
		Snapshot string `json:"snapshot,omitempty"`
		Full     bool   `json:"full_read,omitempty"`
		Logical  int64  `json:"logical_bytes"`
		Read     int64  `json:"read_bytes"`
	}
	out := struct {
		Objects []objOut `json:"objects"`
		Failed  int      `json:"failed"`
	}{Objects: []objOut{}}
	_ = a.State.Update(func(s *state.State) {
		for _, r := range rep.Results {
			o := s.Objects[r.Object.Key()]
			if o == nil {
				o = &state.ObjectState{}
				s.Objects[r.Object.Key()] = o
			}
			o.LastRun = now
			switch {
			case r.OK():
				o.LastStatus, o.LastError, o.LastSnap = "ok", "", r.Snapshot
			case r.Deferred():
				o.LastStatus, o.LastError = "deferred", firstError(r)
			default:
				o.LastStatus, o.LastError = "failed", firstError(r)
			}
		}
	})
	deferred := 0
	for _, r := range rep.Results {
		ok := r.OK()
		switch {
		case ok:
		case r.Deferred():
			deferred++
			a.Log.Warn("backup job: object deferred (repository in maintenance)", "object", r.Object.Key(), "err", firstError(r))
		default:
			out.Failed++
			a.Log.Error("backup job: object failed", "object", r.Object.Key(), "err", firstError(r))
		}
		out.Objects = append(out.Objects, objOut{Project: r.Object.Project, Name: r.Object.Name, Kind: r.Object.Kind, OK: ok, Snapshot: r.Snapshot,
			Full: r.Full, Logical: r.Logical, Read: r.Read})
	}
	if out.Failed > 0 {
		return out, &engine.PublicError{Msg: fmt.Sprintf("%d of %d object(s) failed (the errors are in the host log)", out.Failed, len(rep.Results))}
	}
	if deferred > 0 {
		return out, &engine.PublicError{Msg: fmt.Sprintf("deferred: %d of %d object(s) were not backed up because the repository is in maintenance (read-only or offline); set it online and run again, or wait for the next scheduled run", deferred, len(rep.Results))}
	}
	return out, nil
}

// RestoreTestObjects lists the (object, target) pairs automatic restore tests consider.
func (a *App) RestoreTestObjects(objs []*engine.Object) []jobs.TestObject {
	var out []jobs.TestObject
	for _, o := range objs {
		if o.Skip != "" || o.Kind == "" {
			continue
		}
		p := a.Eng.Policies[o.Policy]
		for _, t := range o.Targets {
			out = append(out, jobs.TestObject{Target: t, Project: o.Project, Name: o.Name, Kind: o.Kind, Policy: o.Policy, Schedule: p.RestoreTestSchedule})
		}
	}
	return out
}

// RestoreTestOutcome is one test the App ran.
type RestoreTestOutcome struct {
	Target string
	Result *jobspec.RestoreTestResult
	Err    error
}

// RunRestoreTests runs planned tests one after the other, records them locally and reports them to
// the target that holds the snapshot.
func (a *App) RunRestoreTests(ctx context.Context, planned []jobs.Planned) []RestoreTestOutcome {
	x, err := a.Executor()
	if err != nil {
		return []RestoreTestOutcome{{Err: err}}
	}
	var out []RestoreTestOutcome
	for _, p := range planned {
		if ctx.Err() != nil {
			break
		}
		started := a.Eng.Now()
		res, err := x.RestoreTest(ctx, p.Target, jobspec.RestoreTest{Project: p.Project, Object: p.Name, Mode: p.Mode}, nil)
		finished := a.Eng.Now()
		out = append(out, RestoreTestOutcome{Target: p.Target, Result: res, Err: err})
		if err != nil {
			a.Log.Error("restore test FAILED", "server", p.Target, "object", p.Project+"/"+p.Name, "mode", p.Mode, "err", engine.Detail(err))
		} else {
			a.Log.Info("restore test passed", "server", p.Target, "object", p.Project+"/"+p.Name, "mode", p.Mode, "snapshot", res.Snapshot, "bytes", res.Bytes)
		}
		if res == nil || ctx.Err() != nil {
			continue
		}
		if t := a.Eng.Targets[p.Target]; t != nil {
			if rerr := t.Client.ReportRestoreTest(ctx, jobspec.RestoreTestReport{Result: *res, Started: started.UTC(), Finished: finished.UTC()}); rerr != nil {
				a.Log.Warn("could not report a restore test to the server", "server", p.Target, "err", rerr)
			}
		}
	}
	return out
}

// PlanRestoreTests returns what the restore-test schedule wants at now.
func (a *App) PlanRestoreTests(objs []*engine.Object, now time.Time) ([]jobs.Planned, []string, error) {
	if _, err := a.Executor(); err != nil {
		return nil, nil, err
	}
	hist, slots := a.rt.hist.Snapshot()
	p, ran := jobs.Plan(a.withSnapshots(a.RestoreTestObjects(objs)), hist, slots, now)
	return p, ran, nil
}

func (a *App) restoreTestsDue(ctx context.Context, objs []*engine.Object, now time.Time) {
	planned, ran, err := a.PlanRestoreTests(objs, now)
	if err != nil {
		a.Log.Error("restore tests cannot be planned", "err", err)
		return
	}
	if len(planned) > 0 {
		a.Log.Info("running scheduled restore tests", "count", len(planned))
		a.RunRestoreTests(ctx, planned)
	}
	if ctx.Err() != nil {
		return
	}
	for _, pol := range ran {
		_ = a.rt.hist.MarkSlot(pol, now)
	}
}

// RestoreTestHistoryPath is where the local restore-test history lives.
func (a *App) RestoreTestHistoryPath() string {
	return filepath.Join(a.Cfg.StateDir, "restore-tests.json")
}

// History returns the local restore-test history.
func (a *App) History() (*jobs.History, error) {
	if _, err := a.Executor(); err != nil {
		return nil, err
	}
	return a.rt.hist, nil
}

// withSnapshots drops (target, object) pairs for which the target holds no snapshot yet (never backed up,
// or a manual-only policy nobody has run): a restore test of nothing can only fail, and a permanent
// failure would hide real ones. When a target cannot be asked, its objects are kept (test rather than skip).
func (a *App) withSnapshots(in []jobs.TestObject) []jobs.TestObject {
	have := map[string]map[string]bool{}
	for _, o := range in {
		if _, done := have[o.Target]; done {
			continue
		}
		have[o.Target] = nil
		t := a.Eng.Targets[o.Target]
		if t == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		l, err := t.Client.Objects(ctx)
		cancel()
		if err != nil {
			continue
		}
		m := map[string]bool{}
		for _, ro := range l {
			if ro.Snapshots > 0 {
				m[ro.Project+"|"+ro.Name] = true
			}
		}
		have[o.Target] = m
	}
	var out []jobs.TestObject
	for _, o := range in {
		if m := have[o.Target]; m != nil && !m[o.Project+"|"+o.Name] {
			continue
		}
		out = append(out, o)
	}
	return out
}
