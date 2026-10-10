package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/config"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/httpapi"
	"github.com/unidoc/incus-backup/internal/server/maint"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/server/webui"
)

func (a *app) cmdInit() *cobra.Command {
	var stateDir, dbURL, repoName, datastore, listen string
	var sans []string
	c := &cobra.Command{
		Use:   "init",
		Short: "Create the server state (CA, certificate), migrate the database and create the first repository",
		Long: `init prepares a new server in one step:

  * creates the private CA and the server certificate in the state directory
  * migrates the PostgreSQL schema
  * creates the first repository and its datastore directory
  * writes the configuration file

It does NOT create enrolment tokens. Enrol hosts afterwards with
"incus-backup-server token create <host> --scope backup".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Defaults()
			if stateDir != "" {
				cfg.StateDir = stateDir
			}
			if listen != "" {
				cfg.Listen = listen
			}
			if dbURL == "" {
				dbURL = os.Getenv("INCUS_BACKUP_DATABASE_URL")
			}
			if dbURL == "" {
				return fmt.Errorf("--database-url (or INCUS_BACKUP_DATABASE_URL) is required: PostgreSQL is a required dependency")
			}
			cfg.DatabaseURL = dbURL
			cfg.SANs = sans
			if datastore == "" {
				return fmt.Errorf("--datastore is required (the directory that will hold the encrypted chunks)")
			}
			ctx := cmd.Context()
			d, err := db.Open(ctx, dbURL)
			if err != nil {
				return err
			}
			defer d.Close()
			if err := d.Migrate(ctx); err != nil {
				return err
			}
			svc := service.New(d, cfg.StateDir, version, a.log)
			if err := svc.InitPKI(sans); err != nil {
				return err
			}
			if _, err := svc.CreateRepository(ctx, repoName, datastore); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(a.cfgPath), 0o755); err != nil {
				return err
			}
			if err := cfg.Save(a.cfgPath); err != nil {
				return err
			}
			if err := chownTree(serviceUser, cfg.StateDir, datastore, a.cfgPath); err != nil {
				return err
			}
			fp, _ := svc.CAFingerprint()
			fmt.Printf("Server initialised.\n  config:      %s\n  state:       %s\n  repository:  %s (%s)\n  CA fingerprint: %s\n\nNext: incus-backup-server token create <host> --scope backup\n",
				a.cfgPath, cfg.StateDir, repoName, datastore, fp)
			return nil
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "state directory (default /var/lib/incus-backup-server)")
	c.Flags().StringVar(&dbURL, "database-url", "", "PostgreSQL connection URL")
	c.Flags().StringVar(&repoName, "repository", "default", "name of the first repository")
	c.Flags().StringVar(&datastore, "datastore", "", "datastore directory")
	c.Flags().StringVar(&listen, "listen", "", "listen address (default :8007)")
	c.Flags().StringSliceVar(&sans, "san", nil, "DNS name or IP clients will use (repeatable)")
	return c
}

func (a *app) cmdMigrate() *cobra.Command {
	return &cobra.Command{
		Use: "migrate", Short: "Apply pending database migrations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.open(cmd); err != nil {
				return err
			}
			a.close()
			fmt.Println("Database is up to date.")
			return nil
		},
	}
}

func (a *app) cmdServe() *cobra.Command {
	return &cobra.Command{
		Use: "serve", Short: "Run the server (API, maintenance and sync scheduler)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()
			cmd.SetContext(ctx)
			ring := a.installLogRing()
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			// ONE daemon per database: it owns the session pins and the writer slots. A second one
			// would free every live writer slot and run housekeeping without seeing the pins.
			releaseDaemon, err := a.svc.BecomeDaemon(ctx)
			if err != nil {
				return err
			}
			defer releaseDaemon()
			if err := a.svc.ExpireStaleSessions(ctx); err != nil {
				return err
			}
			hub := mgmt.NewHub()
			tm, err := a.startTasks(ctx, hub)
			if err != nil {
				return err
			}
			defer tm.Close()
			r := &maint.Runner{Svc: a.svc, Sync: a.sync, Tasks: tm}
			go r.Loop(ctx)
			go a.svc.CertLoop(ctx) // server certificate: renewal, SAN edits, hot reload
			ne := mgmt.NewNotifyEngine(a.svc)
			upd := a.updateCheck(ctx, ne) // wires ne.UpdateStatus before the engine runs
			go ne.Run(ctx, time.Minute)
			ms := &mgmt.Server{Svc: a.svc, Sync: a.sync, Log: a.log, Auth: a.authStore(), Notify: ne, AgentListen: a.cfg.Listen, UI: webui.Handler(), Secure: true, WebCert: webCertInfo, Tasks: tm, Hub: hub}
			a.wireOps(ctx, ms, upd, ring)
			ms.BridgeNotify(ctx)
			ms.StartBridges(ctx)
			go a.housekeepingLoop(ctx, ms)
			if sock := a.cfg.AdminSocketPath(); sock != "" {
				go func() {
					if err := ms.Serve(ctx, sock); err != nil {
						a.log.Error("admin API stopped", "socket", sock, "err", err)
					}
				}()
				a.log.Info("admin API listening", "socket", sock)
			}
			if addr := a.cfg.WebAddr(); addr != "" {
				if err := a.serveWeb(ctx, ms, addr); err != nil {
					return err
				}
			}
			a.log.Info("incus-backup-server listening", "addr", a.cfg.Listen, "version", version)
			return (&httpapi.Server{Svc: a.svc, Log: a.log}).Serve(ctx, a.cfg.Listen)
		},
	}
}

func (a *app) cmdRepo() *cobra.Command {
	root := &cobra.Command{Use: "repo", Short: "Manage repositories (filesystem or S3)"}
	var path string
	var s3 service.S3Params
	var s3SecretFile string
	var adopt bool
	s3Flags := func(f *pflag.FlagSet) {
		f.StringVar(&s3.Endpoint, "s3-endpoint", "", "S3 endpoint, host[:port] or URL (any S3-compatible service)")
		f.StringVar(&s3.Region, "s3-region", "", "S3 region (optional for most non-AWS services)")
		f.StringVar(&s3.Bucket, "s3-bucket", "", "S3 bucket (must exist)")
		f.StringVar(&s3.Prefix, "s3-prefix", "", "key prefix inside the bucket")
		f.StringVar(&s3.Credential, "s3-credential", "", "name of an existing stored S3 credential (see `credential list`)")
		f.StringVar(&s3.AccessKey, "s3-access-key", "", "S3 access key (or INCUS_BACKUP_S3_ACCESS_KEY); stored sealed as a credential")
		f.StringVar(&s3SecretFile, "s3-secret-file", "", "file holding the S3 secret key (or INCUS_BACKUP_S3_SECRET_KEY)")
		f.BoolVar(&s3.NoTLS, "s3-no-tls", false, "plain HTTP (LAN/test endpoints only)")
		f.StringVar(&s3CAFile, "s3-ca-file", "", "PEM CA certificate to trust for the endpoint")
		f.BoolVar(&s3.InsecureSkipVerify, "s3-insecure-skip-verify", false, "do not verify the endpoint certificate (last resort)")
		f.BoolVar(&s3.PathStyle, "s3-path-style", false, "force path-style addressing")
	}
	fill := func() error {
		if s3CAFile != "" {
			b, err := os.ReadFile(s3CAFile)
			if err != nil {
				return err
			}
			s3.CAPEM = string(b)
		}
		if s3.Credential != "" {
			return nil
		}
		s3.AccessKey = firstNonEmpty(s3.AccessKey, os.Getenv("INCUS_BACKUP_S3_ACCESS_KEY"))
		if s3SecretFile != "" {
			b, err := os.ReadFile(s3SecretFile)
			if err != nil {
				return err
			}
			s3.SecretKey = strings.TrimSpace(string(b))
		}
		s3.SecretKey = firstNonEmpty(s3.SecretKey, os.Getenv("INCUS_BACKUP_S3_SECRET_KEY"))
		return nil
	}
	create := &cobra.Command{Use: "create NAME", Short: "Create a repository (its own datastore and key domain)", Args: cobra.ExactArgs(1),
		Long: `Create a repository on a filesystem directory (--datastore) or in an S3-compatible bucket
(--s3-endpoint, --s3-bucket, ...). The S3 secret key is read from INCUS_BACKUP_S3_SECRET_KEY or
--s3-secret-file, never from a flag, and is stored sealed in the server's secret store (PostgreSQL holds
only ciphertext). It is never shown again: rotate it with "credential rotate".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			if s3.Endpoint != "" || s3.Bucket != "" {
				if path != "" {
					return fmt.Errorf("--datastore and --s3-* are mutually exclusive")
				}
				if err := fill(); err != nil {
					return err
				}
				r, err := a.svc.AddS3Repository(ctx, args[0], s3)
				if err != nil {
					return err
				}
				fmt.Printf("Repository %s created on %s\n", r.Name, r.Path)
				return nil
			}
			if path == "" {
				return fmt.Errorf("--datastore (filesystem) or --s3-endpoint/--s3-bucket (S3) is required")
			}
			r, err := a.svc.CreateRepository(ctx, args[0], path)
			if err != nil {
				return err
			}
			fmt.Printf("Repository %s created at %s\n", r.Name, r.Path)
			return nil
		}}
	create.Flags().StringVar(&path, "datastore", "", "datastore directory (filesystem backend)")
	s3Flags(create.Flags())

	attach := &cobra.Command{Use: "attach NAME", Short: "Register an EXISTING repository (disaster recovery, or moving it to this server)", Args: cobra.ExactArgs(1),
		Long: `A repository belongs to one server at a time (owner.json inside it). Attaching one that another server owns
is refused unless you pass --adopt, which you should only do when that server is gone for good: two servers
running garbage collection and retention on one repository would destroy data.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			var r *service.Repo
			if s3.Endpoint != "" || s3.Bucket != "" {
				if err := fill(); err != nil {
					return err
				}
				r, err = a.svc.AttachS3Repository(ctx, args[0], s3, adopt)
			} else if path != "" {
				r, err = a.svc.AttachRepository(ctx, args[0], path, adopt)
			} else {
				return fmt.Errorf("--datastore or --s3-endpoint/--s3-bucket is required")
			}
			if err != nil {
				return err
			}
			fmt.Printf("Repository %s attached (%s). Run `reconcile` to rebuild its snapshot records in PostgreSQL.\n", r.Name, r.Path)
			return nil
		}}
	attach.Flags().StringVar(&path, "datastore", "", "existing datastore directory")
	attach.Flags().BoolVar(&adopt, "adopt", false, "take over a repository another server still claims")
	s3Flags(attach.Flags())

	list := &cobra.Command{Use: "list", Short: "List repositories", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			rs, err := a.svc.Repositories(ctx)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "NAME\tBACKEND\tDATASTORE\tKEY DOMAIN")
			for _, r := range rs {
				d := r.DomainID
				if d == "" {
					d = "(not pinned yet)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Backend, r.Path, d)
			}
			return tw.Flush()
		}}
	probe := &cobra.Command{Use: "probe NAME", Short: "Check connectivity and every storage operation a repository needs (S3: permissions, conditional write, list)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			rs, err := a.svc.ProbeRepository(ctx, args[0])
			if err != nil {
				return err
			}
			failed := 0
			for _, r := range rs {
				if r.OK {
					fmt.Printf("ok    %s\n", r.Op)
				} else {
					failed++
					fmt.Printf("FAIL  %s: %s\n", r.Op, r.Err)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		}}
	root.AddCommand(create, attach, list, probe)
	root.AddCommand(a.repoOpsCmds()...)
	return root
}

var s3CAFile string

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a *app) cmdToken() *cobra.Command {
	root := &cobra.Command{Use: "token", Short: "Enrolment tokens"}
	var scope string
	var ttl time.Duration
	create := &cobra.Command{Use: "create HOST", Short: "Create a one-time enrolment token for a host or sync peer", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			tok, err := a.svc.CreateToken(ctx, repo, args[0], scope, ttl)
			if err != nil {
				return err
			}
			fmt.Printf("Enrolment token for %s (scope %s, valid %s, usable once):\n\n  %s\n\n", args[0], scope, ttl, tok)
			switch scope {
			case api.ScopeBackup:
				fmt.Printf("On the host:  incus-backup init --server %s --token <token>\n", a.cfg.Listen)
			default:
				fmt.Printf("On the peer server:  incus-backup-server sync add NAME --remote %s --token <token> --direction %s\n", a.cfg.Listen, map[string]string{api.ScopeSyncWrite: "push", api.ScopeSyncRead: "pull"}[scope])
			}
			return nil
		})}
	create.Flags().StringVar(&scope, "scope", api.ScopeBackup, "backup | sync-read | sync-write")
	create.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the token is valid")
	root.AddCommand(create)
	return root
}

func (a *app) cmdHost() *cobra.Command {
	root := &cobra.Command{Use: "host", Short: "Enrolled hosts and sync peers"}
	var scope string
	list := &cobra.Command{Use: "list", Short: "List enrolled identities", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			hs, err := a.svc.Hosts(ctx, repo)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "NAME\tSCOPE\tSTATE\tLAST SEEN\tADDRESS")
			for _, h := range hs {
				st, seen := "active", "never"
				if h.Disabled {
					st = "DISABLED"
				}
				if h.LastSeen != nil {
					seen = h.LastSeen.Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Name, h.Scope, st, seen, h.LastAddr)
			}
			return tw.Flush()
		})}
	mk := func(use string, disabled bool, short string) *cobra.Command {
		c := &cobra.Command{Use: use + " HOST", Short: short, Args: cobra.ExactArgs(1),
			RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
				if err := a.svc.SetHostDisabled(ctx, repo, args[0], scope, disabled); err != nil {
					return err
				}
				fmt.Printf("%s %s (%s)\n", args[0], map[bool]string{true: "disabled", false: "enabled"}[disabled], scope)
				return nil
			})}
		c.Flags().StringVar(&scope, "scope", api.ScopeBackup, "scope of the identity")
		return c
	}
	root.AddCommand(list, mk("disable", true, "Revoke a host immediately"), mk("enable", false, "Re-enable a host"))
	return root
}

var _ = syncer.Job{}
