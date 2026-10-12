// Command ibdemo starts a complete, real incus-backup server stack in one process (embedded
// PostgreSQL, agent API, management API + web UI on plain HTTP, admin socket, optional MinIO) and
// seeds it with weeks of realistic history produced by the real client engine. It is a development
// and end-to-end test tool for the web UI; it is never packaged. See docs/dev-demo.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/unidoc/incus-backup/internal/demo"
)

func main() {
	var opt demo.Options
	var profile, logLevel string
	flag.StringVar(&opt.Dir, "dir", "/tmp/ibdemo", "demo directory (PostgreSQL data, datastores, MinIO data, host state)")
	flag.BoolVar(&opt.Reset, "reset", false, "wipe --dir first and seed again")
	flag.StringVar(&opt.Web, "web", "127.0.0.1:8099", "plain-HTTP listener of the management API and web UI")
	flag.StringVar(&opt.Agent, "agent", "", "agent API (mutual TLS) listener (default: the port of the previous run, else a free one)")
	flag.StringVar(&opt.Socket, "socket", "", "unix admin socket (default DIR/admin.sock)")
	flag.StringVar(&opt.UIDir, "ui-dir", demo.DefaultUIDir(), "serve the web UI from this Vite build directory (empty: the UI embedded in the binary)")
	flag.BoolVar(&opt.Seed, "seed", true, "seed realistic data into a new directory")
	flag.StringVar(&profile, "profile", "full", "seed size: full (30 days, about 1 minute) or test (5 days, small, about 15 s)")
	flag.StringVar(&opt.Password, "admin-password", demo.DefaultPassword, "password of every demo user (admin, operator, restorer, viewer)")
	flag.StringVar(&opt.Minio, "minio", os.Getenv("INCUS_BACKUP_TEST_MINIO"), "MinIO server binary for the S3 repository \"offsite\" (default $INCUS_BACKUP_TEST_MINIO; empty: a filesystem repository)")
	flag.BoolVar(&opt.Maintenance, "maintenance", false, "run the real maintenance scheduler after seeding (retention may thin the seeded history)")
	flag.BoolVar(&opt.NoJobs, "no-jobs", false, "do not run the job executors of the simulated hosts bosch and benfica (no host jobs; less memory)")
	flag.StringVar(&logLevel, "log", "warn", "server log level: debug, info, warn, error")
	flag.Parse()
	// a dev tool next to other work: the seed allocates gigabytes of short-lived buffers, keep the heap
	// close to the live data instead of letting it double
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(1 << 30)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}

	switch profile {
	case "full":
		opt.Profile = demo.FullProfile()
	case "test":
		opt.Profile = demo.TestProfile()
	default:
		fmt.Fprintf(os.Stderr, "ibdemo: unknown --profile %q (full or test)\n", profile)
		os.Exit(2)
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "ibdemo: --log: %v\n", err)
		os.Exit(2)
	}
	opt.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	opt.Progress = os.Stderr

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	began := time.Now()
	st, err := demo.Start(ctx, opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibdemo: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(st.Banner(time.Since(began)))
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "ibdemo: shutting down")
	st.Close()
}
