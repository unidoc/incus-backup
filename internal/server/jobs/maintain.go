package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/jobspec"
)

// MaintainStats reports what one housekeeping pass did.
type MaintainStats struct {
	Requeued   int `json:"requeued"`
	Failed     int `json:"failed"`
	Expired    int `json:"expired"`
	Deliveries int `json:"deliveries"`
	Deleted    int `json:"deleted"`
}

// Maintain runs every job housekeeping step: stale leases, queue expiry, delivery reaping and the
// retention of finished jobs. The server daemon calls it from its maintenance tick.
func (s *Service) Maintain(ctx context.Context) (MaintainStats, error) {
	var st MaintainStats
	var errs []error
	var err error
	if st.Requeued, st.Failed, err = s.RequeueStale(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stale jobs: %w", err))
	}
	if st.Expired, err = s.ExpireQueued(ctx); err != nil {
		errs = append(errs, fmt.Errorf("queue expiry: %w", err))
	}
	if st.Deliveries, err = s.ReapDeliveries(ctx); err != nil {
		errs = append(errs, fmt.Errorf("deliveries: %w", err))
	}
	if st.Deleted, err = s.Retention(ctx); err != nil {
		errs = append(errs, fmt.Errorf("retention: %w", err))
	}
	return st, errors.Join(errs...)
}

// RequeueStale handles running jobs whose lease ran out (the host stopped reporting for
// jobspec.Lease): a job with attempts left goes back to the queue, any other fails; a job whose
// cancellation was requested is cancelled. Their partial deliveries are removed.
func (s *Service) RequeueStale(ctx context.Context) (requeued, failed int, err error) {
	now := s.now()
	rows, err := s.Svc.DB.Pool().Query(ctx, `SELECT id::text FROM host_jobs WHERE state='running' AND lease_until < $1`, now)
	if err != nil {
		return 0, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var errs []error
	for _, id := range ids {
		r, err := s.expireLease(ctx, id, now)
		switch {
		case err != nil:
			errs = append(errs, err)
		case r == jobspec.StateQueued:
			requeued++
		case r != "":
			failed++
		}
		if err == nil && r != "" {
			s.publishID(ctx, id)
		}
	}
	if requeued > 0 {
		s.ring()
	}
	return requeued, failed, errors.Join(errs...)
}

func (s *Service) expireLease(ctx context.Context, id string, now time.Time) (string, error) {
	tx, err := s.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var attempt, max int
	var cancel bool
	var repoID string
	err = tx.QueryRow(ctx, `SELECT attempt, max_attempts, cancel_requested, repository_id::text FROM host_jobs WHERE id=$1 AND state='running' AND lease_until < $2 FOR UPDATE`,
		id, now).Scan(&attempt, &max, &cancel, &repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // renewed or finished meanwhile
	}
	if err != nil {
		return "", err
	}
	why := fmt.Sprintf("the host stopped reporting for %s (attempt %d of %d)", jobspec.Lease, attempt, max)
	var next string
	switch {
	case cancel:
		next = jobspec.StateCancelled
		_, err = tx.Exec(ctx, `UPDATE host_jobs SET state='cancelled', finished_at=$2, lease_until=NULL, message=$3 WHERE id=$1`, id, now, why+"; cancelled as requested")
	case attempt < max:
		next = jobspec.StateQueued
		_, err = tx.Exec(ctx, `UPDATE host_jobs SET state='queued', lease_until=NULL, started_at=NULL, progress=0, bytes_done=0, bytes_total=0, message=$2, queue_until=$3 WHERE id=$1`,
			id, why+"; queued again", now.Add(jobspec.QueueTTL))
	default:
		next = jobspec.StateFailed
		_, err = tx.Exec(ctx, `UPDATE host_jobs SET state='failed', finished_at=$2, lease_until=NULL, message=$3 WHERE id=$1`, id, now, why)
	}
	if err != nil {
		return "", err
	}
	if err := appendLog(ctx, tx, id, now, []jobspec.LogLine{{Level: "warn", Line: why + " -> " + next}}); err != nil {
		return "", err
	}
	if next == jobspec.StateQueued {
		_, err = tx.Exec(ctx, `UPDATE restore_tests SET state='running', details='{}' WHERE job_id=$1`, id)
	} else {
		err = failRunningRestoreTest(ctx, tx, id, why, now)
	}
	if err != nil {
		return "", err
	}
	var key string
	_ = tx.QueryRow(ctx, `DELETE FROM job_deliveries WHERE job_id=$1 RETURNING storage_key`, id).Scan(&key)
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	if b, err := s.backendByRepoID(ctx, repoID); err == nil {
		// the attempt's (partial) parts; anything missed is an orphan the reaper removes
		_ = deletePrefix(b, deliveryPrefix(id, attempt))
		if key != "" {
			_ = deletePrefix(b, key)
		}
	}
	return next, nil
}

// ExpireQueued expires queued jobs no host picked up in time.
func (s *Service) ExpireQueued(ctx context.Context) (int, error) {
	now := s.now()
	rows, err := s.Svc.DB.Pool().Query(ctx, `UPDATE host_jobs SET state='expired', finished_at=$1, message='no host picked the job up in time'
		WHERE state='queued' AND queue_until <= $1 RETURNING id::text`, now)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.publishID(ctx, id)
	}
	return len(ids), rows.Err()
}

// Retention deletes finished jobs older than KeepFor and, per repository, all but the newest
// KeepPerRepo finished jobs. Their deliveries are deleted from the datastore first; restore test
// records survive (their job reference becomes NULL).
func (s *Service) Retention(ctx context.Context) (int, error) {
	now := s.now()
	keep := s.KeepPerRepo
	if keep <= 0 {
		keep = 1000
	}
	rows, err := s.Svc.DB.Pool().Query(ctx, `SELECT id::text, repository_id::text FROM (
			SELECT id, repository_id, finished_at, row_number() OVER (PARTITION BY repository_id ORDER BY finished_at DESC, id) AS rn
			FROM host_jobs WHERE state IN ('succeeded','failed','cancelled','expired')) x
		WHERE finished_at < $1 OR rn > $2`, now.Add(-s.KeepFor), keep)
	if err != nil {
		return 0, err
	}
	type victim struct{ id, repo string }
	var vs []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.repo); err != nil {
			rows.Close()
			return 0, err
		}
		vs = append(vs, v)
	}
	rows.Close()
	n := 0
	var errs []error
	for _, v := range vs {
		var key string
		if err := s.Svc.DB.Pool().QueryRow(ctx, `SELECT storage_key FROM job_deliveries WHERE job_id=$1`, v.id).Scan(&key); err == nil {
			b, berr := s.backendByRepoID(ctx, v.repo)
			if berr == nil {
				berr = deletePrefix(b, key)
			}
			if berr != nil {
				errs = append(errs, berr)
				continue // the job (and its row) stay until the bytes are gone
			}
		}
		ct, err := s.Svc.DB.Pool().Exec(ctx, `DELETE FROM host_jobs WHERE id=$1 AND state IN ('succeeded','failed','cancelled','expired')`, v.id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n += int(ct.RowsAffected())
	}
	return n, errors.Join(errs...)
}
