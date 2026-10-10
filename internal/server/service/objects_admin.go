package service

import (
	"context"
	"time"
)

// ObjectRow is a storage object as the administrator sees it.
type ObjectRow struct {
	Project         string `json:"project"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Host            string `json:"host"`
	DeclaredPolicy  string `json:"declared_policy"`
	RetentionPolicy string `json:"retention_policy"`
	Snapshots       int    `json:"snapshots"`
	Notes           string `json:"notes"`
	GroupID         string `json:"group_id"`
}

// ObjectRows lists the objects of a repository.
func (s *Service) ObjectRows(ctx context.Context, repoName string) ([]ObjectRow, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT project, name, kind, host_name, policy_name, retention_policy, notes, group_id,
		(SELECT count(*) FROM snapshots sn WHERE sn.object_id=o.id) FROM storage_objects o WHERE repository_id=$1 ORDER BY project, name`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectRow
	for rows.Next() {
		var o ObjectRow
		if err := rows.Scan(&o.Project, &o.Name, &o.Kind, &o.Host, &o.DeclaredPolicy, &o.RetentionPolicy, &o.Notes, &o.GroupID, &o.Snapshots); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RunRow is one housekeeping run.
type RunRow struct {
	Kind      string     `json:"kind"`
	Started   time.Time  `json:"started"`
	Finished  *time.Time `json:"finished"`
	Status    string     `json:"status"`
	Error     string     `json:"error"`
	StatsJSON string     `json:"stats"`
}

// MaintenanceRuns returns the latest housekeeping runs (newest first).
func (s *Service) MaintenanceRuns(ctx context.Context, repoName string, limit int) ([]RunRow, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT kind, started_at, finished_at, status, error, stats::text FROM maintenance_runs
		WHERE repository_id=$1 ORDER BY started_at DESC LIMIT $2`, repo.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		var r RunRow
		if err := rows.Scan(&r.Kind, &r.Started, &r.Finished, &r.Status, &r.Error, &r.StatsJSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
