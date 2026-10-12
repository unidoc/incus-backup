package mgmt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/server/syncer"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) {
	testdb.AtExit(s3test.Start())
	testdb.Main(m)
}

const group = "0123456789abcdef0123456789abcdef"

type client struct {
	t    *testing.T
	http *http.Client
}

func newClient(t *testing.T, n *testsupport.Node) (*client, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := &mgmt.Server{Svc: n.Svc, Sync: &syncer.Manager{Svc: n.Svc}}
	t.Cleanup(srv.Close) // background tasks end before the database goes away
	go func() { _ = srv.Serve(ctx, sock) }()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &client{t: t, http: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}}, sock
}

func (c *client) do(method, path string, in any, out any) (int, string) {
	c.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://admin"+path, body)
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: %v\n%s", method, path, err, b)
		}
	}
	return resp.StatusCode, string(b)
}

func (c *client) must(method, path string, in, out any) {
	c.t.Helper()
	if code, body := c.do(method, path, in, out); code >= 300 {
		c.t.Fatalf("%s %s -> %d %s", method, path, code, body)
	}
}

var hostClients = map[*testsupport.Node]*remote.Client{}

// one backup host per node: an object belongs to the host that first backed it up
func hostClient(n *testsupport.Node, k *format.Keys) *remote.Client {
	if c := hostClients[n]; c != nil {
		return c
	}
	c := n.Client(n.Enrol("benfica", api.ScopeBackup), k.DomainID)
	hostClients[n] = c
	return c
}

func backup(t *testing.T, n *testsupport.Node, k *format.Keys, snap string, created time.Time, parts ...string) {
	t.Helper()
	ctx := context.Background()
	cl := hostClient(n, k)
	s, err := cl.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: snap, Created: created, Policy: "p", Project: "default", Object: "vm/root", Kind: "block"})
	if err != nil {
		t.Fatal(err)
	}
	x := &format.Index{Kind: format.IndexDynamic}
	var off uint64
	for _, p := range parts {
		id, blob, _ := k.EncodeChunk([]byte(p), format.EncodeOpts{})
		if err := s.PutChunk(ctx, id.String(), blob); err != nil {
			t.Fatal(err)
		}
		off += uint64(len(p))
		x.Entries = append(x.Entries, format.Entry{End: off, ID: id})
	}
	x.Size = off
	idx, _ := x.Marshal()
	_ = s.PutIndex(ctx, idx)
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created, IndexDigest: format.IndexDigest(idx)}, group, nil)
	_ = s.PutManifest(ctx, man)
	if _, err := s.Commit(ctx, api.CommitRequest{LogicalBytes: int64(off)}); err != nil {
		t.Fatal(err)
	}
}

func masterKeys() *format.Keys {
	m := make([]byte, 32)
	m[0] = 5
	k, _ := format.DeriveKeys(m)
	return k
}

// Everything a management UI needs, end to end over the real socket against real PostgreSQL and MinIO.
func TestAdminAPIDrivesTheWholeControlPlane(t *testing.T) {
	n := testsupport.NewNode(t)
	c, sock := newClient(t, n)
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", err, fi)
	}
	k := masterKeys()

	// policies with a time zone; invalid zones are rejected with a 400
	c.must("PUT", "/api/v1/policies/nightly", api.Policy{Schedule: "03:00", Targets: []string{"a"}, KeepLast: 2, KeepDaily: 3, Timezone: "Atlantic/Reykjavik", ReverifyDays: 14}, nil)
	if code, _ := c.do("PUT", "/api/v1/policies/bad", api.Policy{KeepLast: 1, Timezone: "Mars/Base"}, nil); code != 400 {
		t.Fatalf("bad zone -> %d", code)
	}
	var pols []api.Policy
	c.must("GET", "/api/v1/policies", nil, &pols)
	if len(pols) != 1 || pols[0].Timezone != "Atlantic/Reykjavik" || pols[0].ReverifyDays != 14 {
		t.Fatalf("%+v", pols)
	}

	// tokens are shown once and enrol hosts; hosts list
	var tok struct {
		Token string `json:"token"`
	}
	c.must("POST", "/api/v1/tokens", map[string]any{"host": "benfica", "scope": "backup", "ttl_hours": 1}, &tok)
	if !strings.HasPrefix(tok.Token, "ibt1.") {
		t.Fatalf("token %q", tok.Token)
	}
	if code, _ := c.do("POST", "/api/v1/tokens", map[string]any{"host": "x", "scope": "admin"}, nil); code != 400 {
		t.Fatalf("admin scope token -> %d", code)
	}

	// backups, then snapshot management over the API
	for i := 1; i <= 4; i++ {
		backup(t, n, k, fmt.Sprintf("2026100%d120000-%08x", i, i), n.Now.Add(time.Duration(i)*time.Hour), fmt.Sprintf("v%d", i), "shared")
	}
	var snaps []api.SnapshotRef
	c.must("GET", "/api/v1/snapshots", nil, &snaps)
	if len(snaps) != 4 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	first := snaps[0]
	c.must("PUT", "/api/v1/snapshots/"+first.GroupID+"/"+first.SnapshotID+"/protected", map[string]any{"protected": true}, nil)
	c.must("PUT", "/api/v1/snapshots/"+first.GroupID+"/"+first.SnapshotID+"/notes", map[string]any{"notes": "before the upgrade"}, nil)
	if code, body := c.do("DELETE", "/api/v1/snapshots/"+first.GroupID+"/"+first.SnapshotID, nil, nil); code != 409 {
		t.Fatalf("forgetting a protected snapshot -> %d %s", code, body)
	}
	c.must("GET", "/api/v1/snapshots", nil, &snaps)
	if !snaps[0].Protected || snaps[0].Notes != "before the upgrade" {
		t.Fatalf("%+v", snaps[0])
	}

	// the retention preview equals what is applied
	var pre, applied struct {
		Kept, Removed, Protected int
		Plan                     []struct {
			Snapshot, Reason string
			Keep             bool
		}
	}
	c.must("POST", "/api/v1/prune?dry_run=1", nil, &pre)
	c.must("POST", "/api/v1/prune?wait=1", nil, &applied)
	if pre.Kept != applied.Kept || pre.Removed != applied.Removed {
		t.Fatalf("preview %+v differs from the applied run %+v", pre, applied)
	}

	// verify finds nothing wrong, records state, and the run history shows it
	var vr struct {
		Ok bool `json:"ok"`
	}
	c.must("POST", "/api/v1/verify?wait=1", map[string]any{"mode": "all"}, &vr)
	if !vr.Ok {
		t.Fatal("verify not ok")
	}
	var runs []struct{ Kind, Status string }
	c.must("GET", "/api/v1/runs", nil, &runs)
	if len(runs) < 2 {
		t.Fatalf("runs %+v", runs)
	}
	c.must("GET", "/api/v1/snapshots/"+first.GroupID+"/"+first.SnapshotID+"/verifications", nil, nil)

	// maintenance schedules: partial update
	c.must("PUT", "/api/v1/maintenance", map[string]any{"prune": "manual", "gc_grace_hours": 36}, nil)
	var ms struct {
		Prune        string  `json:"Prune"`
		GCGraceHours float64 `json:"gc_grace_hours"`
		Verify       string  `json:"Verify"`
	}
	c.must("GET", "/api/v1/maintenance", nil, &ms)
	if ms.Prune != "manual" || ms.GCGraceHours != 36 || ms.Verify != "daily" {
		t.Fatalf("%+v", ms)
	}
	if code, _ := c.do("PUT", "/api/v1/maintenance", map[string]any{"gc_grace_hours": 1}, nil); code != 400 {
		t.Fatalf("unsafe grace -> %d", code)
	}

	// capacity is valid JSON even without a growth history (no NaN)
	var cap struct {
		Chunks int64               `json:"chunks"`
		Growth map[string]*float64 `json:"growth_per_day"`
	}
	c.must("GET", "/api/v1/capacity", nil, &cap)
	if cap.Chunks == 0 {
		t.Fatalf("%+v", cap)
	}

	// unknown fields and unknown repositories are errors, not silently ignored
	if code, _ := c.do("PUT", "/api/v1/snapshots/"+first.GroupID+"/"+first.SnapshotID+"/protected", map[string]any{"protect": true}, nil); code != 400 {
		t.Fatalf("unknown field -> %d", code)
	}
	if code, _ := c.do("GET", "/api/v1/status?repository=nope", nil, nil); code != 404 {
		t.Fatalf("unknown repository -> %d", code)
	}
}

// S3 repositories are created through the API; the secret goes in once and never comes back.
func TestAdminAPIS3RepositoryAndCredentialNeverRevealTheSecret(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	svc, bucket := s3test.NewBucket(t)
	req := map[string]any{"name": "offsite", "kind": "s3", "access_key": svc.AccessKey, "secret_key": svc.SecretKey,
		"s3": map[string]any{"endpoint": svc.Endpoint, "bucket": bucket, "prefix": "ib", "no_tls": svc.NoTLS, "path_style": true}}
	var rv struct {
		Name, Backend, Location, Credential string
	}
	c.must("POST", "/api/v1/repositories", req, &rv)
	if rv.Backend != "s3" || !strings.HasPrefix(rv.Location, "s3://") {
		t.Fatalf("%+v", rv)
	}
	var all []byte
	for _, path := range []string{"/api/v1/repositories", "/api/v1/credentials", "/api/v1/status?repository=offsite"} {
		_, body := c.do("GET", path, nil, nil)
		all = append(all, body...)
	}
	if bytes.Contains(all, []byte(svc.SecretKey)) {
		t.Fatal("the secret key came back through the API")
	}
	var probe []struct {
		Op string `json:"op"`
		OK bool   `json:"ok"`
	}
	c.must("POST", "/api/v1/repositories/offsite/probe", nil, &probe)
	for _, p := range probe {
		if !p.OK {
			t.Fatalf("probe step failed: %+v", p)
		}
	}
	if len(probe) < 8 {
		t.Fatalf("probe ran only %d steps", len(probe))
	}
	// a repository on a bucket another server owns is refused with 409 unless adopted
	n2 := testsupport.NewNode(t)
	c2, _ := newClient(t, n2)
	req2 := map[string]any{"name": "stolen", "kind": "s3", "access_key": svc.AccessKey, "secret_key": svc.SecretKey,
		"s3": map[string]any{"endpoint": svc.Endpoint, "bucket": bucket, "prefix": "ib", "no_tls": svc.NoTLS, "path_style": true}}
	if code, body := c2.do("POST", "/api/v1/repositories/attach", req2, nil); code != 409 {
		t.Fatalf("attaching an owned bucket -> %d %s", code, body)
	}
	req2["adopt"] = true
	if code, body := c2.do("POST", "/api/v1/repositories/attach", req2, nil); code != 201 {
		t.Fatalf("adopt -> %d %s", code, body)
	}
	// rotate, then list: still no secret
	c.must("PUT", "/api/v1/credentials/offsite-s3", map[string]any{"access_key": svc.AccessKey, "secret_key": svc.SecretKey}, nil)
	if code, _ := c.do("DELETE", "/api/v1/credentials/offsite-s3", nil, nil); code != 409 {
		t.Fatalf("deleting a credential in use -> %d", code)
	}
}

// Sync jobs managed through the API, including the PBS-class options.
func TestAdminAPIManagesSyncJobs(t *testing.T) {
	a, b := testsupport.NewNode(t), testsupport.NewNode(t)
	ca, _ := newClient(t, a)
	k := masterKeys()
	backup(t, a, k, "20261001120000-00000001", a.Now, "one", "shared")
	tok, err := b.Svc.CreateToken(context.Background(), "custody", "peer-a", api.ScopeSyncWrite, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ca.must("POST", "/api/v1/sync", map[string]any{"name": "offsite", "direction": "push", "remote": b.Addr(), "schedule": "02:00", "verified_only": true, "token": tok}, nil)
	var jobs []map[string]any
	ca.must("GET", "/api/v1/sync", nil, &jobs)
	if len(jobs) != 1 || jobs[0]["verified_only"] != true || jobs[0]["direction"] != "push" {
		t.Fatalf("%+v", jobs)
	}
	// verified-only: nothing is verified yet, so nothing moves
	var run struct {
		SkippedUnverified int `json:"skipped_unverified"`
		Synced            int `json:"synced"`
	}
	ca.must("POST", "/api/v1/sync/offsite/run?wait=1", nil, &run)
	if run.Synced != 0 || run.SkippedUnverified != 1 {
		t.Fatalf("%+v", run)
	}
	ca.must("POST", "/api/v1/verify?wait=1", map[string]any{"mode": "all"}, nil)
	ca.must("POST", "/api/v1/sync/offsite/run?wait=1", nil, &run)
	if run.Synced != 1 {
		t.Fatalf("%+v", run)
	}
	ca.must("PUT", "/api/v1/sync/offsite", map[string]any{"transfer_last": 3, "rate_limit_bps": 1000000, "resync_corrupt": true}, nil)
	var st map[string]any
	ca.must("GET", "/api/v1/sync/offsite", nil, &st)
	job := st["job"].(map[string]any)
	if job["transfer_last"].(float64) != 3 || job["resync_corrupt"] != true {
		t.Fatalf("%+v", st)
	}
	ca.must("DELETE", "/api/v1/sync/offsite", nil, nil)
	if code, _ := ca.do("GET", "/api/v1/sync/offsite", nil, nil); code != 404 {
		t.Fatalf("removed job -> %d", code)
	}
}

func TestOverviewAndMaintenanceModeOverTheAPI(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	k := masterKeys()
	c.must("PUT", "/api/v1/policies/p", api.Policy{Schedule: "03:00", Targets: []string{"a"}, KeepLast: 5}, nil)
	backup(t, n, k, "20261007100000-00000001", n.Now, "one", "two")
	if err := n.Svc.SampleCapacity(context.Background(), "custody"); err != nil {
		t.Fatal(err)
	}

	var ov struct {
		Repositories []struct {
			Name, Mode         string
			Snapshots, Objects int
			PhysicalBytes      int64 `json:"physical_bytes"`
			LogicalBytes       int64 `json:"logical_bytes"`
		}
		Hosts []struct {
			Name   string
			Disks  int
			Online bool
		}
		Protection struct{ Objects, Protected, Stale int }
		Runs24h    struct{ OK, Failed int }
	}
	c.must("GET", "/api/v1/overview", nil, &ov)
	if len(ov.Repositories) != 1 || ov.Repositories[0].Snapshots != 1 || ov.Repositories[0].Mode != "online" || ov.Repositories[0].PhysicalBytes == 0 {
		t.Fatalf("%+v", ov.Repositories)
	}
	if len(ov.Hosts) != 1 || ov.Hosts[0].Name != "benfica" || ov.Hosts[0].Disks != 1 || !ov.Hosts[0].Online {
		t.Fatalf("%+v", ov.Hosts)
	}
	if ov.Protection.Objects != 1 || ov.Protection.Protected != 1 || ov.Protection.Stale != 0 {
		t.Fatalf("protection %+v", ov.Protection)
	}

	// three days later the nightly object is overdue and the host silent: both show up as problems
	n.Now = n.Now.Add(72 * time.Hour)
	var ov2 struct {
		Protection struct{ Stale int }
		Problems   []struct{ Kind, Severity string }
	}
	c.must("GET", "/api/v1/overview", nil, &ov2)
	kinds := map[string]bool{}
	for _, p := range ov2.Problems {
		kinds[p.Kind] = true
	}
	if ov2.Protection.Stale != 1 || !kinds["stale_backup"] || !kinds["host_offline"] {
		t.Fatalf("%+v", ov2)
	}

	// maintenance mode over the API: a read-only repository refuses new backups with 503
	c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "read-only", "reason": "storage move"}, nil)
	var repos []struct {
		Name, Mode string
		ModeReason string `json:"mode_reason"`
	}
	c.must("GET", "/api/v1/repositories", nil, &repos)
	if repos[0].Mode != "read-only" || repos[0].ModeReason != "storage move" {
		t.Fatalf("%+v", repos)
	}
	if code, _ := c.do("POST", "/api/v1/gc", nil, nil); code != 503 {
		t.Fatalf("GC in read-only -> %d, want 503", code)
	}
	if code, _ := c.do("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "bogus"}, nil); code != 400 {
		t.Fatalf("bogus mode -> %d", code)
	}
	var series []map[string]any
	c.must("PUT", "/api/v1/repositories/custody/mode", map[string]string{"mode": "online"}, nil)
	c.must("GET", "/api/v1/capacity/series?days=7", nil, &series)
	if len(series) == 0 {
		t.Fatal("no capacity series")
	}
}

func TestSettingsOnboardingAndRawSnapshotMaterial(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	k := masterKeys()

	if code, _ := c.do("PUT", "/api/v1/settings", map[string]string{"base_url": "ftp://nope"}, nil); code != 400 {
		t.Fatalf("bad base_url -> %d", code)
	}
	if code, _ := c.do("PUT", "/api/v1/settings", map[string]string{"mystery": "x"}, nil); code != 400 {
		t.Fatalf("unknown key -> %d", code)
	}
	c.must("PUT", "/api/v1/settings", map[string]string{"base_url": "https://backup.example.com", "instance_name": "Bosch"}, nil)
	var st map[string]string
	c.must("GET", "/api/v1/settings", nil, &st)
	if st["base_url"] != "https://backup.example.com" || st["instance_name"] != "Bosch" {
		t.Fatalf("%v", st)
	}

	var ob struct {
		Token, Server, Command string
	}
	c.must("POST", "/api/v1/onboarding", map[string]any{"host": "benfica"}, &ob)
	if !strings.HasPrefix(ob.Token, "ibt1.") || !strings.Contains(ob.Command, "--token "+ob.Token) || !strings.Contains(ob.Command, "--name benfica") {
		t.Fatalf("%+v", ob)
	}
	c.must("PUT", "/api/v1/settings", map[string]string{"advertise_address": "backup.example.com:8007"}, nil)
	c.must("POST", "/api/v1/onboarding", map[string]any{"host": "other"}, &ob)
	if ob.Server != "backup.example.com:8007" {
		t.Fatalf("advertise setting ignored: %+v", ob)
	}

	snap := "20261007100000-00000001"
	backup(t, n, k, snap, n.Now, "alpha", "beta")
	code, idx := c.do("GET", "/api/v1/snapshots/"+group+"/"+snap+"/index", nil, nil)
	if code != 200 {
		t.Fatalf("index -> %d", code)
	}
	pi, err := format.ParseIndex([]byte(idx))
	if err != nil || len(pi.Entries) != 2 {
		t.Fatalf("index does not parse: %v", err)
	}
	if code, man := c.do("GET", "/api/v1/snapshots/"+group+"/"+snap+"/manifest", nil, nil); code != 200 || len(man) < 64 || man[:4] != "IBMF" {
		t.Fatalf("manifest -> %d", code)
	}
	code, blob := c.do("GET", "/api/v1/chunks/"+pi.Entries[0].ID.String(), nil, nil)
	if code != 200 {
		t.Fatalf("chunk -> %d", code)
	}
	if pt, err := k.DecodeChunk([]byte(blob), pi.Entries[0].ID); err != nil || string(pt) != "alpha" {
		t.Fatalf("the chunk the server hands out does not decrypt with the host key: %v %q", err, pt)
	}
	if code, _ := c.do("GET", "/api/v1/chunks/not-a-chunk-id", nil, nil); code != 400 {
		t.Fatalf("bad chunk id -> %d", code)
	}
}
