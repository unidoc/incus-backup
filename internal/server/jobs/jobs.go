// Package jobs lets the management plane ASK hosts to do work that needs the payload key, without
// the server ever holding one. The server (CLI, admin API, later the browser GUI) creates a job for
// one host; the host pulls it (GET /v1/jobs/next, a long poll over mutual TLS), runs it with its own
// key, reports progress and log lines, may upload a file sealed to a recipient key the server does
// not have, and finishes it with a result.
//
// Rules:
//   - a host only ever sees, renews and finishes its own jobs (repository + host name of its
//     certificate, backup scope); everything else answers "not found";
//   - every host write is fenced by the attempt number, so a host declared dead that comes back after
//     the job was re-queued gets 409 instead of overwriting the new attempt;
//   - a running job is a lease (jobspec.Lease): a host that stops reporting is failed, and re-queued
//     while the type allows more attempts;
//   - every timestamp comes from the service clock (tests drive it).
//
// One Service exists per *service.Service (For), so the host routes, the management routes and the
// housekeeping loop share one change hub.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// Service is the job layer of one server.
type Service struct {
	Svc *service.Service

	hub *Hub

	wmu  sync.Mutex
	wake chan struct{}

	// PollInterval is how often a long poll re-checks the database (jobs created by another process
	// do not ring the in-process bell). Tests may lower it.
	PollInterval time.Duration
	// Retention of finished jobs.
	KeepFor     time.Duration
	KeepPerRepo int
}

var (
	regMu    sync.Mutex
	registry = map[*service.Service]*Service{}
)

// For returns THE job service of svc (created on first use).
func For(svc *service.Service) *Service {
	regMu.Lock()
	defer regMu.Unlock()
	if j, ok := registry[svc]; ok {
		return j
	}
	j := &Service{Svc: svc, hub: newHub(), wake: make(chan struct{}), PollInterval: time.Second, KeepFor: 30 * 24 * time.Hour, KeepPerRepo: 1000}
	registry[svc] = j
	return j
}

func (s *Service) now() time.Time { return s.Svc.Now().UTC() }

func (s *Service) ring() {
	s.wmu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.wmu.Unlock()
}

func (s *Service) bell() <-chan struct{} {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.wake
}

func bad(f string, a ...any) error {
	return fmt.Errorf("%w: %s", service.ErrBadRequest, fmt.Sprintf(f, a...))
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func checkID(id string) error {
	if !uuidRe.MatchString(id) {
		return fmt.Errorf("%w: job", service.ErrNotFound)
	}
	return nil
}

// Job is a job as the management plane sees it.
type Job struct {
	ID              string          `json:"id"`
	Repository      string          `json:"repository"`
	Host            string          `json:"host"`
	Type            string          `json:"type"`
	Params          json.RawMessage `json:"params"`
	State           string          `json:"state"`
	Progress        int             `json:"progress"`
	BytesDone       int64           `json:"bytes_done"`
	BytesTotal      int64           `json:"bytes_total"`
	Message         string          `json:"message"`
	Result          json.RawMessage `json:"result,omitempty"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
	CancelRequested bool            `json:"cancel_requested"`
	Attempt         int             `json:"attempt"`
	MaxAttempts     int             `json:"max_attempts"`
	LeaseUntil      *time.Time      `json:"lease_until,omitempty"`
	Permission      string          `json:"permission"`
	Delivery        *Delivery       `json:"delivery,omitempty"`
	Log             []LogEntry      `json:"log,omitempty"`
}

// Delivery describes a sealed file a job produced.
type Delivery struct {
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LogEntry is one log line.
type LogEntry struct {
	Seq   int       `json:"seq"`
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Line  string    `json:"line"`
}

const jobCols = `j.id::text, r.name, j.host_name, j.type, j.params, j.state, j.progress, j.bytes_done, j.bytes_total, j.message, j.result,
	j.created_by, j.created_at, j.started_at, j.finished_at, j.cancel_requested, j.attempt, j.max_attempts, j.lease_until,
	d.size, d.sha256, d.created_at, d.expires_at`

const jobFrom = ` FROM host_jobs j JOIN repositories r ON r.id=j.repository_id LEFT JOIN job_deliveries d ON d.job_id=j.id`

func scanJob(row pgx.Row) (*Job, error) {
	j := &Job{}
	var params, result []byte
	var dSize *int64
	var dSHA *string
	var dCreated, dExpires *time.Time
	if err := row.Scan(&j.ID, &j.Repository, &j.Host, &j.Type, &params, &j.State, &j.Progress, &j.BytesDone, &j.BytesTotal, &j.Message, &result,
		&j.CreatedBy, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.CancelRequested, &j.Attempt, &j.MaxAttempts, &j.LeaseUntil,
		&dSize, &dSHA, &dCreated, &dExpires); err != nil {
		return nil, err
	}
	j.Params = params
	if len(result) > 0 {
		j.Result = result
	}
	if dSize != nil {
		j.Delivery = &Delivery{Size: *dSize, SHA256: *dSHA, CreatedAt: *dCreated, ExpiresAt: *dExpires}
	}
	j.Permission = jobspec.RequiredPermission(j.Type)
	return j, nil
}

// ---- management API ------------------------------------------------------------------------------

// RequiredPermission is the authorisation the caller of CreateJob must hold for a type.
func RequiredPermission(typ string) string { return jobspec.RequiredPermission(typ) }

// CreateJob validates and queues a job for one host of a repository. principal is recorded as
// created_by (the management plane authenticates and authorises it; see RequiredPermission).
func (s *Service) CreateJob(ctx context.Context, principal, repo, host, typ string, params json.RawMessage) (*Job, error) {
	info, ok := jobspec.Lookup(typ)
	if !ok {
		return nil, bad("unknown job type %q", typ)
	}
	_, canon, err := jobspec.Validate(typ, params)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", service.ErrBadRequest, err)
	}
	principal = clean(principal, jobspec.MaxNameLen)
	r, err := s.Svc.Repository(ctx, repo)
	if err != nil {
		return nil, err
	}
	var disabled bool
	err = s.Svc.DB.Pool().QueryRow(ctx, `SELECT disabled FROM hosts WHERE repository_id=$1 AND name=$2 AND scope=$3`, r.ID, host, api.ScopeBackup).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: host %q (backup scope) in repository %q", service.ErrNotFound, host, repo)
	}
	if err != nil {
		return nil, err
	}
	if disabled {
		return nil, fmt.Errorf("%w: host %q is disabled", service.ErrConflict, host)
	}
	now := s.now()
	var id string
	err = s.Svc.DB.Pool().QueryRow(ctx, `INSERT INTO host_jobs (repository_id, host_name, type, params, created_by, created_at, queue_until, max_attempts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, r.ID, host, typ, canon, principal, now, now.Add(jobspec.QueueTTL), info.MaxAttempts).Scan(&id)
	if err != nil {
		return nil, err
	}
	j, err := s.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	s.publish(j)
	s.ring()
	return j, nil
}

// JobFilter narrows ListJobs. Empty fields match everything.
type JobFilter struct {
	Repository string
	Host       string
	Type       string
	States     []string
	Before     time.Time // created before (paging)
	Limit      int       // default 100, max 1000
}

// ListJobs lists jobs, newest first (without their logs).
func (s *Service) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	q := `SELECT ` + jobCols + jobFrom + ` WHERE true`
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.Repository != "" {
		add("r.name=$%d", f.Repository)
	}
	if f.Host != "" {
		add("j.host_name=$%d", f.Host)
	}
	if f.Type != "" {
		add("j.type=$%d", f.Type)
	}
	if len(f.States) > 0 {
		add("j.state=ANY($%d)", f.States)
	}
	if !f.Before.IsZero() {
		add("j.created_at<$%d", f.Before)
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	q += fmt.Sprintf(" ORDER BY j.created_at DESC, j.seq DESC LIMIT %d", f.Limit)
	rows, err := s.Svc.DB.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// GetJob returns one job with its log.
func (s *Service) GetJob(ctx context.Context, id string) (*Job, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	j, err := scanJob(s.Svc.DB.Pool().QueryRow(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: job", service.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Svc.DB.Pool().Query(ctx, `SELECT seq, at, level, line FROM host_job_log WHERE job_id=$1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	j.Log = []LogEntry{}
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.Seq, &e.At, &e.Level, &e.Line); err != nil {
			return nil, err
		}
		j.Log = append(j.Log, e)
	}
	return j, rows.Err()
}

// Cancel cancels a job: a queued one at once, a running one by asking its host (the host sees it in
// its next progress answer and stops). principal is logged.
func (s *Service) Cancel(ctx context.Context, principal, id string) (*Job, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	now := s.now()
	tx, err := s.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM host_jobs WHERE id=$1 FOR UPDATE`, id).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: job", service.ErrNotFound)
		}
		return nil, err
	}
	who := clean(principal, jobspec.MaxNameLen)
	switch state {
	case jobspec.StateQueued:
		if _, err := tx.Exec(ctx, `UPDATE host_jobs SET state='cancelled', cancel_requested=true, finished_at=$2, message=$3 WHERE id=$1`,
			id, now, "cancelled by "+who+" before a host picked it up"); err != nil {
			return nil, err
		}
	case jobspec.StateRunning:
		if _, err := tx.Exec(ctx, `UPDATE host_jobs SET cancel_requested=true WHERE id=$1`, id); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: the job already ended (%s)", service.ErrConflict, state)
	}
	if err := appendLog(ctx, tx, id, now, []jobspec.LogLine{{Level: "info", Line: "cancel requested by " + who}}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	j, err := s.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	s.publish(j)
	return j, nil
}

// ---- host protocol ---------------------------------------------------------------------------------

func hostOK(h *service.Host) error {
	if h == nil || h.Scope != api.ScopeBackup {
		return fmt.Errorf("%w: only backup-scope hosts run jobs", service.ErrForbidden)
	}
	return nil
}

// Next claims the oldest queued job of the host whose type is in types (all when empty), waiting up
// to wait for one. It returns nil when there is none.
func (s *Service) Next(ctx context.Context, h *service.Host, wait time.Duration, types []string) (*jobspec.Job, error) {
	if err := hostOK(h); err != nil {
		return nil, err
	}
	for _, t := range types {
		if _, ok := jobspec.Lookup(t); !ok {
			return nil, bad("unknown job type %q", t)
		}
	}
	if wait < 0 {
		wait = 0
	}
	if wait > jobspec.MaxWait {
		wait = jobspec.MaxWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := s.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	for {
		bell := s.bell()
		j, err := s.claim(ctx, h, types)
		if err != nil || j != nil {
			return j, err
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, nil
		case <-deadline.C:
			t.Stop()
			return nil, nil
		case <-bell:
		case <-t.C:
		}
		t.Stop()
	}
}

func (s *Service) claim(ctx context.Context, h *service.Host, types []string) (*jobspec.Job, error) {
	now := s.now()
	if types == nil {
		types = jobspec.Types
	}
	tx, err := s.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	j := &jobspec.Job{}
	var params []byte
	err = tx.QueryRow(ctx, `UPDATE host_jobs SET state='running', attempt=attempt+1, started_at=$4, lease_until=$5, progress=0, bytes_done=0, bytes_total=0
		WHERE id = (SELECT id FROM host_jobs WHERE repository_id=$1 AND host_name=$2 AND state='queued' AND queue_until > $4 AND type=ANY($3)
			ORDER BY created_at, seq LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id::text, type, params, attempt, created_by, created_at`, h.RepoID, h.Name, types, now, now.Add(jobspec.Lease)).
		Scan(&j.ID, &j.Type, &params, &j.Attempt, &j.CreatedBy, &j.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Params = params
	info, _ := jobspec.Lookup(j.Type)
	j.LeaseSeconds = int(jobspec.Lease / time.Second)
	j.TimeoutSeconds = int(info.Timeout / time.Second)
	if err := appendLog(ctx, tx, j.ID, now, []jobspec.LogLine{{Level: "info", Line: fmt.Sprintf("picked up by host %s (attempt %d)", h.Name, j.Attempt)}}); err != nil {
		return nil, err
	}
	if j.Type == jobspec.TypeRestoreTest {
		var p jobspec.RestoreTest
		if json.Unmarshal(params, &p) == nil {
			if _, err := tx.Exec(ctx, `INSERT INTO restore_tests (repository_id, host_name, object, snapshot_id, method, state, started, job_id)
				VALUES ($1,$2,$3,$4,$5,'running',$6,$7)
				ON CONFLICT (job_id) WHERE job_id IS NOT NULL DO UPDATE SET state='running', started=EXCLUDED.started, finished=NULL, details='{}'`,
				h.RepoID, h.Name, p.Project+"/"+p.Object, p.Snapshot, p.Mode, now, j.ID); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.publishID(ctx, j.ID)
	return j, nil
}

// owned locks the job row of host h for an update by the given attempt. It returns ErrNotFound for a
// job that is not the host's (it must not learn that other hosts' jobs exist) and ErrConflict for a
// stale attempt or a job that is not running.
func owned(ctx context.Context, tx pgx.Tx, h *service.Host, id string, attempt int) (typ string, cancel bool, err error) {
	if err := checkID(id); err != nil {
		return "", false, err
	}
	var state string
	var cur int
	err = tx.QueryRow(ctx, `SELECT type, state, attempt, cancel_requested FROM host_jobs WHERE id=$1 AND repository_id=$2 AND host_name=$3 FOR UPDATE`,
		id, h.RepoID, h.Name).Scan(&typ, &state, &cur, &cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("%w: job", service.ErrNotFound)
	}
	if err != nil {
		return "", false, err
	}
	if state != jobspec.StateRunning {
		return typ, cancel, fmt.Errorf("%w: the job is %s", service.ErrConflict, state)
	}
	if cur != attempt {
		return typ, cancel, fmt.Errorf("%w: attempt %d is stale (the job is on attempt %d)", service.ErrConflict, attempt, cur)
	}
	return typ, cancel, nil
}

// Progress records progress and log lines, renews the lease and tells the host whether to stop.
func (s *Service) Progress(ctx context.Context, h *service.Host, id string, p jobspec.Progress) (*jobspec.ProgressResponse, error) {
	if err := hostOK(h); err != nil {
		return nil, err
	}
	if p.Progress < 0 || p.Progress > 100 || p.BytesDone < 0 || p.BytesTotal < 0 {
		return nil, bad("progress out of range")
	}
	if len(p.Log) > jobspec.MaxLinesPerCall {
		return nil, bad("at most %d log lines per call", jobspec.MaxLinesPerCall)
	}
	now := s.now()
	tx, err := s.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	_, cancel, err := owned(ctx, tx, h, id, p.Attempt)
	if err != nil {
		return nil, err
	}
	lease := now.Add(jobspec.Lease)
	msg := clean(p.Message, jobspec.MaxMessageLen)
	if _, err := tx.Exec(ctx, `UPDATE host_jobs SET progress=$2, bytes_done=$3, bytes_total=$4, message=CASE WHEN $5='' THEN message ELSE $5 END, lease_until=$6 WHERE id=$1`,
		id, p.Progress, p.BytesDone, p.BytesTotal, msg, lease); err != nil {
		return nil, err
	}
	if err := appendLog(ctx, tx, id, now, p.Log); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.publishID(ctx, id)
	return &jobspec.ProgressResponse{CancelRequested: cancel, LeaseUntil: lease}, nil
}

// CancelRequested tells a host whether its job should stop (progress answers carry the same flag).
func (s *Service) CancelRequested(ctx context.Context, h *service.Host, id string) (*jobspec.CancelState, error) {
	if err := hostOK(h); err != nil {
		return nil, err
	}
	if err := checkID(id); err != nil {
		return nil, err
	}
	cs := &jobspec.CancelState{}
	err := s.Svc.DB.Pool().QueryRow(ctx, `SELECT cancel_requested, state FROM host_jobs WHERE id=$1 AND repository_id=$2 AND host_name=$3`,
		id, h.RepoID, h.Name).Scan(&cs.CancelRequested, &cs.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: job", service.ErrNotFound)
	}
	return cs, err
}

const maxResultBytes = 1 << 20

// Finish ends a job with its result. A restore_test result is recorded in restore_tests; a delivery
// job can only succeed when its sealed file was uploaded completely (and the host's checksum of it
// matches the server's).
func (s *Service) Finish(ctx context.Context, h *service.Host, id string, f jobspec.Finish) error {
	if err := hostOK(h); err != nil {
		return err
	}
	switch f.State {
	case jobspec.StateSucceeded, jobspec.StateFailed, jobspec.StateCancelled:
	default:
		return bad("a host can finish a job only as succeeded, failed or cancelled")
	}
	if len(f.Result) > maxResultBytes {
		return bad("result too large")
	}
	if len(f.Result) > 0 && !json.Valid(f.Result) {
		return bad("result is not JSON")
	}
	if len(f.Log) > jobspec.MaxLinesPerCall {
		return bad("at most %d log lines per call", jobspec.MaxLinesPerCall)
	}
	now := s.now()
	tx, err := s.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	typ, _, err := owned(ctx, tx, h, id, f.Attempt)
	if err != nil {
		return err
	}
	info, _ := jobspec.Lookup(typ)
	state, msg := f.State, clean(f.Message, jobspec.MaxMessageLen)
	if info.Delivery && state == jobspec.StateSucceeded {
		var dr jobspec.DeliveryResult
		if err := json.Unmarshal(f.Result, &dr); err != nil {
			return bad("a delivery job needs a delivery result")
		}
		var size int64
		var sum string
		err := tx.QueryRow(ctx, `SELECT size, sha256 FROM job_deliveries WHERE job_id=$1 AND storage_key=$2`, id, deliveryPrefix(id, f.Attempt)).Scan(&size, &sum)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			state, msg = jobspec.StateFailed, "the host reported success but no complete delivery was uploaded"
		case err != nil:
			return err
		case size != dr.Size || sum != dr.SHA256:
			state, msg = jobspec.StateFailed, fmt.Sprintf("the delivery the server stored (%d bytes, sha256 %s) differs from what the host sent (%d bytes, sha256 %s)", size, sum, dr.Size, dr.SHA256)
		}
	}
	var result any
	if len(f.Result) > 0 {
		result = []byte(f.Result)
	}
	progress := "progress"
	if state == jobspec.StateSucceeded {
		progress = "100"
	}
	if _, err := tx.Exec(ctx, `UPDATE host_jobs SET state=$2, message=$3, result=$4, finished_at=$5, lease_until=NULL, progress=`+progress+` WHERE id=$1`,
		id, state, msg, result, now); err != nil {
		return err
	}
	if err := appendLog(ctx, tx, id, now, f.Log); err != nil {
		return err
	}
	line := "finished: " + state
	if msg != "" {
		line += ": " + msg
	}
	if err := appendLog(ctx, tx, id, now, []jobspec.LogLine{{Level: "info", Line: line}}); err != nil {
		return err
	}
	if typ == jobspec.TypeRestoreTest {
		if err := recordJobRestoreTest(ctx, tx, id, state, msg, f.Result, now); err != nil {
			return err
		}
	}
	// a delivery of a job that did not succeed is never handed out: drop it now
	var dropKey string
	if info.Delivery && state != jobspec.StateSucceeded {
		if err := tx.QueryRow(ctx, `DELETE FROM job_deliveries WHERE job_id=$1 RETURNING storage_key`, id).Scan(&dropKey); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if info.Delivery && state != jobspec.StateSucceeded {
		if b, err := s.backendByRepoID(ctx, h.RepoID); err == nil {
			_ = deletePrefix(b, deliveryPrefix(id, f.Attempt)) // anything missed is an orphan for the reaper
			if dropKey != "" {
				_ = deletePrefix(b, dropKey)
			}
		}
	}
	s.publishID(ctx, id)
	return nil
}

// ---- helpers ---------------------------------------------------------------------------------------

// clean makes host- or operator-supplied text safe to store and show: control characters (except
// tab) become spaces, the length is bounded.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return ' '
		}
		return r
	}, s)
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// appendLog adds lines to a job's log, keeping at most jobspec.MaxLogLines (one marker line notes
// the truncation).
func appendLog(ctx context.Context, tx pgx.Tx, id string, at time.Time, lines []jobspec.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0) FROM host_job_log WHERE job_id=$1`, id).Scan(&n); err != nil {
		return err
	}
	for _, l := range lines {
		if n >= jobspec.MaxLogLines+1 {
			return nil
		}
		lvl := l.Level
		switch lvl {
		case "debug", "info", "warn", "error":
		case "":
			lvl = "info"
		default:
			lvl = "info"
		}
		line := clean(l.Line, jobspec.MaxLogLineLen)
		if n == jobspec.MaxLogLines {
			lvl, line = "warn", fmt.Sprintf("log truncated after %d lines", jobspec.MaxLogLines)
		}
		n++
		if _, err := tx.Exec(ctx, `INSERT INTO host_job_log (job_id, seq, at, level, line) VALUES ($1,$2,$3,$4,$5)`, id, n, at, lvl, line); err != nil {
			return err
		}
	}
	return nil
}
