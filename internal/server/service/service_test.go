package service_test

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) {
	testdb.AtExit(s3test.Start())
	testdb.Main(m)
}

type env struct {
	t    *testing.T
	svc  *service.Service
	repo *service.Repo
	now  time.Time
}

func newEnv(t *testing.T) *env { t.Helper(); return newEnvOn(t, false) }

// newEnvS3 is the same server with its repository in a fresh S3 bucket.
func newEnvS3(t *testing.T) *env { t.Helper(); return newEnvOn(t, true) }

func newEnvOn(t *testing.T, s3 bool) *env {
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
	e := &env{t: t, now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	e.svc = service.New(d, filepath.Join(dir, "state"), "test", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	e.svc.Now = func() time.Time { return e.now }
	if err := e.svc.InitPKI(nil); err != nil {
		t.Fatal(err)
	}
	if s3 {
		svc, bucket := s3test.NewBucket(t)
		e.repo, err = e.svc.CreateS3Repository(ctx, "custody", datastore.S3Config{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: "ib",
			AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: svc.NoTLS, PathStyle: true})
	} else {
		e.repo, err = e.svc.CreateRepository(ctx, "custody", filepath.Join(dir, "ds"))
	}
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func keys(seed byte) *format.Keys {
	m := make([]byte, 32)
	for i := range m {
		m[i] = seed + byte(i)
	}
	k, _ := format.DeriveKeys(m)
	return k
}

// enrol runs the real enrolment: token -> CSR -> signed certificate -> identity.
func (e *env) enrol(name, scope string) *service.Host {
	h, _ := e.enrolCert(name, scope)
	return h
}

func (e *env) enrolCert(name, scope string) (*service.Host, *x509.Certificate) {
	e.t.Helper()
	ctx := context.Background()
	tok, err := e.svc.CreateToken(ctx, "custody", name, scope, time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	_, csr, _ := pki.NewKeyAndCSR(name)
	resp, err := e.svc.Enroll(ctx, api.EnrollRequest{Token: tok, CSR: string(csr)})
	if err != nil {
		e.t.Fatal(err)
	}
	cert, _ := pki.ParseCertPEM([]byte(resp.Certificate))
	h, err := e.svc.HostFromCert(ctx, cert, "127.0.0.1")
	if err != nil {
		e.t.Fatal(err)
	}
	return h, cert
}

const group = "0123456789abcdef0123456789abcdef"

// upload runs a full upload session with the given plaintext chunks.
func (e *env) upload(h *service.Host, k *format.Keys, grp, snap string, created time.Time, policy string, parts ...string) error {
	e.t.Helper()
	e.now = created // the server commits "now": an honest host dates its snapshot now
	return e.uploadAt(h, k, grp, snap, created, policy, parts...)
}

func (e *env) uploadAt(h *service.Host, k *format.Keys, grp, snap string, created time.Time, policy string, parts ...string) error {
	e.t.Helper()
	ctx := context.Background()
	resp, err := e.svc.BeginSession(ctx, h, k.DomainID, api.SessionRequest{GroupID: grp, SnapshotID: snap, Created: created, Policy: policy, Project: "p", Object: "vm/root", Kind: "block"})
	if err != nil {
		return err
	}
	x := &format.Index{Kind: format.IndexDynamic}
	var off uint64
	var ids []string
	blobs := map[string][]byte{}
	for _, p := range parts {
		id, blob, err := k.EncodeChunk([]byte(p), format.EncodeOpts{})
		if err != nil {
			e.t.Fatal(err)
		}
		off += uint64(len(p))
		x.Entries = append(x.Entries, format.Entry{End: off, ID: id})
		ids = append(ids, id.String())
		blobs[id.String()] = blob
	}
	x.Size = off
	miss, err := e.svc.SessionMissing(ctx, h, resp.Session, ids)
	if err != nil {
		return err
	}
	for _, m := range miss {
		if err := e.svc.SessionPutChunk(ctx, h, resp.Session, m, blobs[m]); err != nil {
			return err
		}
	}
	idx, _ := x.Marshal()
	if err := e.svc.SessionPutIndex(ctx, h, resp.Session, idx); err != nil {
		return err
	}
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created, Object: format.ObjectInfo{Name: "vm/root", Kind: "block", Size: off}, IndexDigest: format.IndexDigest(idx)}, grp, nil)
	if err := e.svc.SessionPutManifest(ctx, h, resp.Session, man); err != nil {
		return err
	}
	_, err = e.svc.SessionCommit(ctx, h, resp.Session, api.CommitRequest{LogicalBytes: int64(off), UploadedBytes: 1})
	return err
}

// upload2 uploads without moving the server clock (the server's "now" stays what it is).
func (e *env) upload2(h *service.Host, k *format.Keys, grp, snap string, created time.Time, policy string, parts ...string) error {
	e.t.Helper()
	saved := e.now
	defer func() { e.now = saved }()
	return e.uploadAt(h, k, grp, snap, created, policy, parts...)
}

func snapID(t time.Time, n int) string {
	return t.UTC().Format("20060102150405") + fmt.Sprintf("-%08x", n)
}

func TestEnrolmentTokenIsOneTimeAndExpires(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tok, _ := e.svc.CreateToken(ctx, "custody", "benfica", api.ScopeBackup, time.Hour)
	_, csr, _ := pki.NewKeyAndCSR("x")
	if _, err := e.svc.Enroll(ctx, api.EnrollRequest{Token: tok, CSR: string(csr)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Enroll(ctx, api.EnrollRequest{Token: tok, CSR: string(csr)}); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("reuse: %v", err)
	}
	tok2, _ := e.svc.CreateToken(ctx, "custody", "h2", api.ScopeBackup, time.Minute)
	e.now = e.now.Add(2 * time.Minute)
	if _, err := e.svc.Enroll(ctx, api.EnrollRequest{Token: tok2, CSR: string(csr)}); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := e.svc.Enroll(ctx, api.EnrollRequest{Token: "ibt1." + string(make([]byte, 0)) + "x", CSR: string(csr)}); err == nil {
		t.Fatal("garbage token accepted")
	}
	if _, err := e.svc.CreateToken(ctx, "custody", "x", "admin", time.Hour); err == nil {
		t.Fatal("admin scope token must not exist")
	}
}

func TestHostFromCertAndRevocation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h, cert := e.enrolCert("benfica", api.ScopeBackup)
	if h.Name != "benfica" || h.Scope != api.ScopeBackup {
		t.Fatalf("%+v", h)
	}
	if err := e.svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.HostFromCert(ctx, cert, "127.0.0.1"); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("revoked host still authenticates: %v", err)
	}
	if err := e.svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.HostFromCert(ctx, cert, "127.0.0.1"); err != nil {
		t.Fatalf("re-enabled host refused: %v", err)
	}
	// A certificate from an unknown identity is unauthorized.
	other, _, _ := pki.NewCA("evil", e.now)
	_, csr, _ := pki.NewKeyAndCSR("x")
	pemC, _, _ := other.SignClient(csr, "benfica", e.now, time.Hour)
	ec, _ := pki.ParseCertPEM(pemC)
	if _, err := e.svc.HostFromCert(ctx, ec, "x"); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("unknown certificate: %v", err)
	}
}

func TestBackupSessionHappyPath(t *testing.T)     { happyPathScenario(t, newEnv(t)) }
func TestBackupSessionHappyPathOnS3(t *testing.T) { happyPathScenario(t, newEnvS3(t)) }

func happyPathScenario(t *testing.T, e *env) {
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	snap := snapID(e.now, 1)
	if err := e.upload(h, k, group, snap, e.now, "nightly", "chunk-a", "chunk-b"); err != nil {
		t.Fatal(err)
	}
	l, err := e.svc.Snapshots(ctx, h, group)
	if err != nil || len(l.Snapshots) != 1 || l.Snapshots[0].SnapshotID != snap || l.Snapshots[0].ChunkCount != 2 || l.Snapshots[0].Policy != "nightly" {
		t.Fatalf("%v %+v", err, l)
	}
	if l.Snapshots[0].Object != "vm/root" || l.Snapshots[0].Host != "benfica" {
		t.Fatalf("control-plane names missing: %+v", l.Snapshots[0])
	}
	idx, err := e.svc.ReadIndex(ctx, h, group, snap)
	if err != nil {
		t.Fatal(err)
	}
	pi, err := format.ParseIndex(idx)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := e.svc.ReadChunk(ctx, h, pi.Entries[0].ID.String())
	if err != nil {
		t.Fatal(err)
	}
	pt, err := k.DecodeChunk(blob, pi.Entries[0].ID)
	if err != nil || string(pt) != "chunk-a" {
		t.Fatalf("restore read: %v %q", err, pt)
	}
	// second backup with one new chunk: dedup means "chunk-a"/"chunk-b" are not requested again
	snap2 := snapID(e.now.Add(time.Hour), 2)
	if err := e.upload(h, k, group, snap2, e.now.Add(time.Hour), "nightly", "chunk-a", "chunk-c"); err != nil {
		t.Fatal(err)
	}
	var newChunks int64
	if err := e.svc.DB.Pool().QueryRow(ctx, `SELECT chunks_new FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&newChunks); err != nil || newChunks != 1 {
		t.Fatalf("dedup failed: chunks_new=%d err=%v", newChunks, err)
	}
	if err := e.upload(h, k, group, snap, e.now, "nightly", "chunk-a"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("duplicate snapshot id: %v", err)
	}
}

func TestKeyDomainPinningRejectsOtherKey(t *testing.T) {
	e := newEnv(t)
	h := e.enrol("benfica", api.ScopeBackup)
	if err := e.upload(h, keys(1), group, snapID(e.now, 1), e.now, "p", "a"); err != nil {
		t.Fatal(err)
	}
	err := e.upload(h, keys(77), "fedcba9876543210fedcba9876543210", snapID(e.now, 2), e.now, "p", "a")
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("a different master key must be refused, got %v", err)
	}
}

func TestScopesAndSessionIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	backup := e.enrol("benfica", api.ScopeBackup)
	other := e.enrol("other", api.ScopeBackup)
	syncR := e.enrol("amsterdam", api.ScopeSyncRead)
	k := keys(1)
	if _, err := e.svc.BeginSession(ctx, syncR, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snapID(e.now, 1), Kind: "block"}); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("sync-read must not write backups: %v", err)
	}
	if _, err := e.svc.StartRun(ctx, syncR, api.RunStart{}); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("sync-read must not start runs: %v", err)
	}
	if _, err := e.svc.DesiredState(ctx, syncR); !errors.Is(err, service.ErrForbidden) {
		t.Fatal("sync-read must not read desired state")
	}
	resp, err := e.svc.BeginSession(ctx, backup, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snapID(e.now, 1), Kind: "block", Created: e.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SessionMissing(ctx, other, resp.Session, nil); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("another host used my session: %v", err)
	}
	if err := e.svc.SessionAbort(ctx, backup, resp.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SessionMissing(ctx, backup, resp.Session, nil); err == nil {
		t.Fatal("aborted session still usable")
	}
	// sync-read may read, backup may read
	if _, err := e.svc.Snapshots(ctx, syncR, group); err != nil {
		t.Fatal(err)
	}
}

func TestCommitWithMissingChunkIsConflict(t *testing.T)     { missingChunkScenario(t, newEnv(t)) }
func TestCommitWithMissingChunkIsConflictOnS3(t *testing.T) { missingChunkScenario(t, newEnvS3(t)) }

func missingChunkScenario(t *testing.T, e *env) {
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	snap := snapID(e.now, 1)
	resp, _ := e.svc.BeginSession(ctx, h, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snap, Kind: "block", Created: e.now})
	id := k.ChunkID([]byte("never uploaded"))
	idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: 3, Entries: []format.Entry{{End: 3, ID: id}}}).Marshal()
	_ = e.svc.SessionPutIndex(ctx, h, resp.Session, idx)
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: e.now}, group, nil)
	_ = e.svc.SessionPutManifest(ctx, h, resp.Session, man)
	if _, err := e.svc.SessionCommit(ctx, h, resp.Session, api.CommitRequest{}); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("%v", err)
	}
	if l, _ := e.svc.Snapshots(ctx, h, group); len(l.Snapshots) != 0 {
		t.Fatal("snapshot visible")
	}
}

func TestPolicyPrecedenceAndDesiredState(t *testing.T) {
	as := []api.Assignment{{Policy: "host"}, {Project: "custody", Policy: "proj"}, {Project: "custody", Object: "bosch", Policy: "obj"}}
	cases := map[[2]string]string{{"custody", "bosch"}: "obj", {"custody", "other"}: "proj", {"x", "y"}: "host"}
	for in, want := range cases {
		if got := service.ResolvePolicy(as, in[0], in[1]); got != want {
			t.Fatalf("%v -> %q want %q", in, got, want)
		}
	}
	if service.ResolvePolicy(nil, "a", "b") != "" {
		t.Fatal("empty must resolve to no policy")
	}
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	if err := e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "production", Targets: []string{"cikarang", "amsterdam"}, KeepDaily: 14, KeepWeekly: 8}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "bad", CommitRequired: "maybe"}); err == nil {
		t.Fatal("bad commit_required accepted")
	}
	if err := e.svc.Assign(ctx, "custody", "benfica", "custody", "", "production"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Assign(ctx, "custody", "benfica", "", "obj", "production"); err == nil {
		t.Fatal("object assignment without project accepted")
	}
	if err := e.svc.Assign(ctx, "custody", "nobody", "", "", "production"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
	ds, err := e.svc.DesiredState(ctx, h)
	if err != nil || len(ds.Policies) != 1 || ds.Policies[0].CommitRequired != "all" || len(ds.Policies[0].Targets) != 2 || len(ds.Assignments) != 1 {
		t.Fatalf("%v %+v", err, ds)
	}
	if err := e.svc.Unassign(ctx, "custody", "benfica", "custody", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPruneAppliesPolicyAndNeverTouchesNewest(t *testing.T)     { pruneScenario(t, newEnv(t)) }
func TestPruneAppliesPolicyAndNeverTouchesNewestOnS3(t *testing.T) { pruneScenario(t, newEnvS3(t)) }

func pruneScenario(t *testing.T, e *env) {
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "short", KeepDaily: 3})
	for i := 0; i < 10; i++ {
		ts := e.now.AddDate(0, 0, -i)
		if err := e.upload(h, k, group, snapID(ts, i), ts, "short", fmt.Sprintf("data-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	dry, err := e.svc.Prune(ctx, "custody", true)
	if err != nil || dry.Removed != 7 || dry.Kept != 3 {
		t.Fatalf("dry run: %v %+v", err, dry)
	}
	st, _ := e.svc.Store(e.repo)
	if l, _ := st.ListSnapshots(group); len(l) != 10 {
		t.Fatal("dry run removed something")
	}
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Removed != 7 {
		t.Fatalf("%v %+v", err, res)
	}
	l, _ := e.svc.Snapshots(ctx, h, group)
	if len(l.Snapshots) != 3 {
		t.Fatalf("left %d", len(l.Snapshots))
	}
	if l.Snapshots[2].Created.Before(l.Snapshots[0].Created) {
		t.Fatal("order")
	}
	var n int
	_ = e.svc.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM snapshots`).Scan(&n)
	if n != 3 {
		t.Fatalf("PostgreSQL rows=%d", n)
	}
	// An unknown policy must keep everything (fail safe).
	g2 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for i := 0; i < 5; i++ {
		ts := e.now.AddDate(0, 0, -i)
		_ = e.upload(h, k, g2, snapID(ts, 100+i), ts, "does-not-exist", fmt.Sprintf("u-%d", i))
	}
	res, _ = e.svc.Prune(ctx, "custody", false)
	l2, _ := e.svc.Snapshots(ctx, h, g2)
	if len(l2.Snapshots) != 5 {
		t.Fatalf("unknown policy pruned data: %d left (%+v)", len(l2.Snapshots), res)
	}
}

func TestGCGraceMustExceedSessionLifetime(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.GC(context.Background(), "custody", time.Hour, true); err == nil {
		t.Fatal("unsafe grace accepted")
	}
	if _, err := e.svc.GC(context.Background(), "custody", service.DefaultGCGrace, true); err != nil {
		t.Fatal(err)
	}
}

// The database can be lost and rebuilt from the datastore; and a snapshot that
// vanished from the datastore disappears from the database.
func TestReconcileRebuildsDatabaseFromDatastore(t *testing.T)     { reconcileScenario(t, newEnv(t)) }
func TestReconcileRebuildsDatabaseFromDatastoreOnS3(t *testing.T) { reconcileScenario(t, newEnvS3(t)) }

func reconcileScenario(t *testing.T, e *env) {
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	for i := 0; i < 3; i++ {
		ts := e.now.Add(time.Duration(i) * time.Hour)
		if err := e.upload(h, k, group, snapID(ts, i), ts, "p", fmt.Sprintf("d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.DB.Pool().Exec(ctx, `DELETE FROM snapshots`); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Reconcile(ctx, "custody")
	if err != nil || res.Inserted != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	l, _ := e.svc.Snapshots(ctx, h, group)
	if len(l.Snapshots) != 3 || l.Snapshots[0].ChunkCount != 1 {
		t.Fatalf("%+v", l)
	}
	st, _ := e.svc.Store(e.repo)
	infos, _ := st.ListSnapshots(group)
	_ = st.DeleteSnapshot(group, infos[0].Snapshot)
	res, _ = e.svc.Reconcile(ctx, "custody")
	if res.Removed != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestInventoryAndStatusShowUnprotected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	if err := e.svc.UpsertInventory(ctx, h, api.Inventory{Objects: []api.ObjectInfo{
		{Project: "custody", Name: "vm/root", Kind: "block", GroupID: group},
		{Project: "custody", Name: "biggi", Kind: "filesystem", GroupID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", SharedBy: []string{"a", "b", "c"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UpsertInventory(ctx, h, api.Inventory{Objects: []api.ObjectInfo{{Name: "x", Kind: "weird", GroupID: group}}}); err == nil {
		t.Fatal("bad kind accepted")
	}
	_ = e.upload(h, k, group, snapID(e.now, 1), e.now, "p", "x")
	st, err := e.svc.Status(ctx, "custody")
	if err != nil || len(st.Hosts) != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	hs := st.Hosts[0]
	if hs.Objects != 2 || hs.Protected != 1 || len(hs.Unprotected) != 1 || hs.Unprotected[0] != "custody/biggi" {
		t.Fatalf("%+v", hs)
	}
}

func TestCapacityForecast(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const gib = int64(1) << 30
	// 10 GiB/day growth on a 1000 GiB filesystem with 400 GiB used now.
	for d := 20; d >= 0; d-- {
		taken := e.now.AddDate(0, 0, -d)
		phys := 200*gib + int64(20-d)*10*gib
		if _, err := e.svc.DB.Pool().Exec(ctx, `INSERT INTO capacity_samples (repository_id, taken_at, physical_bytes, logical_bytes, chunk_count, snapshot_count, fs_total_bytes, fs_free_bytes)
			VALUES ($1,$2,$3,$4,1,1,$5,$6)`, e.repo.ID, taken, phys, phys*3, 1000*gib, 1000*gib-phys-200*gib); err != nil {
			t.Fatal(err)
		}
	}
	c, err := e.svc.Capacity(ctx, "custody")
	if err != nil {
		t.Fatal(err)
	}
	if g := c.GrowthPerDay[30]; math.Abs(g-float64(10*gib)) > float64(gib)/10 {
		t.Fatalf("growth %.0f want %d", g, 10*gib)
	}
	if c.DedupRatio < 2.9 || c.DedupRatio > 3.1 {
		t.Fatalf("dedup %.2f", c.DedupRatio)
	}
	if c.Projected80 == nil || c.ProjectedFull == nil || !c.Projected80.Before(*c.ProjectedFull) {
		t.Fatalf("forecast %v %v", c.Projected80, c.ProjectedFull)
	}
	// used = 1000-free; free = 1000-400-200=400 -> used 600. 80% = 800 -> 200 GiB at 10/day = 20 days.
	if d := c.Projected80.Sub(e.now).Hours() / 24; math.Abs(d-20) > 1 {
		t.Fatalf("80%% in %.1f days, want about 20", d)
	}
}

func TestStaleSessionsExpireOnRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	if _, err := e.svc.BeginSession(ctx, h, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snapID(e.now, 1), Kind: "block", Created: e.now}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ExpireStaleSessions(ctx); err != nil {
		t.Fatal(err)
	}
	var open int
	_ = e.svc.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM sessions WHERE state='open'`).Scan(&open)
	if open != 0 {
		t.Fatal("open sessions remain")
	}
	e.now = e.now.Add(service.DefaultLease + time.Minute)
	if n := e.svc.ReapSessions(ctx); n != 1 {
		t.Fatalf("reaped %d", n)
	}
}

// A backup-scope host must not be able to shorten retention by declaring a more
// aggressive policy name later: retention is pinned when the object is first seen.
func TestCompromisedHostCannotShortenRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "long", KeepLast: 100})
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "scratch", KeepLast: 1})
	for i := 0; i < 5; i++ {
		ts := e.now.Add(time.Duration(i) * time.Hour)
		if err := e.upload(h, k, group, snapID(ts, i), ts, "long", fmt.Sprintf("d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// the attacker now declares the aggressive policy for the same object
	ts := e.now.Add(10 * time.Hour)
	if err := e.upload(h, k, group, snapID(ts, 99), ts, "scratch", "evil"); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Removed != 0 {
		t.Fatalf("history was pruned through a client-chosen policy: %v %+v", err, res)
	}
	// an admin can still choose the retention explicitly
	if err := e.svc.SetObjectRetention(ctx, "custody", "p", "vm/root", "scratch"); err != nil {
		t.Fatal(err)
	}
	res, err = e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Removed != 5 {
		t.Fatalf("admin-pinned policy not applied: %v %+v", err, res)
	}
	if err := e.svc.SetObjectRetention(ctx, "custody", "p", "vm/root", "nope"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown policy accepted: %v", err)
	}
}

// An admin assignment on the server beats whatever the host declared.
func TestAdminAssignmentOverridesDeclaredPolicyForRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "keepall", KeepLast: 100})
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "two", KeepLast: 2})
	for i := 0; i < 5; i++ {
		ts := e.now.Add(time.Duration(i) * time.Hour)
		_ = e.upload(h, k, group, snapID(ts, i), ts, "keepall", fmt.Sprintf("d%d", i))
	}
	// the helper names the object "vm/root" in project "p": assign by project
	if err := e.svc.Assign(ctx, "custody", "benfica", "p", "", "two"); err != nil {
		t.Fatal(err)
	}
	// ownerName("vm/root") == "vm/root", so a project-level assignment applies
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Removed != 3 {
		t.Fatalf("assignment not used for retention: %v %+v", err, res)
	}
}

// Retention buckets by the SERVER's commit time: a host dating its snapshots into the
// future (one per daily slot) must neither be accepted nor push real history out.
func TestFutureDatedSnapshotsAreRefusedAndCannotDisplaceHistory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "daily3", KeepDaily: 3})
	start := e.now
	for d := 0; d < 5; d++ { // five real days of history
		ts := start.AddDate(0, 0, d)
		if err := e.upload(h, k, group, snapID(ts, d), ts, "daily3", fmt.Sprintf("real%d", d)); err != nil {
			t.Fatal(err)
		}
	}
	// the attacker dates ten snapshots on ten different future days
	for d := 1; d <= 10; d++ {
		fake := e.now.AddDate(0, 0, 30+d)
		err := e.upload2(h, k, group, snapID(fake, 100+d), fake, "daily3", "evil")
		if !errors.Is(err, service.ErrBadRequest) {
			t.Fatalf("a snapshot dated %d days in the future was accepted: %v", 30+d, err)
		}
	}
	// within the allowed skew the attacker can only land in "today"'s bucket
	for i := 0; i < 5; i++ {
		fake := e.now.Add(time.Duration(i+1) * time.Minute)
		if err := e.upload2(h, k, group, snapID(fake, 200+i), fake, "daily3", fmt.Sprintf("e%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := e.svc.Snapshots(ctx, h, group)
	// day 4 (today: the real one and 5 minutes of evil: one bucket), day 3, day 2 survive
	days := map[string]bool{}
	for _, s := range l.Snapshots {
		days[s.Committed.Format("2006-01-02")] = true
	}
	for d := 2; d <= 4; d++ {
		if !days[start.AddDate(0, 0, d).Format("2006-01-02")] {
			t.Fatalf("real history of day %d was pruned (%+v, kept=%d)", d, days, res.Kept)
		}
	}
}

// A synchronised snapshot keeps the SOURCE's commit time, so a 30-day sync does not
// collapse into one retention bucket on the destination.
func TestSyncedSnapshotKeepsSourceCommitTimeForRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := keys(1)
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "daily30", KeepDaily: 30})
	w := e.enrol("peer", api.ScopeSyncWrite)
	repo, _ := e.svc.SyncHostRepo(ctx, w, api.ScopeSyncWrite)
	now := e.now
	for d := 0; d < 30; d++ {
		src := now.AddDate(0, 0, -29+d)
		snap := snapID(src, d)
		id, blob, _ := k.EncodeChunk([]byte(fmt.Sprintf("v%d", d)), format.EncodeOpts{})
		if err := e.svc.SyncPutChunk(ctx, repo, id.String(), blob); err != nil {
			t.Fatal(err)
		}
		idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: 3, Entries: []format.Entry{{End: 3, ID: id}}}).Marshal()
		man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: src}, group, nil)
		put := api.SyncSnapshotPut{Meta: api.SnapshotRef{GroupID: group, SnapshotID: snap, Created: src, Committed: src, Policy: "daily30", Project: "p", Object: "vm/root", Kind: "block"}, Index: idx, Manifest: man}
		if err := e.svc.SyncPutSnapshot(ctx, repo, k.DomainID, put); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Removed != 0 {
		t.Fatalf("a freshly synchronised 30-day history was pruned: %v %+v", err, res)
	}
	// a source that lies about the future is clamped to now
	snap := snapID(now.AddDate(1, 0, 0), 999)
	id, blob, _ := k.EncodeChunk([]byte("lie"), format.EncodeOpts{})
	_ = e.svc.SyncPutChunk(ctx, repo, id.String(), blob)
	idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: 3, Entries: []format.Entry{{End: 3, ID: id}}}).Marshal()
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap}, group, nil)
	if err := e.svc.SyncPutSnapshot(ctx, repo, k.DomainID, api.SyncSnapshotPut{Meta: api.SnapshotRef{GroupID: group, SnapshotID: snap, Created: now, Committed: now.AddDate(1, 0, 0), Policy: "daily30", Project: "p", Object: "vm/root", Kind: "block"}, Index: idx, Manifest: man}); err != nil {
		t.Fatal(err)
	}
	l, _ := e.svc.Snapshots(ctx, e.enrol("reader", api.ScopeBackup), group)
	if last := l.Snapshots[len(l.Snapshots)-1]; last.SnapshotID == snap && last.Committed.After(now.Add(time.Minute)) {
		t.Fatalf("a future commit time was stored: %v", last.Committed)
	}
}
