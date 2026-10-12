// Package testdb gives tests a real, empty PostgreSQL database.
//
// It uses INCUS_BACKUP_TEST_DATABASE_URL (any reachable server, e.g. devenv/)
// when set, and otherwise starts embedded-postgres. If neither works the test
// FAILS: a database test that silently skips proves nothing.
//
// Packages using it must call Main from TestMain.
package testdb

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
)

var (
	adminURL string // connects to the maintenance database of a running server
	counter  atomic.Int64
	startErr error
)

// Main starts PostgreSQL if needed, runs the tests and stops it.
func Main(m *testing.M) {
	var stop func()
	if u := os.Getenv("INCUS_BACKUP_TEST_DATABASE_URL"); u != "" {
		adminURL = u
	} else {
		adminURL, stop, startErr = startEmbedded()
	}
	code := m.Run()
	if stop != nil {
		stop()
	}
	for _, f := range atExit {
		f()
	}
	os.Exit(code)
}

var atExit []func()

// AtExit registers cleanup that runs after the tests, before the process exits
// (for other test services started next to the database, e.g. s3test).
func AtExit(f func()) {
	if f != nil {
		atExit = append(atExit, f)
	}
}

func startEmbedded() (string, func(), error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	dir, err := os.MkdirTemp("", "incus-backup-pg-*")
	if err != nil {
		return "", nil, err
	}
	cache := os.Getenv("INCUS_BACKUP_PG_CACHE")
	if cache == "" {
		home, _ := os.UserCacheDir()
		cache = home + "/incus-backup-embedded-postgres"
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		DataPath(dir + "/data").
		RuntimePath(dir + "/run").
		BinariesPath(cache).
		Username("postgres").Password("postgres").
		Logger(io.Discard).
		StartTimeout(120 * time.Second)
	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("starting embedded postgres: %w", err)
	}
	stop := func() {
		_ = pg.Stop()
		_ = os.RemoveAll(dir)
	}
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port), stop, nil
}

// New returns the URL of a fresh, empty database, dropped when the test ends.
func New(t testing.TB) string {
	t.Helper()
	if startErr != nil {
		t.Fatalf("no PostgreSQL available for tests (set INCUS_BACKUP_TEST_DATABASE_URL or allow embedded-postgres): %v", startErr)
	}
	if adminURL == "" {
		t.Fatal("testdb.Main was not called from TestMain")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting to test PostgreSQL: %v", err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("ib_test_%d_%d", os.Getpid(), counter.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + strings.TrimPrefix(name, "/")
	return u.String()
}
