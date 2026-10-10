// Package tasks runs long server-side operations (GC, verify, retention, reconcile, capacity sampling,
// sync runs, repository probes and destroys, group deletes) as TASKS: each has an id, a kind, the
// repository it works on, who started it, a state (queued, running, succeeded, failed, cancelled),
// progress, a persisted bounded log, a JSON result and can be cancelled.
//
// The manager lives in the daemon. A task runs in its own goroutine under a context the manager owns
// (never the HTTP request's), so a browser that goes away does not stop it, and Cancel cancels exactly
// that context: operations honour ctx and stop at their next check. The operation reports progress and
// log lines through internal/progress, so the service layer needs no knowledge of tasks.
//
// At most one DESTRUCTIVE task (Spec.Exclusive) runs per repository: a partial unique index in
// PostgreSQL enforces it for the scheduler and people alike, and a second one is refused with ErrBusy.
//
// Tasks that were running when the daemon stopped are failed ("server restarted") by Recover, which
// only the daemon calls (a CLI command must never touch the daemon's live tasks).
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unidoc/incus-backup/internal/progress"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// States.
const (
	Queued    = "queued"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Actor kinds.
const (
	ActorUser   = "user"
	ActorToken  = "token"
	ActorLocal  = "local"
	ActorSystem = "system"
)

// ErrBusy means the repository already runs a destructive task. It wraps service.ErrConflict (409).
var ErrBusy = fmt.Errorf("%w: the repository is busy", service.ErrConflict)

// Task is a task without its log.
type Task struct {
	ID              string          `json:"id"`
	Kind            string          `json:"kind"`
	Repository      string          `json:"repository"`
	StartedBy       string          `json:"started_by"`
	ActorKind       string          `json:"actor_kind"`
	State           string          `json:"state"`
	Progress        int             `json:"progress"`
	Message         string          `json:"message"`
	Params          json.RawMessage `json:"params"`
	Result          json.RawMessage `json:"result"`
	Error           string          `json:"error"`
	Exclusive       bool            `json:"exclusive"`
	CancelRequested bool            `json:"cancel_requested"`
	LogLines        int             `json:"log_lines"`
	LogDropped      int             `json:"log_dropped"`
	Created         time.Time       `json:"created"`
	Started         *time.Time      `json:"started"`
	Finished        *time.Time      `json:"finished"`
	// DurationMs is finished-started, or the time running so far.
	DurationMs int64 `json:"duration_ms"`
}

// Finished reports whether the task reached a final state.
func (t *Task) Done() bool { return t.State == Succeeded || t.State == Failed || t.State == Cancelled }

// LogLine is one line of a task's log.
type LogLine struct {
	Seq   int       `json:"seq"`
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Line  string    `json:"line"`
}

// Detail is a task with (a slice of) its log.
type Detail struct {
	Task
	Log []LogLine `json:"log"`
	// NextSeq is the seq to pass as ?after= to continue tailing.
	NextSeq int `json:"next_seq"`
}

// Spec describes a task to start.
type Spec struct {
	Kind         string
	RepositoryID string // "" = not tied to a repository
	Repository   string
	Actor        string
	ActorKind    string
	Params       any
	// Exclusive marks a destructive maintenance task: one per repository at a time.
	Exclusive bool
}

// Func is the work of a task. It must honour ctx (cancellation) and may report through progress.
type Func func(ctx context.Context) (result any, err error)

// Manager runs and records tasks.
type Manager struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	Now  func() time.Time
	// Publish is called (outside any lock, possibly often: throttled to about once a second per task for
	// progress) with the task's current summary; the daemon forwards it as the SSE event "task".
	Publish func(Task)
	// MaxLogLines caps the persisted log of one task (default 5000); further lines are counted as dropped.
	MaxLogLines int
	// KeepFor and KeepMax bound the history (defaults 30 days and 2000 finished tasks).
	KeepFor time.Duration
	KeepMax int

	once   sync.Once
	base   context.Context
	stop   context.CancelFunc
	mu     sync.Mutex
	active map[string]*run
	wg     sync.WaitGroup
	closed bool
}

func (m *Manager) init() {
	m.once.Do(func() {
		m.base, m.stop = context.WithCancel(context.Background())
		m.active = map[string]*run{}
		if m.Log == nil {
			m.Log = slog.Default()
		}
		if m.Now == nil {
			m.Now = time.Now
		}
		if m.MaxLogLines <= 0 {
			m.MaxLogLines = 5000
		}
		if m.KeepFor <= 0 {
			m.KeepFor = 30 * 24 * time.Hour
		}
		if m.KeepMax <= 0 {
			m.KeepMax = 2000
		}
	})
}

// Handle is a task started by this process.
type Handle struct {
	ID   string
	done chan struct{}
	res  any
	err  error
}

// Done is closed when the task ended.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Outcome is the Func's result and error (valid after Done).
func (h *Handle) Outcome() (any, error) { return h.res, h.err }

// Wait blocks until the task ended (or ctx ends) and returns its outcome.
func (h *Handle) Wait(ctx context.Context) (any, error) {
	select {
	case <-h.done:
		return h.res, h.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run is the in-memory state of an active task; it is the progress.Reporter of its Func.
type run struct {
	m      *Manager
	id     string
	cancel context.CancelFunc
	by     string // who asked to cancel

	mu        sync.Mutex
	seq       int
	dropped   int
	pct       int
	msg       string
	lastWrite time.Time
	lastPub   time.Time
	h         *Handle
}

func (r *run) ID() string { return r.id }

// Progress records completion; the database row and the live event are updated at most once a second
// (always for 100 %).
func (r *run) Progress(pct int, msg string) {
	if pct > 100 {
		pct = 100
	}
	r.mu.Lock()
	if pct >= 0 {
		r.pct = pct
	}
	if msg != "" {
		r.msg = truncate(msg, 500)
	}
	now := time.Now()
	write := now.Sub(r.lastWrite) >= time.Second || pct == 100
	if write {
		r.lastWrite = now
	}
	p, mm := r.pct, r.msg
	r.mu.Unlock()
	if !write {
		return
	}
	_, _ = r.m.Pool.Exec(r.m.base, `UPDATE tasks SET progress=$2, message=$3 WHERE id=$1 AND state='running'`, r.id, p, mm)
	r.publish(false)
}

// Log appends a line (capped per task).
func (r *run) Log(level progress.Level, line string) {
	switch level {
	case progress.Debug, progress.Info, progress.Warn, progress.Error:
	default:
		level = progress.Info
	}
	r.mu.Lock()
	if r.seq >= r.m.MaxLogLines {
		r.dropped++
		r.mu.Unlock()
		return
	}
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	_, err := r.m.Pool.Exec(r.m.base, `INSERT INTO task_log (task_id, seq, at, level, line) VALUES ($1,$2,$3,$4,$5)`, r.id, seq, r.m.Now(), string(level), truncate(line, 4000))
	if err != nil {
		r.m.Log.Warn("writing a task log line failed", "task", r.id, "err", err)
	}
}

func (r *run) publish(force bool) {
	if r.m.Publish == nil {
		return
	}
	r.mu.Lock()
	now := time.Now()
	if !force && now.Sub(r.lastPub) < time.Second {
		r.mu.Unlock()
		return
	}
	r.lastPub = now
	r.mu.Unlock()
	if t, err := r.m.get(r.m.base, r.id); err == nil {
		r.m.Publish(*t)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Start records a task and runs fn in the background. A second exclusive task of one repository is
// refused with ErrBusy (naming the task that holds the repository).
func (m *Manager) Start(ctx context.Context, sp Spec, fn Func) (*Handle, error) {
	m.init()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, errors.New("the task manager is shutting down")
	}
	if sp.Kind == "" {
		return nil, errors.New("tasks: kind is required")
	}
	if sp.ActorKind == "" {
		sp.ActorKind = ActorSystem
	}
	if sp.Actor == "" && sp.ActorKind == ActorSystem {
		sp.Actor = "system"
	}
	params := []byte("{}")
	if sp.Params != nil {
		b, err := json.Marshal(sp.Params)
		if err != nil {
			return nil, err
		}
		params = b
	}
	var repoID *string
	if sp.RepositoryID != "" {
		repoID = &sp.RepositoryID
	}
	now := m.Now()
	var id string
	err := m.Pool.QueryRow(ctx, `INSERT INTO tasks (kind, repository_id, repository_name, started_by, actor_kind, state, params, exclusive, created_at, started_at, message)
		VALUES ($1,$2,$3,$4,$5,'running',$6,$7,$8,$8,'starting') RETURNING id`,
		sp.Kind, repoID, sp.Repository, sp.Actor, sp.ActorKind, params, sp.Exclusive, now).Scan(&id)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" && sp.Exclusive {
			return nil, m.busy(ctx, sp)
		}
		return nil, fmt.Errorf("recording the task: %w", err)
	}
	tctx, cancel := context.WithCancel(m.base)
	h := &Handle{ID: id, done: make(chan struct{})}
	r := &run{m: m, id: id, cancel: cancel, h: h, msg: "starting"}
	m.mu.Lock()
	m.active[id] = r
	m.wg.Add(1)
	m.mu.Unlock()
	r.publish(true)
	go m.execute(progress.With(tctx, r), r, sp, fn)
	return h, nil
}

// busy builds the ErrBusy error naming the task that holds the repository.
func (m *Manager) busy(ctx context.Context, sp Spec) error {
	var id, kind, by string
	var started time.Time
	err := m.Pool.QueryRow(ctx, `SELECT id, kind, started_by, created_at FROM tasks WHERE repository_id=$1 AND exclusive AND state IN ('queued','running') LIMIT 1`, sp.RepositoryID).
		Scan(&id, &kind, &by, &started)
	if err != nil {
		return fmt.Errorf("%w: another destructive task runs on repository %s; wait for it to finish", ErrBusy, sp.Repository)
	}
	return fmt.Errorf("%w: task %s (%s, started by %s at %s) runs on repository %s; wait for it to finish or cancel it", ErrBusy, kind, id, by, started.UTC().Format(time.RFC3339), sp.Repository)
}

func (m *Manager) execute(ctx context.Context, r *run, sp Spec, fn Func) {
	defer m.wg.Done()
	var res any
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("internal error: %v", p)
				m.Log.Error("task panicked", "task", r.id, "kind", sp.Kind, "panic", p)
			}
		}()
		res, err = fn(ctx)
	}()
	state := Succeeded
	msg := "done"
	errText := ""
	if err != nil {
		state, msg, errText = Failed, "failed", err.Error()
	}
	if ctx.Err() != nil && (err == nil || errors.Is(err, context.Canceled)) {
		r.mu.Lock()
		by := r.by
		r.mu.Unlock()
		if by == "" {
			by = "the server shutting down"
		}
		state, msg, errText = Cancelled, "cancelled", "cancelled by "+by
		r.Log(progress.Warn, "cancelled by "+by)
	} else if err != nil {
		r.Log(progress.Error, err.Error())
	}
	var rb []byte
	if res != nil {
		rb, _ = json.Marshal(res)
	}
	r.mu.Lock()
	lines, dropped, pct := r.seq, r.dropped, r.pct
	r.mu.Unlock()
	if state == Succeeded {
		pct = 100
	}
	if dropped > 0 {
		msg += fmt.Sprintf(" (%d log lines dropped)", dropped)
	}
	// written with the manager's context: a cancelled task still records how it ended
	wctx, cancel := context.WithTimeout(context.WithoutCancel(m.base), 30*time.Second)
	_, werr := m.Pool.Exec(wctx, `UPDATE tasks SET state=$2, progress=$3, message=$4, error=$5, result=$6, finished_at=$7, log_lines=$8, log_dropped=$9 WHERE id=$1`,
		r.id, state, pct, msg, truncate(errText, 4000), rb, m.Now(), lines, dropped)
	cancel()
	if werr != nil {
		m.Log.Error("recording the end of a task failed", "task", r.id, "err", werr)
	}
	r.cancel()
	m.mu.Lock()
	delete(m.active, r.id)
	m.mu.Unlock()
	r.h.res, r.h.err = res, err
	if state == Cancelled && err == nil {
		r.h.err = context.Canceled
	}
	close(r.h.done)
	r.publish(true)
}

// Run starts a task and waits for it (the scheduler, and ?wait=1 requests).
func (m *Manager) Run(ctx context.Context, sp Spec, fn Func) (string, any, error) {
	h, err := m.Start(ctx, sp, fn)
	if err != nil {
		return "", nil, err
	}
	res, err := h.Wait(ctx)
	return h.ID, res, err
}

// Cancel asks a task to stop. A task of this process is cancelled through its context (it ends as
// "cancelled" when the operation returns); a queued or running row no process runs (stale) is closed
// directly. A finished task gives ErrConflict.
func (m *Manager) Cancel(ctx context.Context, id, by string) (*Task, error) {
	m.init()
	t, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Done() {
		return t, fmt.Errorf("%w: the task already ended (%s)", service.ErrConflict, t.State)
	}
	if by == "" {
		by = "someone"
	}
	m.mu.Lock()
	r := m.active[id]
	m.mu.Unlock()
	_, _ = m.Pool.Exec(ctx, `UPDATE tasks SET cancel_requested=true WHERE id=$1`, id)
	if r != nil {
		r.mu.Lock()
		if r.by == "" {
			r.by = by
		}
		r.mu.Unlock()
		r.Log(progress.Warn, "cancel requested by "+by)
		r.cancel()
	} else {
		_, err = m.Pool.Exec(ctx, `UPDATE tasks SET state='cancelled', error=$2, finished_at=$3, message='cancelled' WHERE id=$1 AND state IN ('queued','running')`,
			id, "cancelled by "+by+" (no process was running it)", m.Now())
		if err != nil {
			return nil, err
		}
	}
	return m.get(ctx, id)
}

// Recover fails every task left queued or running by a previous daemon, and closes their maintenance
// runs. Only the daemon calls it, right after taking the daemon lock.
func (m *Manager) Recover(ctx context.Context) (int, error) {
	m.init()
	now := m.Now()
	ct, err := m.Pool.Exec(ctx, `UPDATE tasks SET state='failed', error='server restarted while the task was running', message='interrupted', finished_at=$1
		WHERE state IN ('queued','running')`, now)
	if err != nil {
		return 0, err
	}
	// scheduled runs that never finished (the process died): failed, so they are visible and retried
	_, _ = m.Pool.Exec(ctx, `UPDATE maintenance_runs SET status='failed', finished_at=$1, error='server restarted while the run was in progress' WHERE status='running'`, now)
	_, _ = m.Pool.Exec(ctx, `UPDATE sync_runs SET status='failed', finished_at=$1, error='server restarted while the run was in progress' WHERE status='running'`, now)
	return int(ct.RowsAffected()), nil
}

// Purge removes finished tasks older than KeepFor and all but the newest KeepMax finished ones.
func (m *Manager) Purge(ctx context.Context) (int64, error) {
	m.init()
	ct, err := m.Pool.Exec(ctx, `DELETE FROM tasks WHERE state NOT IN ('queued','running') AND COALESCE(finished_at, created_at) < $1`, m.Now().Add(-m.KeepFor))
	if err != nil {
		return 0, err
	}
	n := ct.RowsAffected()
	ct, err = m.Pool.Exec(ctx, `DELETE FROM tasks WHERE id IN (SELECT id FROM tasks WHERE state NOT IN ('queued','running') ORDER BY created_at DESC, seq DESC OFFSET $1)`, m.KeepMax)
	if err != nil {
		return n, err
	}
	return n + ct.RowsAffected(), nil
}

// Loop purges the history every hour until ctx ends.
func (m *Manager) Loop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, err := m.Purge(ctx); err != nil && ctx.Err() == nil {
			m.Log.Warn("purging the task history failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Close cancels every running task and waits for them to record their end.
func (m *Manager) Close() {
	m.init()
	m.mu.Lock()
	m.closed = true
	for _, r := range m.active {
		r.mu.Lock()
		if r.by == "" {
			r.by = "the server shutting down"
		}
		r.mu.Unlock()
		r.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.stop()
}

// ActiveHere reports whether this process runs the task.
func (m *Manager) ActiveHere(id string) bool {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[id] != nil
}

const taskCols = `id, kind, repository_name, started_by, actor_kind, state, progress, message, params, result, error, exclusive, cancel_requested,
	log_lines, log_dropped, created_at, started_at, finished_at`

func (m *Manager) scan(row pgx.Row) (*Task, error) {
	t := &Task{}
	var params, result []byte
	if err := row.Scan(&t.ID, &t.Kind, &t.Repository, &t.StartedBy, &t.ActorKind, &t.State, &t.Progress, &t.Message, &params, &result, &t.Error,
		&t.Exclusive, &t.CancelRequested, &t.LogLines, &t.LogDropped, &t.Created, &t.Started, &t.Finished); err != nil {
		return nil, err
	}
	t.Params = params
	if len(result) > 0 {
		t.Result = result
	}
	if t.Started != nil {
		end := m.Now()
		if t.Finished != nil {
			end = *t.Finished
		}
		t.DurationMs = end.Sub(*t.Started).Milliseconds()
	}
	return t, nil
}

func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

func (m *Manager) get(ctx context.Context, id string) (*Task, error) {
	if !validID(id) {
		return nil, fmt.Errorf("%w: task %q", service.ErrNotFound, id)
	}
	t, err := m.scan(m.Pool.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %s", service.ErrNotFound, id)
	}
	if err == nil && !t.Done() {
		// lines written but not yet counted in the row
		m.mu.Lock()
		r := m.active[id]
		m.mu.Unlock()
		if r != nil {
			r.mu.Lock()
			t.LogLines, t.LogDropped = r.seq, r.dropped
			r.mu.Unlock()
		}
	}
	return t, err
}

// Get returns a task and its log lines with seq > after (at most limit, default 1000).
func (m *Manager) Get(ctx context.Context, id string, after, limit int) (*Detail, error) {
	m.init()
	t, err := m.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := m.Pool.Query(ctx, `SELECT seq, at, level, line FROM task_log WHERE task_id=$1 AND seq > $2 ORDER BY seq LIMIT $3`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d := &Detail{Task: *t, Log: []LogLine{}, NextSeq: after}
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.Seq, &l.At, &l.Level, &l.Line); err != nil {
			return nil, err
		}
		d.Log = append(d.Log, l)
		d.NextSeq = l.Seq
	}
	return d, rows.Err()
}

// Filter selects tasks for List.
type Filter struct {
	State      string // queued|running|succeeded|failed|cancelled|active (= queued or running)
	Kind       string
	Repository string
	Actor      string // started_by
	Limit      int    // default 200, at most 2000
	Before     int64  // seq cursor: only tasks with seq < Before
}

// List returns tasks, newest first.
func (m *Manager) List(ctx context.Context, f Filter) ([]Task, error) {
	m.init()
	if f.Limit <= 0 {
		f.Limit = 200
	}
	if f.Limit > 2000 {
		f.Limit = 2000
	}
	q := `SELECT ` + taskCols + ` FROM tasks WHERE true`
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	switch f.State {
	case "":
	case "active":
		q += ` AND state IN ('queued','running')`
	case Queued, Running, Succeeded, Failed, Cancelled:
		add("state=$%d", f.State)
	default:
		return nil, fmt.Errorf("%w: state must be queued, running, succeeded, failed, cancelled or active", service.ErrBadRequest)
	}
	if f.Kind != "" {
		add("kind=$%d", f.Kind)
	}
	if f.Repository != "" {
		add("repository_name=$%d", f.Repository)
	}
	if f.Actor != "" {
		add("started_by=$%d", f.Actor)
	}
	if f.Before > 0 {
		add("seq < $%d", f.Before)
	}
	args = append(args, f.Limit)
	q += fmt.Sprintf(" ORDER BY created_at DESC, seq DESC LIMIT $%d", len(args))
	rows, err := m.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := m.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// RunningByKind counts active tasks per kind (metrics, the top-bar indicator).
func (m *Manager) RunningByKind(ctx context.Context) (map[string]int, error) {
	rows, err := m.Pool.Query(ctx, `SELECT kind, count(*) FROM tasks WHERE state IN ('queued','running') GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
