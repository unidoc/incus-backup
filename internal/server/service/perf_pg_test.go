package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unidoc/incus-backup/internal/server/service"

	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/testdb"
)

// Control-plane query performance: the hot queries of the server, run against a realistic seeded
// volume, without and with the perf indexes (section 13 of migrations/20261008000000_initial_schema.sql).
//
// TestPerfIndexesUsed (part of `go test`) seeds a medium volume and asserts that the planner uses the
// new indexes for the queries they exist for. BenchmarkControlPlaneQueries (run by `just bench`)
// seeds the full volume (100k snapshots, 10k objects), prints EXPLAIN ANALYZE timings for every
// query without and with the indexes, and writes the plans to $IB_BENCH_OUT/pg-plans.txt if set.

// perfIndexes are the indexes added for these queries (section 13 of migrations/20261008000000_initial_schema.sql).
var perfIndexes = []struct{ name, ddl string }{
	{"snapshots_object", `CREATE INDEX snapshots_object ON snapshots (object_id)`},
	{"storage_objects_host", `CREATE INDEX storage_objects_host ON storage_objects (host_id)`},
	{"backup_runs_repo_time", `CREATE INDEX backup_runs_repo_time ON backup_runs (repository_id, started_at)`},
	{"backup_runs_host_time", `CREATE INDEX backup_runs_host_time ON backup_runs (host_id, started_at DESC)`},
	{"maintenance_runs_time", `CREATE INDEX maintenance_runs_time ON maintenance_runs (repository_id, started_at DESC)`},
	{"sessions_host", `CREATE INDEX sessions_host ON sessions (host_id)`},
}

// seedScale is the volume of the main repository; two smaller repositories are seeded next to it
// so that every repository_id predicate is selective, as on a server that holds several.
type seedScale struct {
	objects, snapsPerObject, hosts int
}

// pgQuery is one control-plane query, written exactly as the service issues it.
type pgQuery struct {
	name, sql string
	args      func(f *fixture) []any
	// wantIndex: with the perf indexes the plan must use this index (asserted in the test).
	wantIndex string
}

type fixture struct {
	repo, host, group, fp string
	now                   time.Time
}

var hotQueries = []pgQuery{
	{name: "snapshotList (GET /v1/groups/{g}/snapshots)",
		sql: `SELECT sn.snapshot_id, sn.created_at, sn.committed_at, sn.index_digest, sn.chunk_count, sn.policy_name,
		COALESCE(o.project,''), COALESCE(o.name,''), COALESCE(o.kind,''), COALESCE(o.host_name,''), sn.logical_bytes, sn.verify_state, sn.notes
		FROM snapshots sn LEFT JOIN storage_objects o ON o.id=sn.object_id WHERE sn.repository_id=$1 AND sn.group_id=$2`,
		args: func(f *fixture) []any { return []any{f.repo, f.group} }},
	{name: "snapshot rows of a whole repository (ListAllSnapshots, after)",
		sql: `SELECT sn.group_id, sn.snapshot_id, sn.created_at, sn.committed_at, sn.index_digest, sn.chunk_count, sn.policy_name,
		COALESCE(o.project,''), COALESCE(o.name,''), COALESCE(o.kind,''), COALESCE(o.host_name,''), sn.logical_bytes, sn.verify_state, sn.notes
		FROM snapshots sn LEFT JOIN storage_objects o ON o.id=sn.object_id WHERE sn.repository_id=$1`,
		args: func(f *fixture) []any { return []any{f.repo} }},
	{name: "Objects (GET /v1/objects) / admin objects",
		sql: `SELECT o.group_id, o.project, o.name, o.kind, o.host_name, o.policy_name, o.shared_by,
		(SELECT count(*) FROM snapshots sn WHERE sn.object_id=o.id)
		FROM storage_objects o WHERE o.repository_id=$1 ORDER BY o.project, o.name`,
		args: func(f *fixture) []any { return []any{f.repo} }, wantIndex: "snapshots_object"},
	{name: "Status: snapshot count",
		sql:  `SELECT count(*) FROM snapshots WHERE repository_id=$1`,
		args: func(f *fixture) []any { return []any{f.repo} }},
	{name: "Status: ok runs 24h",
		sql:  `SELECT count(*) FROM backup_runs WHERE repository_id=$1 AND status='ok' AND started_at > $2`,
		args: func(f *fixture) []any { return []any{f.repo, f.now.Add(-24 * time.Hour)} }, wantIndex: "backup_runs_repo_time"},
	{name: "Status: failed runs 24h",
		sql:  `SELECT count(*) FROM backup_runs WHERE repository_id=$1 AND status IN ('failed','partial') AND started_at > $2`,
		args: func(f *fixture) []any { return []any{f.repo, f.now.Add(-24 * time.Hour)} }, wantIndex: "backup_runs_repo_time"},
	{name: "Status: last verify",
		sql:  `SELECT max(finished_at) FROM maintenance_runs WHERE repository_id=$1 AND kind='verify' AND status='ok'`,
		args: func(f *fixture) []any { return []any{f.repo} }},
	{name: "Status: objects of one host (per host, before)",
		sql:  `SELECT o.project, o.name, EXISTS (SELECT 1 FROM snapshots sn WHERE sn.object_id=o.id) FROM storage_objects o WHERE o.host_id=$1`,
		args: func(f *fixture) []any { return []any{f.host} }, wantIndex: "storage_objects_host"},
	{name: "Status: last run of one host (per host, before)",
		sql:  `SELECT finished_at, status FROM backup_runs WHERE host_id=$1 AND finished_at IS NOT NULL ORDER BY started_at DESC LIMIT 1`,
		args: func(f *fixture) []any { return []any{f.host} }, wantIndex: "backup_runs_host_time"},
	{name: "Status: objects of all hosts (single-query alternative, not adopted)",
		sql: `SELECT o.host_id::text, o.project, o.name, EXISTS (SELECT 1 FROM snapshots sn WHERE sn.object_id=o.id)
		FROM storage_objects o JOIN hosts h ON h.id=o.host_id WHERE h.repository_id=$1 AND h.scope='backup'`,
		args: func(f *fixture) []any { return []any{f.repo} }, wantIndex: "snapshots_object"},
	{name: "Status: last run of all hosts (single-query alternative, not adopted)",
		sql: `SELECT h.id::text, r.finished_at, r.status FROM hosts h CROSS JOIN LATERAL (SELECT finished_at, status FROM backup_runs b
		WHERE b.host_id=h.id AND b.finished_at IS NOT NULL ORDER BY b.started_at DESC LIMIT 1) r WHERE h.repository_id=$1 AND h.scope='backup'`,
		args: func(f *fixture) []any { return []any{f.repo} }, wantIndex: "backup_runs_host_time"},
	{name: "maintenance runs list (admin)",
		sql:  `SELECT kind, started_at, finished_at, status, error, stats::text FROM maintenance_runs WHERE repository_id=$1 ORDER BY started_at DESC LIMIT $2`,
		args: func(f *fixture) []any { return []any{f.repo, 50} }, wantIndex: "maintenance_runs_time"},
	{name: "HostFromCert",
		sql:  `SELECT id, repository_id, name, scope, disabled FROM hosts WHERE cert_fingerprint=$1`,
		args: func(f *fixture) []any { return []any{f.fp} }},
	{name: "reconcile: snapshot exists (per snapshot, before)",
		sql:  `SELECT EXISTS (SELECT 1 FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3)`,
		args: func(f *fixture) []any { return []any{f.repo, f.group, "20260101000000-00000000"} }},
	{name: "reconcile: bulk read of all rows (one query, after)",
		sql:  `SELECT group_id, snapshot_id, protected, notes, verify_state, verified_at FROM snapshots WHERE repository_id=$1`,
		args: func(f *fixture) []any { return []any{f.repo} }},
	{name: "pinnedBases (prune)",
		sql:  `SELECT group_id, base_snapshot_id FROM sessions WHERE repository_id=$1 AND state='open' AND base_snapshot_id <> '' AND expires_at >= $2`,
		args: func(f *fixture) []any { return []any{f.repo, f.now} }},
	{name: "groupBusy",
		sql:  `SELECT count(*) FROM sessions WHERE repository_id=$1 AND group_id=$2 AND state='open' AND expires_at >= $3`,
		args: func(f *fixture) []any { return []any{f.repo, f.group, f.now} }},
	{name: "corrupt chunks list",
		sql:  `SELECT chunk_id, detected_at, state, healed_at, detail, affected FROM corrupt_chunks WHERE repository_id=$1 AND ($2 OR state='quarantined') ORDER BY detected_at DESC`,
		args: func(f *fixture) []any { return []any{f.repo, false} }},
	{name: "latest capacity sample",
		sql:  `SELECT physical_bytes, logical_bytes, chunk_count, snapshot_count, fs_total_bytes, fs_free_bytes FROM capacity_samples WHERE repository_id=$1 ORDER BY taken_at DESC LIMIT 1`,
		args: func(f *fixture) []any { return []any{f.repo} }},
	{name: "reverify days by group",
		sql:  `SELECT o.group_id, p.reverify_days FROM storage_objects o JOIN policies p ON p.repository_id=o.repository_id AND p.name=o.retention_policy WHERE o.repository_id=$1`,
		args: func(f *fixture) []any { return []any{f.repo} }},
	{name: "verification history count (reconcile)",
		sql:  `SELECT count(*) FROM snapshot_verifications WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`,
		args: func(f *fixture) []any { return []any{f.repo, f.group, "20260101000000-00000000"} }},
	{name: "delete a host (FK ON DELETE SET NULL on storage_objects, sessions, backup_runs)",
		sql:  `DELETE FROM hosts WHERE id=$1`,
		args: func(f *fixture) []any { return []any{f.host} }},
}

// seedSQL fills one repository. All names are deterministic, so before/after runs see the same data.
func seedSQL(repoName string, sc seedScale) string {
	r := strings.NewReplacer("{R}", repoName, "{O}", strconv.Itoa(sc.objects), "{S}", strconv.Itoa(sc.snapsPerObject), "{H}", strconv.Itoa(sc.hosts))
	return r.Replace(`
INSERT INTO repositories (name, datastore_path) VALUES ('{R}', '/srv/{R}') ON CONFLICT (name) DO NOTHING;
INSERT INTO policies (repository_id, name) SELECT id, 'daily' FROM repositories WHERE name='{R}';
INSERT INTO hosts (repository_id, name, scope, cert_fingerprint, last_seen_at)
  SELECT r.id, 'host' || i, 'backup', md5('{R}-fp-' || i), now() FROM repositories r, generate_series(1, {H}) i WHERE r.name='{R}';
INSERT INTO hosts (repository_id, name, scope, cert_fingerprint)
  SELECT r.id, 'peer' || i, 'sync-read', md5('{R}-peer-' || i) FROM repositories r, generate_series(1, 3) i WHERE r.name='{R}';
-- objects spread over the hosts; every 20th has no snapshot (shows as unprotected)
INSERT INTO storage_objects (repository_id, host_id, host_name, project, name, kind, group_id, policy_name, retention_policy)
  SELECT r.id, h.id, h.name, 'p' || (i % 20), 'vm' || i || '/root', 'block', md5('{R}-g-' || i), 'daily', 'daily'
  FROM repositories r, generate_series(1, {O}) i
  JOIN LATERAL (SELECT id, name FROM hosts WHERE repository_id=(SELECT id FROM repositories WHERE name='{R}') AND name = 'host' || (1 + i % {H})) h ON true
  WHERE r.name='{R}';
INSERT INTO snapshots (repository_id, object_id, group_id, snapshot_id, created_at, committed_at, policy_name, logical_bytes, index_digest, chunk_count, verify_state)
  SELECT o.repository_id, o.id, o.group_id,
         to_char(timestamptz '2026-01-01 00:00:00+00' + (s || ' hours')::interval, 'YYYYMMDDHH24MISS') || '-' || substr(md5(o.group_id || s), 1, 8),
         timestamptz '2026-01-01 00:00:00+00' + (s || ' hours')::interval, timestamptz '2026-01-01 00:00:00+00' + (s || ' hours')::interval,
         'daily', 10737418240, md5(o.group_id || s), 2560, CASE WHEN s % 3 = 0 THEN 'ok' ELSE '' END
  FROM storage_objects o, generate_series(1, {S}) s
  WHERE o.repository_id=(SELECT id FROM repositories WHERE name='{R}') AND (split_part(substr(o.name, 3), '/', 1))::int % 20 <> 0;
-- sessions: one per snapshot (history) plus a handful open
INSERT INTO sessions (repository_id, host_id, group_id, snapshot_id, state, created_at, expires_at, base_snapshot_id)
  SELECT sn.repository_id, o.host_id, sn.group_id, sn.snapshot_id, 'committed', sn.created_at, sn.created_at + interval '1 hour', ''
  FROM snapshots sn JOIN storage_objects o ON o.id=sn.object_id WHERE sn.repository_id=(SELECT id FROM repositories WHERE name='{R}');
INSERT INTO sessions (repository_id, host_id, group_id, snapshot_id, state, expires_at, base_snapshot_id)
  SELECT o.repository_id, o.host_id, o.group_id, '20261008000000-' || substr(md5(o.group_id), 1, 8), 'open', now() + interval '1 hour', 'base'
  FROM storage_objects o WHERE o.repository_id=(SELECT id FROM repositories WHERE name='{R}') ORDER BY o.group_id LIMIT 10;
-- backup runs: two per host per day over a year
INSERT INTO backup_runs (repository_id, host_id, started_at, finished_at, status)
  SELECT h.repository_id, h.id, timestamptz '2025-10-08 00:00:00+00' + (d || ' hours')::interval * 12,
         timestamptz '2025-10-08 00:30:00+00' + (d || ' hours')::interval * 12, CASE WHEN d % 17 = 0 THEN 'failed' ELSE 'ok' END
  FROM hosts h, generate_series(0, 729) d WHERE h.repository_id=(SELECT id FROM repositories WHERE name='{R}') AND h.scope='backup';
INSERT INTO maintenance_runs (repository_id, kind, started_at, finished_at, status)
  SELECT r.id, (ARRAY['gc','verify','scrub','retention','reconcile'])[1 + i % 5], timestamptz '2025-10-08 00:00:00+00' + (i || ' hours')::interval,
         timestamptz '2025-10-08 00:10:00+00' + (i || ' hours')::interval, 'ok'
  FROM repositories r, generate_series(1, 8760) i WHERE r.name='{R}';
INSERT INTO snapshot_verifications (repository_id, group_id, snapshot_id, verified_at, state)
  SELECT repository_id, group_id, snapshot_id, committed_at + interval '1 day', 'ok' FROM snapshots
  WHERE repository_id=(SELECT id FROM repositories WHERE name='{R}');
INSERT INTO capacity_samples (repository_id, taken_at, physical_bytes, chunk_count, snapshot_count)
  SELECT r.id, timestamptz '2025-10-08 00:00:00+00' + (i || ' hours')::interval, i, i, i FROM repositories r, generate_series(1, 8760) i WHERE r.name='{R}';
INSERT INTO corrupt_chunks (repository_id, chunk_id, state)
  SELECT r.id, md5('{R}-c-' || i) || md5('x' || i), CASE WHEN i % 4 = 0 THEN 'quarantined' ELSE 'healed' END FROM repositories r, generate_series(1, 2000) i WHERE r.name='{R}';
`)
}

// newPerfDB returns a pool on a fresh, fully migrated database from which the perf indexes were dropped
// again, so that the queries can be measured without and then with them. Before dropping them it
// asserts that the plain migration path creates every one of them.
func newPerfDB(t testing.TB) (*db.DB, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ix := range perfIndexes {
		var def string
		if err := d.Pool().QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, ix.name).Scan(&def); err != nil {
			t.Fatalf("the migrations do not create index %s: %v", ix.name, err)
		}
		if _, err := d.Pool().Exec(ctx, `DROP INDEX `+ix.name); err != nil {
			t.Fatal(err)
		}
	}
	return d, d.Pool()
}

func seed(t testing.TB, p *pgxpool.Pool, main seedScale) *fixture {
	t.Helper()
	ctx := context.Background()
	for _, r := range []struct {
		name string
		sc   seedScale
	}{{"custody", main}, {"other1", seedScale{main.objects / 5, main.snapsPerObject, 10}}, {"other2", seedScale{main.objects / 5, main.snapsPerObject, 10}}} {
		if _, err := p.Exec(ctx, seedSQL(r.name, r.sc)); err != nil {
			t.Fatalf("seeding %s: %v", r.name, err)
		}
	}
	if _, err := p.Exec(ctx, `VACUUM ANALYZE`); err != nil {
		t.Fatal(err)
	}
	f := &fixture{now: time.Date(2025, 10, 8, 0, 0, 0, 0, time.UTC).Add(365 * 24 * time.Hour)}
	if err := p.QueryRow(ctx, `SELECT id::text FROM repositories WHERE name='custody'`).Scan(&f.repo); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT id::text, cert_fingerprint FROM hosts WHERE repository_id=$1 AND name='host7'`, f.repo).Scan(&f.host, &f.fp); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT group_id FROM storage_objects WHERE repository_id=$1 AND name='vm123/root'`, f.repo).Scan(&f.group); err != nil {
		t.Fatal(err)
	}
	return f
}

var execTimeRe = regexp.MustCompile(`Execution Time: ([0-9.]+) ms`)

// explain runs EXPLAIN (ANALYZE, BUFFERS) rounds times and returns the fastest execution time and its plan.
// Data-modifying statements run inside a rolled-back transaction.
func explain(t testing.TB, p *pgxpool.Pool, q pgQuery, f *fixture, rounds int) (float64, string) {
	t.Helper()
	ctx := context.Background()
	best := -1.0
	var bestPlan string
	for i := 0; i < rounds; i++ {
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q.sql, q.args(f)...)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: %v", q.name, err)
		}
		var sb strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			sb.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: %v", q.name, err)
		}
		_ = tx.Rollback(ctx)
		m := execTimeRe.FindStringSubmatch(sb.String())
		if m == nil {
			t.Fatalf("%s: no execution time in plan:\n%s", q.name, sb.String())
		}
		ms, _ := strconv.ParseFloat(m[1], 64)
		if best < 0 || ms < best {
			best, bestPlan = ms, sb.String()
		}
	}
	return best, bestPlan
}

// topNode is the first plan line (the root node), shortened for a table.
func topNode(plan string) string {
	var nodes []string
	for _, l := range strings.Split(plan, "\n") {
		s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "->"))
		s = strings.TrimPrefix(s, "Parallel ")
		for _, k := range []string{"Seq Scan on ", "Index Scan using ", "Index Only Scan using ", "Bitmap Index Scan on "} {
			if strings.HasPrefix(s, k) {
				w := strings.Fields(strings.TrimPrefix(s, k))
				name := k + w[0]
				dup := false
				for _, n := range nodes {
					dup = dup || n == name
				}
				if !dup {
					nodes = append(nodes, name)
				}
			}
		}
	}
	return strings.Join(nodes, ", ")
}

func applyPerfMigration(t testing.TB, d *db.DB) {
	t.Helper()
	ctx := context.Background()
	for _, ix := range perfIndexes {
		if _, err := d.Pool().Exec(ctx, ix.ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool().Exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
}

// TestPerfIndexesUsed: on a seeded database the perf indexes exist and the planner uses them for the
// queries they were added for (newPerfDB checks that the plain migration path creates them).
func TestPerfIndexesUsed(t *testing.T) {
	d, p := newPerfDB(t)
	f := seed(t, p, seedScale{objects: 1000, snapsPerObject: 10, hosts: 20})
	applyPerfMigration(t, d)
	ctx := context.Background()
	// planner statistics and the visibility map must be current whatever the server's commit settings are
	if _, err := p.Exec(ctx, `VACUUM ANALYZE`); err != nil {
		t.Fatal(err)
	}
	for _, ix := range perfIndexes {
		var n int
		if err := p.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, ix.name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s missing (%d, %v)", ix.name, n, err)
		}
	}
	for _, q := range hotQueries {
		if q.wantIndex == "" {
			continue
		}
		_, plan := explain(t, p, q, f, 1)
		if !strings.Contains(plan, q.wantIndex) {
			t.Errorf("%s: plan does not use %s:\n%s", q.name, q.wantIndex, plan)
		}
	}
	// negative control: without the index the same query cannot use it (the assertion above is not vacuous)
	if _, err := p.Exec(ctx, `DROP INDEX snapshots_object`); err != nil {
		t.Fatal(err)
	}
	_, plan := explain(t, p, hotQueries[2], f, 1)
	if strings.Contains(plan, "snapshots_object") || !strings.Contains(plan, "Seq Scan on snapshots") {
		t.Fatalf("negative control: expected a sequential scan of snapshots without the index:\n%s", plan)
	}
}

// BenchmarkControlPlaneQueries prints a before/after table of the hot queries on the full volume.
func BenchmarkControlPlaneQueries(b *testing.B) {
	d, p := newPerfDB(b)
	start := time.Now()
	f := seed(b, p, seedScale{objects: 10000, snapsPerObject: 11, hosts: 50})
	var counts [2]int
	_ = p.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM snapshots), (SELECT count(*) FROM snapshots WHERE repository_id=$1)`, f.repo).Scan(&counts[0], &counts[1])
	b.Logf("seeded in %s: %d snapshot rows (%d in the measured repository)", time.Since(start).Round(time.Second), counts[0], counts[1])
	type res struct {
		ms   float64
		node string
		plan string
	}
	before := map[string]res{}
	// slow plans (seconds per run) get one round, the rest five
	rounds := func(name string) int {
		if strings.HasPrefix(name, "Objects") || strings.HasPrefix(name, "Status: objects of all") {
			return 1
		}
		return 5
	}
	for _, q := range hotQueries {
		ms, plan := explain(b, p, q, f, rounds(q.name))
		before[q.name] = res{ms, topNode(plan), plan}
	}
	applyPerfMigration(b, d)
	var out strings.Builder
	out.WriteString("| query | before: plan | before ms | after: plan | after ms |\n|---|---|---:|---|---:|\n")
	var plans strings.Builder
	for _, q := range hotQueries {
		ms, plan := explain(b, p, q, f, 5)
		bf := before[q.name]
		fmt.Fprintf(&out, "| %s | %s | %.2f | %s | %.2f |\n", q.name, bf.node, bf.ms, topNode(plan), ms)
		fmt.Fprintf(&plans, "==== %s\n-- before\n%s-- after\n%s\n", q.name, bf.plan, plan)
	}
	b.Log("\n" + out.String())
	if dir := os.Getenv("IB_BENCH_OUT"); dir != "" {
		_ = os.WriteFile(dir+"/pg-plans.txt", []byte(plans.String()), 0o644)
		_ = os.WriteFile(dir+"/pg-table.md", []byte(out.String()), 0o644)
	}
	for i := 0; i < b.N; i++ {
		// the measurement is the table above; b.N iterations re-run the cheapest hot query
		if _, err := p.Exec(context.Background(), hotQueries[0].sql, hotQueries[0].args(f)...); err != nil {
			b.Fatal(err)
		}
	}
}

// xacts is the number of transactions committed in the current database so far: every autocommitted
// statement the service issues is one, so a delta counts round trips that wrote or read PostgreSQL.
func xacts(t testing.TB, p *pgxpool.Pool) int64 {
	t.Helper()
	// pg_stat_database is updated when a backend reports its statistics: force it first
	_, _ = p.Exec(context.Background(), `SELECT pg_stat_force_next_flush()`)
	var n int64
	if err := p.QueryRow(context.Background(), `SELECT xact_commit FROM pg_stat_database WHERE datname=current_database()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// reconcileFixture builds a server whose filesystem repository "custody" holds a commit marker for every
// seeded snapshot row (steady state: PostgreSQL already agrees with the datastore, as on every daily run).
func reconcileFixture(t testing.TB, sc seedScale) (*service.Service, *pgxpool.Pool, *service.Repo) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc := service.New(d, filepath.Join(dir, "state"), "test", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err := svc.InitPKI(nil); err != nil {
		t.Fatal(err)
	}
	ds := filepath.Join(dir, "ds")
	repo, err := svc.CreateRepository(ctx, "custody", ds)
	if err != nil {
		t.Fatal(err)
	}
	p := d.Pool()
	if _, err := p.Exec(ctx, seedSQL("custody", sc)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `VACUUM ANALYZE`); err != nil {
		t.Fatal(err)
	}
	// verified snapshots carry verify.json in the datastore and the same state in PostgreSQL
	if _, err := p.Exec(ctx, `UPDATE snapshots SET verified_at=committed_at + interval '1 day' WHERE repository_id=$1 AND verify_state='ok'`, repo.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := p.Query(ctx, `SELECT group_id, snapshot_id, committed_at, chunk_count, verify_state, verified_at FROM snapshots WHERE repository_id=$1`, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var g, sn string
		var at time.Time
		var cc int
		var vs string
		var vat *time.Time
		if err := rows.Scan(&g, &sn, &at, &cc, &vs, &vat); err != nil {
			t.Fatal(err)
		}
		// test fixture: plain files (the layout of the filesystem backend) without fsync, for speed
		dd := filepath.Join(ds, "groups", g, sn)
		if err := os.MkdirAll(dd, 0o750); err != nil {
			t.Fatal(err)
		}
		rc, _ := json.Marshal(map[string]any{"committed": at.UTC(), "chunk_count": cc, "source": "client"})
		if err := os.WriteFile(filepath.Join(dd, "commit.json"), rc, 0o640); err != nil {
			t.Fatal(err)
		}
		if vat != nil {
			vr, _ := json.Marshal(map[string]any{"state": vs, "at": vat.UTC(), "history": []map[string]any{{"at": vat.UTC(), "state": vs}}})
			if err := os.WriteFile(filepath.Join(dd, "verify.json"), vr, 0o640); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return svc, p, repo
}

// BenchmarkReconcileAndSyncList runs the real steady-state Reconcile and the real ListAllSnapshots
// (the sync source listing) and reports wall time and PostgreSQL transactions per call.
func BenchmarkReconcileAndSyncList(b *testing.B) {
	svc, p, repo := reconcileFixture(b, seedScale{objects: 2000, snapsPerObject: 11, hosts: 20})
	ctx := context.Background()
	var n, groups int
	_ = p.QueryRow(ctx, `SELECT count(*), count(DISTINCT group_id) FROM snapshots WHERE repository_id=$1`, repo.ID).Scan(&n, &groups)
	for i := 0; i < b.N; i++ {
		x0 := xacts(b, p)
		t0 := time.Now()
		res, err := svc.Reconcile(ctx, "custody")
		if err != nil || res.Inserted != 0 || res.Removed != 0 {
			b.Fatalf("steady-state reconcile changed something: %+v %v", res, err)
		}
		d1 := time.Since(t0)
		x1 := xacts(b, p)
		t1 := time.Now()
		l, err := svc.ListAllSnapshots(ctx, repo, nil)
		if err != nil || len(l) != n {
			b.Fatalf("ListAllSnapshots: %d of %d, %v", len(l), n, err)
		}
		d2 := time.Since(t1)
		x2 := xacts(b, p)
		b.Logf("%d snapshots / %d groups: Reconcile %s, %d PG transactions; ListAllSnapshots %s, %d PG transactions",
			n, groups, d1.Round(time.Millisecond), x1-x0-1, d2.Round(time.Millisecond), x2-x1-1)
	}
}

// TestReconcileWritesOnlyDifferences: the steady-state shortcut of reconcile (no write when PostgreSQL
// already agrees) still copies every difference the datastore holds, and the unchanged rows cost no
// per-snapshot round trip.
func TestReconcileWritesOnlyDifferences(t *testing.T) {
	svc, p, repo := reconcileFixture(t, seedScale{objects: 40, snapsPerObject: 3, hosts: 2})
	ctx := context.Background()
	store, err := svc.Store(repo)
	if err != nil {
		t.Fatal(err)
	}
	pick := func(i int) (g, sn string) {
		t.Helper()
		if err := p.QueryRow(ctx, `SELECT group_id, snapshot_id FROM snapshots WHERE repository_id=$1 ORDER BY group_id, snapshot_id OFFSET $2 LIMIT 1`, repo.ID, i).Scan(&g, &sn); err != nil {
			t.Fatal(err)
		}
		return
	}
	// the first run on this fixture records nothing new
	x0 := xacts(t, p)
	if res, err := svc.Reconcile(ctx, "custody"); err != nil || res.Inserted != 0 || res.Removed != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := xacts(t, p) - x0; n > 20 {
		t.Fatalf("a steady-state reconcile of %d snapshots issued %d transactions", 114, n)
	}
	// differences made in the datastore only (as after restoring PostgreSQL from an old dump)
	g1, s1 := pick(0)
	g2, s2 := pick(5)
	g3, s3 := pick(10)
	if err := store.SetProtected(g1, s1, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNotes(g2, s2, "hello"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 1, 2, 3, 456789123, time.UTC)
	if err := store.RecordVerify(g3, s3, datastore.VerifyEvent{At: at, State: datastore.VerifyFailed}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupNotes(g3, "group note"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reconcile(ctx, "custody"); err != nil {
		t.Fatal(err)
	}
	var prot bool
	var notes, vs, gnotes string
	var vat time.Time
	_ = p.QueryRow(ctx, `SELECT protected FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, g1, s1).Scan(&prot)
	_ = p.QueryRow(ctx, `SELECT notes FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, g2, s2).Scan(&notes)
	_ = p.QueryRow(ctx, `SELECT verify_state, verified_at FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, g3, s3).Scan(&vs, &vat)
	_ = p.QueryRow(ctx, `SELECT notes FROM storage_objects WHERE repository_id=$1 AND group_id=$2`, repo.ID, g3).Scan(&gnotes)
	if !prot || notes != "hello" || vs != "failed" || !vat.Equal(at.Truncate(time.Microsecond)) || gnotes != "group note" {
		t.Fatalf("reconcile missed a difference: protected=%v notes=%q verify=%q@%s group notes=%q", prot, notes, vs, vat, gnotes)
	}
	// and the reverse: the datastore drops the protection, PostgreSQL follows
	if err := store.SetProtected(g1, s1, false); err != nil {
		t.Fatal(err)
	}
	// a third run with nothing changed writes nothing again (verified_at compared at microsecond precision)
	if _, err := svc.Reconcile(ctx, "custody"); err != nil {
		t.Fatal(err)
	}
	_ = p.QueryRow(ctx, `SELECT protected FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, g1, s1).Scan(&prot)
	if prot {
		t.Fatal("protection removed in the datastore is still recorded in PostgreSQL")
	}
	x0 = xacts(t, p)
	if _, err := svc.Reconcile(ctx, "custody"); err != nil {
		t.Fatal(err)
	}
	if n := xacts(t, p) - x0; n > 20 {
		t.Fatalf("a reconcile after convergence issued %d transactions", n)
	}
	// ListAllSnapshots: one query for all groups, same content as the per-group listing
	all, err := svc.ListAllSnapshots(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = p.QueryRow(ctx, `SELECT count(*) FROM snapshots WHERE repository_id=$1`, repo.ID).Scan(&n)
	if len(all) != n {
		t.Fatalf("ListAllSnapshots returned %d of %d", len(all), n)
	}
	for _, r := range all {
		if r.Project == "" || r.Policy != "daily" || r.IndexDigest == "" {
			t.Fatalf("snapshot %s/%s lacks its PostgreSQL metadata: %+v", r.GroupID, r.SnapshotID, r)
		}
		if r.GroupID == g2 && r.SnapshotID == s2 && r.Notes != "hello" {
			t.Fatalf("notes not listed: %+v", r)
		}
		if r.GroupID == g3 && r.SnapshotID == s3 && r.VerifyState != "failed" {
			t.Fatalf("verify state not listed: %+v", r)
		}
	}
	// a snapshot without a row (committed after the bulk read) falls back to the per-group read
	if _, err := p.Exec(ctx, `DELETE FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, g2, s2); err != nil {
		t.Fatal(err)
	}
	all, err = svc.ListAllSnapshots(ctx, repo, nil)
	if err != nil || len(all) != n {
		t.Fatalf("%d %v", len(all), err)
	}
}
