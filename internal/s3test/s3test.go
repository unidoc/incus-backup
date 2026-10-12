// Package s3test gives tests a real S3-compatible service and a fresh bucket.
//
// It uses INCUS_BACKUP_TEST_S3_ENDPOINT / _ACCESS_KEY / _SECRET_KEY (any service,
// plain HTTP unless the endpoint starts with https://) when set, and otherwise
// starts a MinIO server binary (INCUS_BACKUP_TEST_MINIO, else `minio` in PATH).
// If neither works the test FAILS: an S3 test that silently skips proves nothing.
//
// Packages using it must call Main from TestMain.
package s3test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	endpoint, access, secret string
	startErr                 error
	counter                  atomic.Int64
)

// Main starts the service if needed, runs the tests and stops it.
func Main(m *testing.M) {
	stop := Start()
	code := m.Run()
	if stop != nil {
		stop()
	}
	os.Exit(code)
}

// Start starts the service if needed and returns its stop function (may be nil).
// Use it with testdb.AtExit when a package also needs PostgreSQL:
//
//	testdb.AtExit(s3test.Start()); testdb.Main(m)
func Start() (stop func()) {
	if e := os.Getenv("INCUS_BACKUP_TEST_S3_ENDPOINT"); e != "" {
		endpoint = e
		access = os.Getenv("INCUS_BACKUP_TEST_S3_ACCESS_KEY")
		secret = os.Getenv("INCUS_BACKUP_TEST_S3_SECRET_KEY")
	} else {
		endpoint, access, secret, stop, startErr = startMinio()
	}
	return stop
}

func startMinio() (ep, ak, sk string, stop func(), err error) {
	bin := os.Getenv("INCUS_BACKUP_TEST_MINIO")
	if bin == "" {
		if bin, err = exec.LookPath("minio"); err != nil {
			return "", "", "", nil, fmt.Errorf("no S3 service: set INCUS_BACKUP_TEST_S3_ENDPOINT or put a minio binary in PATH / INCUS_BACKUP_TEST_MINIO: %w", err)
		}
	}
	free := func() (string, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		defer l.Close()
		return l.Addr().String(), nil
	}
	addr, err := free()
	if err != nil {
		return
	}
	caddr, err := free()
	if err != nil {
		return
	}
	dir, err := os.MkdirTemp("", "incus-backup-minio-*")
	if err != nil {
		return
	}
	ak, sk = "testaccess", "testsecret-12345"
	cmd := exec.Command(bin, "server", dir, "--address", addr, "--console-address", caddr, "--quiet")
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER="+ak, "MINIO_ROOT_PASSWORD="+sk, "MINIO_BROWSER=off")
	if err = cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return
	}
	stop = func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		os.RemoveAll(dir)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr == nil {
			c.Close()
			return addr, ak, sk, stop, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	return "", "", "", nil, fmt.Errorf("minio did not start on %s", addr)
}

// Config is a connection to the shared service.
type Config struct {
	Endpoint, AccessKey, SecretKey string
	NoTLS                          bool
}

// Service returns the shared service's credentials; it fails the test if none is available.
func Service(t testing.TB) Config {
	t.Helper()
	if startErr != nil {
		t.Fatalf("no S3 test service: %v", startErr)
	}
	if endpoint == "" {
		t.Fatal("s3test.Main was not called from TestMain")
	}
	return Config{Endpoint: endpoint, AccessKey: access, SecretKey: secret, NoTLS: !strings.HasPrefix(endpoint, "https://")}
}

// NewBucket creates a fresh, empty bucket and returns the service config plus its name.
func NewBucket(t testing.TB) (Config, string) {
	t.Helper()
	c := Service(t)
	ep := strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "https://"), "http://")
	cl, err := minio.New(ep, &minio.Options{Creds: credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""), Secure: !c.NoTLS})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ib-test-%d-%d", time.Now().UnixNano()%1_000_000_000, counter.Add(1))
	if err := cl.MakeBucket(context.Background(), name, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}
	return c, name
}
