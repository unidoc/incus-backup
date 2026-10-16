package service_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"

	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/service"
)

func levelOf(rep *service.RepoCheckReport, id string) string {
	for _, c := range rep.Checks {
		if c.ID == id {
			return c.Level
		}
	}
	return ""
}

func detailOf(rep *service.RepoCheckReport, id string) string {
	for _, c := range rep.Checks {
		if c.ID == id {
			return c.Detail
		}
	}
	return ""
}

// The filesystem check explains every situation an operator meets, and creates nothing.
func TestCheckFilesystemSettingsExplainsAndCreatesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	fresh := filepath.Join(dir, "a", "b")
	rep, err := e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new", Kind: "filesystem", Path: fresh})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ready || levelOf(rep, "path") != "pass" || !strings.Contains(detailOf(rep, "path"), "will create") || rep.FreeBytes == nil {
		t.Fatalf("missing dir: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatal("the check created a directory")
	}
	// relative path, taken name, not empty, already a repository
	for name, req := range map[string]service.RepoCheckRequest{
		"relative":  {Name: "new", Kind: "filesystem", Path: "rel/dir"},
		"taken":     {Name: "custody", Kind: "filesystem", Path: fresh},
		"bad name":  {Name: "-x", Kind: "filesystem", Path: fresh},
		"same path": {Name: "new", Kind: "filesystem", Path: e.repo.Path},
	} {
		rep, err := e.svc.CheckRepositorySettings(ctx, req)
		if err != nil || rep.Ready {
			t.Errorf("%s: ready=%v err=%v %+v", name, rep != nil && rep.Ready, err, rep)
		}
	}
	notEmpty := filepath.Join(dir, "full")
	_ = os.MkdirAll(notEmpty, 0o750)
	_ = os.WriteFile(filepath.Join(notEmpty, "x"), []byte("x"), 0o600)
	rep, _ = e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new", Kind: "filesystem", Path: notEmpty})
	if rep.Ready || !strings.Contains(detailOf(rep, "path"), "not empty") {
		t.Fatalf("not empty: %+v", rep.Checks)
	}
	// create on an existing repository says "attach"; attach on it reports its id and that WE own it
	rep, _ = e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new", Kind: "filesystem", Path: e.repo.Path})
	if rep.Ready || !strings.Contains(detailOf(rep, "path"), "Attach") {
		t.Fatalf("create on a repository: %+v", rep.Checks)
	}
	rep, _ = e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new", Kind: "filesystem", Path: e.repo.Path, Attach: true})
	if !rep.Datastore.Exists || rep.Datastore.ID != e.repo.DatastoreID || !rep.Datastore.OwnedByThisServer || rep.Datastore.AttachedAs != "custody" || rep.Ready {
		t.Fatalf("attach own repository: %+v %+v", rep.Datastore, rep.Checks)
	}
	// attach on an empty directory is refused with the way out
	rep, _ = e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new", Kind: "filesystem", Path: dir, Attach: true})
	if rep.Ready || !strings.Contains(detailOf(rep, "path"), "Create") {
		t.Fatalf("attach on a non-repository: %+v", rep.Checks)
	}
	// a repository owned by ANOTHER server: fail without adopt, warn with it
	other := newEnv(t)
	rep, _ = e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new", Kind: "filesystem", Path: other.repo.Path, Attach: true})
	if rep.Ready || !rep.NeedsAdopt || levelOf(rep, "owner") != "fail" || rep.Datastore.Owner == nil {
		t.Fatalf("foreign owner: %+v %+v", rep.Datastore, rep.Checks)
	}
	rep, _ = e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new", Kind: "filesystem", Path: other.repo.Path, Attach: true, Adopt: true})
	if !rep.Ready || levelOf(rep, "owner") != "warn" {
		t.Fatalf("foreign owner with adopt: %+v", rep.Checks)
	}
	// ... and the prediction holds: the real attach with adopt works
	if _, err := e.svc.AttachRepository(ctx, "new", other.repo.Path, true); err != nil {
		t.Fatalf("attach predicted ready failed: %v", err)
	}
	// unwritable directory (a non-root user only: root passes every access check)
	if os.Geteuid() != 0 {
		ro := filepath.Join(dir, "ro")
		_ = os.MkdirAll(ro, 0o500)
		rep, _ = e.svc.CheckRepositorySettings(ctx, service.RepoCheckRequest{Name: "new2", Kind: "filesystem", Path: filepath.Join(ro, "sub")})
		if rep.Ready || !strings.Contains(detailOf(rep, "path"), "cannot create") {
			t.Fatalf("unwritable parent: %+v", rep.Checks)
		}
	}
	// the existing-repository check passes and does not report itself as a conflict
	rep, err = e.svc.CheckRepository(ctx, "custody")
	if err != nil || !rep.Ready || levelOf(rep, "open") != "pass" || levelOf(rep, "attached") != "" {
		t.Fatalf("existing: %v %+v", err, rep.Checks)
	}
	info, err := e.svc.RepositoryInfo(ctx, "custody")
	if err != nil || info.Owner == nil || info.Owner.ServerID != info.ServerID || info.DatastoreID != e.repo.DatastoreID || info.Created == nil {
		t.Fatalf("info: %v %+v", err, info)
	}
}

func s3Client(t *testing.T, c s3test.Config) *minio.Client { return s3test.Client(t, c) }

// The S3 check runs every probe step, finds an existing repository and its owner, and warns about
// bucket settings that cost space or destroy chunks (versioning, lifecycle expiry, object lock).
func TestCheckS3SettingsProbesAndWarnsAboutBucketConfig(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc, bucket := s3test.NewBucket(t)
	s3set := &service.S3Settings{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: "ib", NoTLS: svc.NoTLS, PathStyle: true}
	req := service.RepoCheckRequest{Name: "off", Kind: "s3", S3: s3set, AccessKey: svc.AccessKey, SecretKey: svc.SecretKey}
	rep, err := e.svc.CheckRepositorySettings(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"credential", "connect", "put", "conditional", "get", "stat", "list", "delete", "datastore", "versioning", "lifecycle", "object-lock"} {
		if l := levelOf(rep, id); l != "pass" {
			t.Errorf("%s: %q (%s)", id, l, detailOf(rep, id))
		}
	}
	if levelOf(rep, "tls") != "warn" || !rep.Ready {
		t.Fatalf("plain-HTTP S3 must be a warning, not a failure: %+v", rep.Checks)
	}
	if cs, _ := e.svc.Credentials(ctx); len(cs) != 0 {
		t.Fatal("the check stored a credential")
	}
	// nothing was written but the probe's scratch object, which is gone again
	cl := s3Client(t, svc)
	for o := range cl.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		t.Errorf("object left behind: %s", o.Key)
	}

	// versioning + an expiry rule on the prefix: two warnings, still usable
	if err := cl.EnableVersioning(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	lc := lifecycle.NewConfiguration()
	lc.Rules = []lifecycle.Rule{{ID: "cleanup", Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: "ib/"}, Expiration: lifecycle.Expiration{Days: 30}}}
	if err := cl.SetBucketLifecycle(ctx, bucket, lc); err != nil {
		t.Fatal(err)
	}
	rep, _ = e.svc.CheckRepositorySettings(ctx, req)
	if levelOf(rep, "versioning") != "warn" || levelOf(rep, "lifecycle") != "warn" || !strings.Contains(detailOf(rep, "lifecycle"), "cleanup") || !rep.Ready {
		t.Fatalf("versioned bucket with expiry: %+v", rep.Checks)
	}
	// a rule for another prefix does not concern us
	lc.Rules[0].RuleFilter.Prefix = "logs/"
	_ = cl.SetBucketLifecycle(ctx, bucket, lc)
	rep, _ = e.svc.CheckRepositorySettings(ctx, req)
	if levelOf(rep, "lifecycle") != "pass" {
		t.Fatalf("rule on another prefix: %+v", rep.Checks)
	}

	// object lock with a default retention
	lockBucket := bucket + "-lock"
	if err := cl.MakeBucket(ctx, lockBucket, minio.MakeBucketOptions{ObjectLocking: true}); err != nil {
		t.Fatal(err)
	}
	mode, days, unit := minio.Governance, uint(7), minio.Days
	if err := cl.SetObjectLockConfig(ctx, lockBucket, &mode, &days, &unit); err != nil {
		t.Fatal(err)
	}
	lreq := req
	lreq.S3 = &service.S3Settings{Endpoint: svc.Endpoint, Bucket: lockBucket, Prefix: "ib", NoTLS: svc.NoTLS, PathStyle: true}
	rep, _ = e.svc.CheckRepositorySettings(ctx, lreq)
	if levelOf(rep, "object-lock") != "warn" || !strings.Contains(detailOf(rep, "object-lock"), "7 days") {
		t.Fatalf("object lock: %+v", rep.Checks)
	}

	// failures name the step: wrong secret, missing bucket, refused port
	for name, mut := range map[string]func(*service.RepoCheckRequest){
		"wrong secret": func(r *service.RepoCheckRequest) { r.SecretKey = "wrong-wrong-wrong" },
		"no bucket": func(r *service.RepoCheckRequest) {
			r.S3 = &service.S3Settings{Endpoint: svc.Endpoint, Bucket: "no-such-bucket-xyz", NoTLS: true, PathStyle: true}
		},
		"refused": func(r *service.RepoCheckRequest) {
			r.S3 = &service.S3Settings{Endpoint: "127.0.0.1:1", Bucket: bucket, NoTLS: true, PathStyle: true}
		},
	} {
		r := req
		mut(&r)
		rep, err := e.svc.CheckRepositorySettings(ctx, r)
		if err != nil || rep.Ready || levelOf(rep, "connect") != "fail" || levelOf(rep, "delete") != "skip" {
			t.Errorf("%s: %v %+v", name, err, rep)
		}
	}

	// create the repository, then: create again = "attach instead"; attach from another server = adopt
	if _, err := e.svc.AddS3Repository(ctx, "off", service.S3Params{S3Settings: *s3set, AccessKey: svc.AccessKey, SecretKey: svc.SecretKey}); err != nil {
		t.Fatal(err)
	}
	req.Name = "off2"
	rep, _ = e.svc.CheckRepositorySettings(ctx, req)
	if rep.Ready || levelOf(rep, "datastore") != "fail" || !rep.Datastore.Exists {
		t.Fatalf("create on an existing repository: %+v", rep.Checks)
	}
	other := newEnv(t)
	req.Attach = true
	rep, _ = other.svc.CheckRepositorySettings(ctx, req)
	if rep.Ready || !rep.NeedsAdopt || rep.Datastore.Owner == nil || rep.Datastore.Owner.ServerID == rep.ServerID {
		t.Fatalf("attach a foreign S3 repository: %+v %+v", rep.Datastore, rep.Checks)
	}
	// an existing credential by name works the same, and the existing-repository check passes
	creq := service.RepoCheckRequest{Name: "off3", Kind: "s3", S3: s3set, Credential: "off-s3", Attach: true}
	rep, _ = e.svc.CheckRepositorySettings(ctx, creq)
	if levelOf(rep, "credential") != "pass" || levelOf(rep, "connect") != "pass" || rep.Datastore.AttachedAs != "off" {
		t.Fatalf("stored credential: %+v", rep.Checks)
	}
	rep, err = e.svc.CheckRepository(ctx, "off")
	if err != nil || !rep.Ready || levelOf(rep, "open") != "pass" || levelOf(rep, "owner") != "pass" {
		t.Fatalf("existing S3: %v %+v", err, rep.Checks)
	}
	info, err := e.svc.RepositoryInfo(ctx, "off")
	if err != nil || info.Owner == nil || info.Owner.ServerID != info.ServerID {
		t.Fatalf("info: %v %+v", err, info)
	}
}

// The check honours the maintenance mode: read-only writes nothing (no probe object), offline touches nothing.
func TestCheckRepositoryHonoursMaintenanceMode(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	if err := e.svc.SetRepositoryMode(ctx, "custody", "read-only", "test"); err != nil {
		t.Fatal(err)
	}
	// with versioning on, a probe object written and deleted again leaves a version under _probe/
	var set service.S3Settings
	if err := json.Unmarshal(e.repo.S3Raw, &set); err != nil {
		t.Fatal(err)
	}
	cl := s3Client(t, s3test.Service(t))
	if err := cl.EnableVersioning(ctx, set.Bucket); err != nil {
		t.Fatal(err)
	}
	probeVersions := func() int {
		n := 0
		for o := range cl.ListObjects(ctx, set.Bucket, minio.ListObjectsOptions{Prefix: "ib/_probe/", Recursive: true, WithVersions: true}) {
			if o.Err == nil {
				n++
			}
		}
		return n
	}
	rep, err := e.svc.CheckRepository(ctx, "custody")
	if err != nil {
		t.Fatal(err)
	}
	if n := probeVersions(); n != 0 {
		t.Fatalf("the read-only check wrote %d probe versions", n)
	}
	if levelOf(rep, "put") != "skip" || levelOf(rep, "delete") != "skip" || levelOf(rep, "get") != "pass" || levelOf(rep, "connect") != "pass" || !rep.Ready {
		t.Fatalf("read-only check: %+v", rep.Checks)
	}
	// negative control: online, the same check does write its scratch object
	if err := e.svc.SetRepositoryMode(ctx, "custody", "online", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CheckRepository(ctx, "custody"); err != nil {
		t.Fatal(err)
	}
	if probeVersions() == 0 {
		t.Fatal("negative control: the online check left no probe version, so the read-only assertion proves nothing")
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "offline", "test"); err != nil {
		t.Fatal(err)
	}
	rep, _ = e.svc.CheckRepository(ctx, "custody")
	if len(rep.Checks) != 1 || rep.Checks[0].ID != "mode" || rep.Checks[0].Level != "skip" {
		t.Fatalf("offline check: %+v", rep.Checks)
	}
	info, _ := e.svc.RepositoryInfo(ctx, "custody")
	if info.Owner != nil || !strings.Contains(info.OwnerError, "offline") {
		t.Fatalf("offline info: %+v", info)
	}
}
