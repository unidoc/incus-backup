// Package db is the PostgreSQL control-plane layer of incus-backup-server.
// PostgreSQL (pgx/v5) is the only supported control-plane implementation.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unidoc/incus-backup/migrations"
)

// DB wraps a pgx pool.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects and pings.
func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parsing database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close closes the pool.
func (d *DB) Close() { d.pool.Close() }

// Pool returns the pool for direct queries.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Migrate applies every pending embedded migration.
func (d *DB) Migrate(ctx context.Context) error {
	// two processes migrating at once (a CLI command next to the daemon) must not race: serialise on an advisory lock
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(0x69626b5f6d696772)); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(0x69626b5f6d696772))
	if err := d.MigrateFS(ctx, migrations.FS, "."); err != nil {
		return err
	}
	return d.checkNotNewer(ctx)
}

// checkNotNewer refuses to run an older binary against a database that a newer release migrated: the old
// code would silently ignore columns and tables it does not know and could corrupt them.
func (d *DB) checkNotNewer(ctx context.Context) error {
	known := map[string]bool{}
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	for _, f := range files {
		known[strings.TrimSuffix(path.Base(f), ".sql")] = true
	}
	rows, err := d.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		if !known[v] && !known[strings.TrimSuffix(v, ".sql")] {
			return fmt.Errorf("the database was migrated by a newer release (unknown migration %s): upgrade incus-backup-server instead of running this older binary", v)
		}
	}
	return rows.Err()
}

// MigrateFS applies pending *.sql files from dir inside fsys in filename order,
// each in its own transaction together with its schema_migrations row, so a
// failed migration leaves no partial state. Applied files are never re-run.
func (d *DB) MigrateFS(ctx context.Context, fsys fs.FS, dir string) error {
	if _, err := d.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	rows, err := d.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("reading migrations: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		if applied[name] {
			continue
		}
		sql, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return err
		}
		tx, err := d.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("applying %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("committing %s: %w", name, err)
		}
	}
	return nil
}
