package conformance_test

import (
	"os"
	"testing"

	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/s3test/conformance"
	"github.com/unidoc/incus-backup/internal/s3test/s3fake"
)

func fakeTarget(t *testing.T, opts s3fake.Options) (*s3fake.Server, conformance.Target) {
	t.Helper()
	srv, err := s3fake.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv, conformance.Target{Endpoint: srv.Endpoint(), AccessKey: srv.AccessKey(), SecretKey: srv.SecretKey()}
}

// The configured S3 service passes the whole conformance suite. By default that is the in-process
// fake (every normal `go test` runs it, which keeps the fake honest); with
// INCUS_BACKUP_TEST_S3_ENDPOINT/_ACCESS_KEY/_SECRET_KEY (and optionally _UNSUPPORTED) it is that real
// service, which is how the fake is shown to behave like one (CI job s3-conformance).
func TestConformance(t *testing.T) {
	if s3test.External() {
		ep := os.Getenv("INCUS_BACKUP_TEST_S3_ENDPOINT")
		t.Logf("conformance against the external service %s (unsupported features declared: %v)", ep, s3test.Unsupported())
		conformance.RunConformance(t, conformance.Target{Endpoint: ep, AccessKey: os.Getenv("INCUS_BACKUP_TEST_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("INCUS_BACKUP_TEST_S3_SECRET_KEY"), Unsupported: s3test.Unsupported()})
		return
	}
	_, tgt := fakeTarget(t, s3fake.Options{})
	conformance.RunConformance(t, tgt)
}
