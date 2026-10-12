package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/server/maint"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

// Profile sizes the seed.
type Profile struct {
	Name string
	// Days of simulated history (backups, maintenance, capacity samples).
	Days int
	// DiskUnit is the size of one "GiB" of the nominal VM disk sizes (the fake keeps disks in RAM).
	DiskUnit int64
	// DataDiv divides the real data written into each VM (1 = full).
	DataDiv int
	// TreeFiles / TreeLargeMiB size the browsable custom volume; ContainerFiles each container root.
	TreeFiles, TreeLargeMiB, ContainerFiles int
	// HourlyHours is how long before "now" the hourly object switched to the hourly policy.
	HourlyHours int
}

// FullProfile is what cmd/ibdemo seeds: 30 days, 32-128 MiB disks, a few thousand files. The fake Incus
// holds every VM disk as a byte slice, so disks stay small: multi-GiB "sparse" disks were tried and
// paged in gigabytes during the seed.
func FullProfile() Profile {
	return Profile{Name: "full", Days: 30, DiskUnit: 16 << 20, DataDiv: 1, TreeFiles: 3000, TreeLargeMiB: 12, ContainerFiles: 400, HourlyHours: 48}
}

// TestProfile is the reduced seed of the package test: every situation, little data.
func TestProfile() Profile {
	return Profile{Name: "test", Days: 5, DiskUnit: 8 << 20, DataDiv: 8, TreeFiles: 300, TreeLargeMiB: 2, ContainerFiles: 40, HourlyHours: 6}
}

// seedNow is "now" for the seed (a test pins it to check the seed at any hour of the day).
var seedNow = time.Now

// seedStart is the first simulated hour: Days before now.
func seedStart(now time.Time, p Profile) time.Time {
	return now.Truncate(time.Hour).Add(-time.Duration(p.Days) * 24 * time.Hour)
}

// SeedFacts records where the seeded situations are, for the banner, docs and tests.
type SeedFacts struct {
	Repositories    []string          `json:"repositories"`
	OfflineHost     string            `json:"offline_host"`
	FailingObject   string            `json:"failing_object"` // host/project/name whose backups fail
	StaleObject     string            `json:"stale_object"`
	NeverBackedUp   string            `json:"never_backed_up"` // host/project/name
	HourlyObject    string            `json:"hourly_object"`   // host/project/name
	Protected       SnapRef           `json:"protected"`       // protected snapshot with a note
	FailedVerify    SnapRef           `json:"failed_verify"`   // snapshot whose chunk was corrupted
	CorruptChunk    string            `json:"corrupt_chunk"`   //
	FSObject        SnapRef           `json:"fs_object"`       // latest catalog snapshot of the browsable volume
	FSFile          string            `json:"fs_file"`         // a unicode path inside it
	FSFileSHA256    string            `json:"fs_file_sha256"`  // its content hash at that snapshot
	SyncJobs        map[string]string `json:"sync_jobs"`       // name -> state of its last run
	Groups          map[string]string `json:"groups"`          // "host/project/name" -> group id
	Snapshots       int               `json:"snapshots"`
	BackupRuns      int               `json:"backup_runs"`
	SeedDurationSec float64           `json:"seed_duration_sec"`
}

// SnapRef names one snapshot.
type SnapRef struct {
	Repository string `json:"repository"`
	Object     string `json:"object"` // host/project/name
	Group      string `json:"group"`
	Snapshot   string `json:"snapshot"`
	Note       string `json:"note,omitempty"`
}

// demoMaster is the fixed master key of the demo's key domain: the printed recovery key stays the
// same across --reset, so E2E tests can hard-code it. Never use a fixed key for anything real.
func demoMaster() []byte {
	h := sha256.Sum256([]byte("incus-backup dev demo master key (public, never use for real data)"))
	return h[:]
}

// demoIncus is the fake Incus of one host plus failure injection for one instance at a time: a VM
// whose NBD export keeps failing (FailNext is one-shot and hits whatever runs first).
type demoIncus struct {
	*fake.Incus
	mu     sync.Mutex
	broken map[string]error // project/instance -> error
}

func (d *demoIncus) InstanceNBD(ctx context.Context, project, instance string, reuse bool) (net.Conn, error) {
	d.mu.Lock()
	err := d.broken[project+"/"+instance]
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.Incus.InstanceNBD(ctx, project, instance, reuse)
}

func (d *demoIncus) setBroken(project, instance string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.broken, project+"/"+instance)
	} else {
		d.broken[project+"/"+instance] = err
	}
}

// vmSpec is one VM of the seeded world.
type vmSpec struct {
	host, project, name string
	diskGiB             int64
	randMiB, textMiB    int // real data at the start of the disk: "binaries" then text
}

var vms = []vmSpec{
	{"bosch", "prod", "api-01", 4, 8, 16},
	{"bosch", "prod", "db-01", 8, 12, 32},
	{"bosch", "prod", "web-01", 2, 6, 12},
	{"bosch", "staging", "ci-runner", 6, 10, 12},
	{"benfica", "default", "gitlab", 8, 16, 24},
	{"benfica", "customer-acme", "erp", 4, 8, 20},
	{"uni-02", "default", "monitoring", 2, 4, 16},
}

type ctSpec struct{ host, project, name string }

var containers = []ctSpec{
	{"bosch", "prod", "redis"}, {"bosch", "prod", "nginx"}, {"bosch", "staging", "scratch"}, {"uni-02", "default", "pihole"},
}

var hostProjects = map[string][]string{"bosch": {"prod", "staging"}, "benfica": {"default", "customer-acme"}, "uni-02": {"default"}}

type simHost struct {
	name string
	inc  *demoIncus
	app  *app.App
	// active until this time (the offline host stops reporting)
	until time.Time
}

type seeder struct {
	lastTreeChurn time.Time // simulated time of the last edit of the volume trees
	s             *Stack
	p             Profile
	ctx           context.Context
	clock         *Clock
	start         time.Time
	t0            time.Time
	keys          *format.Keys
	hosts         map[string]*simHost
	order         []string
	runner        *maint.Runner
	facts         *SeedFacts
	dir           string
	trees         map[string]string // host/project/name -> live directory of a filesystem object
	log           *slog.Logger
	mib           int64
	offsite       string
	// timings of the seed phases (progress output)
	runnerDur, syncWait time.Duration
}

func (sd *seeder) mustGroup(project, kind, name string) string {
	return sd.keys.GroupID(project, kind, name)
}

func (s *Stack) seed(ctx context.Context) error {
	p := s.Opt.Profile
	start := s.Clock.Now()
	t0 := start.Add(time.Duration(p.Days) * 24 * time.Hour)
	if real := seedNow().Truncate(time.Minute); real.After(t0) {
		t0 = real
	}
	keys, err := format.DeriveKeys(demoMaster())
	if err != nil {
		return err
	}
	remote.RetryDelay = 20 * time.Millisecond // a sync peer that refuses us must not sleep through the seed
	sd := &seeder{s: s, p: p, ctx: ctx, clock: s.Clock, start: start, t0: t0, keys: keys, hosts: map[string]*simHost{},
		facts: &SeedFacts{SyncJobs: map[string]string{}, Groups: map[string]string{}}, dir: filepath.Join(s.Opt.Dir, "hosts"),
		trees: map[string]string{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), mib: 1 << 20}
	sd.runner = &maint.Runner{Svc: s.Svc, Sync: s.Sync}
	began := time.Now()
	if err := sd.setup(); err != nil {
		return err
	}
	if err := sd.simulate(); err != nil {
		return err
	}
	if err := sd.finish(); err != nil {
		return err
	}
	for _, h := range jobHosts {
		s.seededHosts = append(s.seededHosts, sd.hosts[h].app)
	}
	sd.facts.SeedDurationSec = time.Since(began).Seconds()
	s.State.Facts = sd.facts
	return nil
}

// ---- setup: repositories, policies, users, hosts (real enrolment), the Incus worlds -----------------

func (sd *seeder) setup() error {
	ctx, s := sd.ctx, sd.s
	s.progress("seed: %s profile, %d simulated days ending %s", sd.p.Name, sd.p.Days, sd.t0.Format(time.RFC3339))
	if err := s.createUsers(ctx); err != nil {
		return err
	}
	// a directory first started with --seed=false already has an (empty) custody repository
	if _, err := s.Svc.Repository(ctx, "custody"); err != nil {
		if _, err := s.Svc.CreateRepository(ctx, "custody", filepath.Join(s.Opt.Dir, "datastore", "custody")); err != nil {
			return err
		}
	}
	if s.S3Endpoint != "" {
		if _, err := s.Svc.AddS3Repository(ctx, "offsite", service.S3Params{
			S3Settings: service.S3Settings{Endpoint: s.S3Endpoint, Bucket: "offsite", Prefix: "ib", NoTLS: true, PathStyle: true},
			AccessKey:  minioAccess, SecretKey: minioSecret}); err != nil {
			return fmt.Errorf("S3 repository: %w", err)
		}
		sd.offsite = "s3"
	} else {
		if _, err := s.Svc.CreateRepository(ctx, "offsite", filepath.Join(s.Opt.Dir, "datastore", "offsite")); err != nil {
			return err
		}
		sd.offsite = "filesystem"
	}
	s.State.Offsite = sd.offsite
	sd.facts.Repositories = []string{"custody", "offsite"}
	pols := []api.Policy{
		{Name: "nightly", Schedule: "daily", Targets: []string{"custody"}, KeepDaily: 14, KeepWeekly: 8, KeepMonthly: 12, Timezone: "UTC"},
		{Name: "hourly", Schedule: "hourly", Targets: []string{"custody"}, KeepHourly: 48, KeepDaily: 14, KeepWeekly: 8, Timezone: "UTC"},
		{Name: "weekly-offsite", Schedule: "weekly", Targets: []string{"custody"}, KeepWeekly: 12, KeepMonthly: 12, Timezone: "UTC"},
		{Name: "ci", Schedule: "01:30", Targets: []string{"custody"}, KeepDaily: 7, KeepWeekly: 4, Timezone: "UTC"},
		{Name: "none", Schedule: "manual", Targets: []string{"custody"}, KeepLast: 1, Timezone: "UTC"},
	}
	// capacity every few hours instead of hourly: 30 days of chart points without sampling 720 times
	// the S3 offsite copy is checked weekly (object-store requests cost money)
	for repo, every := range map[string]string{"custody": "every 4h", "offsite": "every 12h"} {
		m, err := s.Svc.Maintenance(ctx, repo)
		if err != nil {
			return err
		}
		m.Capacity = every
		if repo == "offsite" {
			m.Verify, m.Reconcile, m.Prune, m.GC = "sun 07:00", "sun 07:00", "sun 04:00", "sun 05:00"
		}
		if err := s.Svc.SetMaintenance(ctx, repo, *m); err != nil {
			return err
		}
	}
	for _, pl := range pols {
		if err := s.Svc.SetPolicy(ctx, "custody", pl); err != nil {
			return fmt.Errorf("policy %s: %w", pl.Name, err)
		}
	}
	// the master key and the escrow pair (the offline key that can recover the master key)
	rec, err := format.EncodeRecoveryKey(demoMaster())
	if err != nil {
		return err
	}
	priv, pub, err := format.GenerateEscrowKeyPair()
	if err != nil {
		return err
	}
	s.State.RecoveryKey, s.State.MasterHex, s.State.DomainID, s.State.EscrowPriv, s.State.EscrowPub = rec, hex.EncodeToString(demoMaster()), sd.keys.DomainID, priv, pub
	kf, err := format.NewKeyFile(demoMaster(), "", format.DefaultArgon2(), nil, nil)
	if err != nil {
		return err
	}
	keyFile, err := kf.Marshal()
	if err != nil {
		return err
	}

	// the Incus worlds
	for _, h := range []string{"bosch", "benfica", "uni-02"} {
		inc, trees, err := buildWorld(sd.dir, sd.p, h, true)
		if err != nil {
			return err
		}
		for k, v := range trees {
			sd.trees[k] = v
		}
		sh := &simHost{name: h, inc: inc, until: sd.t0.Add(time.Hour)}
		if h == "uni-02" {
			sh.until = sd.t0.Add(-72 * time.Hour)
		}
		sd.hosts[h] = sh
		sd.order = append(sd.order, h)
	}
	for _, v := range vms {
		sd.facts.Groups[v.host+"/"+v.project+"/instance/"+v.name+"/root"] = sd.mustGroup(v.project, "block", "instance/"+v.name+"/root")
	}
	for _, c := range containers {
		sd.facts.Groups[c.host+"/"+c.project+"/instance/"+c.name+"/root"] = sd.mustGroup(c.project, "filesystem", "instance/"+c.name+"/root")
	}
	sd.facts.Groups["benfica/default/volume/nextcloud-data"] = sd.mustGroup("default", "filesystem", "volume/nextcloud-data")
	sd.facts.FSFile = "Þórsmörk-ferð.txt"

	// enrol every host through the real token flow and load it as `incus-backup` would
	for _, h := range sd.order {
		if err := sd.enrol(sd.hosts[h], keyFile); err != nil {
			return fmt.Errorf("host %s: %w", h, err)
		}
	}
	// desired state: host-wide defaults, a project override, an object override
	for _, a := range [][4]string{
		{"bosch", "", "", "nightly"}, {"benfica", "", "", "nightly"}, {"benfica", "customer-acme", "", "weekly-offsite"},
		{"uni-02", "", "", "nightly"}, {"bosch", "staging", "scratch", "none"},
	} {
		if err := s.Svc.Assign(ctx, "custody", a[0], a[1], a[2], a[3]); err != nil {
			return fmt.Errorf("assignment %v: %w", a, err)
		}
	}
	sd.facts.OfflineHost = "uni-02"
	sd.facts.FailingObject = "bosch/staging/instance/ci-runner/root"
	sd.facts.StaleObject = "bosch/prod/instance/web-01/root"
	sd.facts.NeverBackedUp = "bosch/staging/instance/scratch/root"
	sd.facts.HourlyObject = "bosch/prod/instance/redis/root"
	return nil
}

func (sd *seeder) enrol(h *simHost, keyFile []byte) error {
	ctx := sd.ctx
	dir := filepath.Join(sd.dir, h.name)
	cfg := config.Defaults()
	cfg.Host = h.name
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.SpoolDir = filepath.Join(dir, "spool")
	cfg.KeyFile = filepath.Join(dir, "key.json")
	cfg.VerifySchedule = "manual" // client-side restore verification is not part of the seed
	for _, j := range jobHosts {
		if j == h.name {
			allowAllJobs(&cfg)
		}
	}
	cfgPath := filepath.Join(dir, "client.yaml")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := cfg.Save(cfgPath); err != nil {
		return err
	}
	// one repository = one key domain: every host carries the same key file
	if err := os.WriteFile(cfg.KeyFile, keyFile, 0o600); err != nil {
		return err
	}
	tok, err := sd.s.Svc.CreateToken(ctx, "custody", h.name, api.ScopeBackup, time.Hour)
	if err != nil {
		return err
	}
	opt := app.InitOptions{ConfigPath: cfgPath, Name: "custody", Address: sd.s.AgentAddr, Token: tok, Host: h.name}
	if h.name == "bosch" {
		opt.EscrowPublic = sd.s.State.EscrowPub
	}
	if _, err := app.Init(ctx, opt); err != nil {
		return err
	}
	c2, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	a, err := app.Load(ctx, c2, h.inc, sd.log, nil)
	if err != nil {
		return err
	}
	a.Eng.Now = sd.clock.Now
	h.app = a
	return nil
}

// ---- the simulated timeline -------------------------------------------------------------------------

func (sd *seeder) day(t time.Time) int { return int(t.Sub(sd.start) / (24 * time.Hour)) }

func (sd *seeder) simulate() error {
	ctx := sd.ctx
	day0 := sd.t0.Truncate(24 * time.Hour) // midnight (UTC) of "today"
	at := func(daysBefore int, hm time.Duration) time.Time {
		return day0.Add(-time.Duration(daysBefore)*24*time.Hour + hm)
	}
	// ci-runner's disk breaks before the last "01:30" run: every retry fails until now
	brokenSince := at(0, 90*time.Minute)
	if brokenSince.Add(time.Hour).After(sd.t0) {
		brokenSince = brokenSince.Add(-24 * time.Hour)
	}
	excludeAt := sd.t0.Add(-72 * time.Hour).Truncate(time.Hour) // web-01 excluded "temporarily" three days ago
	excluded := false
	transient := at(min(9, sd.p.Days-1), 3*time.Hour)
	hourlyFrom := sd.t0.Add(-time.Duration(sd.p.HourlyHours) * time.Hour).Truncate(time.Hour)
	syncFrom := at(sd.p.Days-1, 5*time.Hour)
	outageFrom := at(sd.p.Days/2, 5*time.Hour)
	archiveBreak := sd.t0.Add(-3 * time.Hour)
	protectAt := at(max(4, sd.p.Days*2/3), 3*time.Hour)
	// the corrupted chunk's data lives only in the snapshot of the night between these two writes
	rotWrite, rotOverwrite := at(3, 22*time.Hour), at(2, 22*time.Hour)
	lastDay := -1
	broken := false
	var syncSetUp, outage, outageOver, archiveBroken, hourly, protected bool
	steps := 0
	var hostDur, maintDur time.Duration
	defer func() {
		sd.s.progress("seed: %d steps, hosts %s, maintenance %s (scheduler %s, waiting for sync %s)", steps, hostDur.Round(time.Millisecond),
			maintDur.Round(time.Millisecond), sd.runnerDur.Round(time.Millisecond), sd.syncWait.Round(time.Millisecond))
	}()
	for t := sd.start; !t.After(sd.t0); t = sd.nextStep(t, hourlyFrom) {
		sd.clock.Set(t)
		d := sd.day(t)
		if d != lastDay {
			lastDay = d
			if d > 0 && d%5 == 0 {
				sd.s.progress("seed: day %d/%d (%s)", d, sd.p.Days, t.Format("2006-01-02"))
			}
		}
		// guest activity in the evening, before the 03:00 nightly
		if t.Hour() == 22 {
			if err := sd.dailyChurn(d, !t.Add(nightBackupsDone).After(sd.t0)); err != nil {
				return err
			}
			if t.Equal(rotWrite) {
				// a scratch area of db-01 is filled once and overwritten the next evening
				sd.hosts["bosch"].inc.GuestWrite("prod", "db-01", "root", sd.rotOffset(), randBytes(1<<20, 777))
			}
			if t.Equal(rotOverwrite) {
				sd.hosts["bosch"].inc.GuestWrite("prod", "db-01", "root", sd.rotOffset(), randBytes(1<<20, 778))
			}
		}
		// redis rewrites its dump file every hour
		if err := os.WriteFile(filepath.Join(sd.trees["bosch/prod/instance/redis/root"], "var/lib/redis-dump.rdb"), textBytes(int(sd.mib)/max(1, sd.p.DataDiv), t.Unix()), 0o644); err != nil {
			return err
		}
		// a transient NBD failure one night (retried successfully an hour later)
		if t.Equal(transient) {
			sd.hosts["bosch"].inc.setBroken("prod", "api-01", errors.New("qemu-nbd: Failed to get shared \"write\" lock: is another process using the image?"))
		} else if t.Equal(transient.Add(time.Hour)) {
			sd.hosts["bosch"].inc.setBroken("prod", "api-01", nil)
		}
		if !broken && !t.Before(brokenSince) {
			broken = true
			sd.hosts["bosch"].inc.setBroken("staging", "ci-runner", errCIRunner)
		}
		if !excluded && !t.Before(excludeAt) {
			excluded = true
			sd.hosts["bosch"].inc.SetConfig("prod", "web-01", engine.KeyExclude, "true")
		}
		if !hourly && !t.Before(hourlyFrom) {
			hourly = true
			if err := sd.s.Svc.Assign(ctx, "custody", "bosch", "prod", "redis", "hourly"); err != nil {
				return err
			}
			if err := sd.s.Svc.SetObjectRetentionByGroup(ctx, "custody", sd.mustGroup("prod", "filesystem", "instance/redis/root"), "hourly"); err != nil {
				return fmt.Errorf("retention of redis: %w", err)
			}
		}
		if !syncSetUp && !t.Before(syncFrom) {
			syncSetUp = true
			if err := sd.setupSync(); err != nil {
				return err
			}
		}
		if syncSetUp && !outage && !t.Before(outageFrom) {
			outage = true
			if err := sd.s.Svc.SetHostDisabled(ctx, "offsite", "sync-custody-offsite", api.ScopeSyncWrite, true); err != nil {
				return err
			}
		}
		if outage && !outageOver && !t.Before(outageFrom.Add(3*time.Hour)) {
			outageOver = true
			if err := sd.s.Svc.SetHostDisabled(ctx, "offsite", "sync-custody-offsite", api.ScopeSyncWrite, false); err != nil {
				return err
			}
		}
		if syncSetUp && !archiveBroken && !t.Before(archiveBreak) {
			archiveBroken = true
			if err := sd.s.Svc.SetHostDisabled(ctx, "offsite", "sync-custody-offsite-archive", api.ScopeSyncWrite, true); err != nil {
				return err
			}
			// an operator presses "run now" on the archive job: it fails (its peer identity is revoked)
			if _, err := sd.s.Sync.Run(ctx, "custody", "offsite-archive"); err == nil {
				return errors.New("the archive sync with a revoked peer identity succeeded")
			}
		}

		// the hosts' schedulers, concurrently like real hosts
		tk := time.Now()
		if err := sd.tickHosts(t); err != nil {
			return err
		}
		if !protected && t.Equal(protectAt) {
			protected = true
			if err := sd.protect(); err != nil {
				return err
			}
		}
		tm := time.Now()
		hostDur += tm.Sub(tk)
		if os.Getenv("IBDEMO_TRACE") != "" && tm.Sub(tk) > 300*time.Millisecond {
			sd.s.progress("trace: hosts at %s took %s", t.Format("01-02 15:04"), tm.Sub(tk).Round(time.Millisecond))
		}
		// the server's maintenance scheduler (capacity, verify, retention, GC, reconcile, scrub, sync)
		if err := sd.maintTick(); err != nil {
			return err
		}
		maintDur += time.Since(tm)
		if os.Getenv("IBDEMO_TRACE") != "" && time.Since(tm) > 300*time.Millisecond {
			sd.s.progress("trace: maintenance at %s took %s", t.Format("01-02 15:04"), time.Since(tm).Round(time.Millisecond))
		}
		steps++
	}
	// the very last moment: exactly now
	sd.clock.Set(sd.t0)
	return sd.tickHosts(sd.t0)
}

// tickHosts runs one scheduler tick of every host that is still reporting, in parallel.
func (sd *seeder) tickHosts(t time.Time) error {
	var wg sync.WaitGroup
	errs := make([]error, len(sd.order))
	for i, h := range sd.order {
		sh := sd.hosts[h]
		if t.After(sh.until) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sh.app.Tick(sd.ctx); err != nil {
				errs[i] = fmt.Errorf("%s tick at %s: %w", sh.name, t, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// nextStep is the next simulated moment: every hour in the night window (backups 01:30-03:00,
// retention 04:00, GC 05:00, sync 06:00), the evening churn at 22:00 and the last hours before now
// (hourly object, retries); a few daytime hours otherwise (capacity samples, liveness).
func (sd *seeder) nextStep(t time.Time, hourlyFrom time.Time) time.Time {
	n := t.Add(time.Hour)
	for !sd.wanted(n, hourlyFrom) {
		n = n.Add(time.Hour)
	}
	return n
}

func (sd *seeder) wanted(t, hourlyFrom time.Time) bool {
	if !t.Before(hourlyFrom.Add(-time.Hour)) || t.After(sd.t0) {
		return true
	}
	switch h := t.Hour(); {
	case h <= 8, h == 12, h == 17, h == 22:
		return true
	}
	return false
}

func (sd *seeder) rotOffset() int { return rotOffset(sd.p) }

// nightBackupsDone is how long after the 22:00 churn the night's backups (01:30-03:00) are finished.
const nightBackupsDone = 5 * time.Hour

// dailyChurn is one day of guest writes: a rewritten text region and a growing log per VM, edited
// files in the volumes and containers. The files are only edited when a night's backup follows before
// "now": a demo started at 22:47 must not hold edits that no snapshot contains (the volume tree is
// compared with its latest snapshot).
func (sd *seeder) dailyChurn(d int, editFiles bool) error {
	for _, h := range sd.order {
		vmChurn(sd.hosts[h].inc, h, sd.p, d)
	}
	if !editFiles {
		return nil
	}
	sd.lastTreeChurn = sd.clock.Now()
	for k, dir := range sd.trees {
		n := 3
		if k == "benfica/default/volume/nextcloud-data" {
			n = 15
		}
		if err := MutateTree(dir, d, int64(len(k)), n); err != nil {
			return err
		}
	}
	return nil
}

// setupSync creates the two push jobs of custody into offsite through the real enrolment flow.
func (sd *seeder) setupSync() error {
	ctx, s := sd.ctx, sd.s
	groups := []string{
		sd.facts.Groups["bosch/prod/instance/db-01/root"], sd.facts.Groups["bosch/prod/instance/api-01/root"],
		sd.facts.Groups["benfica/default/volume/nextcloud-data"],
	}
	jobs := []struct {
		job syncer.Job
	}{
		{syncer.Job{Name: "offsite", Direction: "push", Remote: s.AgentAddr, Schedule: "06:00", GroupFilter: groups, VerifiedOnly: true}},
		{syncer.Job{Name: "offsite-archive", Direction: "push", Remote: s.AgentAddr, Schedule: "02:00", TransferLast: 2,
			GroupFilter: []string{sd.facts.Groups["benfica/customer-acme/instance/erp/root"]}}},
	}
	for _, j := range jobs {
		tok, err := s.Svc.CreateToken(ctx, "offsite", "sync-custody-"+j.job.Name, api.ScopeSyncWrite, time.Hour)
		if err != nil {
			return err
		}
		if err := s.Sync.AddJob(ctx, "custody", j.job, tok); err != nil {
			return fmt.Errorf("sync job %s: %w", j.job.Name, err)
		}
	}
	return nil
}

// maintTick runs the real maintenance scheduler once and waits for the sync jobs it started.
func (sd *seeder) maintTick() error {
	ctx := sd.ctx
	due, err := sd.s.Sync.Due(ctx, "custody")
	if err != nil {
		return err
	}
	var before int
	p := sd.s.DB.Pool()
	if err := p.QueryRow(ctx, `SELECT count(*) FROM sync_runs WHERE finished_at IS NOT NULL`).Scan(&before); err != nil {
		return err
	}
	tr := time.Now()
	sd.runner.Tick(ctx)
	sd.runnerDur += time.Since(tr)
	if len(due) == 0 {
		return nil
	}
	defer func(t time.Time) { sd.syncWait += time.Since(t) }(time.Now())
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var n int
		if err := p.QueryRow(ctx, `SELECT count(*) FROM sync_runs WHERE finished_at IS NOT NULL`).Scan(&n); err != nil {
			return err
		}
		if n >= before+len(due) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sync jobs started at %s did not finish", sd.clock.Now())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// protect marks the db-01 snapshot taken this night protected and annotates it.
func (sd *seeder) protect() error {
	ctx := sd.ctx
	g := sd.facts.Groups["bosch/prod/instance/db-01/root"]
	l, err := sd.s.Svc.AdminSnapshots(ctx, "custody", []string{g})
	if err != nil || len(l) == 0 {
		return fmt.Errorf("no db-01 snapshot to protect: %v", err)
	}
	sort.Slice(l, func(i, j int) bool { return l[i].SnapshotID < l[j].SnapshotID })
	snap := l[len(l)-1].SnapshotID
	note := "Before the PostgreSQL 16 → 17 upgrade (change CHG-2291). Keep until the migration is signed off."
	if err := sd.s.Svc.SetSnapshotProtected(ctx, "custody", g, snap, true); err != nil {
		return err
	}
	if err := sd.s.Svc.SetSnapshotNotes(ctx, "custody", g, snap, note); err != nil {
		return err
	}
	if err := sd.s.Svc.SetGroupNotes(ctx, "custody", g, "Primary PostgreSQL of the shop. Restore drill monthly; owner: platform team."); err != nil {
		return err
	}
	sd.facts.Protected = SnapRef{Repository: "custody", Object: "bosch/prod/instance/db-01/root", Group: g, Snapshot: snap, Note: note}
	return nil
}

// ---- the end: rot one chunk, verify, record facts ----------------------------------------------------

func (sd *seeder) finish() error {
	ctx, s := sd.ctx, sd.s
	if err := sd.corrupt(); err != nil {
		return fmt.Errorf("corrupting a chunk: %w", err)
	}
	// an operator runs a full verification now: it finds the rotten chunk, quarantines it and marks the
	// snapshot failed (the verify run itself is recorded as failed)
	if _, err := s.Svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "all"}); err == nil {
		return errors.New("the verification did not notice the corrupted chunk")
	}
	if err := s.Svc.SampleCapacity(ctx, "custody"); err != nil {
		return err
	}
	if err := s.Svc.SampleCapacity(ctx, "offsite"); err != nil {
		return err
	}
	// the browsable filesystem snapshot and one of its files
	g := sd.facts.Groups["benfica/default/volume/nextcloud-data"]
	l, err := s.Svc.AdminSnapshots(ctx, "custody", []string{g})
	if err != nil || len(l) == 0 {
		return fmt.Errorf("no snapshot of the volume: %v", err)
	}
	sort.Slice(l, func(i, j int) bool { return l[i].SnapshotID < l[j].SnapshotID })
	sd.facts.FSObject = SnapRef{Repository: "custody", Object: "benfica/default/volume/nextcloud-data", Group: g, Snapshot: l[len(l)-1].SnapshotID}
	// the e2e tests and the demo compare the tree on disk with the latest snapshot: an edit after it would
	// make them differ (the evening churn at 22:00 when "now" is later than that and no night has followed)
	if at, err := time.Parse("20060102150405", l[len(l)-1].SnapshotID[:14]); err != nil || sd.lastTreeChurn.After(at) {
		return fmt.Errorf("the volume tree was edited at %s, after its latest snapshot %s", sd.lastTreeChurn.Format(time.RFC3339), l[len(l)-1].SnapshotID)
	}
	var found string
	_ = filepath.Walk(sd.trees["benfica/default/volume/nextcloud-data"], func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && filepath.Base(p) == sd.facts.FSFile && found == "" {
			found = p
		}
		return nil
	})
	if found == "" {
		return errors.New("the unicode test file is missing from the tree")
	}
	b, err := os.ReadFile(found)
	if err != nil {
		return err
	}
	rel, _ := filepath.Rel(sd.trees["benfica/default/volume/nextcloud-data"], found)
	sum := sha256.Sum256(b)
	sd.facts.FSFile, sd.facts.FSFileSHA256 = filepath.ToSlash(rel), hex.EncodeToString(sum[:])
	for _, name := range []string{"offsite", "offsite-archive"} {
		st, err := s.Sync.Status(ctx, "custody", name)
		if err != nil {
			return err
		}
		sd.facts.SyncJobs[name] = st.LastRunStatus
	}
	_ = s.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM snapshots`).Scan(&sd.facts.Snapshots)
	_ = s.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM backup_runs`).Scan(&sd.facts.BackupRuns)
	return nil
}

// corrupt flips bytes in a chunk that only one (older, unprotected) db-01 snapshot references: the
// scratch area written on rotDay and overwritten the next evening.
func (sd *seeder) corrupt() error {
	ctx, s := sd.ctx, sd.s
	g := sd.facts.Groups["bosch/prod/instance/db-01/root"]
	repo, err := s.Svc.Repository(ctx, "custody")
	if err != nil {
		return err
	}
	l, err := s.Svc.AdminSnapshots(ctx, "custody", []string{g})
	if err != nil {
		return err
	}
	pos := sd.rotOffset() / format.FixedChunkSize
	refs := map[string][]string{}
	for _, sn := range l {
		ib, err := s.Svc.RepoIndex(ctx, repo, g, sn.SnapshotID)
		if err != nil {
			return err
		}
		idx, err := format.ParseIndex(ib)
		if err != nil {
			return err
		}
		if pos < len(idx.Entries) && !idx.Entries[pos].ID.IsZero() {
			id := idx.Entries[pos].ID.String()
			refs[id] = append(refs[id], sn.SnapshotID)
		}
	}
	for id, snaps := range refs {
		if len(snaps) != 1 || snaps[0] == sd.facts.Protected.Snapshot {
			continue
		}
		st, err := s.Svc.Store(repo)
		if err != nil {
			return err
		}
		key := "chunks/" + id[:2] + "/" + id[2:4] + "/" + id
		blob, err := st.Backend().Get(key)
		if err != nil {
			return err
		}
		blob[len(blob)/2] ^= 0xff // bit rot in the middle of the ciphertext
		if err := st.Backend().Put(key, blob, false); err != nil {
			return err
		}
		sd.facts.FailedVerify = SnapRef{Repository: "custody", Object: "bosch/prod/instance/db-01/root", Group: g, Snapshot: snaps[0]}
		sd.facts.CorruptChunk = id
		return nil
	}
	return fmt.Errorf("no chunk unique to one snapshot at position %d (refs %v)", pos, refs)
}
