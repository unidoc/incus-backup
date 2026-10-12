package app

import (
	"context"
	"fmt"
	"github.com/unidoc/incus-backup/internal/remote"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
)

// Check is one doctor finding.
type Check struct {
	Name   string
	Status string // ok, warn, fail, info
	Detail string
}

func (c Check) String() string {
	return fmt.Sprintf("%-5s %-28s %s", strings.ToUpper(c.Status), c.Name, c.Detail)
}

// Doctor inspects everything a backup run depends on and explains what is wrong.
func (a *App) Doctor(ctx context.Context) []Check {
	var out []Check
	add := func(name, status, f string, args ...any) {
		out = append(out, Check{name, status, fmt.Sprintf(f, args...)})
	}

	if err := a.Cfg.Validate(); err != nil {
		add("configuration", "fail", "%v", err)
	} else {
		add("configuration", "ok", "host %s, %d server(s), control server %s", a.Cfg.Host, len(a.Cfg.Servers), a.Cfg.ControlServer())
	}

	info, err := a.Eng.Inc.Info(ctx)
	if err != nil {
		add("incus", "fail", "cannot reach Incus: %v", err)
	} else {
		have := map[string]bool{}
		for _, x := range info.Extensions {
			have[x] = true
		}
		var missing []string
		for _, need := range incusx.RequiredExtensions {
			if !have[need] {
				missing = append(missing, need)
			}
		}
		if len(missing) > 0 {
			add("incus", "fail", "Incus %s lacks the API extensions %s (incus-backup needs Incus 7.5.1 or newer with NBD and bitmaps)", info.Version, strings.Join(missing, ", "))
		} else {
			add("incus", "ok", "Incus %s with NBD and dirty-bitmap support", info.Version)
		}
		if info.Clustered {
			add("incus cluster", "warn", "clustered Incus: run incus-backup on every member; shared storage objects must be tagged on one member only")
		}
	}

	for _, name := range a.Cfg.ServerNames() {
		t := a.Eng.Targets[name]
		who, err := t.Client.Whoami(ctx)
		if err != nil {
			add("server "+name, "fail", "%s: %v", a.Cfg.Servers[name].Address, err)
			continue
		}
		switch {
		case who.Scope != "backup":
			add("server "+name, "fail", "enrolled with scope %q (needs backup)", who.Scope)
		case who.DomainID != "" && who.DomainID != t.Keys.DomainID:
			add("server "+name, "fail", "the repository %s uses key domain %s, this host's key is %s", who.Repository, who.DomainID, t.Keys.DomainID)
		default:
			skew := time.Since(who.ServerTime)
			if skew < 0 {
				skew = -skew
			}
			st := "ok"
			note := ""
			if skew > time.Minute {
				st, note = "warn", fmt.Sprintf(" (clock differs by %s: certificates and schedules need correct time)", skew.Round(time.Second))
			}
			add("server "+name, st, "repository %s, server %s%s", who.Repository, who.Version, note)
		}
	}
	// client certificates: expiry and the state of the automatic renewal
	out = append(out, CertChecks(a.Cfg, a.Eng.Now())...)
	// scratch objects of an interrupted guest-file recovery
	if projects, err := a.Eng.Inc.Projects(ctx); err == nil {
		var left []string
		for _, pr := range projects {
			if insts, err := a.Eng.Inc.Instances(ctx, pr); err == nil {
				for _, i := range insts {
					if strings.HasPrefix(i.Name, "ibk-rec-") {
						left = append(left, "instance "+pr+"/"+i.Name)
					}
				}
			}
			if vols, err := a.Eng.Inc.Volumes(ctx, pr); err == nil {
				for _, v := range vols {
					if strings.HasPrefix(v.Name, "ibk-rec-") {
						left = append(left, "volume "+pr+"/"+v.Name)
					}
				}
			}
		}
		if len(left) > 0 {
			add("recovery leftovers", "warn", "a guest-file recovery was interrupted; remove by hand: %s", strings.Join(left, ", "))
		}
	}
	// offline escrow of the key
	for _, name := range a.Cfg.ServerNames() {
		if _, err := a.Eng.Targets[name].Client.GetEscrow(ctx); err != nil {
			if remote.IsStatus(err, 404) {
				add("key escrow "+name, "warn", "no escrow blob stored: the key can be recovered only from the recovery key (incus-backup key escrow upload --public IBP1-...)")
			}
		} else {
			add("key escrow "+name, "ok", "a copy of the key wrapped to your offline escrow key is stored")
		}
	}
	for n, e := range a.Unreachable {
		add("desired state", "warn", "control server %s unreachable: %v (local policies only)", n, e)
	}

	if err := os.MkdirAll(a.Cfg.SpoolDir, 0o700); err != nil {
		add("spool", "fail", "%s: %v", a.Cfg.SpoolDir, err)
	} else {
		var st syscall.Statfs_t
		if syscall.Statfs(a.Cfg.SpoolDir, &st) == nil {
			free := int64(st.Bavail) * int64(st.Bsize)
			status := "ok"
			if free < 5<<30 {
				status = "warn"
			}
			add("spool", status, "%s has %.1f GiB free (captured chunks wait here between the freeze and the upload)", a.Cfg.SpoolDir, float64(free)/(1<<30))
		}
	}

	objs, err := a.Plan(ctx)
	if err != nil {
		add("discovery", "fail", "%v", err)
		return out
	}
	var sel, skipped int
	reasons := map[string]int{}
	for _, o := range objs {
		if o.Skip != "" {
			skipped++
			reasons[o.Skip]++
			if strings.HasPrefix(o.Skip, "unknown policy") {
				add("policy", "fail", "%s: %s", o.Project+"/"+o.Name, o.Skip)
			}
			continue
		}
		sel++
		if o.Kind == engine.KindBlock && len(o.SharedBy) > 1 {
			add("shared volume", "warn", "%s is shared by %s: no per-VM bitmap can see all writers, it is read in full every run (deduplicated) and is only crash-consistent across the guests", o.Name, strings.Join(o.SharedBy, ", "))
		}
		if o.Kind == engine.KindBlock && o.Instance == nil && len(o.Attached) > 0 && len(o.SharedBy) <= 1 {
			add("bitmaps", "info", "%s is a raw custom volume: only in-memory bitmaps are possible, so it is read in full (deduplicated) after every VM restart", o.Name)
		}
		if o.Kind == engine.KindBlock && o.Instance != nil && !o.Instance.Running {
			add("stopped VM", "info", "%s is stopped: read in full through the volume endpoint (no bitmaps exist while stopped)", o.Instance.Name)
		}
	}
	add("objects", "ok", "%d storage object(s) selected, %d skipped", sel, skipped)
	var rs []string
	for r, n := range reasons {
		rs = append(rs, fmt.Sprintf("%d x %s", n, r))
	}
	sort.Strings(rs)
	for _, r := range rs {
		add("skipped", "info", "%s", r)
	}
	// stale generations left behind by failed runs
	for _, o := range objs {
		if o.Skip != "" || o.Kind != engine.KindBlock || o.Instance == nil || !o.Instance.Running {
			continue
		}
		bms, err := a.Eng.Inc.Bitmaps(ctx, o.Ref)
		if err != nil {
			continue
		}
		n := 0
		for _, b := range bms {
			if strings.HasPrefix(b.Name, engine.BitmapPrefix) {
				n++
			}
		}
		if n > 2 {
			add("bitmaps", "warn", "%s carries %d incus-backup generations (a target has been unreachable for a while); each recording bitmap costs some write overhead", o.Name, n)
		}
	}
	return out
}

// Failed reports whether any check failed.
func Failed(cs []Check) bool {
	for _, c := range cs {
		if c.Status == "fail" {
			return true
		}
	}
	return false
}
