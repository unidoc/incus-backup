package mgmt_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// backupObject commits one snapshot of an arbitrary object through the real host protocol.
func backupObject(t *testing.T, cl *remote.Client, k *format.Keys, grp, project, object, kind, snap string, created time.Time, parts ...string) {
	t.Helper()
	ctx := context.Background()
	s, err := cl.Begin(ctx, api.SessionRequest{GroupID: grp, SnapshotID: snap, Created: created, Policy: "p", Project: project, Object: object, Kind: kind})
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
	if err := s.PutIndex(ctx, idx); err != nil {
		t.Fatal(err)
	}
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created, IndexDigest: format.IndexDigest(idx)}, grp, nil)
	if err := s.PutManifest(ctx, man); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, api.CommitRequest{LogicalBytes: int64(off), UploadedBytes: int64(off), DurationMs: 1500}); err != nil {
		t.Fatal(err)
	}
}

type objStat struct {
	GroupID      string `json:"group_id"`
	Host, Name   string
	Kind, Policy string
	Schedule     string
	Snapshots    int
	Protected    int
	Freshness    string
	Due          *time.Time
	OverdueAt    *time.Time `json:"overdue_at"`
	Uploaded     int64      `json:"uploaded_bytes"`
	Latest       *struct {
		ID           string
		Created      time.Time
		LogicalBytes int64 `json:"logical_bytes"`
		DurationMs   int64 `json:"duration_ms"`
	}
	History []struct {
		At           time.Time
		LogicalBytes int64 `json:"logical_bytes"`
	}
}

// GET /objects/stats judges freshness with exactly the overview's rule, so the object badges and the
// dashboard's protection counts can never disagree; it carries the latest snapshot and the size history.
func TestObjectStatsAgreeWithTheOverview(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	k := masterKeys()
	ctx := context.Background()
	c.must("PUT", "/api/v1/policies/p", api.Policy{Schedule: "03:00", Targets: []string{"a"}, KeepLast: 5}, nil)
	cl := hostClient(n, k)
	const g1, g2, g3 = "11111111111111111111111111111111", "22222222222222222222222222222222", "33333333333333333333333333333333"
	t0 := n.Now.Add(-2 * time.Hour)
	backupObject(t, cl, k, g1, "default", "instance/web/root", "block", "20261007010000-00000001", t0.Add(-24*time.Hour), "a", "bb")
	backupObject(t, cl, k, g1, "default", "instance/web/root", "block", "20261008010000-00000002", t0, "a", "bb", "ccc")
	backupObject(t, cl, k, g2, "default", "instance/web", "instance", "20261008010000-00000003", t0, "cfg")
	// an object the host declared but never backed up
	if _, err := n.Svc.DB.Pool().Exec(ctx, `INSERT INTO storage_objects (repository_id, host_name, project, name, kind, group_id, policy_name, retention_policy)
		SELECT id, 'benfica', 'default', 'volume/scratch', 'filesystem', $1, 'p', 'p' FROM repositories WHERE name='custody'`, g3); err != nil {
		t.Fatal(err)
	}
	c.must("PUT", "/api/v1/snapshots/"+g1+"/20261007010000-00000001/protected", map[string]bool{"protected": true}, nil)

	get := func(q string) map[string]objStat {
		var l []objStat
		c.must("GET", "/api/v1/objects/stats"+q, nil, &l)
		m := map[string]objStat{}
		for _, o := range l {
			m[o.GroupID] = o
		}
		return m
	}
	m := get("")
	if len(m) != 3 {
		t.Fatalf("%d objects: %+v", len(m), m)
	}
	a := m[g1]
	if a.Freshness != "ok" || a.Snapshots != 2 || a.Protected != 1 || a.Policy != "p" || a.Schedule != "03:00" || a.Due == nil || a.OverdueAt == nil {
		t.Fatalf("web/root: %+v", a)
	}
	if a.Latest == nil || a.Latest.ID != "20261008010000-00000002" || a.Latest.LogicalBytes != 6 || a.Latest.DurationMs != 1500 || a.Uploaded != 3+6 {
		t.Fatalf("latest: %+v uploaded %d", a.Latest, a.Uploaded)
	}
	if len(a.History) != 2 || a.History[0].LogicalBytes != 3 || a.History[1].LogicalBytes != 6 || !a.History[0].At.Before(a.History[1].At) {
		t.Fatalf("history (oldest first): %+v", a.History)
	}
	if m[g2].Freshness != "reference" || m[g3].Freshness != "never" || m[g3].Latest != nil || m[g3].History == nil {
		t.Fatalf("instance %+v / never %+v", m[g2], m[g3])
	}
	if l := get("?host=nobody"); len(l) != 0 {
		t.Fatalf("host filter: %+v", l)
	}
	if l := get("?host=benfica"); len(l) != 3 {
		t.Fatalf("host filter benfica: %d", len(l))
	}

	// the same counts as the overview, now and three days later (stale)
	for _, shift := range []time.Duration{0, 72 * time.Hour} {
		n.Now = n.Now.Add(shift)
		var ov struct {
			Protection struct {
				Objects, Protected, Stale, Unscheduled int
				Never                                  int `json:"never_backed_up"`
			}
		}
		c.must("GET", "/api/v1/overview", nil, &ov)
		cnt := map[string]int{}
		for _, o := range get("") {
			cnt[o.Freshness]++
		}
		p := ov.Protection
		if cnt["stale"] != p.Stale || cnt["never"] != p.Never || cnt["ok"]+cnt["unscheduled"] != p.Protected || cnt["unscheduled"] != p.Unscheduled ||
			cnt["ok"]+cnt["stale"]+cnt["never"]+cnt["unscheduled"] != p.Objects {
			t.Fatalf("shift %v: object stats %v disagree with overview %+v", shift, cnt, p)
		}
		if shift > 0 && cnt["stale"] != 1 {
			t.Fatalf("after 3 days the nightly object must be stale: %v", cnt)
		}
	}
}

// GET /runs/backups lists the runs the hosts reported, filterable and pageable.
func TestBackupRunHistory(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	k := masterKeys()
	ctx := context.Background()
	cl := hostClient(n, k)
	start := n.Now.Add(-3 * time.Hour)
	for i, st := range []string{"ok", "failed", "partial"} {
		id, err := cl.StartRun(ctx, api.RunStart{Started: start.Add(time.Duration(i) * time.Hour), ObjectsTotal: 3})
		if err != nil {
			t.Fatal(err)
		}
		f := api.RunFinish{Status: st, ObjectsOK: 3 - i, ObjectsFailed: i, LogicalBytes: 100, UploadedBytes: 10}
		if st != "ok" {
			f.Error = "object vm" + st + " failed"
		}
		if err := cl.FinishRun(ctx, id, f); err != nil {
			t.Fatal(err)
		}
	}
	type run struct {
		ID, Host, Status, Error string
		Started                 time.Time
		Finished                *time.Time
		ObjectsTotal            int   `json:"objects_total"`
		ObjectsFailed           int   `json:"objects_failed"`
		Uploaded                int64 `json:"uploaded_bytes"`
	}
	var all []run
	c.must("GET", "/api/v1/runs/backups", nil, &all)
	if len(all) != 3 || all[0].Status != "partial" || all[2].Status != "ok" || all[0].Host != "benfica" || all[0].Finished == nil ||
		all[1].Error != "object vmfailed failed" || all[1].ObjectsFailed != 1 || all[0].ObjectsTotal != 3 || all[0].Uploaded != 10 {
		t.Fatalf("%+v", all)
	}
	var l []run
	c.must("GET", "/api/v1/runs/backups?host=benfica&status=failed", nil, &l)
	if len(l) != 1 || l[0].Status != "failed" {
		t.Fatalf("status filter: %+v", l)
	}
	c.must("GET", "/api/v1/runs/backups?host=nobody", nil, &l)
	if l == nil || len(l) != 0 {
		t.Fatalf("unknown host must give [] not null: %+v", l)
	}
	c.must("GET", "/api/v1/runs/backups?limit=1&before="+all[0].Started.Format(time.RFC3339Nano), nil, &l)
	if len(l) != 1 || l[0].ID != all[1].ID {
		t.Fatalf("paging: %+v", l)
	}
	if code, _ := c.do("GET", "/api/v1/runs/backups?limit=x", nil, nil); code != 400 {
		t.Fatalf("bad limit -> %d", code)
	}
	if code, _ := c.do("GET", "/api/v1/runs/backups?before=yesterday", nil, nil); code != 400 {
		t.Fatalf("bad before -> %d", code)
	}
}

// Pending enrolment tokens: listed without the secret, revocable, gone once used or expired; the
// onboarding token really enrols a host that then appears in GET /hosts (what the wizard waits for).
func TestPendingEnrolments(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	ctx := context.Background()
	n.Enrol("benfica", api.ScopeBackup)

	type pending struct {
		ID, Host, Scope  string
		Created, Expires time.Time
		Replaces         bool
		Token            string // must never be present
	}
	var ob struct{ Token string }
	c.must("POST", "/api/v1/onboarding", map[string]any{"host": "newhost", "ttl_hours": 2}, &ob)
	c.must("POST", "/api/v1/onboarding", map[string]any{"host": "benfica"}, nil)
	var l []pending
	c.must("GET", "/api/v1/onboarding/pending", nil, &l)
	if len(l) != 2 || l[0].Host != "benfica" || !l[0].Replaces || l[1].Host != "newhost" || l[1].Replaces || l[1].Scope != "backup" || l[0].Token != "" {
		t.Fatalf("%+v", l)
	}
	if d := l[1].Expires.Sub(n.Now); d < 119*time.Minute || d > 121*time.Minute { // created_at is the database clock, expiry the service clock
		t.Fatalf("ttl %v", d)
	}
	_, body := c.do("GET", "/api/v1/onboarding/pending", nil, nil)
	for _, p := range []string{"token", "hash", ob.Token[10:30]} {
		if strings.Contains(body, p) {
			t.Fatalf("pending list leaks %q: %s", p, body)
		}
	}

	// revoke the re-enrolment token
	c.must("DELETE", "/api/v1/onboarding/pending/"+l[0].ID, nil, nil)
	if code, _ := c.do("DELETE", "/api/v1/onboarding/pending/"+l[0].ID, nil, nil); code != 404 {
		t.Fatalf("second revoke -> %d", code)
	}
	if code, _ := c.do("DELETE", "/api/v1/onboarding/pending/not-a-uuid", nil, nil); code != 404 {
		t.Fatalf("bad id -> %d", code)
	}

	// the onboarding token enrols for real; afterwards it is no longer pending and the host is listed
	if _, err := remote.Enroll(ctx, n.Addr(), ob.Token, "newhost", nil); err != nil {
		t.Fatal(err)
	}
	c.must("GET", "/api/v1/onboarding/pending", nil, &l)
	if len(l) != 0 {
		t.Fatalf("used token still pending: %+v", l)
	}
	var hosts []struct{ Name, Scope string }
	c.must("GET", "/api/v1/hosts", nil, &hosts)
	found := false
	for _, h := range hosts {
		found = found || (h.Name == "newhost" && h.Scope == "backup")
	}
	if !found {
		t.Fatalf("enrolled host not listed: %+v", hosts)
	}

	// expired tokens are not pending
	c.must("POST", "/api/v1/onboarding", map[string]any{"host": "late", "ttl_hours": 1}, nil)
	n.Now = n.Now.Add(61 * time.Minute)
	c.must("GET", "/api/v1/onboarding/pending", nil, &l)
	if len(l) != 0 {
		t.Fatalf("expired token still pending: %+v", l)
	}
}
