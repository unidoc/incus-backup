// Package app wires configuration, credentials, keys, Incus and the engine
// together for the incus-backup command, and holds the scheduler logic.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/state"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/schedule"
)

// App is a configured client.
type App struct {
	Cfg   *config.Config
	Eng   *engine.Engine
	State *state.State
	Log   *slog.Logger
	// Unreachable lists servers that could not be contacted while loading.
	Unreachable map[string]error

	// heavy serialises scheduled backups/verification with heavy host jobs (jobs.go).
	heavy sync.Mutex
	rt    *restoreTests
	// certificate renewal per server (certs.go)
	renewers  map[string]*remote.Renewer
	certCheck map[string]time.Time
}

// Passphrase supplies the key-file passphrase when one is needed.
type Passphrase func(keyFile string) (string, error)

// FilePassphrase reads the passphrase from the file named in the configuration
// or from INCUS_BACKUP_PASSPHRASE.
func FilePassphrase(cfg *config.Config) Passphrase {
	return func(string) (string, error) {
		if v := os.Getenv("INCUS_BACKUP_PASSPHRASE"); v != "" {
			return v, nil
		}
		if cfg.PassphraseFile != "" {
			b, err := os.ReadFile(cfg.PassphraseFile)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(b)), nil
		}
		return "", nil
	}
}

// LoadKeys reads and unwraps a key file.
func LoadKeys(path string, pass Passphrase) (*format.Keys, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the key file: %w (run `incus-backup init` first)", err)
	}
	kf, err := format.ParseKeyFile(b)
	if err != nil {
		return nil, nil, err
	}
	p := ""
	if kf.Protection == "passphrase" && pass != nil {
		if p, err = pass(path); err != nil {
			return nil, nil, err
		}
	}
	master, err := kf.Unwrap(p)
	if err != nil {
		return nil, nil, err
	}
	k, err := format.DeriveKeys(master)
	return k, master, err
}

// Load builds an App. Servers that cannot be reached are recorded in
// Unreachable and still get a Target (their uploads will report the failure).
func Load(ctx context.Context, cfg *config.Config, inc incusx.API, log *slog.Logger, pass Passphrase) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	a := &App{Cfg: cfg, State: st, Log: log, Unreachable: map[string]error{}}
	eng := &engine.Engine{Inc: inc, Targets: map[string]*engine.Target{}, Host: cfg.Host, SpoolDir: cfg.SpoolDir,
		MaxFreeze: cfg.Freeze(), MaxLag: cfg.Lag(), Workers: cfg.Performance.EncodeWorkers, UploadWorkers: cfg.Performance.UploadWorkers, Now: time.Now, Log: log, Policies: map[string]api.Policy{},
		DefaultPolicy: cfg.DefaultPolicy, Projects: cfg.Incus.Projects}
	if cfg.Filesystem.Format == "tar" {
		eng.FSFormat = "tar"
	}
	if d, err := time.ParseDuration(cfg.Filesystem.FullEvery); err == nil {
		eng.FSFullEvery = d
	}
	eng.FSNoTrustCtime = cfg.Filesystem.TrustCtime != nil && !*cfg.Filesystem.TrustCtime
	for name, sv := range cfg.Servers {
		keys, _, err := LoadKeys(cfg.KeyFor(name), pass)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", name, err)
		}
		dir := cfg.ServerDir(name)
		if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
			return nil, fmt.Errorf("server %s is not enrolled (%v): run `incus-backup init --server %s --token ...`", name, err, sv.Address)
		}
		cl, err := remote.NewFromDir(sv.Address, dir, keys.DomainID)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", name, err)
		}
		eng.Targets[name] = &engine.Target{Name: name, Client: cl, Keys: keys}
	}
	a.Eng = eng
	a.initRenewers()
	eng.Memory = st
	// local policies, then the control server's (server definitions win)
	for n, p := range cfg.Policies {
		eng.Policies[n] = api.Policy{Name: n, Schedule: p.Schedule, Targets: p.Targets, CommitRequired: p.CommitRequired}
	}
	a.Refresh(ctx)
	return a, nil
}

// Refresh fetches the desired state from the control server. A failure is not
// fatal: the last known assignments are kept empty and local policies still apply.
func (a *App) Refresh(ctx context.Context) {
	name := a.Cfg.ControlServer()
	t := a.Eng.Targets[name]
	ds, err := t.Client.DesiredState(ctx)
	if err != nil {
		a.Unreachable[name] = err
		a.Log.Warn("could not fetch the desired state from the control server; using local policies only", "server", name, "err", err)
		return
	}
	delete(a.Unreachable, name)
	for _, p := range ds.Policies {
		a.Eng.Policies[p.Name] = p
	}
	a.Eng.Assignments = ds.Assignments
}

// Plan discovers and resolves everything without changing anything.
func (a *App) Plan(ctx context.Context) ([]*engine.Object, error) {
	objs, err := a.Eng.Discover(ctx)
	if err != nil {
		return nil, err
	}
	a.Eng.Resolve(objs)
	return objs, nil
}

// DuePolicies returns the policies whose schedule (or retry time) has come.
func (a *App) DuePolicies(objs []*engine.Object, now time.Time) []string {
	used := map[string]bool{}
	for _, o := range objs {
		if o.Skip == "" {
			used[o.Policy] = true
		}
	}
	var due []string
	for name := range used {
		p := a.Eng.Policies[name]
		sp, err := schedule.Parse(p.Schedule)
		if err != nil {
			continue
		}
		last := a.State.Runs[name]
		retry := a.State.Retry[name]
		if sp.Due(last, now) || (!retry.IsZero() && !now.Before(retry)) {
			due = append(due, name)
		}
	}
	sort.Strings(due)
	return due
}

// RunPolicies runs the given policies and records the outcome.
func (a *App) RunPolicies(ctx context.Context, objs []*engine.Object, policies []string, dry bool) *engine.Report {
	rep := a.Eng.Run(ctx, objs, engine.RunOptions{Policies: policies, DryRun: dry})
	if dry {
		return rep
	}
	a.RecordRun(ctx, objs, rep, policies)
	return rep
}

// RecordRun stores the result of a run in the local state and reports the
// inventory to the servers.
func (a *App) RecordRun(ctx context.Context, objs []*engine.Object, rep *engine.Report, policies []string) {
	a.Eng.ReportInventory(ctx, objs)
	now := a.Eng.Now()
	failed := map[string]bool{}
	ran := map[string]bool{}
	for _, r := range rep.Results {
		ran[r.Object.Policy] = true
		if !r.OK() {
			failed[r.Object.Policy] = true
		}
	}
	if len(policies) == 0 {
		for p := range ran {
			policies = append(policies, p)
		}
	}
	_ = a.State.Update(func(s *state.State) {
		s.LastRun = now
		for _, p := range policies {
			s.Runs[p] = now
			if failed[p] {
				s.Retry[p] = now.Add(15 * time.Minute)
			} else {
				delete(s.Retry, p)
			}
		}
		for _, r := range rep.Results {
			o := s.Objects[r.Object.Key()]
			if o == nil {
				o = &state.ObjectState{}
				s.Objects[r.Object.Key()] = o
			}
			o.LastRun = now
			switch {
			case r.OK():
				o.LastStatus, o.LastError, o.LastSnap = "ok", "", r.Snapshot
			case r.Deferred(): // repository maintenance: retried like a failure, but not one
				o.LastStatus, o.LastError = "deferred", firstError(r)
			default:
				o.LastStatus = "failed"
				o.LastError = firstError(r)
			}
		}
	})
}

func firstError(r *engine.Result) string {
	if r.Err != nil {
		return r.Err.Error()
	}
	for _, t := range r.Targets {
		if t.Err != nil {
			return t.Target + ": " + t.Err.Error()
		}
	}
	return ""
}

// Tick runs everything that is due now (backups, then verification).
func (a *App) Tick(ctx context.Context) (*engine.Report, error) {
	a.heavy.Lock() // heavy host jobs also refresh, plan and run the engine
	defer a.heavy.Unlock()
	a.RenewDue(ctx)
	a.Refresh(ctx)
	objs, err := a.Plan(ctx)
	if err != nil {
		return nil, err
	}
	now := a.Eng.Now()
	var rep *engine.Report
	if due := a.DuePolicies(objs, now); len(due) > 0 {
		a.Log.Info("running due policies", "policies", due)
		rep = a.RunPolicies(ctx, objs, due, false)
	}
	a.verifyDue(ctx, now)
	a.restoreTestsDue(ctx, objs, now)
	return rep, nil
}

func (a *App) verifyDue(ctx context.Context, now time.Time) {
	sp, err := schedule.Parse(a.Cfg.VerifySchedule)
	if err != nil {
		return
	}
	for _, name := range a.Cfg.ServerNames() {
		key := "verify:" + name
		if !sp.Due(a.State.Verify[key], now) {
			continue
		}
		rep, err := a.Eng.Verify(ctx, a.Eng.Targets[name], 1)
		if err != nil {
			a.Log.Error("verification FAILED", "server", name, "err", err, "problems", rep.Problems)
		} else {
			a.Log.Info("verification passed", "server", name, "snapshots", rep.Snapshots, "chunks", rep.Chunks)
		}
		_ = a.State.Update(func(s *state.State) { s.Verify[key] = now })
	}
}

// Loop ticks every minute until ctx ends.
func (a *App) Loop(ctx context.Context) error {
	a.StartJobs(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if _, err := a.Tick(ctx); err != nil {
			a.Log.Error("tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

var errNoServers = errors.New("no servers")
