package db_test

import (
	"io/fs"

	"context"
	"github.com/unidoc/incus-backup/migrations"
	"testing"
	"testing/fstest"

	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func open(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func TestMigrateAppliesOnceAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	files, _ := fs.Glob(migrations.FS, "*.sql")
	var n int
	if err := d.Pool().QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(files) {
		t.Fatalf("schema_migrations rows=%d want %d err=%v", n, len(files), err)
	}
	for _, table := range []string{"repositories", "hosts", "policies", "snapshots", "sync_jobs", "sync_snapshot_state", "capacity_samples"} {
		var ok bool
		if err := d.Pool().QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s missing (%v)", table, err)
		}
	}
}

func TestConstraintsRejectBadData(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p := d.Pool()
	var repo string
	if err := p.QueryRow(ctx, `INSERT INTO repositories (name, datastore_path) VALUES ('r','/x') RETURNING id`).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		`INSERT INTO hosts (repository_id,name,scope,cert_fingerprint) VALUES ('` + repo + `','h','admin','fp')`,
		`INSERT INTO policies (repository_id,name,commit_required) VALUES ('` + repo + `','p','maybe')`,
		`INSERT INTO policies (repository_id,name,keep_daily) VALUES ('` + repo + `','p2',-1)`,
		`INSERT INTO sync_jobs (repository_id,name,direction,remote,remote_repository,mirror) VALUES ('` + repo + `','j','push','x','y',true)`,
		`INSERT INTO sync_jobs (repository_id,name,direction,remote,remote_repository) VALUES ('` + repo + `','j','sideways','x','y')`,
	}
	for _, q := range bad {
		if _, err := p.Exec(ctx, q); err == nil {
			t.Fatalf("accepted invalid row: %s", q)
		}
	}
}

// A failing migration must leave no partial schema and no schema_migrations row.
func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	fsys := fstest.MapFS{
		"1_ok.sql":   {Data: []byte(`CREATE TABLE a (id int);`)},
		"2_fail.sql": {Data: []byte(`CREATE TABLE b (id int); SELECT broken syntax here;`)},
	}
	if err := d.MigrateFS(ctx, fsys, "."); err == nil {
		t.Fatal("expected failure")
	}
	var b, applied int
	_ = d.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename='b'`).Scan(&b)
	_ = d.Pool().QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied)
	if b != 0 || applied != 1 {
		t.Fatalf("partial state: table b=%d, migrations applied=%d (want 0 and 1)", b, applied)
	}
}

func TestOlderBinaryRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ('29991231000000_from_the_future.sql')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err == nil {
		t.Fatal("an older binary accepted a newer schema")
	}
}
