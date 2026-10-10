package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/progress"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/schedule"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// Manager owns sync jobs: their configuration and credentials, scheduling and
// the run history stored in PostgreSQL. Credentials live under
// StateDir/sync/<repo>/<job>/ with mode 0600; the database holds no secrets.
type Manager struct {
	Svc *service.Service
}

// Job is a sync job.
type Job struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Direction        string     `json:"direction"` // push | pull
	Remote           string     `json:"remote"`
	RemoteRepository string     `json:"remote_repository"`
	Schedule         string     `json:"schedule"`
	GroupFilter      []string   `json:"group_filter"`
	Enabled          bool       `json:"enabled"`
	LastSuccess      *time.Time `json:"last_success"`
	// PBS-class controls (see Options).
	VerifiedOnly  bool  `json:"verified_only"`
	ResyncCorrupt bool  `json:"resync_corrupt"`
	TransferLast  int   `json:"transfer_last"`
	RateLimitBps  int64 `json:"rate_limit_bps"`
}

const jobCols = `id, name, direction, remote, remote_repository, schedule, group_filter, enabled, last_success_at, verified_only, resync_corrupt, transfer_last, rate_limit_bps`

func (j *Job) scan(r pgx.Row) error {
	return r.Scan(&j.ID, &j.Name, &j.Direction, &j.Remote, &j.RemoteRepository, &j.Schedule, &j.GroupFilter, &j.Enabled, &j.LastSuccess,
		&j.VerifiedOnly, &j.ResyncCorrupt, &j.TransferLast, &j.RateLimitBps)
}

func (m *Manager) credDir(repo, job string) string {
	return filepath.Join(m.Svc.StateDir, "sync", repo, job)
}

// AddJob enrols this server at the remote with a one-time token and stores the
// job. The token's scope must match the direction: push needs sync-write on
// the remote, pull needs sync-read. Credentials are never admin credentials.
func (m *Manager) AddJob(ctx context.Context, repoName string, j Job, token string) error {
	if !jobNameRe.MatchString(j.Name) {
		return fmt.Errorf("%w: invalid job name %q (letters, digits, . _ - ; not . or ..)", service.ErrBadRequest, j.Name)
	}
	if j.TransferLast < 0 || j.RateLimitBps < 0 {
		return fmt.Errorf("%w: transfer-last and rate limit must not be negative", service.ErrBadRequest)
	}
	if err := validGroupFilter(j.GroupFilter); err != nil {
		return err
	}
	if j.Direction != "push" && j.Direction != "pull" {
		return fmt.Errorf("direction must be push or pull")
	}
	if _, err := schedule.Parse(j.Schedule); err != nil {
		return err
	}
	repo, err := m.Svc.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	var dup int
	if err := m.Svc.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE repository_id=$1 AND name=$2`, repo.ID, j.Name).Scan(&dup); err != nil {
		return err
	}
	if dup > 0 {
		return fmt.Errorf("%w: a sync job called %q exists already (the token would be consumed for nothing)", service.ErrConflict, j.Name)
	}
	en, err := remote.Enroll(ctx, j.Remote, token, "sync-"+repoName+"-"+j.Name, map[string]any{"role": "sync peer", "direction": j.Direction})
	if err != nil {
		return fmt.Errorf("enrolling at %s: %w", j.Remote, err)
	}
	want := api.ScopeSyncRead
	if j.Direction == "push" {
		want = api.ScopeSyncWrite
	}
	if en.Resp.Scope != want {
		return fmt.Errorf("the token grants scope %q but a %s job needs %q", en.Resp.Scope, j.Direction, want)
	}
	dir := m.credDir(repoName, j.Name)
	if _, serr := os.Stat(dir); serr == nil {
		return fmt.Errorf("%w: credentials for %q already exist on disk", service.ErrConflict, j.Name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(dir) // never leave certificates of a job that was not stored
		}
	}()
	for name, b := range map[string][]byte{"ca.crt": en.CAPEM, "client.crt": en.CertPEM, "client.key": en.KeyPEM} {
		mode := os.FileMode(0o600)
		if err := os.WriteFile(filepath.Join(dir, name), b, mode); err != nil {
			return err
		}
	}
	if j.GroupFilter == nil {
		j.GroupFilter = []string{}
	}
	if j.TransferLast < 0 || j.RateLimitBps < 0 {
		return fmt.Errorf("transfer-last and rate limit must not be negative")
	}
	_, err = m.Svc.DB.Pool().Exec(ctx, `INSERT INTO sync_jobs (repository_id, name, direction, remote, remote_repository, schedule, group_filter, verified_only, resync_corrupt, transfer_last, rate_limit_bps)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, repo.ID, j.Name, j.Direction, j.Remote, en.Resp.Repository, j.Schedule, j.GroupFilter,
		j.VerifiedOnly, j.ResyncCorrupt, j.TransferLast, j.RateLimitBps)
	if err != nil {
		return fmt.Errorf("storing sync job: %w", err)
	}
	committed = true
	return nil
}

// RemoveJob deletes a job and its credentials.
func (m *Manager) RemoveJob(ctx context.Context, repoName, name string) error {
	repo, err := m.Svc.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	ct, err := m.Svc.DB.Pool().Exec(ctx, `DELETE FROM sync_jobs WHERE repository_id=$1 AND name=$2`, repo.ID, name)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: sync job %q", service.ErrNotFound, name)
	}
	return os.RemoveAll(m.credDir(repoName, name))
}

// Jobs lists jobs.
func (m *Manager) Jobs(ctx context.Context, repoName string) ([]Job, error) {
	repo, err := m.Svc.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	rows, err := m.Svc.DB.Pool().Query(ctx, `SELECT `+jobCols+` FROM sync_jobs WHERE repository_id=$1 ORDER BY name`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := j.scan(rows); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (m *Manager) job(ctx context.Context, repoName, name string) (*Job, *service.Repo, error) {
	repo, err := m.Svc.Repository(ctx, repoName)
	if err != nil {
		return nil, nil, err
	}
	var j Job
	err = j.scan(m.Svc.DB.Pool().QueryRow(ctx, `SELECT `+jobCols+` FROM sync_jobs WHERE repository_id=$1 AND name=$2`, repo.ID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: sync job %q", service.ErrNotFound, name)
	}
	return &j, repo, err
}

func (m *Manager) remoteClient(repoName string, j *Job) (*remote.Client, error) {
	dir := m.credDir(repoName, j.Name)
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		return nil, fmt.Errorf("sync credentials missing: %w", err)
	}
	return remote.NewFromDir(j.Remote, dir, "")
}

// Run executes one job now. The initiator is this server in both directions.
func (m *Manager) Run(ctx context.Context, repoName, name string) (*Result, error) {
	// a sync writes chunks into a local repository (pull) or reads them (push) and relies on this
	// process's pins: it needs the maintenance lock like every other destructive/long operation
	release, gerr := m.Svc.Guard(ctx)
	if gerr != nil {
		return nil, gerr
	}
	defer release()
	j, repo, err := m.job(ctx, repoName, name)
	if err != nil {
		return nil, err
	}
	rc, err := m.remoteClient(repoName, j)
	if err != nil {
		return nil, err
	}
	m.RenewPeer(ctx, repoName, j.Name, rc)
	return m.RunWith(ctx, repo, j, rc)
}

// RunWith runs a job against an already built remote client (tests use this).
func (m *Manager) RunWith(ctx context.Context, repo *service.Repo, j *Job, rc *remote.Client) (*Result, error) {
	local := &LocalRepo{Svc: m.Svc, Repo: repo, Groups: j.GroupFilter}
	rem := &RemoteRepo{C: rc}
	var src Source = local
	var dst Dest = rem
	if j.Direction == "pull" {
		src, dst = rem, local
	}
	var runID string
	if err := m.Svc.DB.Pool().QueryRow(ctx, `INSERT INTO sync_runs (job_id, started_at) VALUES ($1,$2) RETURNING id`, j.ID, m.Svc.Now()).Scan(&runID); err != nil {
		return nil, err
	}
	res, err := Run(ctx, src, dst, Options{
		GroupFilter: j.GroupFilter, VerifiedOnly: j.VerifiedOnly, ResyncCorrupt: j.ResyncCorrupt, TransferLast: j.TransferLast, RateLimit: j.RateLimitBps,
		OnState: func(g, s, st, e string) {
			if e != "" {
				progress.Logf(ctx, progress.Warn, "%s/%s: %s: %s", g, s, st, e)
			} else {
				progress.Logf(ctx, progress.Info, "%s/%s: %s", g, s, st)
			}
			_, _ = m.Svc.DB.Pool().Exec(ctx, `INSERT INTO sync_snapshot_state (job_id, group_id, snapshot_id, state, error) VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (job_id, group_id, snapshot_id) DO UPDATE SET state=EXCLUDED.state, error=EXCLUDED.error, updated_at=now()`, j.ID, g, s, st, e)
		},
	})
	status, msg := "ok", ""
	if res == nil {
		res = &Result{}
	}
	if len(res.RepairedList) > 0 && j.Direction == "pull" {
		// the destination is this server: check what we just repaired so the failed state clears
		_, _ = m.Svc.VerifyJob(ctx, repo.Name, service.VerifyRequest{Only: res.RepairedList})
	}
	switch {
	case err != nil && res.Synced > 0:
		status, msg = "partial", err.Error()
	case err != nil:
		status, msg = "failed", err.Error()
	}
	ctx = context.WithoutCancel(ctx) // a cancelled run still records how it ended
	_, _ = m.Svc.DB.Pool().Exec(ctx, `UPDATE sync_runs SET finished_at=$2, status=$3, snapshots_pending=$4, snapshots_synced=$5, bytes_transferred=$6,
		chunks_transferred=$7, chunks_reused=$8, snapshots_skipped=$9, snapshots_repaired=$10, error=$11 WHERE id=$1`, runID, m.Svc.Now(), status, res.Pending, res.Synced, res.BytesTransferred,
		res.ChunksTransferred, res.ChunksReused, res.SkippedUnverified+res.SkippedCorrupt, res.Repaired, msg)
	if err == nil {
		_, _ = m.Svc.DB.Pool().Exec(ctx, `UPDATE sync_jobs SET last_success_at=$2 WHERE id=$1`, j.ID, m.Svc.Now())
	}
	return res, err
}

// JobStatus is the operational status of a job.
type JobStatus struct {
	Job               `json:"job"`
	LastRunAt         *time.Time    `json:"last_run_at"`
	LastRunStatus     string        `json:"last_run_status"`
	LastError         string        `json:"last_error"`
	SnapshotsPending  int           `json:"snapshots_pending"`
	SnapshotsSynced   int           `json:"snapshots_synced"`
	BytesTransferred  int64         `json:"bytes_transferred"`
	ChunksTransferred int64         `json:"chunks_transferred"`
	ChunksReused      int64         `json:"chunks_reused"`
	Lag               time.Duration `json:"lag_ns"` // age of the oldest unsynchronised snapshot (0 if none)
	Failed            int           `json:"failed"`
	SnapshotsSkipped  int           `json:"snapshots_skipped"` // unverified/corrupt on the source, not transferred
	SnapshotsRepaired int           `json:"snapshots_repaired"`
}

// JobOptions are the changeable options of a job; nil means "leave as it is".
type JobOptions struct {
	VerifiedOnly  *bool     `json:"verified_only"`
	ResyncCorrupt *bool     `json:"resync_corrupt"`
	TransferLast  *int      `json:"transfer_last"`
	RateLimitBps  *int64    `json:"rate_limit_bps"`
	Groups        *[]string `json:"groups"`
}

var jobNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validGroupFilter(gs []string) error {
	for _, g := range gs {
		if !datastore.ValidGroup(g) {
			return fmt.Errorf("%q is not a group id (32 hex characters; `snapshot list` shows them): a name would make the job copy nothing", g)
		}
	}
	return nil
}

// SetOptions changes the PBS-class controls of an existing job; only the options given change.
func (m *Manager) SetOptions(ctx context.Context, repoName, name string, o JobOptions) error {
	j, repo, err := m.job(ctx, repoName, name)
	if err != nil {
		return err
	}
	if o.VerifiedOnly != nil {
		j.VerifiedOnly = *o.VerifiedOnly
	}
	if o.ResyncCorrupt != nil {
		j.ResyncCorrupt = *o.ResyncCorrupt
	}
	if o.TransferLast != nil {
		j.TransferLast = *o.TransferLast
	}
	if o.RateLimitBps != nil {
		j.RateLimitBps = *o.RateLimitBps
	}
	if o.Groups != nil {
		j.GroupFilter = *o.Groups
	}
	if j.TransferLast < 0 || j.RateLimitBps < 0 {
		return fmt.Errorf("transfer-last and rate limit must not be negative")
	}
	if err := validGroupFilter(j.GroupFilter); err != nil {
		return err
	}
	if j.GroupFilter == nil {
		j.GroupFilter = []string{}
	}
	_, err = m.Svc.DB.Pool().Exec(ctx, `UPDATE sync_jobs SET verified_only=$3, resync_corrupt=$4, transfer_last=$5, rate_limit_bps=$6, group_filter=$7 WHERE repository_id=$1 AND name=$2`,
		repo.ID, name, j.VerifiedOnly, j.ResyncCorrupt, j.TransferLast, j.RateLimitBps, j.GroupFilter)
	return err
}

// Status reports the last run and replication lag of a job.
func (m *Manager) Status(ctx context.Context, repoName, name string) (*JobStatus, error) {
	j, _, err := m.job(ctx, repoName, name)
	if err != nil {
		return nil, err
	}
	st := &JobStatus{Job: *j}
	p := m.Svc.DB.Pool()
	_ = p.QueryRow(ctx, `SELECT finished_at, status, error, snapshots_synced, bytes_transferred, chunks_transferred, chunks_reused, snapshots_skipped, snapshots_repaired
		FROM sync_runs WHERE job_id=$1 ORDER BY started_at DESC LIMIT 1`, j.ID).
		Scan(&st.LastRunAt, &st.LastRunStatus, &st.LastError, &st.SnapshotsSynced, &st.BytesTransferred, &st.ChunksTransferred, &st.ChunksReused, &st.SnapshotsSkipped, &st.SnapshotsRepaired)
	_ = p.QueryRow(ctx, `SELECT count(*) FROM sync_snapshot_state WHERE job_id=$1 AND state='failed'`, j.ID).Scan(&st.Failed)
	var oldest *time.Time
	_ = p.QueryRow(ctx, `SELECT min(sn.created_at) FROM snapshots sn
		WHERE sn.repository_id=(SELECT repository_id FROM sync_jobs WHERE id=$1)
		  AND $2 = 'push'
		  AND NOT EXISTS (SELECT 1 FROM sync_snapshot_state s WHERE s.job_id=$1 AND s.group_id=sn.group_id AND s.snapshot_id=sn.snapshot_id AND s.state IN ('synced','verified'))`,
		j.ID, j.Direction).Scan(&oldest)
	if oldest != nil {
		st.Lag = m.Svc.Now().Sub(*oldest)
		_ = p.QueryRow(ctx, `SELECT count(*) FROM snapshots sn WHERE sn.repository_id=(SELECT repository_id FROM sync_jobs WHERE id=$1)
			AND NOT EXISTS (SELECT 1 FROM sync_snapshot_state s WHERE s.job_id=$1 AND s.group_id=sn.group_id AND s.snapshot_id=sn.snapshot_id AND s.state IN ('synced','verified'))`, j.ID).Scan(&st.SnapshotsPending)
	}
	return st, nil
}

// Due returns the jobs whose schedule says they should run now.
func (m *Manager) Due(ctx context.Context, repoName string) ([]Job, error) {
	jobs, err := m.Jobs(ctx, repoName)
	if err != nil {
		return nil, err
	}
	now := m.Svc.Now()
	var due []Job
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		sp, err := schedule.Parse(j.Schedule)
		if err != nil {
			continue
		}
		var last time.Time
		if j.LastSuccess != nil {
			last = *j.LastSuccess
		}
		if sp.Due(last, now) {
			due = append(due, j)
		}
	}
	return due, nil
}
