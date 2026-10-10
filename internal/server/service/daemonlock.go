package service

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Session pins, "one GC at a time" and the writer slots live in the memory of ONE server process. A
// second process that ran GC, retention, forget or a quarantining verify against the same repository
// would not see them and could delete chunks a running backup relies on. So every destructive
// operation needs the repository maintenance lock, a PostgreSQL advisory lock:
//
//   - the daemon (`serve`) takes it for its whole life: it is the one process that owns the pins;
//   - any other process (the CLI) can take it only while no daemon runs, and holds it for the
//     operation; otherwise it gets ErrDaemonRunning and the CLI forwards the request to the daemon's
//     admin socket.
//
// A second daemon cannot start either, which also stops it from freeing every writer slot
// (ExpireStaleSessions) of the first.
const daemonLockKey int64 = 0x69626b5f73727600 // "ibk_srv"

// ErrDaemonRunning means another incus-backup-server process owns the repositories.
var ErrDaemonRunning = errors.New("the incus-backup-server daemon is running: this operation must go through it (admin socket)")

type lockHolder struct {
	pool *pgxpool.Pool
}

// holdLock acquires the advisory lock on a dedicated connection; release frees it.
func holdLock(ctx context.Context, pool *pgxpool.Pool) (release func(), err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, daemonLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, err
	}
	if !ok {
		conn.Release()
		return nil, ErrDaemonRunning
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, daemonLockKey)
		conn.Release()
	}, nil
}

// BecomeDaemon takes the maintenance lock for the life of the process (call it from `serve`).
func (s *Service) BecomeDaemon(ctx context.Context) (release func(), err error) {
	rel, err := holdLock(ctx, s.DB.Pool())
	if err != nil {
		return nil, fmt.Errorf("another incus-backup-server is already serving this database: %w", err)
	}
	s.mu.Lock()
	s.daemon = true
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.daemon = false
		s.mu.Unlock()
		rel()
	}, nil
}

type guardState struct {
	mu      sync.Mutex
	n       int
	release func()
}

// guard returns a function to call when the destructive operation is done. In the daemon it is free.
// Elsewhere it holds the advisory lock (shared by all operations of this process) or fails with
// ErrDaemonRunning.
func (s *Service) guard(ctx context.Context) (func(), error) {
	s.mu.Lock()
	daemon := s.daemon
	s.mu.Unlock()
	if daemon {
		return func() {}, nil
	}
	g := &s.opGuard
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n == 0 {
		rel, err := holdLock(ctx, s.DB.Pool())
		if err != nil {
			return nil, err
		}
		g.release = rel
	}
	g.n++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.n--
		if g.n == 0 && g.release != nil {
			g.release()
			g.release = nil
		}
	}, nil
}

// Guard is guard for other packages (the sync manager).
func (s *Service) Guard(ctx context.Context) (func(), error) { return s.guard(ctx) }
