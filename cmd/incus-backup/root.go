package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/incusx"
)

// connectIncus is replaced in tests.
var connectIncus = func(socket string) (incusx.API, error) { return incusx.Connect(socket) }

type cli struct {
	cfgPath string
	debug   bool
	log     *slog.Logger
}

func newRoot() *cobra.Command {
	c := &cli{}
	root := &cobra.Command{
		Use:           "incus-backup",
		Short:         "Encrypted, deduplicated, incremental backups of Incus VMs, containers and volumes",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&c.cfgPath, "config", "c", config.DefaultPath, "configuration file")
	root.PersistentFlags().BoolVar(&c.debug, "debug", false, "verbose logging")
	root.PersistentPreRun = func(*cobra.Command, []string) {
		lvl := slog.LevelInfo
		if c.debug {
			lvl = slog.LevelDebug
		}
		c.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	}
	root.AddCommand(c.cmdInit(), c.cmdDoctor(), c.cmdPlan(), c.cmdRun(), c.cmdDaemon(), c.cmdStatus(), c.cmdList(), c.cmdRestore(), c.cmdFS(), c.cmdVerify(), c.cmdRestoreTest(), c.cmdKey(), c.cmdDebug(), c.cmdCert(),
		&cobra.Command{Use: "version", Short: "Print the version", Run: func(*cobra.Command, []string) { fmt.Println(version) }})
	root.Version = version
	root.AddCommand(cmdMan(root))
	return root
}

// passphrase resolves the key-file passphrase: file/env first, then a prompt on
// a terminal.
func (c *cli) passphrase(cfg *config.Config) app.Passphrase {
	base := app.FilePassphrase(cfg)
	return func(path string) (string, error) {
		if p, err := base(path); err != nil || p != "" {
			return p, err
		}
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprintf(os.Stderr, "Passphrase for %s: ", path)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
		return "", fmt.Errorf("the key file %s is passphrase protected: set passphrase_file in the configuration or INCUS_BACKUP_PASSPHRASE", path)
	}
}

// load builds the App. needIncus=false lets list/verify/restore-from-server work
// on a machine without Incus (disaster recovery from a laptop).
func (c *cli) load(ctx context.Context, needIncus bool) (*app.App, error) {
	cfg, err := config.Load(c.cfgPath)
	if err != nil {
		return nil, err
	}
	var inc incusx.API
	r, err := connectIncus(cfg.Incus.Socket)
	if err != nil {
		if needIncus {
			return nil, err
		}
		c.log.Debug("Incus is not reachable; continuing without it", "err", err)
		inc = noIncus{}
	} else {
		if real, ok := r.(*incusx.Real); ok {
			real.SetMountDir(filepath.Join(cfg.StateDir, "mnt"))
			real.CleanStaleMounts()
		}
		inc = r
	}
	return app.Load(ctx, cfg, inc, c.log, c.passphrase(cfg))
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
}

func prompt(q string) string {
	fmt.Fprint(os.Stderr, q)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(s)
}
