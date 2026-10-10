package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/server/service"
)

func (a *app) cmdCredential() *cobra.Command {
	root := &cobra.Command{Use: "credential", Short: "Infrastructure credentials (S3 keys) in the sealed secret store",
		Long: "Credentials let the SERVER reach storage (e.g. an S3 bucket). They are unrelated to backup encryption keys, which the server never has. A stored secret is never shown again."}
	var accessKey, secretFile string
	secret := func() (service.S3Secret, error) {
		sec := service.S3Secret{AccessKey: firstNonEmpty(accessKey, os.Getenv("INCUS_BACKUP_S3_ACCESS_KEY")), SecretKey: os.Getenv("INCUS_BACKUP_S3_SECRET_KEY")}
		if secretFile != "" {
			b, err := os.ReadFile(secretFile)
			if err != nil {
				return sec, err
			}
			sec.SecretKey = strings.TrimSpace(string(b))
		}
		return sec, nil
	}
	flags := func(c *cobra.Command) {
		c.Flags().StringVar(&accessKey, "access-key", "", "S3 access key (or INCUS_BACKUP_S3_ACCESS_KEY)")
		c.Flags().StringVar(&secretFile, "secret-file", "", "file holding the S3 secret key (or INCUS_BACKUP_S3_SECRET_KEY)")
	}
	create := &cobra.Command{Use: "create NAME", Short: "Store an S3 credential", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			sec, err := secret()
			if err != nil {
				return err
			}
			c, err := a.svc.CreateS3Credential(ctx, args[0], sec)
			if err != nil {
				return err
			}
			fmt.Printf("Credential %s stored (access key %s).\n", c.Name, c.AccessKey)
			return nil
		}}
	flags(create)
	rotate := &cobra.Command{Use: "rotate NAME", Short: "Replace the secret of a credential", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			sec, err := secret()
			if err != nil {
				return err
			}
			if err := a.doRotate(ctx, args[0], sec); err != nil {
				return err
			}
			fmt.Println("Rotated. Run `repo probe` to confirm the new key works.")
			return nil
		}}
	flags(rotate)
	list := &cobra.Command{Use: "list", Short: "List credentials (never the secrets)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			cs, err := a.svc.Credentials(ctx)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "NAME\tKIND\tACCESS KEY\tROTATED\tUSED BY")
			for _, c := range cs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, c.Kind, c.AccessKey, ts(c.RotatedAt), dash(strings.Join(c.UsedBy, ",")))
			}
			return tw.Flush()
		}}
	del := &cobra.Command{Use: "delete NAME", Short: "Delete an unused credential", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			return a.svc.DeleteCredential(ctx, args[0])
		}}
	root.AddCommand(create, rotate, list, del)
	return root
}

// resolveObject finds the group id of "project/name" (names as shown by `object list`).
func (a *app) resolveObject(ctx context.Context, repo, spec string) (string, error) {
	project, name, ok := strings.Cut(spec, "/")
	if !ok {
		return "", fmt.Errorf("object must be PROJECT/NAME (see `object list`), e.g. custody/instance/bosch/root")
	}
	return a.svc.FindGroup(ctx, repo, project, name)
}

func (a *app) cmdSnapshot() *cobra.Command {
	root := &cobra.Command{Use: "snapshot", Short: "Inspect and manage snapshots (protect, notes, forget, verification)"}
	var object string
	need := func(c *cobra.Command) {
		c.Flags().StringVar(&object, "object", "", "PROJECT/NAME of the object (see `object list`)")
	}
	list := &cobra.Command{Use: "list", Short: "List snapshots with protection, verification state and notes", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			var groups []string
			if object != "" {
				g, err := a.resolveObject(ctx, repo, object)
				if err != nil {
					return err
				}
				groups = []string{g}
			}
			l, err := a.svc.AdminSnapshots(ctx, repo, groups)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "OBJECT\tSNAPSHOT\tCOMMITTED\tPROTECTED\tVERIFIED\tNOTES")
			for _, r := range l {
				prot := ""
				if r.Protected {
					prot = "yes"
				}
				fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\t%s\t%s\n", r.Project, r.Object, r.SnapshotID, r.Committed.Local().Format("2006-01-02 15:04"), dash(prot), dash(r.VerifyState), dash(r.Notes))
			}
			return tw.Flush()
		})}
	need(list)
	protect := func(use string, on bool) *cobra.Command {
		c := &cobra.Command{Use: use + " SNAPSHOT", Short: map[bool]string{true: "Protect a snapshot from retention and forget", false: "Remove a snapshot's protection"}[on], Args: cobra.ExactArgs(1),
			RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
				g, err := a.resolveObject(ctx, repo, object)
				if err != nil {
					return err
				}
				return a.svc.SetSnapshotProtected(ctx, repo, g, args[0], on)
			})}
		need(c)
		return c
	}
	notes := &cobra.Command{Use: "notes SNAPSHOT [TEXT]", Short: "Set (or, with empty text, clear) a snapshot's notes; without TEXT show the notes", Args: cobra.RangeArgs(1, 2),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			g, err := a.resolveObject(ctx, repo, object)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				l, err := a.svc.AdminSnapshots(ctx, repo, []string{g})
				if err != nil {
					return err
				}
				for _, r := range l {
					if r.SnapshotID == args[0] {
						fmt.Println(r.Notes)
					}
				}
				return nil
			}
			return a.svc.SetSnapshotNotes(ctx, repo, g, args[0], args[1])
		})}
	need(notes)
	forget := &cobra.Command{Use: "forget SNAPSHOT", Short: "Remove one snapshot now (refused when protected or in use by a running backup)", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			g, err := a.resolveObject(ctx, repo, object)
			if err != nil {
				return err
			}
			return a.doForget(ctx, repo, g, args[0])
		})}
	need(forget)
	hist := &cobra.Command{Use: "history SNAPSHOT", Short: "Verification history of a snapshot", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			g, err := a.resolveObject(ctx, repo, object)
			if err != nil {
				return err
			}
			h, err := a.svc.VerificationHistory(ctx, repo, g, args[0])
			if err != nil {
				return err
			}
			for _, e := range h {
				fmt.Printf("%s  %-6s  %d chunks  %s\n", e.At.Local().Format("2006-01-02 15:04"), e.State, e.Chunks, strings.Join(e.Problems, "; "))
			}
			return nil
		})}
	need(hist)
	root.AddCommand(list, protect("protect", true), protect("unprotect", false), notes, forget, hist)
	return root
}

func (a *app) cmdObjectNotes() *cobra.Command {
	return &cobra.Command{Use: "notes PROJECT/NAME [TEXT]", Short: "Set or show the notes of a whole object", Args: cobra.RangeArgs(1, 2),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			g, err := a.resolveObject(ctx, repo, args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				return a.svc.SetGroupNotes(ctx, repo, g, args[1])
			}
			var n string
			_ = a.db.Pool().QueryRow(ctx, `SELECT notes FROM storage_objects WHERE group_id=$1`, g).Scan(&n)
			fmt.Println(n)
			return nil
		})}
}

func (a *app) cmdCorrupt() *cobra.Command {
	var all bool
	return &cobra.Command{Use: "corrupt", Short: "Corrupt chunks found by verify and which snapshots they affect", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			cc, err := a.svc.CorruptChunks(ctx, repo, all)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "CHUNK\tSTATE\tDETECTED\tAFFECTED SNAPSHOTS")
			for _, c := range cc {
				id := c.ChunkID
				if len(id) > 16 {
					id = id[:16] + "…"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", id, c.State, c.DetectedAt.Local().Format("2006-01-02 15:04"), dash(strings.Join(c.Affected, ", ")))
			}
			return tw.Flush()
		})}
}

func (a *app) cmdMaintenance() *cobra.Command {
	root := &cobra.Command{Use: "maintenance", Short: "Housekeeping schedules of a repository (stored in PostgreSQL)"}
	show := &cobra.Command{Use: "show", Short: "Show the schedules", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			m, err := a.svc.Maintenance(ctx, repo)
			if err != nil {
				return err
			}
			fmt.Printf("capacity   %s\nreconcile  %s\nprune      %s\ngc         %s (grace %s)\nverify     %s (new snapshots + stale ones; default re-verify %dd)\nscrub      %s (every chunk object)\n",
				m.Capacity, m.Reconcile, m.Prune, m.GC, m.GCGrace, m.Verify, m.DefaultReverifyDays, m.Scrub)
			return nil
		})}
	var set service.MaintenanceSettings
	setc := &cobra.Command{Use: "set", Short: "Change schedules (only the flags you give)", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			m, err := a.svc.Maintenance(ctx, repo)
			if err != nil {
				return err
			}
			pick := func(dst *string, v string) {
				if v != "" {
					*dst = v
				}
			}
			pick(&m.Capacity, set.Capacity)
			pick(&m.Reconcile, set.Reconcile)
			pick(&m.Prune, set.Prune)
			pick(&m.GC, set.GC)
			pick(&m.Verify, set.Verify)
			pick(&m.Scrub, set.Scrub)
			if set.GCGrace != 0 {
				m.GCGrace = set.GCGrace
			}
			if set.DefaultReverifyDays >= 0 && setReverify {
				m.DefaultReverifyDays = set.DefaultReverifyDays
			}
			return a.svc.SetMaintenance(ctx, repo, *m)
		})}
	f := setc.Flags()
	f.StringVar(&set.Capacity, "capacity", "", `capacity sampling schedule ("every 1h")`)
	f.StringVar(&set.Reconcile, "reconcile", "", "reconcile schedule")
	f.StringVar(&set.Prune, "prune", "", `retention schedule ("04:00"; "manual" disables)`)
	f.StringVar(&set.GC, "gc", "", "garbage collection schedule")
	f.StringVar(&set.Verify, "verify", "", "verify new + stale snapshots schedule")
	f.StringVar(&set.Scrub, "scrub", "", "full chunk scrub schedule")
	f.DurationVar(&set.GCGrace, "gc-grace", 0, "minimum age of an unreferenced chunk (at least 12h)")
	f.IntVar(&set.DefaultReverifyDays, "reverify-days", -1, "default re-verify age for objects without a policy value")
	setc.PreRun = func(cmd *cobra.Command, _ []string) { setReverify = cmd.Flags().Changed("reverify-days") }
	root.AddCommand(show, setc)
	return root
}

var setReverify bool

var _ = time.Second

func (a *app) cmdEscrow() *cobra.Command {
	root := &cobra.Command{Use: "escrow", Short: "The key-escrow blob hosts stored (the master key wrapped to an offline public key)"}
	root.AddCommand(&cobra.Command{Use: "clear", Short: "Remove the stored blob so a host can store a new one (e.g. after rotating the escrow key)", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			return a.svc.ClearEscrow(ctx, repo)
		})})
	return root
}
