// Package testsupport builds complete in-process incus-backup servers (own
// PostgreSQL database, datastore and CA, served over real mutual TLS) for tests
// of the client engines and the synchroniser. Callers must run testdb.Main.
package testsupport

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/httpapi"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

// Node is one running server.
type Node struct {
	T    testing.TB
	Svc  *service.Service
	Repo *service.Repo
	Srv  *httptest.Server
	Now  time.Time
}

// NewNode starts a server with a repository called "custody" on a filesystem datastore.
func NewNode(t testing.TB) *Node { t.Helper(); return newNode(t, false) }

// NewS3Node starts a server whose repository "custody" lives in a fresh bucket of the
// S3 test service (callers must also run s3test.Main). Everything else is identical.
func NewS3Node(t testing.TB) *Node { t.Helper(); return newNode(t, true) }

func newNode(t testing.TB, s3 bool) *Node {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	n := &Node{T: t, Now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	n.Svc = service.New(d, filepath.Join(dir, "state"), "test", log)
	n.Svc.Now = func() time.Time { return n.Now }
	// tests drive hosts with their own fake clocks; the future-date guard has its own test
	n.Svc.MaxClockSkew = 100 * 365 * 24 * time.Hour
	if err := n.Svc.InitPKI([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if s3 {
		svc, bucket := s3test.NewBucket(t)
		n.Repo, err = n.Svc.CreateS3Repository(ctx, "custody", datastore.S3Config{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: "ib",
			AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: svc.NoTLS, PathStyle: true})
	} else {
		n.Repo, err = n.Svc.CreateRepository(ctx, "custody", filepath.Join(dir, "ds"))
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := n.Svc.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	// the production server (timeouts, HTTP/2 and its windows), not a bare handler
	n.Srv = httptest.NewUnstartedServer(nil)
	n.Srv.Config = (&httpapi.Server{Svc: n.Svc, Log: log}).NewHTTPServer()
	n.Srv.EnableHTTP2 = true
	n.Srv.TLS = cfg
	n.Srv.StartTLS()
	t.Cleanup(n.Srv.Close)
	return n
}

// Addr is host:port of the server.
func (n *Node) Addr() string { return n.Srv.Listener.Addr().String() }

// Credentials are what enrolment yields.
type Credentials struct {
	CA, Cert, Key []byte
}

// Enrol runs the real enrolment flow for a host/peer.
func (n *Node) Enrol(name, scope string) Credentials {
	n.T.Helper()
	tok, err := n.Svc.CreateToken(context.Background(), "custody", name, scope, time.Hour)
	if err != nil {
		n.T.Fatal(err)
	}
	en, err := remote.Enroll(context.Background(), n.Addr(), tok, name, nil)
	if err != nil {
		n.T.Fatal(err)
	}
	return Credentials{CA: en.CAPEM, Cert: en.CertPEM, Key: en.KeyPEM}
}

// Client returns an API client for the credentials.
func (n *Node) Client(c Credentials, domain string) *remote.Client {
	n.T.Helper()
	cl, err := remote.New(n.Addr(), c.CA, c.Cert, c.Key, domain)
	if err != nil {
		n.T.Fatal(err)
	}
	return cl
}
