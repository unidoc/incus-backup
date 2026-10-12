package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
)

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0) }

func human(n int64) string {
	f := float64(n)
	for _, s := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if f < 1024 || s == "TiB" {
			if s == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= 1024
	}
	return ""
}

func (c *cli) cmdInit() *cobra.Command {
	var o app.InitOptions
	var passFile, escrowPrivFile string
	var saved bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the encryption key (first time), enrol at a server and write the configuration",
		Long: `init does three things:

  1. creates the repository encryption key if this host has none, and shows you the
     RECOVERY KEY. Store it somewhere safe (a password manager) BEFORE continuing:
     without the key or the recovery key every backup is unreadable, for everyone.
  2. enrols this host at the server with the one-time token the server printed
  3. writes /etc/incus-backup/client.yaml

Run it again with another --server/--token to add a second, independent server
(it reuses the same key, so the same data is sent to both).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.ConfigPath = c.cfgPath
			if passFile != "" {
				b, err := os.ReadFile(passFile)
				if err != nil {
					return err
				}
				o.Passphrase = strings.TrimSpace(string(b))
			}
			if escrowPrivFile != "" {
				b, err := os.ReadFile(escrowPrivFile)
				if err != nil {
					return err
				}
				o.EscrowPrivate = strings.TrimSpace(string(b))
			}
			cfg, _ := configFor(c.cfgPath)
			o.ExistingPassphrase = c.passphrase(cfg)
			o.ConfirmRecovery = func(rec string) error {
				fmt.Fprintf(os.Stderr, "\n  RECOVERY KEY (shown only now):\n\n    %s\n\n", rec)
				fmt.Fprintln(os.Stderr, "  Anyone with this key can read your backups; without it (and without the key file) they cannot be read at all.")
				if saved {
					return nil
				}
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("not on a terminal: store the recovery key and re-run with --i-stored-the-recovery-key")
				}
				parts := strings.Split(rec, "-")
				want := parts[len(parts)-1]
				if got := prompt(fmt.Sprintf("  Type the last group (%d characters) to confirm you stored it: ", len(want))); !strings.EqualFold(got, want) {
					return fmt.Errorf("the confirmation does not match: nothing was written")
				}
				return nil
			}
			res, err := app.Init(cmd.Context(), o)
			if err != nil {
				return err
			}
			if res.NewKey {
				fmt.Printf("Key created (domain %s).\n", res.DomainID)
			}
			if o.EscrowPrivate != "" {
				fmt.Printf("Key recovered from the escrow blob on the server (domain %s).\n", res.DomainID)
			}
			if res.EscrowStored {
				fmt.Println("A copy of the key, wrapped to your offline escrow key, is stored on the server.")
			}
			if res.EscrowExists {
				fmt.Println("WARNING: the server already holds an escrow blob and kept it. Check that it is yours: `incus-backup key escrow fetch` + `key escrow unwrap`; an administrator can clear it (`incus-backup-server escrow clear`).")
			}
			fmt.Printf("Enrolled at %s as repository %q with scope %s.\nNext: incus-backup doctor\n", o.Address, res.Repo, res.Scope)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Address, "server", "", "server address host:port")
	f.StringVar(&o.Token, "token", "", "one-time enrolment token from `incus-backup-server token create`")
	f.StringVar(&o.Name, "name", "", "local name for this server (default: derived from the address)")
	f.StringVar(&o.Host, "host-name", "", "name this host presents (default: hostname)")
	f.StringVar(&passFile, "passphrase-file", "", "protect a NEW key file with the passphrase in this file (default: unprotected, mode 0600, for unattended runs)")
	f.BoolVar(&saved, "i-stored-the-recovery-key", false, "non-interactive: confirm the recovery key was stored")
	f.StringVar(&o.EscrowPublic, "escrow-public", "", "offline escrow PUBLIC key (IBP1-...): store a copy of the key wrapped to it on the server")
	f.StringVar(&escrowPrivFile, "escrow-private-file", "", "disaster recovery on a host without a key: file holding the offline escrow PRIVATE key (IBE1-...); the key is fetched from the server and unwrapped")
	return cmd
}

func (c *cli) cmdDoctor() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check Incus, servers, keys, policies and what would be backed up", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := c.load(cmd.Context(), false)
			if err != nil {
				return err
			}
			cs := a.Doctor(cmd.Context())
			for _, ch := range cs {
				fmt.Println(ch)
			}
			if app.Failed(cs) {
				return fmt.Errorf("doctor found problems")
			}
			return nil
		}}
}

func (c *cli) cmdPlan() *cobra.Command {
	return &cobra.Command{Use: "plan", Short: "Show what would be backed up, with the policy and where it came from", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := c.load(cmd.Context(), true)
			if err != nil {
				return err
			}
			objs, err := a.Plan(cmd.Context())
			if err != nil {
				return err
			}
			for _, o := range objs {
				fmt.Println(engine.Describe(o))
			}
			return nil
		}}
}

func (c *cli) cmdRun() *cobra.Command {
	var policies, names []string
	var dry bool
	var maxFreeze time.Duration
	cmd := &cobra.Command{Use: "run", Short: "Back up now", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, true)
			if err != nil {
				return err
			}
			objs, err := a.Plan(ctx)
			if err != nil {
				return err
			}
			eng := a.Eng
			if maxFreeze > 0 {
				eng.MaxFreeze = maxFreeze
			}
			rep := eng.Run(ctx, objs, engine.RunOptions{Policies: policies, Names: names, DryRun: dry})
			if !dry {
				a.RecordRun(ctx, objs, rep, policies)
			}
			printReport(rep, dry)
			if n := rep.Failed(); n > 0 && !dry {
				return fmt.Errorf("%d object(s) did not meet their commit requirement", n)
			}
			if n := rep.Deferred(); n > 0 && !dry {
				fmt.Printf("%d object(s) deferred: the repository is in maintenance mode; the next run backs them up\n", n)
			}
			return nil
		}}
	cmd.Flags().StringSliceVar(&policies, "policy", nil, "only objects with this policy")
	cmd.Flags().StringSliceVar(&names, "object", nil, "only this instance/volume name")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show the plan only")
	cmd.Flags().DurationVar(&maxFreeze, "max-freeze", 0, "override max_freeze for this run (the first backup of a large VM can need hours: e.g. --max-freeze 3h)")
	return cmd
}

func printReport(rep *engine.Report, dry bool) {
	tw := table()
	fmt.Fprintln(tw, "OBJECT\tMODE\tLOGICAL\tDIRTY\tREAD\tRATE\tRESULT")
	for _, r := range rep.Results {
		mode, res := "-", "planned"
		if !dry {
			switch {
			case r.Object.Kind == engine.KindInstance:
				mode = "manifest"
			case r.Full:
				mode = "full read"
			default:
				mode = "incremental"
			}
			res = "OK"
			if r.Deferred() {
				res = "DEFERRED"
			} else if !r.OK() {
				res = "FAILED"
			}
		}
		rate := "-"
		if r.Duration > time.Second && r.Read > 0 {
			rate = fmt.Sprintf("%.0f MB/s", float64(r.Read)/1e6/r.Duration.Seconds())
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Object.Project+"/"+r.Object.Name, mode, human(r.Logical), human(r.Dirty), human(r.Read), rate, res)
	}
	_ = tw.Flush()
	for _, r := range rep.Results {
		if r.Err != nil {
			fmt.Printf("  %s: %v\n", r.Object.Name, r.Err)
		}
		for _, t := range r.Targets {
			if t.Err != nil {
				fmt.Printf("  %s -> %s: %v\n", r.Object.Name, t.Target, t.Err)
			}
		}
	}
	for _, o := range rep.Skipped {
		if !strings.Contains(o.Skip, "no policy") {
			fmt.Printf("  skipped %s/%s: %s\n", o.Project, o.Name, o.Skip)
		}
	}
}

func (c *cli) cmdDaemon() *cobra.Command {
	return &cobra.Command{Use: "daemon", Short: "Run scheduled backups and verification (the OpenRC service)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, true)
			if err != nil {
				return err
			}
			c.log.Info("incus-backup daemon started", "version", version, "servers", a.Cfg.ServerNames())
			return a.Loop(ctx)
		}}
}

func (c *cli) cmdStatus() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "What is protected, on which server, and how recently", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			a, err := c.load(ctx, true)
			if err != nil {
				return err
			}
			objs, err := a.Plan(ctx)
			if err != nil {
				return err
			}
			servers := a.Cfg.ServerNames()
			tw := table()
			fmt.Fprintf(tw, "OBJECT\tPOLICY")
			for _, s := range servers {
				fmt.Fprintf(tw, "\t%s", strings.ToUpper(s))
			}
			fmt.Fprintln(tw, "\tNOTE")
			problems := 0
			for _, o := range objs {
				if o.Kind == engine.KindInstance {
					continue
				}
				pol := o.Policy
				note := ""
				if o.Skip != "" {
					pol, note = "-", o.Skip
				}
				fmt.Fprintf(tw, "%s\t%s", o.Project+"/"+o.Name, pol)
				for _, s := range servers {
					t := a.Eng.Targets[s]
					l, err := t.Client.Snapshots(ctx, t.Keys.GroupID(o.Project, o.Kind, o.Name))
					switch {
					case err != nil:
						fmt.Fprintf(tw, "\tUNREACHABLE")
						problems++
					case len(l.Snapshots) == 0:
						fmt.Fprintf(tw, "\t-")
						if o.Skip == "" {
							problems++
						}
					default:
						last := l.Snapshots[len(l.Snapshots)-1]
						fmt.Fprintf(tw, "\t%s (%d)", ago(last.Created), len(l.Snapshots))
					}
				}
				fmt.Fprintf(tw, "\t%s\n", note)
			}
			_ = tw.Flush()
			for _, n := range app.CertNotes(a.Cfg, a.Eng.Now()) {
				fmt.Println("  " + n)
			}
			if problems > 0 {
				return fmt.Errorf("%d object(s) have no backup on some server or a server is unreachable", problems)
			}
			return nil
		}}
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func (c *cli) cmdList() *cobra.Command {
	var server, project string
	cmd := &cobra.Command{Use: "list [OBJECT]", Short: "List what a server holds, or the restore points of one object", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			a, err := c.load(ctx, false)
			if err != nil {
				return err
			}
			t, err := a.Eng.PickTarget(server)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				objs, err := t.Client.Objects(ctx)
				if err != nil {
					return err
				}
				tw := table()
				fmt.Fprintln(tw, "PROJECT\tOBJECT\tKIND\tHOST\tSNAPSHOTS")
				for _, o := range objs {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", o.Project, o.Name, o.Kind, o.Host, o.Snapshots)
				}
				return tw.Flush()
			}
			for _, kind := range []string{engine.KindBlock, engine.KindFilesystem, engine.KindInstance} {
				group := t.Keys.GroupID(project, kind, args[0])
				l, err := t.Client.Snapshots(ctx, group)
				if err != nil || len(l.Snapshots) == 0 {
					continue
				}
				tw := table()
				fmt.Fprintln(tw, "SNAPSHOT\tCREATED\tSIZE\tMODE\tUPLOADED")
				for _, s := range l.Snapshots {
					loc, err := a.Eng.Locate(ctx, t, project, args[0], s.SnapshotID)
					if err != nil {
						fmt.Fprintf(tw, "%s\t%s\t?\t?\t%v\n", s.SnapshotID, s.Created.Format("2006-01-02 15:04"), err)
						continue
					}
					mode := "incremental"
					if loc.Manifest.Stats.FullRead {
						mode = "full read"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.SnapshotID, s.Created.Local().Format("2006-01-02 15:04"), human(int64(loc.Manifest.Object.Size)), mode, human(int64(loc.Manifest.Stats.UploadedBytes)))
				}
				return tw.Flush()
			}
			return fmt.Errorf("%s holds nothing called %q in project %q", t.Name, args[0], project)
		}}
	cmd.Flags().StringVar(&server, "server", "", "server to read from (default: first)")
	cmd.Flags().StringVar(&project, "project", "default", "Incus project")
	return cmd
}

func (c *cli) cmdRestore() *cobra.Command {
	root := &cobra.Command{Use: "restore", Short: "Restore an instance, a volume or a directory from a server"}
	var o engine.RestoreOptions
	inst := &cobra.Command{Use: "instance NAME", Short: "Recreate an instance (and optionally its custom volumes) from a backup", Args: cobra.ExactArgs(1),
		Long: "Creates a NEW instance from the stored configuration and writes its disks from the repository. It never overwrites an existing instance.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, true)
			if err != nil {
				return err
			}
			o.Name = args[0]
			res, err := a.Eng.RestoreInstance(ctx, o)
			if res != nil && len(res.Objects) > 0 {
				fmt.Printf("Restored %s: %s (%s)\n", res.Instance, strings.Join(res.Objects, ", "), human(int64(res.Bytes)))
			}
			return err
		}}
	f := inst.Flags()
	f.StringVar(&o.Server, "server", "", "server to restore from (default: first)")
	f.StringVar(&o.Project, "project", "default", "project the instance was backed up in")
	f.StringVar(&o.Snapshot, "snapshot", "", "snapshot id (default: latest)")
	f.StringVar(&o.AsName, "as", "", "restore under this name")
	f.StringVar(&o.AsProject, "as-project", "", "restore into this project")
	f.BoolVar(&o.Volumes, "volumes", false, "also restore the custom volumes it uses that do not exist here")
	f.BoolVar(&o.Start, "start", false, "start the instance afterwards")
	f.BoolVar(&o.NoChown, "no-chown", false, "do not change file ownership (not running as root)")
	f.BoolVar(&o.KeepNetwork, "keep-network", false, "keep MAC/IP addresses of NICs when restoring under another name (default: drop them so a copy cannot clash with the original)")

	var vserver, vproject, vsnap, to string
	vol := &cobra.Command{Use: "volume OBJECT", Short: "Restore one storage object (e.g. volume/biggi or instance/vm/root) into a new volume", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, true)
			if err != nil {
				return err
			}
			parts := strings.SplitN(to, "/", 2)
			if len(parts) != 2 {
				return fmt.Errorf("--to must be POOL/NAME")
			}
			loc, err := a.Eng.RestoreVolume(ctx, vserver, vproject, args[0], vsnap, incusx.VolRef{Pool: parts[0], Project: vproject, Type: "custom", Name: parts[1]}, os.Getuid() != 0)
			if loc != nil {
				fmt.Printf("Restored %s@%s into %s\n", args[0], loc.Snapshot, to)
			}
			return err
		}}
	vol.Flags().StringVar(&vserver, "server", "", "server to restore from")
	vol.Flags().StringVar(&vproject, "project", "default", "project")
	vol.Flags().StringVar(&vsnap, "snapshot", "", "snapshot id (default: latest)")
	vol.Flags().StringVar(&to, "to", "", "destination POOL/NAME (must not exist)")

	var dserver, dproject, dsnap, dir string
	dirCmd := &cobra.Command{Use: "files OBJECT", Short: "Extract a filesystem object into a local directory (no Incus needed)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, false)
			if err != nil {
				return err
			}
			t, err := a.Eng.PickTarget(dserver)
			if err != nil {
				return err
			}
			loc, err := a.Eng.Locate(ctx, t, dproject, args[0], dsnap)
			if err != nil {
				return err
			}
			if es, err := os.ReadDir(dir); err != nil || len(es) > 0 {
				return fmt.Errorf("--to must be an existing, empty directory")
			}
			if err := a.Eng.RestoreFS(ctx, loc, dir, os.Getuid() != 0); err != nil {
				return err
			}
			fmt.Printf("Extracted %s@%s into %s\n", args[0], loc.Snapshot, dir)
			return nil
		}}
	dirCmd.Flags().StringVar(&dserver, "server", "", "server to restore from")
	dirCmd.Flags().StringVar(&dproject, "project", "default", "project")
	dirCmd.Flags().StringVar(&dsnap, "snapshot", "", "snapshot id (default: latest)")
	dirCmd.Flags().StringVar(&dir, "to", "", "empty destination directory")
	var gserver, gproject, gsnap, gdest, gimage, gpool, gmem string
	var gpart int
	var gmax int64
	guestFiles := &cobra.Command{Use: "guest-files OBJECT PATH...", Short: "Recover single files from a VM disk backup through a disposable, isolated recovery VM", Args: cobra.MinimumNArgs(2),
		Long: `OBJECT is a VM disk (instance/NAME/root) or a block volume (volume/NAME). The disk of the chosen snapshot is
written to a scratch volume, a throw-away VM without network and without any of your profiles mounts it
read-only, packs the requested paths and powers off. Nothing from the guest filesystem is ever mounted on
this host, and the result is extracted with strict rules (no devices, no set-id bits, no path escapes, size limit).
Needs a recovery image with cloud-init (recovery.image in client.yaml or --image).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, true)
			if err != nil {
				return err
			}
			if gdest == "" {
				return fmt.Errorf("--to DIRECTORY is required")
			}
			if err := os.MkdirAll(gdest, 0o755); err != nil {
				return err
			}
			rc := a.Cfg.Recovery
			res, err := a.Eng.RestoreGuestFiles(ctx, engine.GuestFilesOptions{Server: gserver, Project: gproject, Name: args[0], Snapshot: gsnap,
				Paths: args[1:], Partition: gpart, Dest: gdest, Image: firstNonEmpty(gimage, rc.Image), Pool: firstNonEmpty(gpool, rc.Pool),
				Memory: firstNonEmpty(gmem, rc.Memory), CPU: rc.CPU, MaxBytes: gmax})
			if res != nil && res.Header != nil {
				for p, st := range res.Header.Paths {
					fmt.Printf("  %-40s %s\n", p, st)
				}
			}
			if err != nil {
				return err
			}
			fmt.Printf("Recovered %d file(s), %d director(ies), %s into %s (from %s, %s)\n", res.Report.Files, res.Report.Dirs, human(res.Report.Bytes), gdest, res.Snapshot, res.Header.FS)
			for _, s := range res.Report.Skipped {
				fmt.Printf("  skipped (not a plain file/directory/symlink): %s\n", s)
			}
			return nil
		}}
	gf := guestFiles.Flags()
	gf.StringVar(&gserver, "server", "", "server to restore from")
	gf.StringVar(&gproject, "project", "default", "project")
	gf.StringVar(&gsnap, "snapshot", "", "snapshot id (default: latest)")
	gf.StringVar(&gdest, "to", "", "destination directory")
	gf.StringVar(&gimage, "image", "", "recovery VM image, e.g. images:alpine/edge/cloud")
	gf.StringVar(&gpool, "pool", "", "storage pool for the scratch volumes")
	gf.StringVar(&gmem, "memory", "", "memory of the recovery VM (default 512MiB)")
	gf.IntVar(&gpart, "partition", 0, "partition number of the guest disk (default: the first one that holds the first path)")
	gf.Int64Var(&gmax, "max-bytes", 8<<30, "refuse to extract more than this many bytes")
	root.AddCommand(inst, vol, dirCmd, guestFiles)
	return root
}

func (c *cli) cmdVerify() *cobra.Command {
	var server string
	var last int
	cmd := &cobra.Command{Use: "verify", Short: "Download and decrypt backups to prove they are restorable", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, false)
			if err != nil {
				return err
			}
			names := a.Cfg.ServerNames()
			if server != "" {
				names = []string{server}
			}
			var failed bool
			for _, n := range names {
				rep, err := a.Eng.Verify(ctx, a.Eng.Targets[n], last)
				fmt.Printf("%s: %d snapshot(s), %d chunk(s), %s decrypted\n", n, rep.Snapshots, rep.Chunks, human(int64(rep.Bytes)))
				for _, p := range rep.Problems {
					fmt.Println("  PROBLEM", p)
				}
				failed = failed || err != nil
			}
			if failed {
				return fmt.Errorf("verification failed")
			}
			return nil
		}}
	cmd.Flags().StringVar(&server, "server", "", "only this server")
	cmd.Flags().IntVar(&last, "last", 1, "check the newest N snapshots of every object (0 = all)")
	return cmd
}

func (c *cli) cmdKey() *cobra.Command {
	root := &cobra.Command{Use: "key", Short: "Inspect the encryption key"}
	root.AddCommand(&cobra.Command{Use: "recovery", Short: "Print the recovery key again", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := configFor(c.cfgPath)
			if err != nil {
				return err
			}
			_, master, err := app.LoadKeys(cfg.KeyFile, c.passphrase(cfg))
			if err != nil {
				return err
			}
			rec, _ := format.EncodeRecoveryKey(master)
			fmt.Println(rec)
			return nil
		}})
	root.AddCommand(c.cmdEscrow())
	var impPass string
	imp := &cobra.Command{Use: "import", Short: "Recreate the key file from a recovery key read on stdin (disaster recovery)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := configFor(c.cfgPath)
			if err != nil {
				return err
			}
			rec, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			pass := ""
			if impPass != "" {
				b, err := os.ReadFile(impPass)
				if err != nil {
					return err
				}
				pass = strings.TrimSpace(string(b))
			}
			dom, err := app.ImportKey(cfg.KeyFile, rec, pass)
			if err != nil {
				return err
			}
			fmt.Printf("Key file %s written (key domain %s).\n", cfg.KeyFile, dom)
			return nil
		}}
	imp.Flags().StringVar(&impPass, "passphrase-file", "", "protect the key file with the passphrase in this file")
	root.AddCommand(imp)
	root.AddCommand(&cobra.Command{Use: "check", Short: "Type your stored recovery key and confirm it matches this host's key", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := configFor(c.cfgPath)
			if err != nil {
				return err
			}
			keys, _, err := app.LoadKeys(cfg.KeyFile, c.passphrase(cfg))
			if err != nil {
				return err
			}
			master, err := format.DecodeRecoveryKey(prompt("Recovery key: "))
			if err != nil {
				return err
			}
			k2, _ := format.DeriveKeys(master)
			if k2.DomainID != keys.DomainID {
				return fmt.Errorf("MISMATCH: that recovery key belongs to key domain %s, this host uses %s", k2.DomainID, keys.DomainID)
			}
			fmt.Println("OK: the recovery key matches this host's key (domain " + keys.DomainID + ").")
			return nil
		}})
	return root
}

var _ = sort.Strings
var _ = context.Background

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (c *cli) cmdEscrow() *cobra.Command {
	root := &cobra.Command{Use: "escrow", Short: "Offline escrow of the key (recover with an offline private key alone)",
		Long: `An escrow key pair is generated once; keep the PRIVATE half offline (safe, hardware token, envelope). The master
key is wrapped to the PUBLIC half and the wrapped copy is stored on the backup servers: they cannot open it. A
new host then needs only the private key and a token: incus-backup init --server ... --token ... --escrow-private-file FILE.`}
	root.AddCommand(&cobra.Command{Use: "generate", Short: "Create an escrow key pair (prints both halves; store the private one offline)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			priv, pub, err := format.GenerateEscrowKeyPair()
			if err != nil {
				return err
			}
			fmt.Println("PUBLIC  (give it to `init --escrow-public` / `key escrow upload`):", pub)
			fmt.Println("PRIVATE (store it OFFLINE, never on a server or on this host):      ", priv)
			return nil
		}})
	var pub string
	upload := &cobra.Command{Use: "upload", Short: "Wrap this host's key to the escrow public key and store it on every server", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			cfg, err := configFor(c.cfgPath)
			if err != nil {
				return err
			}
			a, err := c.load(ctx, false)
			if err != nil {
				return err
			}
			_, master, err := app.LoadKeys(cfg.KeyFile, c.passphrase(cfg))
			if err != nil {
				return err
			}
			wrapped, err := format.WrapMasterKey(master, pub)
			if err != nil {
				return err
			}
			var failed bool
			for _, n := range cfg.ServerNames() {
				switch err := a.Eng.Targets[n].Client.PutEscrow(ctx, wrapped); {
				case err == nil:
					fmt.Printf("%s: stored\n", n)
				case remote.IsStatus(err, 409):
					fmt.Printf("%s: already has an escrow blob (an administrator must clear it to replace it)\n", n)
				default:
					fmt.Printf("%s: FAILED: %v\n", n, err)
					failed = true
				}
			}
			if failed {
				return fmt.Errorf("not every server accepted the escrow blob")
			}
			return nil
		}}
	upload.Flags().StringVar(&pub, "public", "", "escrow public key (IBP1-...)")
	var priv, blobFile string
	unwrap := &cobra.Command{Use: "unwrap", Short: "Recover the recovery key from an escrow blob with the offline private key", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := os.ReadFile(blobFile)
			if err != nil {
				return err
			}
			master, dom, err := format.UnwrapMasterKey(b, strings.TrimSpace(priv))
			if err != nil {
				return err
			}
			rec, _ := format.EncodeRecoveryKey(master)
			fmt.Fprintf(os.Stderr, "key domain %s\n", dom)
			fmt.Println(rec)
			return nil
		}}
	unwrap.Flags().StringVar(&priv, "private", "", "escrow private key (IBE1-...)")
	unwrap.Flags().StringVar(&blobFile, "blob", "", "file with the wrapped key")
	fetch := &cobra.Command{Use: "fetch", Short: "Download the wrapped key from the first server that has it", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, false)
			if err != nil {
				return err
			}
			for _, n := range a.Cfg.ServerNames() {
				if b, err := a.Eng.Targets[n].Client.GetEscrow(ctx); err == nil {
					_, err = os.Stdout.Write(b)
					return err
				}
			}
			return fmt.Errorf("no server holds an escrow blob")
		}}
	root.AddCommand(upload, unwrap, fetch)
	return root
}
