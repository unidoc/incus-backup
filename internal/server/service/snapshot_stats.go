package service

import (
	"context"
	"time"
)

// SnapshotStats is what PostgreSQL recorded for one snapshot when it was committed (or verified):
// the run's byte counters, how long it took, whether it had to read everything, which snapshot it
// built on, and the latest verification. The datastore stays authoritative for the snapshot itself
// (protection, notes, existence: GET /snapshots); these numbers are the control plane's record of
// the backup run and have no other source (a snapshot created by reconcile or sync has zeros).
type SnapshotStats struct {
	GroupID       string     `json:"group_id"`
	SnapshotID    string     `json:"snapshot_id"`
	LogicalBytes  int64      `json:"logical_bytes"`
	DirtyBytes    int64      `json:"dirty_bytes"`
	ReadBytes     int64      `json:"read_bytes"`
	UploadedBytes int64      `json:"uploaded_bytes"`
	FullRead      bool       `json:"full_read"`
	DurationMs    int64      `json:"duration_ms"`
	PreviousID    string     `json:"previous_snapshot_id"`
	Source        string     `json:"source"` // client | sync | reconcile
	VerifyState   string     `json:"verify_state"`
	VerifiedAt    *time.Time `json:"verified_at"`
}

// SnapshotStatsList returns the recorded statistics of every snapshot of a repository (or of the
// given groups), in one query, ordered by group and snapshot id.
func (s *Service) SnapshotStatsList(ctx context.Context, repoName string, groups []string) ([]SnapshotStats, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	q := `SELECT group_id, snapshot_id, logical_bytes, dirty_bytes, read_bytes, uploaded_bytes, full_read, duration_ms,
		previous_snapshot_id, source, verify_state, verified_at
		FROM snapshots WHERE repository_id=$1`
	args := []any{repo.ID}
	if len(groups) > 0 {
		q += ` AND group_id = ANY($2)`
		args = append(args, groups)
	}
	q += ` ORDER BY group_id, snapshot_id`
	rows, err := s.DB.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SnapshotStats{}
	for rows.Next() {
		var st SnapshotStats
		if err := rows.Scan(&st.GroupID, &st.SnapshotID, &st.LogicalBytes, &st.DirtyBytes, &st.ReadBytes, &st.UploadedBytes, &st.FullRead,
			&st.DurationMs, &st.PreviousID, &st.Source, &st.VerifyState, &st.VerifiedAt); err != nil {
			return nil, err
		}
		if st.VerifiedAt != nil {
			t := st.VerifiedAt.UTC()
			st.VerifiedAt = &t
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
