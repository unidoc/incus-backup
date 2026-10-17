package demo

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/unidoc/incus-backup/internal/s3test/s3fake"
)

// freePort returns a TCP port that was free a moment ago on 127.0.0.1.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// portFree reports whether 127.0.0.1:port can be bound right now.
func portFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// killStale stops a process left behind by a demo that was killed hard (its pid is the first line of
// pidFile), so its port and data directory can be used again.
//
// Only a process whose command line names the expected program AND the demo's directory is signalled:
// a pid from a stale file may have been reused by an unrelated process.
func killStale(pidFile, program, dir string) {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]))
	if err != nil || pid <= 1 {
		return
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	cl := strings.ReplaceAll(string(cmdline), "\x00", " ")
	if err != nil || !strings.Contains(cl, program) || !strings.Contains(cl, dir) {
		_ = os.Remove(pidFile) // gone, or the pid now belongs to somebody else
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil || p.Signal(syscall.Signal(0)) != nil {
		_ = os.Remove(pidFile)
		return
	}
	_ = p.Signal(syscall.SIGINT) // postgres: fast shutdown
	for i := 0; i < 100; i++ {
		if p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.Signal(syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
}

// startPostgres runs embedded PostgreSQL with its data in dir (kept across runs: a restarted demo
// finds its seeded database again). The binaries cache is the one internal/testdb uses, so nothing
// is downloaded twice.
func startPostgres(dir string, port int, log io.Writer) (url string, stop func(), err error) {
	data := filepath.Join(dir, "data")
	killStale(filepath.Join(data, "postmaster.pid"), "postgres", data)
	cache := os.Getenv("INCUS_BACKUP_PG_CACHE")
	if cache == "" {
		home, _ := os.UserCacheDir()
		cache = home + "/incus-backup-embedded-postgres"
	}
	if log == nil {
		log = io.Discard
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(uint32(port)).
		DataPath(data).
		RuntimePath(filepath.Join(dir, "run")).
		BinariesPath(cache).
		Username("postgres").Password("postgres").
		Logger(log).
		// a throw-away dev database: durability is not worth the fsyncs (the seed commits thousands of times)
		StartParameters(map[string]string{"fsync": "off", "synchronous_commit": "off", "full_page_writes": "off"}).
		StartTimeout(120 * time.Second)
	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		return "", nil, fmt.Errorf("starting embedded postgres in %s: %w", dir, err)
	}
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port), func() { _ = pg.Stop() }, nil
}

// Credentials of the demo's S3 service (a local dev service; not a secret).
const (
	s3Access = "demoaccess"
	s3Secret = "demosecret-12345"
)

// startS3 runs the built-in S3 server (internal/s3test/s3fake, in this process) with its data in
// dir on port (kept across runs: a restarted demo finds the "offsite" repository's objects again)
// and makes sure bucket exists.
func startS3(dir string, port int, bucket string) (endpoint string, stop func(), err error) {
	srv, err := s3fake.New(s3fake.Options{Addr: fmt.Sprintf("127.0.0.1:%d", port), Dir: filepath.Join(dir, "data"), Access: s3Access, Secret: s3Secret})
	if err != nil {
		return "", nil, fmt.Errorf("starting the built-in S3 server on port %d: %w", port, err)
	}
	if err := srv.EnsureBucket(bucket); err != nil {
		srv.Close()
		return "", nil, err
	}
	return srv.Endpoint(), srv.Close, nil
}
