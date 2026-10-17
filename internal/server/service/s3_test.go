package service_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/service"
)

func TestS3RepositoryProbeReportsEveryOperation(t *testing.T) {
	e := newEnvS3(t)
	rs, err := e.svc.ProbeRepository(context.Background(), "custody")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"connect + bucket exists", "put", "put if-absent", "get", "stat", "list", "delete", "open repository"}
	for _, w := range want {
		found := false
		for _, r := range rs {
			if strings.HasPrefix(r.Op, w) {
				found = true
				if !r.OK {
					t.Errorf("%s failed: %s", r.Op, r.Err)
				}
			}
		}
		if !found {
			t.Errorf("probe has no %q check", w)
		}
	}
}

// A repository on a bucket that does not exist, with wrong credentials, or an
// unreachable endpoint is refused at creation with a clear reason.
func TestCreateS3RepositoryRefusesUnusableTargets(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	svc, bucket := s3test.NewBucket(t)
	base := datastore.S3Config{Endpoint: svc.Endpoint, Bucket: bucket, AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: svc.NoTLS, PathStyle: true}
	for name, mut := range map[string]func(*datastore.S3Config){
		"no-bucket":    func(c *datastore.S3Config) { c.Bucket = "does-not-exist-bucket" },
		"bad-secret":   func(c *datastore.S3Config) { c.SecretKey = "wrong-wrong-wrong" },
		"unreachable":  func(c *datastore.S3Config) { c.Endpoint = "127.0.0.1:1" },
		"missing-keys": func(c *datastore.S3Config) { c.AccessKey = "" },
		"tls-vs-http":  func(c *datastore.S3Config) { c.NoTLS = false },
	} {
		c := base
		mut(&c)
		if _, err := e.svc.CreateS3Repository(ctx, "r-"+name, c); err == nil {
			t.Errorf("%s: repository was created on an unusable target", name)
		}
		if _, err := e.svc.Repository(ctx, "r-"+name); err == nil {
			t.Errorf("%s: a row was left behind", name)
		}
		if cs, _ := e.svc.Credentials(ctx); len(cs) != 1 {
			t.Errorf("%s: a credential was left behind (%d)", name, len(cs))
		}
	}
}

// The S3 secret key is sealed: PostgreSQL holds only ciphertext, the sealing key is a 0600
// file in the state directory, and no API returns a secret after creation.
func TestS3SecretIsSealedAndNeverReadBack(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	kp := filepath.Join(e.svc.StateDir, "secrets.key")
	st, err := os.Stat(kp)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("sealing key: %v %v", err, st)
	}
	var sealed []byte
	var settings string
	if err := e.svc.DB.Pool().QueryRow(ctx, `SELECT c.sealed, r.s3_settings::text FROM credentials c JOIN repositories r ON r.credential_id=c.id WHERE r.name='custody'`).Scan(&sealed, &settings); err != nil {
		t.Fatal(err)
	}
	// The credentials of whichever S3 service the tests use (s3test: the in-process fake, or
	// INCUS_BACKUP_TEST_S3_*), never hard-coded ones.
	creds := s3test.Service(t)
	for _, blob := range []string{string(sealed), settings} {
		if strings.Contains(blob, creds.SecretKey) || strings.Contains(blob, creds.AccessKey) {
			t.Fatal("a credential is stored in clear text in PostgreSQL")
		}
	}
	cs, err := e.svc.Credentials(ctx)
	if err != nil || len(cs) != 1 || cs[0].AccessKey != creds.AccessKey || len(cs[0].UsedBy) != 1 {
		t.Fatalf("%v %+v", err, cs)
	}
	// Losing the sealing key makes the credential unreadable: the database alone is useless
	if err := os.Remove(kp); err != nil {
		t.Fatal(err)
	}
	e.svc.DropStoresForTest()
	if err := os.WriteFile(kp, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Store(e.repo); err == nil {
		t.Fatal("a repository opened with a credential sealed under a different key")
	}
}

func TestCredentialRotationAndDeletion(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	if err := e.svc.DeleteCredential(ctx, "custody-s3"); err == nil {
		t.Fatal("a credential in use was deleted")
	}
	svc, _ := s3test.NewBucket(t)
	if _, err := e.svc.Store(e.repo); err != nil { // an OPEN store, as in a running daemon
		t.Fatal(err)
	}
	if err := e.svc.RotateS3Credential(ctx, "custody-s3", service.S3Secret{AccessKey: svc.AccessKey, SecretKey: "wrong-wrong-wrong"}); err != nil {
		t.Fatal(err)
	}
	// the open store is re-bound in place (its sessions keep their pins) and now really fails
	if st, err := e.svc.Store(e.repo); err != nil {
		t.Fatalf("an open store must be kept, not replaced: %v", err)
	} else if _, err := st.Backend().Get("datastore.json"); err == nil {
		t.Fatal("the open store still used the old (valid) secret after the rotation")
	}
	rs, _ := e.svc.ProbeRepository(ctx, "custody")
	failed := false
	for _, r := range rs {
		failed = failed || !r.OK
	}
	if !failed {
		t.Fatal("probe accepted a wrong secret key")
	}
	if err := e.svc.RotateS3Credential(ctx, "custody-s3", service.S3Secret{AccessKey: svc.AccessKey, SecretKey: svc.SecretKey}); err != nil {
		t.Fatal(err)
	}
	rs, _ = e.svc.ProbeRepository(ctx, "custody")
	for _, r := range rs {
		if !r.OK {
			t.Fatalf("after restoring the right secret: %s: %s", r.Op, r.Err)
		}
	}
	if st, _ := e.svc.Store(e.repo); st != nil {
		if _, err := st.Backend().Get("datastore.json"); err != nil {
			t.Fatalf("the open store did not pick the restored secret up: %v", err)
		}
	}
}

// Two servers must not adopt the same bucket: the second creation/attach is refused until the
// first is explicitly replaced.
func TestSecondServerCannotAdoptAnOwnedBucket(t *testing.T) {
	a := newEnvS3(t)
	b := newEnvOn(t, false) // another server (own PostgreSQL, own identity)
	ctx := context.Background()
	st, _ := a.svc.Store(a.repo)
	d := st.Backend().Describe()
	_ = d
	raw, _ := a.svc.Repository(ctx, "custody")
	var set service.S3Settings
	if err := json.Unmarshal(raw.S3Raw, &set); err != nil {
		t.Fatal(err)
	}
	creds := s3test.Service(t)
	sec := service.S3Params{S3Settings: set, AccessKey: creds.AccessKey, SecretKey: creds.SecretKey}
	if _, err := b.svc.AttachS3Repository(ctx, "stolen", sec, false); err == nil || !strings.Contains(err.Error(), "belongs to another") {
		t.Fatalf("second server attached an owned bucket: %v", err)
	}
	if _, err := b.svc.AttachS3Repository(ctx, "stolen", sec, true); err != nil {
		t.Fatalf("explicit adoption (disaster recovery): %v", err)
	}
	// the old server must now refuse to operate on it
	a.svc.DropStoresForTest()
	if _, err := a.svc.Store(a.repo); err == nil || !strings.Contains(err.Error(), "belongs to another") {
		t.Fatalf("the former owner kept using the repository: %v", err)
	}
}

// Real garbage collection on a bucket removes nothing that is live or young, and the
// repository still verifies clean afterwards.
func TestGCOnS3KeepsLiveAndYoungChunks(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	if err := e.upload(h, k, group, snapID(e.now, 1), e.now, "p", "one", "two"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.svc.Store(e.repo)
	oid, oblob, _ := k.EncodeChunk([]byte("orphan"), format.EncodeOpts{})
	if _, err := st.PutChunk(oid, oblob); err != nil {
		t.Fatal(err)
	}
	gs, err := e.svc.GC(ctx, "custody", service.DefaultGCGrace, false)
	if err != nil || gs.Removed != 0 || gs.Live != 2 || gs.KeptYoung != 1 {
		t.Fatalf("%v %+v", err, gs)
	}
	vs, err := e.svc.Verify(ctx, "custody")
	if err != nil || len(vs.Problems) != 0 {
		t.Fatalf("%v %+v", err, vs)
	}
	if err := st.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// Capacity on an object store: usage is real, free space is reported as unavailable (0), and
// no forecast is invented.
func TestCapacityOnS3IsHonestAboutWhatItCannotKnow(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	if err := e.upload(h, keys(1), group, snapID(e.now, 1), e.now, "p", "aaaa", "bbbb"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SampleCapacity(ctx, "custody"); err != nil {
		t.Fatal(err)
	}
	c, err := e.svc.Capacity(ctx, "custody")
	if err != nil || c.Chunks != 2 || c.PhysicalBytes == 0 {
		t.Fatalf("%v %+v", err, c)
	}
	if c.FSTotal != 0 || c.FSFree != 0 || c.Projected80 != nil || c.ProjectedFull != nil {
		t.Fatalf("an object store must not report filesystem space or a fill forecast: %+v", c)
	}
}
