package demo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
)

// jobHosts are the simulated hosts whose job executors run after seeding (uni-02 stays offline:
// an executor polls the server and would mark it seen).
var jobHosts = []string{"bosch", "benfica"}

// errCIRunner is the persistent failure of ci-runner's disk.
var errCIRunner = errors.New("NBD export of root failed: qemu: Could not open '/var/lib/incus/storage-pools/default/virtual-machines/staging_ci-runner/root.img': Input/output error")

// buildWorld creates the fake Incus of one host under hostsDir/<host>. With write the container roots
// and the custom volume's tree are generated; without it (a restart) the trees on disk are reused.
// VM disks live in memory only: they get their initial content here (replayDisks adds the history).
// It returns the live directories of the filesystem objects ("host/project/name" -> dir).
func buildWorld(hostsDir string, p Profile, host string, write bool) (*demoIncus, map[string]string, error) {
	tmp := filepath.Join(hostsDir, host, "incus")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, nil, err
	}
	inc := &demoIncus{Incus: fake.New(tmp), broken: map[string]error{}}
	inc.ShareFrozen = true // nothing writes to a disk while a backup reads it
	inc.ProjectList = hostProjects[host]
	mib := int64(1 << 20)
	for _, v := range vms {
		if v.host != host {
			continue
		}
		inc.AddVM(v.project, v.name, map[string]int{"root": int(v.diskGiB * p.DiskUnit)}, true)
		r, t := int64(v.randMiB)*mib/int64(p.DataDiv), int64(v.textMiB)*mib/int64(p.DataDiv)
		seed := int64(len(v.name)*1000 + int(v.diskGiB))
		inc.GuestWrite(v.project, v.name, "root", 0, randBytes(int(r), seed))
		inc.GuestWrite(v.project, v.name, "root", int(r), textBytes(int(t), seed+1))
	}
	if host == "bosch" {
		inc.SetConfig("staging", "ci-runner", engine.KeyPolicy, "ci")
	}
	trees := map[string]string{}
	for i, c := range containers {
		if c.host != host {
			continue
		}
		dir := filepath.Join(hostsDir, c.host, "rootfs", c.project, c.name)
		if write {
			if err := WriteContainerRoot(dir, c.name, p.ContainerFiles, int64(100+i)); err != nil {
				return nil, nil, err
			}
		} else if _, err := os.Stat(dir); err != nil {
			return nil, nil, fmt.Errorf("container root of %s is missing (start with --reset): %w", c.name, err)
		}
		inc.AddContainer(c.project, c.name, dir, true)
		trees[c.host+"/"+c.project+"/instance/"+c.name+"/root"] = dir
	}
	if host == "benfica" {
		vol := filepath.Join(hostsDir, "benfica", "volumes", "nextcloud-data")
		if write {
			if err := WriteTree(vol, TreeSpec{Files: p.TreeFiles, LargeMiB: p.TreeLargeMiB, SmallKiB: 6, Seed: 42, BinaryPct: 0.1}); err != nil {
				return nil, nil, err
			}
		} else if _, err := os.Stat(vol); err != nil {
			return nil, nil, fmt.Errorf("the nextcloud-data tree is missing (start with --reset): %w", err)
		}
		inc.AddFSVolume("default", "nextcloud-data", vol)
		inc.Vols["default/nextcloud-data"].Config[engine.KeyPolicy] = "nightly"
		inc.Vols["default/nextcloud-data"].Config["user.description"] = "Nextcloud user files"
		trees["benfica/default/volume/nextcloud-data"] = vol
	}
	return inc, trees, nil
}

// vmChurn is one evening of guest writes to the VMs of one host (day d of the seed).
func vmChurn(inc *demoIncus, host string, p Profile, d int) {
	mib := int64(1 << 20)
	for i, v := range vms {
		if v.host != host {
			continue
		}
		r, t := int64(v.randMiB)*mib/int64(p.DataDiv), int64(v.textMiB)*mib/int64(p.DataDiv)
		seed := int64(d*100 + i)
		// every other day: rewrite 256 KiB somewhere in the text region
		if (d+i)%2 == 0 {
			off := r + (seed*7919*int64(format.FixedChunkSize))%max(1, t-256<<10)
			inc.GuestWrite(v.project, v.name, "root", int(off), textBytes(256<<10, seed))
		}
		// append to the log area after the data
		logOff := r + t + int64(d)*(192<<10)
		inc.GuestWrite(v.project, v.name, "root", int(logOff), textBytes(192<<10, seed+5))
	}
}

// rotOffset is the scratch area of db-01 used for the corrupted chunk (far from any other write).
func rotOffset(p Profile) int {
	return int((8*p.DiskUnit)/2/int64(format.FixedChunkSize)) * format.FixedChunkSize
}

// replayDisks brings a rebuilt host's VM disks to roughly their state at the end of the seed (the same
// deterministic writes; the disks only lived in memory) and re-applies the end state of the world:
// ci-runner's broken disk, web-01's exclusion.
func replayDisks(inc *demoIncus, host string, p Profile) {
	for d := 0; d < p.Days; d++ {
		vmChurn(inc, host, p, d)
	}
	if host == "bosch" {
		inc.GuestWrite("prod", "db-01", "root", rotOffset(p), randBytes(1<<20, 778))
		inc.SetConfig("prod", "web-01", engine.KeyExclude, "true")
		inc.setBroken("staging", "ci-runner", errCIRunner)
	}
}

// loadJobHosts rebuilds the job hosts of a seeded directory from their state (restart).
func (s *Stack) loadJobHosts(ctx context.Context) ([]*app.App, error) {
	p := s.Opt.Profile
	if s.State.ProfileSpec != nil {
		p = *s.State.ProfileSpec
	}
	hostsDir := filepath.Join(s.Opt.Dir, "hosts")
	var out []*app.App
	for _, h := range jobHosts {
		inc, _, err := buildWorld(hostsDir, p, h, false)
		if err != nil {
			return nil, err
		}
		replayDisks(inc, h, p)
		cfg, err := config.Load(filepath.Join(hostsDir, h, "client.yaml"))
		if err != nil {
			return nil, err
		}
		allowAllJobs(cfg)
		a, err := app.Load(ctx, cfg, inc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		if err != nil {
			return nil, fmt.Errorf("loading host %s: %w", h, err)
		}
		a.Eng.Now = s.Clock.Now
		out = append(out, a)
	}
	return out, nil
}

// allowAllJobs lets a demo host run every job type, including the ones a real host must opt into
// (restores into Incus, file deliveries): the UI needs all of them.
func allowAllJobs(cfg *config.Config) {
	cfg.Jobs.Disabled = false
	cfg.Jobs.Allow = append([]string(nil), jobspec.Types...)
}

// startRuntime starts what the real daemon runs next to the APIs: the notification engine and its
// bridges to the event stream, the job bridges, the job-lease housekeeping and the hosts' executors.
func (s *Stack) startRuntime(ctx context.Context, hosts []*app.App) {
	ne := mgmt.NewNotifyEngine(s.Svc)
	s.Mgmt.Notify = ne
	s.wg.Add(1)
	go func() { defer s.wg.Done(); ne.Run(ctx, time.Minute) }()
	s.Mgmt.BridgeNotify(ctx)
	s.Mgmt.StartBridges(ctx)
	if !s.Opt.Maintenance { // the maintenance runner calls Maintain itself
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := jobs.For(s.Svc).Maintain(ctx); err != nil {
						s.Opt.Log.Warn("job housekeeping failed", "err", err)
					}
				}
			}
		}()
	}
	for _, a := range hosts {
		a.StartJobs(ctx)
		s.JobHosts = append(s.JobHosts, a.Cfg.Host)
	}
}
