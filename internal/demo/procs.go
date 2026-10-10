package demo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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
	_ = p.Signal(syscall.SIGINT) // postgres: fast shutdown; minio: graceful stop
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

// MinIO credentials of the demo's S3 service (a local dev service; not a secret).
const (
	minioAccess = "demoaccess"
	minioSecret = "demosecret-12345"
)

// startMinio runs the MinIO binary with its data in dir on port (kept across runs) and makes sure
// bucket exists. It follows internal/s3test's start logic, which is tied to `testing`.
func startMinio(ctx context.Context, bin, dir string, port int, bucket string) (endpoint string, stop func(), err error) {
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return "", nil, err
	}
	pidFile := filepath.Join(dir, "minio.pid")
	killStale(pidFile, "minio", filepath.Join(dir, "data"))
	cport, err := freePort()
	if err != nil {
		return "", nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(bin, "server", filepath.Join(dir, "data"), "--address", addr, "--console-address", fmt.Sprintf("127.0.0.1:%d", cport), "--quiet")
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER="+minioAccess, "MINIO_ROOT_PASSWORD="+minioSecret, "MINIO_BROWSER=off")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // never outlive the demo
	if lf, lerr := os.OpenFile(filepath.Join(dir, "minio.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); lerr == nil {
		cmd.Stdout, cmd.Stderr = lf, lf
		defer lf.Close()
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("starting minio (%s): %w", bin, err)
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	stop = func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = os.Remove(pidFile)
	}
	cl, err := minio.New(addr, &minio.Options{Creds: credentials.NewStaticV4(minioAccess, minioSecret, ""), Secure: false})
	if err != nil {
		stop()
		return "", nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, err := cl.BucketExists(ctx, bucket)
		if err == nil && ok {
			return addr, stop, nil
		}
		if err == nil {
			if err = cl.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err == nil {
				return addr, stop, nil
			}
		}
		select {
		case <-done:
			return "", nil, errors.New("minio exited during start-up (see " + filepath.Join(dir, "minio.log") + ")")
		default:
		}
		if time.Now().After(deadline) {
			stop()
			return "", nil, fmt.Errorf("minio did not become ready on %s: %v", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
