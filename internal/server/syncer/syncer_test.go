package syncer_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/httpapi"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) {
	testdb.AtExit(s3test.Start())
	testdb.Main(m)
}

// node is one complete, independent server: own PostgreSQL, own datastore, own CA.
type node struct {
	t    *testing.T
	svc  *service.Service
	repo *service.Repo
	srv  *httptest.Server
	now  time.Time
}

func newNode(t *testing.T, name string) *node { t.Helper(); return newNodeOn(t, name, false) }

// newS3Node is a server whose repository lives in an S3 bucket.
func newS3Node(t *testing.T, name string) *node { t.Helper(); return newNodeOn(t, name, true) }

func newNodeOn(t *testing.T, name string, s3 bool) *node {
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
	n := &node{t: t, now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	n.svc = service.New(d, filepath.Join(dir, "state"), "test", log)
	n.svc.Now = func() time.Time { return n.now }
	n.svc.MaxClockSkew = 100 * 365 * 24 * time.Hour
	if err := n.svc.InitPKI([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if s3 {
		svc, bucket := s3test.NewBucket(t)
		n.repo, err = n.svc.CreateS3Repository(ctx, "custody", datastore.S3Config{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: name,
			AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: svc.NoTLS, PathStyle: true})
	} else {
		n.repo, err = n.svc.CreateRepository(ctx, "custody", filepath.Join(dir, "ds"))
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := n.svc.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	n.srv = httptest.NewUnstartedServer((&httpapi.Server{Svc: n.svc, Log: log}).Handler())
	n.srv.TLS = cfg
	n.srv.StartTLS()
	t.Cleanup(n.srv.Close)
	return n
}

func (n *node) addr() string { return n.srv.Listener.Addr().String() }

func (n *node) enrol(name, scope string) *remote.Client {
	n.t.Helper()
	tok, err := n.svc.CreateToken(context.Background(), "custody", name, scope, time.Hour)
	if err != nil {
		n.t.Fatal(err)
	}
	en, err := remote.Enroll(context.Background(), n.addr(), tok, name, nil)
	if err != nil {
		n.t.Fatal(err)
	}
	c, err := remote.New(n.addr(), en.CAPEM, en.CertPEM, en.KeyPEM, "")
	if err != nil {
		n.t.Fatal(err)
	}
	return c
}

func keys() *format.Keys {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(3 * i)
	}
	k, _ := format.DeriveKeys(m)
	return k
}

// backup uploads a snapshot as a backup host would, through the HTTP API.
func backup(t *testing.T, c *remote.Client, k *format.Keys, group, snap string, created time.Time, policy string, parts ...string) {
	t.Helper()
	ctx := context.Background()
	c.Domain = k.DomainID
	s, err := c.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: snap, Created: created, Policy: policy, Project: "custody", Object: "bosch/root", Kind: "block"})
	if err != nil {
		t.Fatal(err)
	}
	x := &format.Index{Kind: format.IndexDynamic}
	var off uint64
	blobs := map[string][]byte{}
	var ids []string
	for _, p := range parts {
		id, blob, _ := k.EncodeChunk([]byte(p), format.EncodeOpts{})
		off += uint64(len(p))
		x.Entries = append(x.Entries, format.Entry{End: off, ID: id})
		blobs[id.String()] = blob
		ids = append(ids, id.String())
	}
	x.Size = off
	miss, err := s.Missing(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range miss {
		if err := s.PutChunk(ctx, m, blobs[m]); err != nil {
			t.Fatal(err)
		}
	}
	idx, _ := x.Marshal()
	_ = s.PutIndex(ctx, idx)
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created, Object: format.ObjectInfo{Name: "bosch/root", Kind: "block", Size: off}, IndexDigest: format.IndexDigest(idx)}, group, nil)
	_ = s.PutManifest(ctx, man)
	if _, err := s.Commit(ctx, api.CommitRequest{LogicalBytes: int64(off)}); err != nil {
		t.Fatal(err)
	}
}

const group = "0123456789abcdef0123456789abcdef"

func snapID(i int) string { return fmt.Sprintf("2026100%d120000-%08x", i, i) }

func list(t *testing.T, n *node) []api.SnapshotRef {
	t.Helper()
	l, err := n.svc.ListAllSnapshots(context.Background(), n.repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// decodeAll proves a node is a standalone restore source: every chunk of every
// snapshot is readable and decrypts with the recovery key alone.
func decodeAll(t *testing.T, n *node, k *format.Keys) int {
	t.Helper()
	ctx := context.Background()
	total := 0
	for _, r := range list(t, n) {
		ib, err := n.svc.RepoIndex(ctx, n.repo, r.GroupID, r.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		idx, err := format.ParseIndex(ib)
		if err != nil {
			t.Fatal(err)
		}
		mb, _ := n.svc.RepoManifest(ctx, n.repo, r.GroupID, r.SnapshotID)
		if _, err := k.OpenManifest(mb, r.GroupID, r.SnapshotID); err != nil {
			t.Fatalf("manifest of %s: %v", r.SnapshotID, err)
		}
		for _, e := range idx.Entries {
			blob, err := n.svc.RepoChunk(ctx, n.repo, e.ID.String())
			if err != nil {
				t.Fatalf("chunk %s missing on destination: %v", e.ID, err)
			}
			if _, err := k.DecodeChunk(blob, e.ID); err != nil {
				t.Fatal(err)
			}
			total++
		}
	}
	return total
}

func addJob(t *testing.T, local, remoteNode *node, dir, name string) (*syncer.Manager, *syncer.Job, *remote.Client) {
	t.Helper()
	scope := api.ScopeSyncRead
	if dir == "push" {
		scope = api.ScopeSyncWrite
	}
	tok, err := remoteNode.svc.CreateToken(context.Background(), "custody", "peer-"+name, scope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m := &syncer.Manager{Svc: local.svc}
	if err := m.AddJob(context.Background(), "custody", syncer.Job{Name: name, Direction: dir, Remote: remoteNode.addr(), Schedule: "02:00"}, tok); err != nil {
		t.Fatal(err)
	}
	jobs, _ := m.Jobs(context.Background(), "custody")
	j := jobs[0]
	rc, err := remoteClient(m, local, &j)
	if err != nil {
		t.Fatal(err)
	}
	return m, &j, rc
}

func remoteClient(m *syncer.Manager, local *node, j *syncer.Job) (*remote.Client, error) {
	dir := filepath.Join(local.svc.StateDir, "sync", "custody", j.Name)
	ca, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	crt, _ := os.ReadFile(filepath.Join(dir, "client.crt"))
	key, _ := os.ReadFile(filepath.Join(dir, "client.key"))
	return remote.New(j.Remote, ca, crt, key, "")
}

func TestPushSyncMakesStandaloneRestoreSource(t *testing.T) {
	a, b := newNode(t, "cikarang"), newNode(t, "amsterdam")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "alpha", "beta", "gamma")
	backup(t, bk, k, group, snapID(2), a.now.Add(time.Hour), "p", "alpha", "beta", "delta") // shares 2 chunks
	m, j, _ := addJob(t, a, b, "push", "amsterdam")
	res, err := m.Run(ctx, "custody", "amsterdam")
	if err != nil || res.Synced != 2 || res.Failed != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if res.ChunksTransferred != 4 || res.ChunksReused != 2 {
		t.Fatalf("dedup during sync: transferred=%d reused=%d (want 4 and 2)", res.ChunksTransferred, res.ChunksReused)
	}
	if len(list(t, b)) != 2 {
		t.Fatal("destination missing snapshots")
	}
	if n := decodeAll(t, b, k); n != 6 {
		t.Fatalf("decoded %d chunk refs", n)
	}
	// the destination alone is enough: Cikarang is gone
	a.srv.Close()
	if decodeAll(t, b, k) != 6 {
		t.Fatal("destination not standalone")
	}
	// control-plane metadata arrived in the destination's own PostgreSQL
	var n int
	_ = b.svc.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM snapshots WHERE source='sync'`).Scan(&n)
	if n != 2 {
		t.Fatalf("destination PostgreSQL rows=%d", n)
	}
	st, err := m.Status(ctx, "custody", "amsterdam")
	if err != nil || st.LastRunStatus != "ok" || st.SnapshotsSynced != 2 || st.Lag != 0 {
		t.Fatalf("%v %+v", err, st)
	}
	_ = j
}

func TestPullSyncInitiatedByDestination(t *testing.T) {
	a, b := newNode(t, "source"), newNode(t, "dest")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "one", "two")
	// b pulls from a: b is the initiator and holds sync-read credentials for a.
	m, _, _ := addJob(t, b, a, "pull", "from-cikarang")
	res, err := m.Run(ctx, "custody", "from-cikarang")
	if err != nil || res.Synced != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if decodeAll(t, b, k) != 2 {
		t.Fatal("pulled data not restorable")
	}
	// a never contacted b and holds no credentials for it
	if entries, _ := os.ReadDir(filepath.Join(a.svc.StateDir, "sync")); len(entries) != 0 {
		t.Fatal("source stored sync credentials for a pull")
	}
	// idempotent: second run moves nothing
	res, err = m.Run(ctx, "custody", "from-cikarang")
	if err != nil || res.Pending != 0 || res.ChunksTransferred != 0 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestTokenScopeMustMatchDirection(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	tok, _ := b.svc.CreateToken(context.Background(), "custody", "peer", api.ScopeSyncRead, time.Hour)
	m := &syncer.Manager{Svc: a.svc}
	err := m.AddJob(context.Background(), "custody", syncer.Job{Name: "x", Direction: "push", Remote: b.addr()}, tok)
	if err == nil {
		t.Fatal("a sync-read token must not create a push job")
	}
	jobs, _ := m.Jobs(context.Background(), "custody")
	if len(jobs) != 0 {
		t.Fatal("job stored despite scope mismatch")
	}
}

// Interrupt a sync part-way through the chunk transfer: the destination must not
// expose the partial snapshot, and the next run must resume and finish.
func TestInterruptedSyncNeverExposesPartialSnapshotAndResumes(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "c1", "c2", "c3", "c4", "c5", "c6")
	src := &syncer.LocalRepo{Svc: a.svc, Repo: a.repo}
	dstReal := &syncer.LocalRepo{Svc: b.svc, Repo: b.repo}
	flaky := &failingDest{Dest: dstReal, failAfter: 3}
	res, err := syncer.Run(ctx, src, flaky, syncer.Options{Concurrency: 1})
	if err == nil || res.Failed != 1 || res.Synced != 0 {
		t.Fatalf("expected failure: %v %+v", err, res)
	}
	if got := list(t, b); len(got) != 0 {
		t.Fatalf("PARTIAL SNAPSHOT VISIBLE on destination: %+v", got)
	}
	// negative control for the invariant: committing without the chunks is refused by the destination itself
	ib, _ := a.svc.RepoIndex(ctx, a.repo, group, snapID(1))
	mb, _ := a.svc.RepoManifest(ctx, a.repo, group, snapID(1))
	ref := list(t, a)[0]
	err = b.svc.SyncPutSnapshot(ctx, b.repo, k.DomainID, api.SyncSnapshotPut{Meta: ref, Index: ib, Manifest: mb})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("destination accepted a snapshot with missing chunks: %v", err)
	}
	// resume: only the missing chunks travel
	res, err = syncer.Run(ctx, src, dstReal, syncer.Options{})
	if err != nil || res.Synced != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if res.ChunksTransferred != 3 || res.ChunksReused != 3 {
		t.Fatalf("resume transferred=%d reused=%d, want 3/3", res.ChunksTransferred, res.ChunksReused)
	}
	if decodeAll(t, b, k) != 6 {
		t.Fatal("not restorable after resume")
	}
}

type failingDest struct {
	syncer.Dest
	failAfter int32
	n         int32
}

func (f *failingDest) PutChunk(ctx context.Context, id string, blob []byte) error {
	if atomic.AddInt32(&f.n, 1) > f.failAfter {
		return errors.New("injected network failure")
	}
	return f.Dest.PutChunk(ctx, id, blob)
}

func TestCorruptSourceBlobIsNotReplicated(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "only")
	ib, _ := a.svc.RepoIndex(ctx, a.repo, group, snapID(1))
	idx, _ := format.ParseIndex(ib)
	st, _ := a.svc.Store(a.repo)
	key := "chunks/" + idx.Entries[0].ID.String()[:2] + "/" + idx.Entries[0].ID.String()[2:4] + "/" + idx.Entries[0].ID.String()
	blob, err := st.Backend().Get(key)
	if err != nil {
		t.Fatal(err)
	}
	blob[15] ^= 0xff
	if err := st.Backend().Put(key, blob, false); err != nil {
		t.Fatal(err)
	}
	res, err := syncer.Run(ctx, &syncer.LocalRepo{Svc: a.svc, Repo: a.repo}, &syncer.LocalRepo{Svc: b.svc, Repo: b.repo}, syncer.Options{})
	if err == nil || res.Synced != 0 {
		t.Fatalf("corruption was replicated: %v %+v", err, res)
	}
	if len(list(t, b)) != 0 {
		t.Fatal("destination has a snapshot built from a corrupt blob")
	}
}

// Retention is independent: pruning the source never removes anything from the destination.
func TestRetentionIsIndependentPerRepository(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	_ = a.svc.SetPolicy(ctx, "custody", api.Policy{Name: "short", KeepLast: 1})
	for i := 1; i <= 3; i++ {
		backup(t, bk, k, group, snapID(i), a.now.Add(time.Duration(i)*time.Hour), "short", fmt.Sprintf("v%d", i))
	}
	m, _, _ := addJob(t, a, b, "push", "off-site")
	if _, err := m.Run(ctx, "custody", "off-site"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Prune(ctx, "custody", false); err != nil {
		t.Fatal(err)
	}
	if len(list(t, a)) != 1 {
		t.Fatalf("source left %d", len(list(t, a)))
	}
	res, err := m.Run(ctx, "custody", "off-site") // must not "undo" or propagate anything
	if err != nil || res.Pending != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if len(list(t, b)) != 3 {
		t.Fatalf("destination lost snapshots through source retention: %d left", len(list(t, b)))
	}
	if decodeAll(t, b, k) != 3 {
		t.Fatal("destination not fully restorable")
	}
}

func TestKeyDomainMismatchRefusesSync(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	bkA := a.enrol("h1", api.ScopeBackup)
	bkB := b.enrol("h2", api.ScopeBackup)
	backup(t, bkA, keys(), group, snapID(1), a.now, "p", "x")
	other := make([]byte, 32)
	other[0] = 99
	k2, _ := format.DeriveKeys(other)
	backup(t, bkB, k2, "fedcba9876543210fedcba9876543210", snapID(1), b.now, "p", "y")
	res, err := syncer.Run(ctx, &syncer.LocalRepo{Svc: a.svc, Repo: a.repo}, &syncer.LocalRepo{Svc: b.svc, Repo: b.repo}, syncer.Options{})
	if err == nil || res.Synced != 0 {
		t.Fatalf("sync across different key domains must fail: %v %+v", err, res)
	}
}

func TestSyncScopesCannotDoAdminOrBackup(t *testing.T) {
	a := newNode(t, "a")
	ctx := context.Background()
	w := a.enrol("w", api.ScopeSyncWrite)
	r := a.enrol("r", api.ScopeSyncRead)
	if _, err := r.SyncMissing(ctx, []string{"00"}); !remote.IsStatus(err, 403) {
		t.Fatalf("sync-read writing: %v", err)
	}
	if err := w.SyncPutChunk(ctx, "00"+string(make([]byte, 0)), nil); err == nil {
		t.Fatal("garbage chunk accepted")
	}
	if _, err := w.DesiredState(ctx); !remote.IsStatus(err, 403) {
		t.Fatalf("sync-write read desired state: %v", err)
	}
	if _, err := w.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: snapID(1), Kind: "block"}); !remote.IsStatus(err, 403) {
		t.Fatalf("sync-write opened a backup session: %v", err)
	}
	if _, err := r.SyncList(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDueJobs(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	m, _, _ := addJob(t, a, b, "push", "nightly")
	due, _ := m.Due(ctx, "custody")
	if len(due) != 1 {
		t.Fatal("never-run job must be due")
	}
	_, _ = m.Run(ctx, "custody", "nightly") // nothing to sync, still records success
	due, _ = m.Due(ctx, "custody")
	if len(due) != 0 {
		t.Fatal("just ran, must not be due")
	}
	a.now = a.now.Add(24 * time.Hour)
	due, _ = m.Due(ctx, "custody")
	if len(due) != 1 {
		t.Fatal("must be due a day later")
	}
}
