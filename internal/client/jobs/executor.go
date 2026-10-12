// Package jobs is the host side of host jobs: it pulls jobs from each server this host is enrolled
// with, validates them again (the server is not trusted to have done it), refuses types this host
// does not allow, executes them with the local key through the engine, reports progress, log lines
// and a result, and uploads sealed deliveries. It also plans the automatic restore tests (schedule.go).
//
// What reaches the server never names a file inside a volume: messages and log lines are written by
// this package (counts, object and snapshot names, chunk ids); errors from the engine are reported
// only when they are engine.PublicError, everything else is summarised and the full text goes to a
// local log file (<state>/jobs/<job>.log).
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/archive"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/remote"
)

// Executor runs host jobs.
type Executor struct {
	Eng *engine.Engine
	Log *slog.Logger
	// StateDir holds scratch directories (<state>/scratch) and local job logs (<state>/jobs).
	StateDir string
	// Allow lists the job types this host accepts (nil = the jobspec defaults: backup_now,
	// restore_test, deep_verify). Anything else is refused with a clear message.
	Allow map[string]bool
	// Recovery VM settings for guest_files, and the pool for scratch volumes of full restore tests.
	RecoveryImage, RecoveryPool, RecoveryMemory, RecoveryCPU string
	// PoolFor returns the storage pool of an object (for full restore tests when RecoveryPool is "").
	PoolFor func(ctx context.Context, project, name string) string
	// BackupNow runs a backup of a selection (provided by the app; nil = refuse backup_now).
	BackupNow func(ctx context.Context, p jobspec.BackupNow) (any, error)
	// OnRestoreTest is told about every restore test this executor ran (local history).
	OnRestoreTest func(target string, res *jobspec.RestoreTestResult, started, finished time.Time)
	// Heavy, when set, is held while a heavy job runs (shared with the scheduler's backups).
	Heavy *sync.Mutex
	// Wait is the long-poll wait (default 30s), Heartbeat how often a running job reports (default 30s).
	Wait      time.Duration
	Heartbeat time.Duration
	// LightSlots is how many light jobs may run next to the heavy one (default 2).
	LightSlots int

	once      sync.Once
	heavySem  chan struct{}
	lightSem  chan struct{}
	freed     chan struct{}
	wg        sync.WaitGroup
	timeoutFn func(typ string) time.Duration // tests (export_test.go)
}

func (x *Executor) init() {
	x.once.Do(func() {
		x.heavySem = make(chan struct{}, 1)
		n := x.LightSlots
		if n <= 0 {
			n = 2
		}
		x.lightSem = make(chan struct{}, n)
		x.freed = make(chan struct{}, 1)
		if x.Log == nil {
			x.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
	})
}

// Allowed reports whether this host accepts a job type.
func (x *Executor) Allowed(typ string) bool {
	info, ok := jobspec.Lookup(typ)
	if !ok {
		return false
	}
	if x.Allow == nil {
		return info.DefaultAllowed
	}
	return x.Allow[typ]
}

func (x *Executor) heartbeat() time.Duration {
	if x.Heartbeat > 0 {
		return x.Heartbeat
	}
	return 30 * time.Second
}

func (x *Executor) wait() time.Duration {
	if x.Wait > 0 {
		return x.Wait
	}
	return 30 * time.Second
}

// Run polls every target until ctx ends; running jobs are cancelled with ctx and waited for.
func (x *Executor) Run(ctx context.Context) {
	x.init()
	x.cleanLogs()
	var wg sync.WaitGroup
	for name := range x.Eng.Targets {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			x.loop(ctx, name)
		}(name)
	}
	wg.Wait()
	x.wg.Wait()
}

func (x *Executor) loop(ctx context.Context, target string) {
	backoff := time.Second
	for ctx.Err() == nil {
		_, err := x.poll(ctx, target, x.wait(), false)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			x.Log.Warn("polling for jobs failed", "server", target, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

// Poll claims at most one job from target (waiting up to wait) and runs it to the end before
// returning. It reports whether a job was run. (Tests and `incus-backup jobs run-once`.)
func (x *Executor) Poll(ctx context.Context, target string, wait time.Duration) (bool, error) {
	x.init()
	return x.poll(ctx, target, wait, true)
}

// poll reserves capacity, asks for a job that fits it and starts (or, sync, runs) it.
func (x *Executor) poll(ctx context.Context, target string, wait time.Duration, sync bool) (bool, error) {
	t := x.Eng.Targets[target]
	if t == nil {
		return false, fmt.Errorf("unknown server %q", target)
	}
	heavy, light := tryTake(x.heavySem), tryTake(x.lightSem)
	release := func() {
		if heavy {
			<-x.heavySem
			heavy = false
		}
		if light {
			<-x.lightSem
			light = false
		}
		select {
		case x.freed <- struct{}{}:
		default:
		}
	}
	var types []string
	for _, typ := range jobspec.Types {
		info, _ := jobspec.Lookup(typ)
		switch {
		case !x.Allowed(typ): // claimable at any time, to be refused at once
		case info.Heavy && !heavy:
			continue
		case !info.Heavy && !light:
			continue
		}
		types = append(types, typ)
	}
	if len(types) == 0 || (!heavy && !light && allAllowedNeedCapacity(x)) {
		release()
		// nothing fits now: wait for a slot to free up
		select {
		case <-ctx.Done():
		case <-x.freed:
		case <-time.After(wait):
		}
		return false, nil
	}
	j, err := t.Client.NextJob(ctx, wait, types)
	if err != nil || j == nil {
		release()
		return false, err
	}
	info, known := jobspec.Lookup(j.Type)
	// keep exactly the reservation this job needs
	if known && x.Allowed(j.Type) {
		if info.Heavy && light {
			<-x.lightSem
			light = false
		} else if !info.Heavy && heavy {
			<-x.heavySem
			heavy = false
		}
	} else {
		release()
	}
	run := func() {
		defer release()
		x.execute(ctx, target, t.Client, j)
	}
	if sync {
		run()
		return true, nil
	}
	x.wg.Add(1)
	go func() {
		defer x.wg.Done()
		run()
	}()
	return true, nil
}

// allAllowedNeedCapacity: with no slot free, polling only makes sense when some type is refused
// (those are claimable regardless). It is true when every type is allowed.
func allAllowedNeedCapacity(x *Executor) bool {
	for _, typ := range jobspec.Types {
		if !x.Allowed(typ) {
			return false
		}
	}
	return true
}

func tryTake(ch chan struct{}) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// ---- one job -------------------------------------------------------------------------------------

type jobRun struct {
	x      *Executor
	c      *remote.Client
	job    *jobspec.Job
	target string

	mu        sync.Mutex
	pending   []jobspec.LogLine
	progress  int
	done      int64
	total     int64
	message   string
	cancelled bool // the server asked for it
	lost      bool // the server no longer considers this attempt ours
	logf      *os.File
}

func (r *jobRun) logLine(level, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) < 2000 {
		r.pending = append(r.pending, jobspec.LogLine{Level: level, Line: line})
	}
	if r.logf != nil {
		fmt.Fprintf(r.logf, "%s %s %s\n", time.Now().UTC().Format(time.RFC3339), level, line)
	}
}

// local writes to the local job log only (it may contain names from inside volumes).
func (r *jobRun) local(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logf != nil {
		fmt.Fprintf(r.logf, "%s local %s\n", time.Now().UTC().Format(time.RFC3339), line)
	}
}

func (r *jobRun) setProgress(done, total int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done, r.total = done, total
	if total > 0 {
		p := int(done * 100 / total)
		if p > 99 {
			p = 99
		}
		if p > r.progress {
			r.progress = p
		}
	}
}

func (r *jobRun) setMessage(m string) {
	r.mu.Lock()
	r.message = m
	r.mu.Unlock()
}

func (r *jobRun) takeLog(max int) []jobspec.LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.pending)
	if n > max {
		n = max
	}
	out := append([]jobspec.LogLine(nil), r.pending[:n]...)
	r.pending = r.pending[n:]
	return out
}

// report sends one progress report; it cancels the job when the server asks or the attempt is no
// longer ours.
func (r *jobRun) report(ctx context.Context, cancel context.CancelFunc) {
	lines := r.takeLog(jobspec.MaxLinesPerCall)
	r.mu.Lock()
	p := jobspec.Progress{Attempt: r.job.Attempt, Progress: r.progress, BytesDone: r.done, BytesTotal: r.total, Message: r.message, Log: lines}
	r.mu.Unlock()
	resp, err := r.c.JobProgress(ctx, r.job.ID, p)
	switch {
	case err == nil:
		if resp.CancelRequested {
			r.mu.Lock()
			r.cancelled = true
			r.mu.Unlock()
			cancel()
		}
	case remote.IsStatus(err, 404) || remote.IsStatus(err, 409):
		r.mu.Lock()
		r.lost = true
		r.mu.Unlock()
		r.local("the server no longer considers this attempt ours: " + err.Error())
		cancel()
	default:
		// transient: put the lines back, the next report retries (the lease is 5 minutes)
		r.mu.Lock()
		r.pending = append(lines, r.pending...)
		r.mu.Unlock()
	}
}

func (x *Executor) jobLog(id string) (*os.File, string) {
	if x.StateDir == "" {
		return nil, ""
	}
	dir := filepath.Join(x.StateDir, "jobs")
	if os.MkdirAll(dir, 0o700) != nil {
		return nil, ""
	}
	p := filepath.Join(dir, filepath.Base(id)+".log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, ""
	}
	return f, p
}

// cleanLogs removes local job logs older than 30 days.
func (x *Executor) cleanLogs() {
	if x.StateDir == "" {
		return
	}
	ents, _ := os.ReadDir(filepath.Join(x.StateDir, "jobs"))
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 30*24*time.Hour {
			_ = os.Remove(filepath.Join(x.StateDir, "jobs", e.Name()))
		}
	}
}

func (x *Executor) execute(parent context.Context, target string, c *remote.Client, j *jobspec.Job) {
	r := &jobRun{x: x, c: c, job: j, target: target}
	var logPath string
	r.logf, logPath = x.jobLog(j.ID)
	if r.logf != nil {
		defer r.logf.Close()
	}
	finish := func(state, msg string, result any) {
		var raw json.RawMessage
		if result != nil {
			raw, _ = json.Marshal(result)
		}
		fctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// keep the log in order: everything beyond the last call's worth goes first, through progress
		for {
			r.mu.Lock()
			n, pr := len(r.pending), r.progress
			r.mu.Unlock()
			if n <= jobspec.MaxLinesPerCall {
				break
			}
			more := r.takeLog(jobspec.MaxLinesPerCall)
			if _, err := c.JobProgress(fctx, j.ID, jobspec.Progress{Attempt: j.Attempt, Progress: pr, Log: more}); err != nil {
				break
			}
		}
		f := jobspec.Finish{Attempt: j.Attempt, State: state, Message: msg, Result: raw, Log: r.takeLog(jobspec.MaxLinesPerCall)}
		if err := c.FinishJob(fctx, j.ID, f); err != nil {
			if remote.IsStatus(err, 409) {
				if cs, cerr := c.JobCancelRequested(fctx, j.ID); cerr == nil && jobspec.Terminal(cs.State) {
					return // a lost response of an earlier, successful try
				}
			}
			x.Log.Error("could not report the end of a job", "job", j.ID, "server", target, "err", err)
		}
		r.local("finished " + state + ": " + msg)
	}
	info, known := jobspec.Lookup(j.Type)
	if !known {
		finish(jobspec.StateFailed, fmt.Sprintf("this host does not know the job type %q", j.Type), nil)
		return
	}
	if !x.Allowed(j.Type) {
		finish(jobspec.StateFailed, fmt.Sprintf("job type %s is not enabled on this host (jobs.allow in client.yaml)", j.Type), nil)
		return
	}
	params, _, err := jobspec.Validate(j.Type, j.Params)
	if err != nil {
		finish(jobspec.StateFailed, "refused: "+err.Error(), nil)
		return
	}
	timeout := info.Timeout
	if x.timeoutFn != nil {
		timeout = x.timeoutFn(j.Type)
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// the heartbeat runs independently of engine progress (some steps report none for a long time)
	hbDone := make(chan struct{})
	hbStopped := make(chan struct{})
	go func() {
		defer close(hbStopped)
		tk := time.NewTicker(x.heartbeat())
		defer tk.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-tk.C:
				r.report(ctx, cancel)
			}
		}
	}()
	r.logLine("info", fmt.Sprintf("%s started on %s (attempt %d)", j.Type, x.Eng.Host, j.Attempt))
	r.report(ctx, cancel)
	if info.Heavy && x.Heavy != nil {
		r.setMessage("waiting for a running backup to finish")
		locked := make(chan struct{})
		go func() { x.Heavy.Lock(); close(locked) }()
		select {
		case <-locked:
			defer x.Heavy.Unlock()
		case <-ctx.Done():
			go func() { <-locked; x.Heavy.Unlock() }()
		}
		r.setMessage("")
	}
	var result any
	if ctx.Err() == nil {
		result, err = x.run(ctx, r, params)
	} else {
		err = ctx.Err()
	}
	close(hbDone)
	<-hbStopped
	r.mu.Lock()
	cancelled, lost := r.cancelled, r.lost
	r.mu.Unlock()
	if err != nil {
		r.local("error: " + engine.Detail(err))
	}
	switch {
	case lost:
		x.Log.Warn("a job was taken away from this host (lease lost or job ended); not reporting", "job", j.ID)
	case parent.Err() != nil && !cancelled:
		// the daemon is stopping: do not burn the attempt. The lease runs out and the server
		// re-queues the job if its type allows another attempt.
		r.local("interrupted by the daemon stopping; left to the lease")
		x.Log.Warn("job interrupted by shutdown; the server re-queues it when its lease runs out", "job", j.ID)
	case cancelled:
		finish(jobspec.StateCancelled, "cancelled on request", result)
	case err != nil:
		finish(jobspec.StateFailed, publicMessage(ctx, err, logPath, timeout), result)
	default:
		finish(jobspec.StateSucceeded, "", result)
	}
}

// publicMessage is what the server may see about an error.
func publicMessage(ctx context.Context, err error, logPath string, timeout time.Duration) string {
	var pe *engine.PublicError
	var ae *remote.APIError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("timed out after %s", timeout)
	case errors.Is(err, context.Canceled):
		return "interrupted (the host daemon stopped)"
	case errors.As(err, &pe):
		return pe.Msg
	case errors.As(err, &ae):
		return "the server answered: " + ae.Error()
	case errors.Is(err, jobspec.ErrInvalid):
		return err.Error()
	}
	if logPath != "" {
		return "failed; the full error is in the host's log " + logPath
	}
	return "failed; the full error is in the host's log"
}

func pub(f string, a ...any) error { return &engine.PublicError{Msg: fmt.Sprintf(f, a...)} }

func (x *Executor) run(ctx context.Context, r *jobRun, params any) (any, error) {
	switch p := params.(type) {
	case jobspec.BackupNow:
		if x.BackupNow == nil {
			return nil, pub("this host cannot run backups from jobs")
		}
		r.logLine("info", "backing up "+describeSel(p))
		return x.BackupNow(ctx, p)
	case jobspec.RestoreTest:
		return x.restoreTest(ctx, r, p)
	case jobspec.RestoreInstance:
		return x.restoreInstance(ctx, r, p)
	case jobspec.RestoreVolume:
		return x.restoreVolume(ctx, r, p)
	case jobspec.GuestFiles:
		return x.guestFiles(ctx, r, p)
	case jobspec.FSExtract:
		return x.fsExtract(ctx, r, p)
	case jobspec.DeepVerify:
		return x.deepVerify(ctx, r, p)
	}
	return nil, pub("unsupported job")
}

func describeSel(p jobspec.BackupNow) string {
	var parts []string
	if p.Project != "" {
		parts = append(parts, "project "+p.Project)
	}
	if p.Policy != "" {
		parts = append(parts, "policy "+p.Policy)
	}
	if p.Object != "" {
		parts = append(parts, "object "+p.Object)
	}
	return strings.Join(parts, ", ")
}

func (x *Executor) scratch() (string, error) {
	if x.StateDir == "" {
		return "", pub("no state directory for scratch space")
	}
	d := filepath.Join(x.StateDir, "scratch")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", &engine.PublicError{Msg: "cannot create the scratch directory", Err: err}
	}
	return d, nil
}

// RestoreTest runs one restore test on target (also used by the scheduler and the CLI).
func (x *Executor) RestoreTest(ctx context.Context, target string, p jobspec.RestoreTest, progress func(done, total int64)) (*jobspec.RestoreTestResult, error) {
	sd, err := x.scratch()
	if err != nil {
		return &jobspec.RestoreTestResult{Project: p.Project, Object: p.Object, Method: p.Mode, Details: map[string]any{"error": err.Error()}}, err
	}
	pool := x.RecoveryPool
	if pool == "" && x.PoolFor != nil {
		pool = x.PoolFor(ctx, p.Project, p.Object)
	}
	started := time.Now()
	res, err := x.Eng.RestoreTest(ctx, engine.RestoreTestOptions{Server: target, Project: p.Project, Name: p.Object, Snapshot: p.Snapshot, Mode: p.Mode,
		ScratchDir: sd, Pool: pool, Progress: progress})
	if x.OnRestoreTest != nil && res != nil && ctx.Err() == nil {
		x.OnRestoreTest(target, res, started, time.Now())
	}
	return res, err
}

func (x *Executor) restoreTest(ctx context.Context, r *jobRun, p jobspec.RestoreTest) (any, error) {
	r.logLine("info", fmt.Sprintf("restore test (%s) of %s/%s@%s", p.Mode, p.Project, p.Object, orLatest(p.Snapshot)))
	res, err := x.RestoreTest(ctx, r.target, p, r.setProgress)
	if res != nil {
		r.logLine("info", fmt.Sprintf("snapshot %s: %d chunks, %d bytes, %d files checked, passed=%v", res.Snapshot, res.Chunks, res.Bytes, res.Files, res.Passed))
	}
	return res, err
}

func orLatest(s string) string {
	if s == "" {
		return "latest"
	}
	return s
}

func (x *Executor) restoreInstance(ctx context.Context, r *jobRun, p jobspec.RestoreInstance) (any, error) {
	dp, dn := p.AsProject, p.AsName
	if dp == "" {
		dp = p.Project
	}
	if dn == "" {
		dn = p.Instance
	}
	if ok, err := x.Eng.Inc.InstanceExists(ctx, dp, dn); err != nil {
		return nil, &engine.PublicError{Msg: "cannot ask Incus whether the instance exists", Err: err}
	} else if ok {
		return nil, pub("instance %s/%s already exists: restore under another name (as_name), or delete it first", dp, dn)
	}
	r.logLine("info", fmt.Sprintf("restoring instance %s/%s@%s as %s/%s", p.Project, p.Instance, orLatest(p.Snapshot), dp, dn))
	res, err := x.Eng.RestoreInstance(ctx, engine.RestoreOptions{Server: r.target, Project: p.Project, Name: p.Instance, Snapshot: p.Snapshot,
		AsProject: p.AsProject, AsName: p.AsName, Volumes: p.Volumes, Start: p.Start, KeepNetwork: p.KeepNetwork})
	if res != nil {
		r.logLine("info", fmt.Sprintf("restored %s (%d objects, %d bytes)", res.Instance, len(res.Objects), res.Bytes))
	}
	if err != nil && res != nil && res.Instance != "" {
		err = &engine.PublicError{Msg: "the restore failed part-way; the incomplete instance " + res.Instance + " was left in place for inspection (delete it before retrying)", Err: err}
	}
	return res, err
}

func (x *Executor) restoreVolume(ctx context.Context, r *jobRun, p jobspec.RestoreVolume) (any, error) {
	dp := p.AsProject
	if dp == "" {
		dp = p.Project
	}
	dst := incusx.VolRef{Pool: p.Pool, Project: dp, Type: "custom", Name: p.AsName}
	if ok, err := x.Eng.Inc.VolumeExists(ctx, dst); err != nil {
		return nil, &engine.PublicError{Msg: "cannot ask Incus whether the volume exists", Err: err}
	} else if ok {
		return nil, pub("volume %s/%s already exists: restore into a new name", dp, p.AsName)
	}
	r.logLine("info", fmt.Sprintf("restoring %s/%s@%s into volume %s/%s on pool %s", p.Project, p.Object, orLatest(p.Snapshot), dp, p.AsName, p.Pool))
	loc, err := x.Eng.RestoreVolume(ctx, r.target, p.Project, p.Object, p.Snapshot, dst, false)
	out := map[string]any{"volume": dp + "/" + p.AsName, "pool": p.Pool}
	if loc != nil {
		out["snapshot"] = loc.Snapshot
		out["bytes"] = loc.Index.Size
	}
	if err != nil && loc != nil {
		err = &engine.PublicError{Msg: "the restore failed part-way; the volume " + dp + "/" + p.AsName + " was left in place for inspection (delete it before retrying)", Err: err}
	}
	return out, err
}

func (x *Executor) deepVerify(ctx context.Context, r *jobRun, p jobspec.DeepVerify) (any, error) {
	t := x.Eng.Targets[r.target]
	var match func(api.RepoObject) bool
	if p.Object != "" {
		match = func(o api.RepoObject) bool { return o.Project == p.Project && o.Name == p.Object }
	} else if p.Project != "" {
		match = func(o api.RepoObject) bool { return o.Project == p.Project }
	}
	r.logLine("info", fmt.Sprintf("deep verification of the last %d snapshot(s)", p.Last))
	rep, err := x.Eng.VerifyObjects(ctx, t, p.Last, match, func(done, total int) { r.setProgress(int64(done), int64(total)) })
	probs := rep.Problems
	if len(probs) > 200 {
		probs = probs[:200]
	}
	out := map[string]any{"snapshots": rep.Snapshots, "chunks": rep.Chunks, "bytes": rep.Bytes, "problems": probs, "problem_count": len(rep.Problems)}
	if err != nil {
		if len(rep.Problems) > 0 {
			// problems name objects, snapshots and chunk ids only
			return out, pub("verification found %d problem(s); first: %s", len(rep.Problems), rep.Problems[0])
		}
		return out, err
	}
	r.logLine("info", fmt.Sprintf("verified %d snapshot(s), %d chunk(s), %d bytes", rep.Snapshots, rep.Chunks, rep.Bytes))
	return out, nil
}

// ---- sealed deliveries -------------------------------------------------------------------------------

func (x *Executor) withTemp(fn func(dir string) (any, error)) (any, error) {
	sd, err := x.scratch()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(sd, "ibk-dl-*")
	if err != nil {
		return nil, &engine.PublicError{Msg: "cannot create a temporary directory", Err: err}
	}
	defer removeAll(dir)
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return nil, &engine.PublicError{Msg: "cannot create a temporary directory", Err: err}
	}
	return fn(out)
}

func (x *Executor) guestFiles(ctx context.Context, r *jobRun, p jobspec.GuestFiles) (any, error) {
	key, err := jobspec.RecipientKey(p.RecipientPublicKey)
	if err != nil {
		return nil, err
	}
	return x.withTemp(func(dir string) (any, error) {
		r.logLine("info", fmt.Sprintf("recovering %d path(s) from %s/%s@%s through a disposable VM", len(p.Paths), p.Project, p.Object, orLatest(p.Snapshot)))
		res, err := x.Eng.RestoreGuestFiles(ctx, engine.GuestFilesOptions{Server: r.target, Project: p.Project, Name: p.Object, Snapshot: p.Snapshot,
			Paths: p.Paths, Partition: p.Partition, Dest: dir, Image: x.RecoveryImage, Pool: x.RecoveryPool, Memory: x.RecoveryMemory, CPU: x.RecoveryCPU,
			// the delivery is limited to this size anyway; never extract unbounded from a hostile guest filesystem
			MaxBytes: jobspec.MaxDeliveryBytes})
		if err != nil {
			return nil, err
		}
		if res.Header != nil && res.Header.Status != "" && res.Header.Status != "ok" {
			r.logLine("warn", "the recovery VM reported status "+safeToken(res.Header.Status))
		}
		out, err := x.deliver(ctx, r, dir, key)
		if dr, ok := out.(jobspec.DeliveryResult); ok && res.Header != nil {
			requested := map[string]bool{}
			for _, q := range p.Paths {
				requested[q] = true
			}
			dr.Paths = map[string]string{}
			for k, v := range res.Header.Paths {
				if requested[k] {
					dr.Paths[k] = safeStatus(v)
				}
			}
			out = dr
		}
		return out, err
	})
}

func (x *Executor) fsExtract(ctx context.Context, r *jobRun, p jobspec.FSExtract) (any, error) {
	key, err := jobspec.RecipientKey(p.RecipientPublicKey)
	if err != nil {
		return nil, err
	}
	return x.withTemp(func(dir string) (any, error) {
		r.logLine("info", fmt.Sprintf("extracting %d path(s) from %s/%s@%s", len(p.Paths), p.Project, p.Object, orLatest(p.Snapshot)))
		if _, err := x.Eng.ExtractFSPaths(ctx, r.target, p.Project, p.Object, p.Snapshot, p.Paths, dir); err != nil {
			return nil, err
		}
		return x.deliver(ctx, r, dir, key)
	})
}

func safeToken(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

func safeStatus(s string) string {
	switch s {
	case "ok", "missing", "symlink in path", "not allowed":
		return s
	}
	return "other"
}

type countingWriter struct {
	w io.Writer
	h interface{ Write([]byte) (int, error) }
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}

// deliver tars dir, seals the stream to key and streams it to the server. The plaintext only exists
// in this process and the temporary directory (deleted by the caller).
//
// The stream is IBS2 (format.NewSealWriter): authenticated with the repository key of the target
// for this job id and the digest of the parameters this host received, so the browser can tell a
// real delivery from one the relaying server made up or took from another job.
func (x *Executor) deliver(ctx context.Context, r *jobRun, dir string, key []byte) (any, error) {
	t := x.Eng.Targets[r.target]
	if t == nil || t.Keys == nil {
		return nil, pub("no repository key for server %q: cannot authenticate the delivery", r.target)
	}
	digest, err := jobspec.RequestDigest(r.job.Type, r.job.Params)
	if err != nil {
		return nil, &engine.PublicError{Msg: "the job parameters do not validate", Err: err}
	}
	binding := format.SealBinding{JobID: r.job.ID, Request: digest}
	pr, pw := io.Pipe()
	h := sha256.New()
	cw := &countingWriter{w: pw, h: h}
	type out struct {
		st  *archive.Stats
		err error
	}
	done := make(chan out, 1)
	go func() {
		sw, err := format.NewSealWriter(cw, t.Keys, key, binding)
		if err != nil {
			pw.CloseWithError(err)
			done <- out{err: err}
			return
		}
		st, err := archive.Write(sw, dir)
		if err == nil {
			err = sw.Close()
		}
		if err != nil {
			pw.CloseWithError(err) // the server sees a broken body, never a clean end
		} else {
			pw.Close()
		}
		done <- out{st, err}
	}()
	rc, err := r.c.PutDelivery(ctx, r.job.ID, r.job.Attempt, pr)
	pr.CloseWithError(errors.New("upload ended"))
	o := <-done
	if o.err != nil {
		return nil, &engine.PublicError{Msg: "packing or sealing the files failed", Err: o.err}
	}
	if err != nil {
		return nil, &engine.PublicError{Msg: "uploading the sealed files failed: " + err.Error(), Err: err}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if rc.Size != cw.n || rc.SHA256 != sum {
		return nil, pub("the server stored %d bytes (sha256 %s) but %d bytes (sha256 %s) were sent", rc.Size, rc.SHA256, cw.n, sum)
	}
	r.logLine("info", fmt.Sprintf("delivered %d file(s), %d bytes, sealed to the requester's key (%d bytes)", o.st.Files, o.st.Bytes, cw.n))
	return jobspec.DeliveryResult{Size: cw.n, SHA256: sum, Files: o.st.Files, Bytes: o.st.Bytes, Format: "tar+IBS2"}, nil
}

func removeAll(dir string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
