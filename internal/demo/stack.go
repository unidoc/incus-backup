// Package demo starts a complete, real incus-backup server stack in one process (embedded
// PostgreSQL, the agent API over mutual TLS, the management API and web UI on plain HTTP, the unix
// admin socket, optionally MinIO for an S3 repository) and seeds it with realistic history produced
// by the real client engine against a fake Incus. It is a development and end-to-end test tool
// (cmd/ibdemo); nothing here is shipped.
package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/httpapi"
	"github.com/unidoc/incus-backup/internal/server/maint"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/server/webui"
)

// Options configure a demo stack.
type Options struct {
	Dir   string // everything lives here (PostgreSQL data, datastores, MinIO data, host state)
	Reset bool   // wipe Dir first
	// Web is the plain-HTTP management listener ("127.0.0.1:0" picks a free port).
	Web string
	// Agent is the agent API (mTLS) listener; "" reuses the port of the previous run or picks one.
	Agent string
	// Socket is the unix admin socket path ("" = Dir/admin.sock; must stay under ~100 bytes).
	Socket string
	// UIDir serves a Vite build from disk; "" serves the UI embedded in the binary.
	UIDir string
	Seed  bool
	// Profile sizes the seed (FullProfile or TestProfile).
	Profile Profile
	// Password of every demo user.
	Password string
	// Minio is the MinIO server binary; "" means the second repository is a filesystem one.
	Minio string
	// Maintenance runs the real maintenance scheduler (retention, GC, verify, capacity, sync) on the
	// real clock after seeding. Off by default: a due retention run would thin the seeded history.
	Maintenance bool
	// NoJobs leaves the simulated hosts' job executors off (no host jobs; less memory after seeding).
	NoJobs bool
	Log    *slog.Logger
	// Progress receives one line per seed phase (nil = discard).
	Progress io.Writer
}

// Stack is a running demo.
type Stack struct {
	Opt   Options
	Clock *Clock
	DB    *db.DB
	Svc   *service.Service
	Sync  *syncer.Manager
	Auth  *auth.Store
	Mgmt  *mgmt.Server

	AgentAddr  string
	WebURL     string
	SocketPath string
	S3Endpoint string // "" without MinIO
	State      *State
	// JobHosts are the simulated hosts whose job executors run (they take jobs from the API).
	JobHosts []string

	seededHosts []*app.App // the job hosts of a fresh seed, handed to startRuntime

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closers []func()
	once    sync.Once
}

// State is what a demo directory remembers between runs (demo.json).
type State struct {
	PGPort       int        `json:"pg_port"`
	AgentPort    int        `json:"agent_port"`
	MinioPort    int        `json:"minio_port"`
	Seeded       bool       `json:"seeded"`
	SeededAt     *time.Time `json:"seeded_at,omitempty"`
	SeedSeconds  float64    `json:"seed_seconds,omitempty"`
	Profile      string     `json:"profile,omitempty"`
	ProfileSpec  *Profile   `json:"profile_spec,omitempty"` // rebuilds the hosts' worlds on a restart
	RecoveryKey  string     `json:"recovery_key,omitempty"`
	MasterHex    string     `json:"master_hex,omitempty"`
	DomainID     string     `json:"domain_id,omitempty"`
	EscrowPriv   string     `json:"escrow_private,omitempty"`
	EscrowPub    string     `json:"escrow_public,omitempty"`
	Offsite      string     `json:"offsite_backend,omitempty"`
	Facts        *SeedFacts `json:"facts,omitempty"`
	SeedStarted  bool       `json:"seed_started,omitempty"`
	ServerAddrV4 string     `json:"-"`
}

func (s *Stack) statePath() string { return filepath.Join(s.Opt.Dir, "demo.json") }

func (s *Stack) saveState() error {
	b, _ := json.MarshalIndent(s.State, "", "  ")
	return os.WriteFile(s.statePath(), b, 0o600)
}

func (s *Stack) progress(format string, a ...any) {
	if s.Opt.Progress != nil {
		fmt.Fprintf(s.Opt.Progress, format+"\n", a...)
	}
}

// Users are the demo accounts: one per role, all with Options.Password.
var Users = []struct {
	Name, Display string
	Role          auth.Role
}{
	{"admin", "Demo Administrator", auth.Admin},
	{"operator", "Olga Operator", auth.Operator},
	{"restorer", "Rúnar Restore", auth.Restore},
	{"viewer", "Vera Viewer", auth.ReadOnly},
}

// DefaultPassword is the password of every demo user unless --admin-password says otherwise.
const DefaultPassword = "demo-demo-demo-1"

// Start brings the stack up (seeding first when the directory is new) and returns once every
// listener accepts connections. Close stops it.
func Start(ctx context.Context, opt Options) (st *Stack, err error) {
	if opt.Dir == "" {
		return nil, errors.New("demo: Dir is required")
	}
	if opt.Password == "" {
		opt.Password = DefaultPassword
	}
	if len(opt.Password) < 12 {
		return nil, fmt.Errorf("demo: the password must have at least 12 characters (auth policy)")
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	if opt.Profile.Name == "" {
		opt.Profile = FullProfile()
	}
	if opt.Web == "" {
		opt.Web = "127.0.0.1:0"
	}
	abs, err := filepath.Abs(opt.Dir)
	if err != nil {
		return nil, err
	}
	opt.Dir = abs
	// a throw-away dev repository: fsync per chunk costs most of the seed time on overlay filesystems
	datastore.SkipFsync = true
	if opt.Reset {
		stopLeftovers(opt.Dir)
		if err := os.RemoveAll(opt.Dir); err != nil {
			return nil, fmt.Errorf("demo: --reset: %w", err)
		}
	}
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	s := &Stack{Opt: opt, State: &State{}}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	if b, rerr := os.ReadFile(s.statePath()); rerr == nil {
		if jerr := json.Unmarshal(b, s.State); jerr != nil {
			return nil, fmt.Errorf("demo: %s: %w (start with --reset)", s.statePath(), jerr)
		}
	}
	if s.State.SeedStarted && !s.State.Seeded {
		return nil, fmt.Errorf("demo: %s holds a seed that never finished (the demo was stopped while seeding): start with --reset", opt.Dir)
	}
	s.SocketPath = opt.Socket
	if s.SocketPath == "" {
		s.SocketPath = filepath.Join(opt.Dir, "admin.sock")
		if len(s.SocketPath) > 100 { // unix socket paths are limited to ~108 bytes
			h := sha256.Sum256([]byte(opt.Dir))
			s.SocketPath = filepath.Join(os.TempDir(), "ibdemo-"+hex.EncodeToString(h[:4])+".sock")
		}
	}
	if len(s.SocketPath) > 100 {
		return nil, fmt.Errorf("demo: the admin socket path %s is too long for a unix socket (use --socket)", s.SocketPath)
	}
	pick := func(p *int) error {
		if *p != 0 && portFree(*p) {
			return nil
		}
		np, err := freePort()
		*p = np
		return err
	}
	if s.State.Seeded && s.State.AgentPort != 0 && !portFree(s.State.AgentPort) && opt.Agent == "" {
		return nil, fmt.Errorf("demo: the agent API port %d of this seeded directory is in use (another demo on the same --dir?)", s.State.AgentPort)
	}
	if err := pick(&s.State.PGPort); err != nil {
		return nil, err
	}

	// PostgreSQL
	s.progress("starting embedded PostgreSQL (port %d, data %s)", s.State.PGPort, filepath.Join(opt.Dir, "pg"))
	pgURL, pgStop, err := startPostgres(filepath.Join(opt.Dir, "pg"), s.State.PGPort, nil)
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, pgStop)
	d, err := db.Open(ctx, pgURL)
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, d.Close)
	if err := d.Migrate(ctx); err != nil {
		return nil, err
	}
	s.DB = d

	seeding := opt.Seed && !s.State.Seeded
	if seeding {
		s.Clock = NewSimClock(seedStart(time.Now(), opt.Profile))
	} else {
		s.Clock = NewRealClock()
	}
	s.Svc = service.New(d, filepath.Join(opt.Dir, "server-state"), "demo", opt.Log)
	s.Svc.Now = s.Clock.Now
	s.Sync = &syncer.Manager{Svc: s.Svc}
	s.Auth = auth.New(d.Pool(), s.Svc.Sealer("user-totp"))
	s.Auth.Now = s.Clock.Now
	if _, err := os.Stat(filepath.Join(s.Svc.StateDir, "ca.crt")); err != nil {
		if err := s.Svc.InitPKI([]string{"127.0.0.1", "localhost", "::1"}); err != nil {
			return nil, err
		}
	}
	release, err := s.Svc.BecomeDaemon(ctx)
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, release)
	if err := s.Svc.ExpireStaleSessions(ctx); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// agent API (mutual TLS)
	agentAddr := opt.Agent
	if agentAddr == "" {
		if err := pick(&s.State.AgentPort); err != nil {
			return nil, err
		}
		agentAddr = fmt.Sprintf("127.0.0.1:%d", s.State.AgentPort)
	}
	tc, err := s.Svc.ServerTLSConfig()
	if err != nil {
		return nil, err
	}
	aln, err := net.Listen("tcp", agentAddr)
	if err != nil {
		return nil, fmt.Errorf("agent API listener: %w", err)
	}
	s.AgentAddr = aln.Addr().String()
	if p := aln.Addr().(*net.TCPAddr).Port; p != s.State.AgentPort {
		s.State.AgentPort = p
	}
	agent := &http.Server{Handler: (&httpapi.Server{Svc: s.Svc, Log: opt.Log}).Handler(), TLSConfig: tc, ReadHeaderTimeout: 30 * time.Second}
	s.serve(func() error { return agent.ServeTLS(aln, "", "") }, agent)

	// the second repository's S3 service
	if opt.Minio != "" {
		if err := pick(&s.State.MinioPort); err != nil {
			return nil, err
		}
		s.progress("starting MinIO (port %d) for the S3 repository \"offsite\"", s.State.MinioPort)
		ep, stop, err := startMinio(ctx, opt.Minio, filepath.Join(opt.Dir, "minio"), s.State.MinioPort, "offsite")
		if err != nil {
			return nil, err
		}
		s.S3Endpoint = ep
		s.closers = append(s.closers, stop)
	}
	if err := s.saveState(); err != nil {
		return nil, err
	}

	if seeding {
		s.State.SeedStarted = true
		if err := s.saveState(); err != nil {
			return nil, err
		}
		t0 := time.Now()
		if err := s.seed(ctx); err != nil {
			return nil, fmt.Errorf("demo: seeding: %w", err)
		}
		now := time.Now()
		s.State.Seeded, s.State.SeededAt, s.State.SeedSeconds, s.State.Profile = true, &now, now.Sub(t0).Seconds(), opt.Profile.Name
		prof := opt.Profile
		s.State.ProfileSpec = &prof
		if err := s.saveState(); err != nil {
			return nil, err
		}
		s.Clock.Real()
		debug.FreeOSMemory() // the fake Incus worlds are garbage now: give their memory back
	} else if err := s.ensureBase(ctx); err != nil {
		return nil, err
	}

	// management API: admin socket and the plain-HTTP web listener
	ui := webui.Handler()
	if opt.UIDir != "" {
		if _, err := os.Stat(filepath.Join(opt.UIDir, "index.html")); err != nil {
			return nil, fmt.Errorf("demo: --ui-dir %s has no index.html: %w", opt.UIDir, err)
		}
		ui = uiHandler(opt.UIDir)
	}
	s.Mgmt = &mgmt.Server{Svc: s.Svc, Sync: s.Sync, Log: opt.Log, Auth: s.Auth, AgentListen: s.AgentAddr, UI: ui, Secure: false}
	var jobApps []*app.App
	if !opt.NoJobs && s.State.Seeded {
		if seeding {
			jobApps = s.seededHosts
		} else if jobApps, err = s.loadJobHosts(ctx); err != nil {
			return nil, fmt.Errorf("demo: rebuilding the job hosts: %w", err)
		}
	}
	s.seededHosts = nil
	s.startRuntime(rctx, jobApps)
	debug.FreeOSMemory() // rebuilding the hosts' disks generated garbage: give it back
	_ = os.Remove(s.SocketPath)
	sln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("admin socket: %w", err)
	}
	_ = os.Chmod(s.SocketPath, 0o660)
	local := &http.Server{Handler: s.Mgmt.LocalHandler(), ReadHeaderTimeout: 10 * time.Second}
	s.serve(func() error { return local.Serve(sln) }, local)
	s.closers = append(s.closers, func() { _ = os.Remove(s.SocketPath) })

	wln, err := net.Listen("tcp", opt.Web)
	if err != nil {
		return nil, fmt.Errorf("web listener %s: %w", opt.Web, err)
	}
	s.WebURL = "http://" + wln.Addr().String()
	web := &http.Server{Handler: s.Mgmt.WebHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	s.serve(func() error { return web.Serve(wln) }, web)

	// the small housekeeping the real daemon does; maintenance only on request
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				s.Svc.RefreshModes(rctx)
			}
		}
	}()
	if opt.Maintenance {
		r := &maint.Runner{Svc: s.Svc, Sync: s.Sync}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); r.Loop(rctx) }()
	}
	return s, nil
}

// serve runs one HTTP server until Close.
func (s *Stack) serve(run func() error, srv *http.Server) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Opt.Log.Error("demo listener stopped", "err", err)
		}
	}()
	s.closers = append(s.closers, func() {
		// idle keep-alive connections of the (gone) simulated hosts would hold Shutdown for its whole timeout
		sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if srv.Shutdown(sc) != nil {
			_ = srv.Close()
		}
	})
}

// Close stops everything in reverse order of start.
func (s *Stack) Close() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		for i := len(s.closers) - 1; i >= 0; i-- {
			s.closers[i]()
		}
		s.wg.Wait()
	})
}

// ensureBase makes an unseeded stack usable (--seed=false): users and the first repository.
func (s *Stack) ensureBase(ctx context.Context) error {
	if err := s.createUsers(ctx); err != nil {
		return err
	}
	rs, err := s.Svc.Repositories(ctx)
	if err != nil {
		return err
	}
	if len(rs) == 0 {
		if _, err := s.Svc.CreateRepository(ctx, "custody", filepath.Join(s.Opt.Dir, "datastore", "custody")); err != nil {
			return err
		}
	}
	return nil
}

// createUsers creates the four demo accounts (idempotent).
func (s *Stack) createUsers(ctx context.Context) error {
	for _, u := range Users {
		if _, err := s.Auth.User(ctx, u.Name); err == nil {
			continue
		}
		if _, err := s.Auth.CreateUser(ctx, u.Name, s.Opt.Password, u.Role, u.Display, u.Name+"@demo.invalid", false); err != nil {
			return fmt.Errorf("creating demo user %s: %w", u.Name, err)
		}
	}
	return nil
}

// stopLeftovers stops PostgreSQL/MinIO processes of a hard-killed demo before its directory is wiped.
func stopLeftovers(dir string) {
	killStale(filepath.Join(dir, "pg", "data", "postmaster.pid"), "postgres", filepath.Join(dir, "pg", "data"))
	killStale(filepath.Join(dir, "minio", "minio.pid"), "minio", filepath.Join(dir, "minio", "data"))
}
