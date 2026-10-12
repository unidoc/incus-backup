package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0) }

func human(n int64) string {
	const u = 1024.0
	f := float64(n)
	for _, s := range []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"} {
		if f < u || s == "PiB" {
			if s == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.2f %s", f, s)
		}
		f /= u
	}
	return ""
}

func (a *app) cmdPolicy() *cobra.Command {
	root := &cobra.Command{Use: "policy", Short: "Policies, retention and assignments"}
	var p api.Policy
	var targets string
	set := &cobra.Command{Use: "set NAME", Short: "Create or update a policy", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			p.Name = args[0]
			if targets != "" {
				p.Targets = strings.Split(targets, ",")
			}
			if err := a.svc.SetPolicy(ctx, repo, p); err != nil {
				return err
			}
			fmt.Printf("Policy %s saved.\n", p.Name)
			return nil
		})}
	f := set.Flags()
	f.StringVar(&p.Schedule, "schedule", "03:00", `when clients back up ("03:00", "every 4h", "hourly", "daily", "weekly")`)
	f.StringVar(&targets, "targets", "", "comma separated target server names (as named in the client configuration)")
	f.StringVar(&p.CommitRequired, "commit-required", "all", "all | any: how many targets must commit before a generation advances")
	f.IntVar(&p.KeepLast, "keep-last", 0, "keep the N newest snapshots")
	f.IntVar(&p.KeepHourly, "keep-hourly", 0, "keep one per hour for N hours")
	f.IntVar(&p.KeepDaily, "keep-daily", 0, "keep one per day for N days")
	f.IntVar(&p.KeepWeekly, "keep-weekly", 0, "keep one per week for N weeks")
	f.IntVar(&p.KeepMonthly, "keep-monthly", 0, "keep one per month for N months")
	f.IntVar(&p.KeepYearly, "keep-yearly", 0, "keep one per year for N years")
	f.StringVar(&p.Timezone, "timezone", "UTC", "IANA time zone in which hourly/daily/weekly/monthly/yearly periods are cut (e.g. Atlantic/Reykjavik)")
	f.IntVar(&p.ReverifyDays, "reverify-days", 30, "re-verify snapshots whose last verification is older than N days (0 = only new snapshots)")
	f.StringVar(&p.VerifySchedule, "verify", "weekly", "how often the client verifies and decrypts samples")
	f.StringVar(&p.RestoreTestSchedule, "restore-test", "monthly", "how often the client restore-tests")

	list := &cobra.Command{Use: "list", Short: "List policies", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			ps, err := a.svc.Policies(ctx, repo)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "NAME\tSCHEDULE\tTARGETS\tCOMMIT\tKEEP (last/h/d/w/m/y)\tTIME ZONE\tREVERIFY")
			for _, p := range ps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d/%d/%d/%d/%d\t%s\t%dd\n", p.Name, p.Schedule, strings.Join(p.Targets, ","), p.CommitRequired,
					p.KeepLast, p.KeepHourly, p.KeepDaily, p.KeepWeekly, p.KeepMonthly, p.KeepYearly, p.Timezone, p.ReverifyDays)
			}
			return tw.Flush()
		})}
	del := &cobra.Command{Use: "delete NAME", Short: "Delete a policy and its assignments", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			return a.svc.DeletePolicy(ctx, repo, args[0])
		})}
	var project, object string
	assign := &cobra.Command{Use: "assign HOST POLICY", Short: "Assign a policy to a host, project or object", Args: cobra.ExactArgs(2),
		Long: "Without --project the policy applies to the whole host; with --project to that project; with --object to one instance/volume.",
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			if err := a.svc.Assign(ctx, repo, args[0], project, object, args[1]); err != nil {
				return err
			}
			fmt.Println("Assigned.")
			return nil
		})}
	assign.Flags().StringVar(&project, "project", "", "Incus project")
	assign.Flags().StringVar(&object, "object", "", "instance or volume name (needs --project)")
	unassign := &cobra.Command{Use: "unassign HOST", Short: "Remove an assignment", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			return a.svc.Unassign(ctx, repo, args[0], project, object)
		})}
	unassign.Flags().StringVar(&project, "project", "", "Incus project")
	unassign.Flags().StringVar(&object, "object", "", "instance or volume name")
	assignments := &cobra.Command{Use: "assignments", Short: "List assignments", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			as, err := a.svc.Assignments(ctx, repo)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "HOST\tPROJECT\tOBJECT\tPOLICY")
			for _, x := range as {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", x.Host, dash(x.Project), dash(x.Object), x.Policy)
			}
			return tw.Flush()
		})}
	root.AddCommand(set, list, del, assign, unassign, assignments)
	return root
}

func dash(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

func (a *app) cmdObject() *cobra.Command {
	root := &cobra.Command{Use: "object", Short: "Storage objects the repository knows"}
	var project string
	list := &cobra.Command{Use: "list", Short: "List objects with their pinned retention policy", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			rows, err := a.svc.ObjectRows(ctx, repo)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "PROJECT\tOBJECT\tKIND\tHOST\tDECLARED\tRETENTION\tSNAPSHOTS")
			for _, o := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", o.Project, o.Name, o.Kind, o.Host, dash(o.DeclaredPolicy), dash(o.RetentionPolicy), o.Snapshots)
			}
			return tw.Flush()
		})}
	retention := &cobra.Command{Use: "retention OBJECT POLICY", Short: "Pin the retention policy of an object (overrides what the host declared)", Args: cobra.ExactArgs(2),
		Long: "Retention is decided by the server: an assignment (policy assign) wins, otherwise the policy the object was first seen with. A backup-scope host can never change it. Use this command to pin one explicitly; an empty POLICY (\"\") keeps everything.",
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			return a.svc.SetObjectRetention(ctx, repo, project, args[0], args[1])
		})}
	retention.Flags().StringVar(&project, "project", "default", "Incus project")
	transfer := &cobra.Command{Use: "transfer PROJECT/NAME HOST", Short: "Hand an object to another backup host (e.g. after an instance migrated)", Args: cobra.ExactArgs(2),
		Long: "Objects are owned by the host that first backed them up; another host is refused until an administrator transfers the object.",
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			g, err := a.resolveObject(ctx, repo, args[0])
			if err != nil {
				return err
			}
			return a.svc.TransferObject(ctx, repo, g, args[1])
		})}
	root.AddCommand(list, retention, a.cmdObjectNotes(), transfer, a.cmdObjectDelete())
	return root
}

func (a *app) cmdStatus() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Health overview of the repository", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			st, err := a.svc.Status(ctx, repo)
			if err != nil {
				return err
			}
			fmt.Printf("Repository %s: %d snapshots of %d objects, %d session(s) open\n", st.Repository, st.Snapshots, st.Objects, st.OpenSessions)
			fmt.Printf("Last 24h: %d ok, %d failed/partial runs\n", st.RunsOK24h, st.RunsFail24h)
			fmt.Printf("Last verify: %s   Last GC: %s\n\n", ts(st.LastVerify), ts(st.LastGC))
			tw := table()
			fmt.Fprintln(tw, "HOST\tLAST SEEN\tOBJECTS\tPROTECTED\tLAST RUN")
			for _, h := range st.Hosts {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d/%d\t%s %s\n", h.Name, ts(h.LastSeen), h.Objects, h.Protected, h.Objects, ts(h.LastRunAt), h.LastRunOK)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, h := range st.Hosts {
				for _, u := range h.Unprotected {
					fmt.Printf("WARNING: %s: %s has no backup yet\n", h.Name, u)
				}
			}
			return nil
		})}
}

func ts(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (a *app) cmdCapacity() *cobra.Command {
	return &cobra.Command{Use: "capacity", Short: "Storage usage, growth and forecast", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			if err := a.svc.SampleCapacity(ctx, repo); err != nil {
				return err
			}
			c, err := a.svc.Capacity(ctx, repo)
			if err != nil {
				return err
			}
			fmt.Printf("Physical:  %s in %d chunks (%d snapshots, logical %s, dedup %.1fx)\n", human(c.PhysicalBytes), c.Chunks, c.Snapshots, human(c.LogicalBytes), c.DedupRatio)
			fmt.Printf("Filesystem: %s free of %s\n", human(c.FSFree), human(c.FSTotal))
			for _, w := range []int{7, 30, 90} {
				g := c.GrowthPerDay[w]
				if g != g {
					fmt.Printf("Growth %2dd: not enough data\n", w)
				} else {
					fmt.Printf("Growth %2dd: %s/day\n", w, human(int64(g)))
				}
			}
			if c.Projected80 != nil {
				fmt.Printf("Projected 80%% full: %s\n", c.Projected80.Format("2006-01-02"))
			}
			if c.ProjectedFull != nil {
				fmt.Printf("Projected full:     %s\n", c.ProjectedFull.Format("2006-01-02"))
			}
			return nil
		})}
}

func (a *app) cmdPrune() *cobra.Command {
	var dry bool
	c := &cobra.Command{Use: "prune", Short: "Apply retention policies now (--dry-run previews exactly what would be kept and removed)", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			r, err := a.doPrune(ctx, repo, dry)
			if err != nil {
				return err
			}
			if dry {
				tw := table()
				fmt.Fprintln(tw, "OBJECT\tSNAPSHOT\tDECISION\tWHY\tPOLICY (TIME ZONE)")
				for _, p := range r.Plan {
					d := "remove"
					if p.Keep {
						d = "keep"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s (%s)\n", p.Object, p.Snapshot, d, dash(p.Reason), dash(p.Policy), p.Timezone)
				}
				_ = tw.Flush()
			} else {
				for _, p := range r.Plan {
					if !p.Keep {
						fmt.Printf("removed %s/%s\n", p.Group, p.Snapshot)
					}
				}
			}
			fmt.Printf("%d group(s): kept %d (%d protected), removed %d%s\n", r.Groups, r.Kept, r.Protected, r.Removed, map[bool]string{true: " (dry run)"}[dry])
			return nil
		})}
	c.Flags().BoolVar(&dry, "dry-run", false, "show what would be removed")
	return c
}

func (a *app) cmdGC() *cobra.Command {
	var dry bool
	var grace time.Duration
	c := &cobra.Command{Use: "gc", Short: "Remove unreferenced chunks older than the grace period", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			if grace == 0 {
				ms, err := a.svc.Maintenance(ctx, repo)
				if err != nil {
					return err
				}
				grace = ms.GCGrace
			}
			st, err := a.doGC(ctx, repo, grace, dry)
			if err != nil {
				return err
			}
			fmt.Printf("scanned %d chunks: %d live, %d young kept, %d pinned by running backups, %d removed (%s reclaimed)%s\n",
				st.Scanned, st.Live, st.KeptYoung, st.KeptPinned, st.Removed, human(st.BytesReclaimed), map[bool]string{true: " (dry run)"}[dry])
			return nil
		})}
	c.Flags().BoolVar(&dry, "dry-run", false, "only count what would be removed")
	c.Flags().DurationVar(&grace, "grace", 0, "minimum age of an unreferenced chunk (default: the repository's maintenance setting)")
	return c
}

func (a *app) cmdVerify() *cobra.Command {
	var req service.VerifyRequest
	c := &cobra.Command{Use: "verify", Short: "Check chunks and snapshots (no key needed); corrupt chunks are quarantined and healed by the next backup", Args: cobra.NoArgs,
		Long: `Modes: all (default) reads every chunk object and every snapshot; new only snapshots never verified;
stale new ones plus those whose last verification is older than --stale-days (0 = each policy's reverify_days).
Within one run every chunk is read at most once. The verification state of every snapshot is recorded.`,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			vs, err := a.doVerify(ctx, repo, req)
			if vs != nil {
				fmt.Printf("checked %d snapshots (%d skipped), read %d chunks, quarantined %d\n", vs.Snapshots, vs.Skipped, vs.Chunks, vs.Quarantined)
				for _, p := range vs.Problems {
					fmt.Printf("PROBLEM %s %s: %s\n", p.Kind, p.Object, p.Detail)
				}
			}
			return err
		})}
	c.Flags().StringVar(&req.Mode, "mode", "all", "all | new | stale")
	c.Flags().IntVar(&req.StaleDays, "stale-days", 0, "with --mode stale: re-verify snapshots older than N days (0 = per policy)")
	c.Flags().BoolVar(&req.NoQuarantine, "no-quarantine", false, "report corrupt chunks without moving them aside")
	return c
}

func (a *app) cmdReconcile() *cobra.Command {
	return &cobra.Command{Use: "reconcile", Short: "Make PostgreSQL agree with the datastore (also the rebuild path)", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			r, err := a.svc.Reconcile(ctx, repo)
			if err != nil {
				return err
			}
			fmt.Printf("recorded %d snapshot(s), removed %d stale row(s)\n", r.Inserted, r.Removed)
			return nil
		})}
}

func (a *app) cmdSync() *cobra.Command {
	root := &cobra.Command{Use: "sync", Short: "Server-to-server replication (push and pull)"}
	var remote, direction, sched, token string
	var groups []string
	var verifiedOnly, resyncCorrupt bool
	var cmdFlags *pflag.FlagSet
	var transferLast int
	var rate string
	add := &cobra.Command{Use: "add NAME", Short: "Add a sync job (enrols this server at the peer)", Args: cobra.ExactArgs(1),
		Long: `Add a replication job. The peer gives you a one-time token:
  push: the peer creates it with --scope sync-write ("incus-backup-server token create NAME --scope sync-write")
  pull: the peer creates it with --scope sync-read
This server is always the initiator of its own job.`,
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			if remote == "" || token == "" {
				return fmt.Errorf("--remote and --token are required")
			}
			bps, err := parseRate(rate)
			if err != nil {
				return err
			}
			err = a.sync.AddJob(ctx, repo, syncer.Job{Name: args[0], Direction: direction, Remote: remote, Schedule: sched, GroupFilter: groups,
				VerifiedOnly: verifiedOnly, ResyncCorrupt: resyncCorrupt, TransferLast: transferLast, RateLimitBps: bps}, token)
			if err != nil {
				return err
			}
			fmt.Printf("Sync job %s added (%s %s, schedule %q).\n", args[0], direction, remote, sched)
			return nil
		})}
	add.Flags().StringVar(&remote, "remote", "", "peer address host:port")
	add.Flags().StringVar(&direction, "direction", "push", "push | pull")
	add.Flags().StringVar(&sched, "schedule", "", `when to run ("02:00", "every 4h", ...; empty = manual)`)
	add.Flags().StringVar(&token, "token", "", "one-time enrolment token from the peer")
	add.Flags().StringSliceVar(&groups, "group", nil, "limit to these group ids (default all)")
	syncFlags := func(f *pflag.FlagSet) {
		f.BoolVar(&verifiedOnly, "verified-only", false, "only transfer snapshots whose last verification on the source is ok")
		f.BoolVar(&resyncCorrupt, "resync-corrupt", false, "repair destination snapshots that failed verification from the source")
		f.IntVar(&transferLast, "transfer-last", 0, "only the newest N snapshots of each group (0 = all)")
		f.StringVar(&rate, "rate-limit", "", `cap blob traffic, e.g. "20MB" or "500KiB" per second (empty = unlimited)`)
	}
	syncFlags(add.Flags())
	setc := &cobra.Command{Use: "set NAME", Short: "Change options of a sync job (only the flags you give)", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			var o syncer.JobOptions
			fl := cmdFlags
			if fl.Changed("verified-only") {
				o.VerifiedOnly = &verifiedOnly
			}
			if fl.Changed("resync-corrupt") {
				o.ResyncCorrupt = &resyncCorrupt
			}
			if fl.Changed("transfer-last") {
				o.TransferLast = &transferLast
			}
			if fl.Changed("rate-limit") {
				bps, err := parseRate(rate)
				if err != nil {
					return err
				}
				o.RateLimitBps = &bps
			}
			if fl.Changed("group") {
				o.Groups = &groups
			}
			return a.sync.SetOptions(ctx, repo, args[0], o)
		})}
	cmdFlags = setc.Flags()
	syncFlags(setc.Flags())
	setc.Flags().StringSliceVar(&groups, "group", nil, "limit to these group ids (default all)")
	list := &cobra.Command{Use: "list", Short: "List sync jobs", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			js, err := a.sync.Jobs(ctx, repo)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "NAME\tDIRECTION\tREMOTE\tSCHEDULE\tLAST SUCCESS")
			for _, j := range js {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", j.Name, j.Direction, j.Remote, j.Schedule, ts(j.LastSuccess))
			}
			return tw.Flush()
		})}
	run := &cobra.Command{Use: "run NAME", Short: "Run a sync job now", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			res, err := a.doSyncRun(ctx, repo, args[0])
			if res != nil {
				fmt.Printf("pending %d, synced %d, failed %d; %d chunks transferred (%s), %d reused; skipped %d unverified + %d corrupt, repaired %d\n",
					res.Pending, res.Synced, res.Failed, res.ChunksTransferred, human(res.BytesTransferred), res.ChunksReused,
					res.SkippedUnverified, res.SkippedCorrupt, res.Repaired)
				for _, e := range res.Errors {
					fmt.Println("FAILED", e)
				}
			}
			return err
		})}
	status := &cobra.Command{Use: "status NAME", Short: "Show a sync job's health and replication lag", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			s, err := a.sync.Status(ctx, repo, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s: %s %s, schedule %q\n", s.Name, s.Direction, s.Remote, s.Schedule)
			fmt.Printf("Last run:    %s (%s) %s\n", ts(s.LastRunAt), s.LastRunStatus, s.LastError)
			fmt.Printf("Last success:%s\n", " "+ts(s.LastSuccess))
			fmt.Printf("Last run moved %d snapshot(s): %d chunks (%s), %d reused\n", s.SnapshotsSynced, s.ChunksTransferred, human(s.BytesTransferred), s.ChunksReused)
			fmt.Printf("Pending: %d   Failed: %d   Skipped: %d   Repaired: %d   Replication lag: %s\n", s.SnapshotsPending, s.Failed, s.SnapshotsSkipped, s.SnapshotsRepaired, s.Lag.Round(time.Minute))
			fmt.Printf("Options: verified-only=%v resync-corrupt=%v transfer-last=%d rate-limit=%s/s\n", s.VerifiedOnly, s.ResyncCorrupt, s.TransferLast, human(s.RateLimitBps))
			return nil
		})}
	rm := &cobra.Command{Use: "remove NAME", Short: "Remove a sync job and its credentials", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			return a.sync.RemoveJob(ctx, repo, args[0])
		})}
	root.AddCommand(add, setc, list, run, status, rm)
	return root
}

// parseRate reads "20MB", "500KiB", "1048576" (bytes per second); "" = 0 = unlimited. Units are
// case-insensitive; KB/MB/GB are decimal, KiB/MiB/GiB binary; a bare number is bytes per second.
func parseRate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := rateRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid rate limit %q (examples: 20MB, 500KiB, 1048576)", s)
	}
	mult := map[string]float64{"": 1, "B": 1, "K": 1 << 10, "KIB": 1 << 10, "KB": 1000, "M": 1 << 20, "MIB": 1 << 20, "MB": 1000 * 1000, "G": 1 << 30, "GIB": 1 << 30, "GB": 1000 * 1000 * 1000}[strings.ToUpper(m[2])]
	if mult == 0 {
		return 0, fmt.Errorf("unknown unit in rate limit %q", s)
	}
	var n float64
	if _, err := fmt.Sscanf(m[1], "%g", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("a rate limit must be positive (leave it empty for unlimited)")
	}
	v := n * mult
	if v < 1 || v > 1e12 {
		return 0, fmt.Errorf("rate limit %q is out of range", s)
	}
	return int64(v), nil
}

var rateRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)$`)

func (a *app) cmdConfigExport() *cobra.Command {
	root := &cobra.Command{Use: "config", Short: "Configuration helpers"}
	exp := &cobra.Command{Use: "export", Short: "Print policies, assignments and sync jobs as YAML (backup of the control-plane settings)", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			ps, err := a.svc.Policies(ctx, repo)
			if err != nil {
				return err
			}
			as, err := a.svc.Assignments(ctx, repo)
			if err != nil {
				return err
			}
			js, err := a.sync.Jobs(ctx, repo)
			if err != nil {
				return err
			}
			ms, err := a.svc.Maintenance(ctx, repo)
			if err != nil {
				return err
			}
			rs, err := a.svc.Repositories(ctx)
			if err != nil {
				return err
			}
			creds, _ := a.svc.Credentials(ctx)
			type repoOut struct {
				Name, Backend, Location, Credential string
			}
			credName := map[string]string{}
			for _, c := range creds {
				credName[c.ID] = c.Name
			}
			var ro []repoOut
			for _, r := range rs {
				ro = append(ro, repoOut{r.Name, r.Backend, r.Path, credName[r.CredentialID]})
			}
			type job struct {
				Name, Direction, Remote, Schedule string
				VerifiedOnly, ResyncCorrupt       bool
				TransferLast                      int
				RateLimitBps                      int64
				Groups                            []string
			}
			out := map[string]any{"repository": repo, "repositories": ro, "policies": ps, "assignments": as,
				"maintenance": map[string]any{"capacity": ms.Capacity, "reconcile": ms.Reconcile, "prune": ms.Prune, "gc": ms.GC, "verify": ms.Verify,
					"scrub": ms.Scrub, "gc_grace": ms.GCGrace.String(), "default_reverify_days": ms.DefaultReverifyDays},
				"note": "credentials (secrets) are never exported; re-create them with `credential create` and attach repositories with `repo attach`"}
			var jj []job
			for _, j := range js {
				jj = append(jj, job{j.Name, j.Direction, j.Remote, j.Schedule, j.VerifiedOnly, j.ResyncCorrupt, j.TransferLast, j.RateLimitBps, j.GroupFilter})
			}
			out["sync_jobs"] = jj
			b, err := yaml.Marshal(out)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(b)
			return err
		})}
	root.AddCommand(exp)
	return root
}

func (a *app) cmdConfigExportAlias() {}
