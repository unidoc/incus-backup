package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/server/config"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

type app struct {
	cfgPath string
	repo    string
	cfg     *config.Config
	db      *db.DB
	svc     *service.Service
	sync    *syncer.Manager
	log     *slog.Logger
}

func newRoot() *cobra.Command {
	a := &app{log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	root := &cobra.Command{
		Use:           "incus-backup-server",
		Short:         "Repository server for incus-backup (PostgreSQL control plane, zero-knowledge datastore)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&a.cfgPath, "config", "c", config.DefaultPath, "configuration file")
	root.PersistentFlags().StringVarP(&a.repo, "repository", "r", "", "repository name (default: the only one)")

	root.AddCommand(
		a.cmdInit(), a.cmdMigrate(), a.cmdServe(), a.cmdRepo(), a.cmdToken(), a.cmdHost(),
		a.cmdPolicy(), a.cmdObject(), a.cmdStatus(), a.cmdCapacity(), a.cmdPrune(), a.cmdGC(), a.cmdVerify(),
		a.cmdReconcile(), a.cmdSync(), a.cmdConfigExport(), a.cmdCredential(), a.cmdEscrow(), a.cmdSnapshot(), a.cmdCorrupt(), a.cmdMaintenance(), a.cmdUser(), a.cmdCA(),
		a.cmdTask(), a.cmdJobs(), a.cmdAlerts(), a.cmdAudit(),
		&cobra.Command{Use: "version", Short: "Print the version", Run: func(*cobra.Command, []string) { fmt.Println(version) }},
	)
	root.Version = version
	root.AddCommand(cmdOpenAPI())
	root.AddCommand(cmdMan(root))
	return root
}

// open loads the configuration, connects and migrates. Every command except
// init/version uses it, so the CLI and the daemon share one service layer.
func (a *app) open(cmd *cobra.Command) (context.Context, error) {
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return nil, err
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("no database configured: set database_url in %s or INCUS_BACKUP_DATABASE_URL", a.cfgPath)
	}
	ctx := cmd.Context()
	d, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := d.Migrate(ctx); err != nil {
		d.Close()
		return nil, err
	}
	a.cfg, a.db = cfg, d
	a.svc = service.New(d, cfg.StateDir, version, a.log)
	a.sync = &syncer.Manager{Svc: a.svc}
	return ctx, nil
}

func (a *app) close() {
	if a.db != nil {
		a.db.Close()
	}
	// A CLI command run as root may have created state (secrets.key, sync credentials) the daemon,
	// which runs as the service account, must be able to read: hand it over like `init` does.
	if a.cfg != nil {
		_ = chownTree(serviceUser, a.cfg.StateDir)
	}
}

// repoName resolves --repository, defaulting to the only repository.
func (a *app) repoName(ctx context.Context) (string, error) {
	if a.repo != "" {
		return a.repo, nil
	}
	rs, err := a.svc.Repositories(ctx)
	if err != nil {
		return "", err
	}
	switch len(rs) {
	case 0:
		return "", fmt.Errorf("no repository exists yet: run `incus-backup-server init` or `repo create`")
	case 1:
		return rs[0].Name, nil
	}
	return "", fmt.Errorf("several repositories exist: choose one with --repository")
}

// withRepo wraps a command body that needs the service and a repository name.
func (a *app) withRepo(fn func(ctx context.Context, repo string, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, err := a.open(cmd)
		if err != nil {
			return err
		}
		defer a.close()
		repo, err := a.repoName(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, repo, args)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
