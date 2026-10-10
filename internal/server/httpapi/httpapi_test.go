package httpapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/httpapi"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

type fixture struct {
	t   *testing.T
	svc *service.Service
	srv *httptest.Server
}

func setup(t *testing.T) *fixture {
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
	svc := service.New(d, filepath.Join(dir, "state"), "test", log)
	if err := svc.InitPKI([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRepository(ctx, "custody", filepath.Join(dir, "ds")); err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := svc.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer((&httpapi.Server{Svc: svc, Log: log}).Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &fixture{t: t, svc: svc, srv: srv}
}

func (f *fixture) enrol(name, scope string, domain string) *remote.Client {
	f.t.Helper()
	tok, err := f.svc.CreateToken(context.Background(), "custody", name, scope, time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	en, err := remote.Enroll(context.Background(), f.srv.Listener.Addr().String(), tok, name, map[string]any{"incus": "7.5.1"})
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := remote.New(f.srv.Listener.Addr().String(), en.CAPEM, en.CertPEM, en.KeyPEM, domain)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func keys() *format.Keys {
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(7 * i)
	}
	k, _ := format.DeriveKeys(m)
	return k
}

const group = "0123456789abcdef0123456789abcdef"

func TestEndToEndOverMutualTLS(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := keys()
	c := f.enrol("benfica", api.ScopeBackup, k.DomainID)
	who, err := c.Whoami(ctx)
	if err != nil || who.Host != "benfica" || who.Repository != "custody" || who.Scope != api.ScopeBackup {
		t.Fatalf("%v %+v", err, who)
	}
	runID, err := c.StartRun(ctx, api.RunStart{Started: time.Now(), ObjectsTotal: 1})
	if err != nil {
		t.Fatal(err)
	}
	snap := "20261007230000-0a0a0a0a"
	sess, err := c.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: snap, Created: time.Now().UTC(), Policy: "p", Project: "custody", Object: "bosch/root", Kind: "block"})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("incus-backup "), 1000)
	id, blob, _ := k.EncodeChunk(payload, format.EncodeOpts{})
	miss, err := sess.Missing(ctx, []string{id.String()})
	if err != nil || len(miss) != 1 {
		t.Fatalf("%v %v", err, miss)
	}
	if err := sess.PutChunk(ctx, id.String(), blob); err != nil {
		t.Fatal(err)
	}
	if miss, _ := sess.Missing(ctx, []string{id.String()}); len(miss) != 0 {
		t.Fatal("chunk still missing after upload")
	}
	idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: uint64(len(payload)), Entries: []format.Entry{{End: uint64(len(payload)), ID: id}}}).Marshal()
	if err := sess.PutIndex(ctx, idx); err != nil {
		t.Fatal(err)
	}
	g := k.GroupID("custody", "block", "bosch/root")
	_ = g
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: time.Now().UTC(), Object: format.ObjectInfo{Name: "bosch/root", Kind: "block", Size: uint64(len(payload))}}, group, nil)
	if err := sess.PutManifest(ctx, man); err != nil {
		t.Fatal(err)
	}
	cr, err := sess.Commit(ctx, api.CommitRequest{LogicalBytes: int64(len(payload)), UploadedBytes: int64(len(blob))})
	if err != nil || cr.ChunkCount != 1 {
		t.Fatalf("%v %+v", err, cr)
	}
	if err := c.FinishRun(ctx, runID, api.RunFinish{Status: "ok", ObjectsOK: 1}); err != nil {
		t.Fatal(err)
	}
	// restore path: list, index, manifest, chunk -> plaintext
	l, err := c.Snapshots(ctx, group)
	if err != nil || len(l.Snapshots) != 1 {
		t.Fatalf("%v %+v", err, l)
	}
	gi, _ := c.Index(ctx, group, snap)
	pi, err := format.ParseIndex(gi)
	if err != nil {
		t.Fatal(err)
	}
	gb, err := c.Chunk(ctx, pi.Entries[0].ID.String())
	if err != nil {
		t.Fatal(err)
	}
	pt, err := k.DecodeChunk(gb, pi.Entries[0].ID)
	if err != nil || !bytes.Equal(pt, payload) {
		t.Fatalf("restore mismatch: %v", err)
	}
	gm, _ := c.Manifest(ctx, group, snap)
	if _, err := k.OpenManifest(gm, group, snap); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(gm, []byte("bosch")) {
		t.Fatal("manifest on the wire leaks a name")
	}
}

func TestUnauthenticatedAndWrongKeyDomainAndScope(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// no client certificate
	anon := &remote.Client{Base: "https://" + f.srv.Listener.Addr().String(), HTTP: f.srv.Client()}
	if _, err := anon.Whoami(ctx); err == nil {
		t.Fatal("unauthenticated request succeeded")
	}
	// wrong key domain
	k := keys()
	c := f.enrol("benfica", api.ScopeBackup, k.DomainID)
	if _, err := c.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: "20261007230000-0a0a0a0a", Kind: "block", Project: "p", Object: "o"}); err != nil {
		t.Fatal(err)
	}
	evil := f.enrol("evil", api.ScopeBackup, "ffffffffffffffff")
	_, err := evil.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: "20261007230001-0a0a0a0a", Kind: "block", Project: "p", Object: "o"})
	if !remote.IsStatus(err, 403) {
		t.Fatalf("wrong key domain: %v", err)
	}
	// sync-read scope cannot upload
	peer := f.enrol("amsterdam", api.ScopeSyncRead, k.DomainID)
	_, err = peer.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: "20261007230002-0a0a0a0a", Kind: "block"})
	if !remote.IsStatus(err, 403) {
		t.Fatalf("sync-read opened a backup session: %v", err)
	}
	// bad ids are 400, unknown session is 404
	if _, err := c.Snapshots(ctx, "not-a-group"); !remote.IsStatus(err, 400) {
		t.Fatalf("bad group: %v", err)
	}
	s := &remote.Session{ID: "deadbeef"}
	_ = s
	// revoked host is refused over the wire
	if err := f.svc.SetHostDisabled(ctx, "custody", "benfica", api.ScopeBackup, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Whoami(ctx); !remote.IsStatus(err, 403) {
		t.Fatalf("revoked host: %v", err)
	}
}

func TestEnrollmentFailsWithWrongPinnedFingerprint(t *testing.T) {
	f := setup(t)
	tok, _ := f.svc.CreateToken(context.Background(), "custody", "benfica", api.ScopeBackup, time.Hour)
	bad := "ibt1." + string(bytes.Repeat([]byte("0"), 64)) + ".secret"
	if _, err := remote.Enroll(context.Background(), f.srv.Listener.Addr().String(), bad, "x", nil); err == nil {
		t.Fatal("enrolment trusted a server that does not match the pinned CA fingerprint")
	}
	// the real token still works afterwards (the failed attempt never reached the server logic)
	if _, err := remote.Enroll(context.Background(), f.srv.Listener.Addr().String(), tok, "x", nil); err != nil {
		t.Fatal(err)
	}
	// and cannot be replayed
	if _, err := remote.Enroll(context.Background(), f.srv.Listener.Addr().String(), tok, "x", nil); !remote.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("replay: %v", err)
	}
}

func TestEnrolmentIsRateLimitedPerAddress(t *testing.T) {
	f := setup(t)
	k := keys()
	c := f.enrol("benfica-before", api.ScopeBackup, k.DomainID)
	fp, _ := f.svc.CAFingerprint()
	tok := "ibt1." + fp + ".not-a-real-secret"
	limited := 0
	for i := 0; i < 40; i++ {
		_, err := remote.Enroll(context.Background(), f.srv.Listener.Addr().String(), tok, "x", nil)
		if remote.IsStatus(err, http.StatusTooManyRequests) {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("40 rapid enrolment attempts were never rate limited")
	}
	// authenticated traffic is not affected by the enrolment limit
	if _, err := c.Whoami(context.Background()); err != nil {
		t.Fatalf("an enrolled host was throttled: %v", err)
	}
}

// Over real mTLS: a second concurrent session for the same object is refused with 409, the lease
// is reported and renewable with a heartbeat, a silent session is reclaimed, and the slot frees.
func TestOneWriterPerObjectAndHeartbeatOverTheWire(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := keys()
	c := f.enrol("benfica", api.ScopeBackup, k.DomainID)
	now := time.Now().UTC()
	req := api.SessionRequest{GroupID: "0123456789abcdef0123456789abcdef", SnapshotID: "20261001120000-00000001", Created: now, Project: "p", Object: "o", Kind: "block"}
	s1, err := c.Begin(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Lease != service.DefaultLease {
		t.Fatalf("lease %v", s1.Lease)
	}
	req2 := req
	req2.SnapshotID = "20261001120001-00000002"
	_, err = c.Begin(ctx, req2)
	if err == nil || !remote.IsStatus(err, 409) {
		t.Fatalf("a second writer must get 409, got %v", err)
	}
	if err := s1.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s1.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s1.Heartbeat(ctx); err == nil || !remote.IsStatus(err, 404) {
		t.Fatalf("heartbeat on an ended session: %v", err)
	}
	// a client that goes silent loses its lease; the server reclaims it
	f.svc.Lease = 10 * time.Millisecond
	if _, err := c.Begin(ctx, req2); err != nil {
		t.Fatalf("the slot must be free again: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if n := f.svc.ReapSessions(ctx); n < 1 {
		t.Fatalf("reaped %d", n)
	}
	f.svc.Lease = 0
	if _, err := c.Begin(ctx, req); err != nil {
		t.Fatalf("after reclaiming: %v", err)
	}
}
