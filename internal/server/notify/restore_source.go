package notify

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DBRestoreTests reads the latest finished restore test per object from the restore_tests table
// (created by the jobs migration). Without that table it reports nothing, so the engine can be wired
// with it before the table exists.
type DBRestoreTests struct{ Pool *pgxpool.Pool }

// LatestRestoreTests implements RestoreTestSource.
func (d DBRestoreTests) LatestRestoreTests(ctx context.Context) ([]RestoreTestResult, error) {
	var exists bool
	if err := d.Pool.QueryRow(ctx, `SELECT to_regclass('restore_tests') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := d.Pool.Query(ctx, `SELECT DISTINCT ON (t.repository_id, t.object) r.name, t.host_name, t.object, t.state, COALESCE(t.finished, t.started),
		COALESCE(t.details->>'error', '')
		FROM restore_tests t JOIN repositories r ON r.id=t.repository_id
		WHERE t.state IN ('passed','failed') ORDER BY t.repository_id, t.object, t.started DESC, t.seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RestoreTestResult
	for rows.Next() {
		var r RestoreTestResult
		var state, obj string
		if err := rows.Scan(&r.Repository, &r.Host, &obj, &state, &r.At, &r.Error); err != nil {
			return nil, err
		}
		// "default/instance/vm1/root" -> object "default/vm1"; the stored name is the stable key
		project, name, _ := strings.Cut(obj, "/")
		r.Object = project + "/" + ownerName(name)
		r.GroupID = obj
		r.OK = state == "passed"
		out = append(out, r)
	}
	return out, rows.Err()
}
