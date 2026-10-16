package mgmt_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

type check struct {
	ID     string `json:"id"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

func levels(cs []check) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		m[c.ID] = c.Level
	}
	return m
}

func TestStorageRoutesRepositoryCheckInfoAndEffectivePolicies(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	var rep struct {
		Ready     bool    `json:"ready"`
		Purpose   string  `json:"purpose"`
		Checks    []check `json:"checks"`
		FreeBytes *uint64 `json:"free_bytes"`
	}
	dir := filepath.Join(t.TempDir(), "new-repo")
	c.must("POST", "/api/v1/repositories/check", map[string]any{"name": "second", "kind": "filesystem", "path": dir}, &rep)
	if !rep.Ready || rep.Purpose != "create" || levels(rep.Checks)["path"] != "pass" || rep.FreeBytes == nil {
		t.Fatalf("%+v", rep)
	}
	// S3 (the configured test service), inline secret: every probe step, and the secret never comes back
	svc, bucket := s3test.NewBucket(t)
	req := map[string]any{"name": "off", "kind": "s3", "access_key": svc.AccessKey, "secret_key": svc.SecretKey,
		"s3": map[string]any{"endpoint": svc.Endpoint, "bucket": bucket, "prefix": "ib", "no_tls": svc.NoTLS, "path_style": true}}
	code, body := c.do("POST", "/api/v1/repositories/check", req, &rep)
	if code != 200 || !rep.Ready || levels(rep.Checks)["conditional"] != "pass" || levels(rep.Checks)["versioning"] != "pass" {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(body, svc.SecretKey) {
		t.Fatal("the secret key came back")
	}
	// unknown fields are refused like every other route
	if code, _ := c.do("POST", "/api/v1/repositories/check", map[string]any{"name": "x", "kind": "filesystem", "bogus": 1}, nil); code != 400 {
		t.Fatalf("unknown field -> %d", code)
	}
	// existing repository: deep check and identity
	c.must("POST", "/api/v1/repositories/custody/check", nil, &rep)
	if !rep.Ready || rep.Purpose != "existing" || levels(rep.Checks)["open"] != "pass" {
		t.Fatalf("%+v", rep)
	}
	var info struct {
		ServerID    string `json:"server_id"`
		DatastoreID string `json:"datastore_id"`
		Owner       *struct {
			ServerID string `json:"server_id"`
		} `json:"owner"`
	}
	c.must("GET", "/api/v1/repositories/custody/info", nil, &info)
	if info.Owner == nil || info.Owner.ServerID != info.ServerID || info.DatastoreID == "" {
		t.Fatalf("%+v", info)
	}
	if code, _ := c.do("GET", "/api/v1/repositories/nope/info", nil, nil); code != 404 {
		t.Fatalf("unknown repository -> %d", code)
	}
	// effective policies: the object a host backed up, with the policy pinned at first sight
	k := masterKeys()
	backup(t, n, k, "20261001120000-00000001", n.Now, "one")
	var eff []struct {
		GroupID string `json:"group_id"`
		Policy  string `json:"policy"`
		Source  string `json:"source"`
	}
	c.must("GET", "/api/v1/policies/effective", nil, &eff)
	if len(eff) != 1 || eff[0].GroupID != group || eff[0].Policy != "p" || eff[0].Source != "pinned" {
		t.Fatalf("%+v", eff)
	}
	c.must("PUT", "/api/v1/policies/keep3", map[string]any{"keep_last": 3}, nil)
	c.must("PUT", "/api/v1/assignments", map[string]any{"host": "benfica", "project": "", "object": "", "policy": "keep3"}, nil)
	c.must("GET", "/api/v1/policies/effective", nil, &eff)
	if eff[0].Policy != "keep3" || eff[0].Source != "host" {
		t.Fatalf("%+v", eff)
	}
}

func TestStorageRoutesSchedulePreview(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	var p struct {
		Valid  bool        `json:"valid"`
		Error  string      `json:"error"`
		Manual bool        `json:"manual"`
		Next   []time.Time `json:"next"`
		Zone   string      `json:"zone"`
		DueNow bool        `json:"due_now"`
	}
	c.must("GET", "/api/v1/schedule/preview?spec=mon+02:30&count=3", nil, &p)
	if !p.Valid || len(p.Next) != 3 || p.Zone == "" {
		t.Fatalf("%+v", p)
	}
	for i, x := range p.Next {
		if x.Weekday() != time.Monday || x.Hour() != 2 || x.Minute() != 30 || (i > 0 && x.Sub(p.Next[i-1]) != 7*24*time.Hour) {
			t.Fatalf("next %d = %v", i, x)
		}
	}
	c.must("GET", "/api/v1/schedule/preview?spec=every+30s", nil, &p)
	if p.Valid || !strings.Contains(p.Error, "interval") {
		t.Fatalf("%+v", p)
	}
	c.must("GET", "/api/v1/schedule/preview?spec=manual", nil, &p)
	if !p.Valid || !p.Manual || len(p.Next) != 0 {
		t.Fatalf("%+v", p)
	}
	old := n.Now.Add(-48 * time.Hour).Format(time.RFC3339)
	c.must("GET", "/api/v1/schedule/preview?spec=daily&after="+old, nil, &p)
	if !p.DueNow {
		t.Fatalf("a daily job last run two days ago must be due: %+v", p)
	}
}

// The peer check proves reachability and identity WITHOUT spending the token: the same token then
// adds the job. Run history and the schedule/pause route work on the job.
func TestStorageRoutesSyncCheckRemoteRunsAndSchedule(t *testing.T) {
	a, b, other := testsupport.NewNode(t), testsupport.NewNode(t), testsupport.NewNode(t)
	ca, _ := newClient(t, a)
	k := masterKeys()
	backup(t, a, k, "20261001120000-00000001", a.Now, "one", "shared")
	tok, err := b.Svc.CreateToken(context.Background(), "custody", "sync-custody-offsite", api.ScopeSyncWrite, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := other.Svc.CreateToken(context.Background(), "custody", "x", api.ScopeSyncWrite, time.Hour)
	var res struct {
		OK     bool    `json:"ok"`
		Checks []check `json:"checks"`
	}
	ca.must("POST", "/api/v1/sync/check-remote", map[string]any{"remote": b.Addr(), "token": tok}, &res)
	if lv := levels(res.Checks); !res.OK || lv["identity"] != "pass" || lv["api"] != "pass" || lv["tls"] != "pass" {
		t.Fatalf("%+v", res)
	}
	// a token of another server: the identity check fails (wrong address or wrong token)
	ca.must("POST", "/api/v1/sync/check-remote", map[string]any{"remote": b.Addr(), "token": wrong}, &res)
	if res.OK || levels(res.Checks)["identity"] != "fail" {
		t.Fatalf("%+v", res)
	}
	// refused port, no port, garbage token
	ca.must("POST", "/api/v1/sync/check-remote", map[string]any{"remote": "127.0.0.1:1"}, &res)
	if lv := levels(res.Checks); res.OK || lv["tcp"] != "fail" || lv["tls"] != "skip" {
		t.Fatalf("%+v", res)
	}
	ca.must("POST", "/api/v1/sync/check-remote", map[string]any{"remote": "backup.example.net"}, &res)
	if res.OK || levels(res.Checks)["address"] != "fail" {
		t.Fatalf("%+v", res)
	}
	ca.must("POST", "/api/v1/sync/check-remote", map[string]any{"remote": b.Addr(), "token": "nonsense"}, &res)
	if res.OK || levels(res.Checks)["token"] != "fail" {
		t.Fatalf("%+v", res)
	}
	// the checked token is still good
	ca.must("POST", "/api/v1/sync", map[string]any{"name": "offsite", "direction": "push", "remote": b.Addr(), "schedule": "02:00", "token": tok}, nil)
	ca.must("POST", "/api/v1/sync/offsite/run?wait=1", nil, nil)
	var runs []struct {
		Status          string     `json:"status"`
		SnapshotsSynced int        `json:"snapshots_synced"`
		FinishedAt      *time.Time `json:"finished_at"`
	}
	ca.must("GET", "/api/v1/sync/offsite/runs?limit=10", nil, &runs)
	if len(runs) != 1 || runs[0].Status != "ok" || runs[0].SnapshotsSynced != 1 || runs[0].FinishedAt == nil {
		t.Fatalf("%+v", runs)
	}
	// schedule and pause
	if code, body := ca.do("PUT", "/api/v1/sync/offsite/schedule", map[string]any{"schedule": "every 30s"}, nil); code != 400 {
		t.Fatalf("bad schedule -> %d %s", code, body)
	}
	ca.must("PUT", "/api/v1/sync/offsite/schedule", map[string]any{"schedule": "every 6h", "enabled": false}, nil)
	var jobs []struct {
		Schedule string `json:"schedule"`
		Enabled  bool   `json:"enabled"`
	}
	ca.must("GET", "/api/v1/sync", nil, &jobs)
	if len(jobs) != 1 || jobs[0].Schedule != "every 6h" || jobs[0].Enabled {
		t.Fatalf("%+v", jobs)
	}
	ca.must("PUT", "/api/v1/sync/offsite/schedule", map[string]any{"enabled": true}, nil)
	ca.must("GET", "/api/v1/sync", nil, &jobs)
	if jobs[0].Schedule != "every 6h" || !jobs[0].Enabled {
		t.Fatalf("%+v", jobs)
	}
	if code, _ := ca.do("GET", "/api/v1/sync/nope/runs", nil, nil); code != 404 {
		t.Fatalf("unknown job -> %d", code)
	}
}
