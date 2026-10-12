package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// RestoreTest is one recorded restore test.
type RestoreTest struct {
	ID         string          `json:"id"`
	Repository string          `json:"repository"`
	Host       string          `json:"host"`
	Object     string          `json:"object"` // "<project>/<logical name>"
	SnapshotID string          `json:"snapshot_id"`
	Method     string          `json:"method"`
	State      string          `json:"state"` // passed | failed | running
	Started    time.Time       `json:"started"`
	Finished   *time.Time      `json:"finished,omitempty"`
	DurationMs int64           `json:"duration_ms"`
	Bytes      int64           `json:"bytes"`
	Files      int64           `json:"files"`
	Details    json.RawMessage `json:"details"`
	JobID      *string         `json:"job_id,omitempty"`
}

const maxDetailsBytes = 64 << 10

func detailsJSON(d map[string]any, errMsg string) []byte {
	if d == nil {
		d = map[string]any{}
	}
	if errMsg != "" {
		d["error"] = errMsg
	}
	b, err := json.Marshal(d)
	if err != nil || len(b) > maxDetailsBytes {
		b, _ = json.Marshal(map[string]any{"truncated": true, "error": errMsg})
	}
	return b
}

func validMethod(m string) bool {
	return m == jobspec.ModeDeep || m == jobspec.ModeExtract || m == jobspec.ModeFull
}

// recordJobRestoreTest turns the end of a restore_test job into its restore_tests row.
func recordJobRestoreTest(ctx context.Context, tx pgx.Tx, jobID, state, msg string, raw json.RawMessage, now time.Time) error {
	if state == jobspec.StateCancelled {
		_, err := tx.Exec(ctx, `DELETE FROM restore_tests WHERE job_id=$1 AND state='running'`, jobID)
		return err
	}
	var r jobspec.RestoreTestResult
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &r)
	}
	st := "failed"
	if state == jobspec.StateSucceeded && r.Passed {
		st = "passed"
	}
	errMsg := ""
	if st == "failed" {
		errMsg = msg
	}
	var started time.Time
	if err := tx.QueryRow(ctx, `SELECT started FROM restore_tests WHERE job_id=$1`, jobID).Scan(&started); err != nil {
		return err
	}
	dur := r.DurationMs
	if dur <= 0 {
		dur = now.Sub(started).Milliseconds()
	}
	_, err := tx.Exec(ctx, `UPDATE restore_tests SET state=$2, finished=$3, duration_ms=$4, bytes=$5, files=$6, details=$7,
		snapshot_id=CASE WHEN $8='' THEN snapshot_id ELSE $8 END WHERE job_id=$1`,
		jobID, st, now, dur, r.Bytes, r.Files, detailsJSON(r.Details, errMsg), clean(r.Snapshot, 64))
	return err
}

// failRunningRestoreTest marks the restore_tests row of a job whose host vanished.
func failRunningRestoreTest(ctx context.Context, tx pgx.Tx, jobID, why string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE restore_tests SET state='failed', finished=$2, duration_ms=GREATEST(0, (EXTRACT(EPOCH FROM ($2 - started))*1000)::bigint), details=$3
		WHERE job_id=$1 AND state='running'`, jobID, now, detailsJSON(nil, why))
	return err
}

// ReportRestoreTest records a restore test a host ran on its own schedule.
func (s *Service) ReportRestoreTest(ctx context.Context, h *service.Host, rep jobspec.RestoreTestReport) (string, error) {
	if err := hostOK(h); err != nil {
		return "", err
	}
	r := rep.Result
	if !validMethod(r.Method) {
		return "", bad("invalid method")
	}
	if _, _, err := jobspec.Validate(jobspec.TypeRestoreTest, mustJSON(jobspec.RestoreTest{Project: r.Project, Object: r.Object, Snapshot: r.Snapshot, Mode: r.Method})); err != nil {
		return "", fmt.Errorf("%w: %v", service.ErrBadRequest, err)
	}
	if rep.Started.IsZero() || rep.Finished.Before(rep.Started) {
		return "", bad("invalid start/finish times")
	}
	skew := s.Svc.MaxClockSkew
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	if rep.Finished.After(s.now().Add(skew)) {
		return "", bad("a restore test cannot finish in the future")
	}
	st := "failed"
	if r.Passed {
		st = "passed"
	}
	if r.Bytes < 0 || r.Files < 0 {
		return "", bad("negative counts")
	}
	// a host reports evidence only for objects it owns (it must not be able to post a "passed" result
	// that shows up as the latest test of another host's object)
	var owner string
	err := s.Svc.DB.Pool().QueryRow(ctx, `SELECT host_name FROM storage_objects WHERE repository_id=$1 AND project=$2 AND name=$3 ORDER BY (host_name=$4) DESC LIMIT 1`,
		h.RepoID, r.Project, r.Object, h.Name).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: unknown object %s/%s", service.ErrNotFound, r.Project, r.Object)
	}
	if err != nil {
		return "", err
	}
	if owner != h.Name {
		return "", fmt.Errorf("%w: %s/%s belongs to host %s", service.ErrForbidden, r.Project, r.Object, owner)
	}
	var id string
	err = s.Svc.DB.Pool().QueryRow(ctx, `INSERT INTO restore_tests (repository_id, host_name, object, snapshot_id, method, state, started, finished, duration_ms, bytes, files, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id::text`,
		h.RepoID, h.Name, r.Project+"/"+r.Object, r.Snapshot, r.Method, st, rep.Started.UTC(), rep.Finished.UTC(),
		rep.Finished.Sub(rep.Started).Milliseconds(), r.Bytes, r.Files, detailsJSON(r.Details, "")).Scan(&id)
	return id, err
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// RestoreTestFilter narrows ListRestoreTests.
type RestoreTestFilter struct {
	Repository string
	Host       string
	Object     string // "<project>/<logical name>"
	State      string
	Limit      int // default 100, max 1000
}

const rtCols = `t.id::text, r.name, t.host_name, t.object, t.snapshot_id, t.method, t.state, t.started, t.finished, t.duration_ms, t.bytes, t.files, t.details, t.job_id::text`

func scanRT(row pgx.Row) (*RestoreTest, error) {
	t := &RestoreTest{}
	var det []byte
	if err := row.Scan(&t.ID, &t.Repository, &t.Host, &t.Object, &t.SnapshotID, &t.Method, &t.State, &t.Started, &t.Finished, &t.DurationMs,
		&t.Bytes, &t.Files, &det, &t.JobID); err != nil {
		return nil, err
	}
	t.Details = det
	return t, nil
}

// ListRestoreTests lists restore tests, newest first.
func (s *Service) ListRestoreTests(ctx context.Context, f RestoreTestFilter) ([]RestoreTest, error) {
	q := `SELECT ` + rtCols + ` FROM restore_tests t JOIN repositories r ON r.id=t.repository_id WHERE true`
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.Repository != "" {
		add("r.name=$%d", f.Repository)
	}
	if f.Host != "" {
		add("t.host_name=$%d", f.Host)
	}
	if f.Object != "" {
		add("t.object=$%d", f.Object)
	}
	if f.State != "" {
		add("t.state=$%d", f.State)
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	q += fmt.Sprintf(" ORDER BY t.started DESC, t.seq DESC LIMIT %d", f.Limit)
	return s.queryRT(ctx, q, args...)
}

func (s *Service) queryRT(ctx context.Context, q string, args ...any) ([]RestoreTest, error) {
	rows, err := s.Svc.DB.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RestoreTest{}
	for rows.Next() {
		t, err := scanRT(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// LatestRestoreTestPerObject returns, per (host, object) of a repository, its most recent FINISHED
// restore test (a running one does not hide the last real result). Keyed by host too, so one host's
// reports can never stand in for another's.
func (s *Service) LatestRestoreTestPerObject(ctx context.Context, repo string) ([]RestoreTest, error) {
	if _, err := s.Svc.Repository(ctx, repo); err != nil {
		return nil, err
	}
	return s.queryRT(ctx, `SELECT DISTINCT ON (t.object, t.host_name) `+rtCols+` FROM restore_tests t JOIN repositories r ON r.id=t.repository_id
		WHERE r.name=$1 AND t.state<>'running' ORDER BY t.object, t.host_name, t.started DESC, t.seq DESC`, repo)
}
