// Package s3test gives tests an S3-compatible service and a fresh bucket.
//
// By default the service is an in-process fake (package s3fake): hermetic (no binary, no network
// beyond loopback), fast, strict about the S3 behaviour incus-backup relies on, and it can inject
// faults. Its conformance suite (internal/s3test/conformance) is the proof that it behaves like a
// real S3 service, and runs against any real service too.
//
// INCUS_BACKUP_TEST_S3_ENDPOINT / _ACCESS_KEY / _SECRET_KEY point the SAME tests at a real
// S3-compatible service instead (host:port is plain HTTP, https://host:port is TLS); see
// docs/testing-s3.md. INCUS_BACKUP_TEST_S3_UNSUPPORTED lists optional features that service lacks
// (see conformance.Features). If the endpoint is set but unusable the tests FAIL: an S3 test that
// silently skips proves nothing.
//
// Packages using it must call Main from TestMain (or Start, next to testdb).
package s3test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/unidoc/incus-backup/internal/s3test/s3fake"
)

// Server is the in-process fake; tests that want direct control (fault injection, request log,
// counters, state inspection) create their own with NewServer.
type Server = s3fake.Server

// Options configure a private fake server.
type Options = s3fake.Options

var (
	endpoint, access, secret string
	startErr                 error
	counter                  atomic.Int64
	shared                   *s3fake.Server
)

// Main starts the shared service, runs the tests and stops it.
func Main(m *testing.M) {
	stop := Start()
	code := m.Run()
	if stop != nil {
		stop()
	}
	os.Exit(code)
}

// Start starts the shared service (the in-process fake, or nothing if INCUS_BACKUP_TEST_S3_ENDPOINT
// names an external one) and returns its stop function (may be nil).
// Use it with testdb.AtExit when a package also needs PostgreSQL:
//
//	testdb.AtExit(s3test.Start()); testdb.Main(m)
func Start() (stop func()) {
	if e := os.Getenv("INCUS_BACKUP_TEST_S3_ENDPOINT"); e != "" {
		endpoint = e
		access = os.Getenv("INCUS_BACKUP_TEST_S3_ACCESS_KEY")
		secret = os.Getenv("INCUS_BACKUP_TEST_S3_SECRET_KEY")
		if access == "" || secret == "" {
			startErr = fmt.Errorf("INCUS_BACKUP_TEST_S3_ENDPOINT is set but INCUS_BACKUP_TEST_S3_ACCESS_KEY / _SECRET_KEY are not")
		}
		return nil
	}
	srv, err := s3fake.New(s3fake.Options{})
	if err != nil {
		startErr = fmt.Errorf("starting the in-process S3 fake: %w", err)
		return nil
	}
	shared = srv
	endpoint, access, secret = srv.Endpoint(), srv.AccessKey(), srv.SecretKey()
	return srv.Close
}

// External reports whether the shared service is a real, external one (INCUS_BACKUP_TEST_S3_*).
func External() bool { return os.Getenv("INCUS_BACKUP_TEST_S3_ENDPOINT") != "" }

// Shared is the package-wide in-process fake, or nil when an external service is configured.
func Shared() *Server { return shared }

// Unsupported lists the optional features the external service lacks (INCUS_BACKUP_TEST_S3_UNSUPPORTED,
// comma separated). Always empty for the fake.
func Unsupported() []string {
	var out []string
	for _, f := range strings.Split(os.Getenv("INCUS_BACKUP_TEST_S3_UNSUPPORTED"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Config is a connection to a service.
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

// NewServer starts a private fake for one test (stopped by t.Cleanup) and returns it with its
// connection settings. It is independent of INCUS_BACKUP_TEST_S3_*: fault injection and request
// logs need the fake, and they run in every environment.
func NewServer(t testing.TB, opts ...Options) (*Server, Config) {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	srv, err := s3fake.New(o)
	if err != nil {
		t.Fatalf("starting the in-process S3 fake: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, ConfigOf(srv)
}

// ConfigOf is the connection to a fake.
func ConfigOf(srv *Server) Config {
	return Config{Endpoint: srv.Endpoint(), AccessKey: srv.AccessKey(), SecretKey: srv.SecretKey(), NoTLS: true}
}

// Client returns a minio client for c.
func Client(t testing.TB, c Config) *minio.Client {
	t.Helper()
	ep := strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "https://"), "http://")
	cl, err := minio.New(ep, &minio.Options{Creds: credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""), Secure: !c.NoTLS})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// NewBucket creates a fresh, empty bucket on the shared service and returns its config plus its name.
func NewBucket(t testing.TB) (Config, string) {
	t.Helper()
	return NewBucketAt(t, Service(t))
}

// NewBucketAt creates a fresh, empty bucket on the service c.
func NewBucketAt(t testing.TB, c Config) (Config, string) {
	t.Helper()
	cl := Client(t, c)
	name := fmt.Sprintf("ib-test-%d-%d", time.Now().UnixNano()%1_000_000_000, counter.Add(1))
	if err := cl.MakeBucket(context.Background(), name, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}
	return c, name
}
