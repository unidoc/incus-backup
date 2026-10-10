package mgmt_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// backupWith commits one snapshot with the given run counters and base snapshot.
func backupWith(t *testing.T, n *testsupport.Node, k *format.Keys, snap, prev string, created time.Time, cr api.CommitRequest, parts ...string) {
	t.Helper()
	ctx := context.Background()
	s, err := hostClient(n, k).Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: snap, Created: created, Policy: "p", Project: "default",
		Object: "volume/data", Kind: "filesystem", PreviousID: prev})
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
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created, IndexDigest: format.IndexDigest(idx)}, group, nil)
	if err := s.PutManifest(ctx, man); err != nil {
		t.Fatal(err)
	}
	cr.LogicalBytes = int64(off)
	if _, err := s.Commit(ctx, cr); err != nil {
		t.Fatal(err)
	}
}

type statsRow struct {
	GroupID       string     `json:"group_id"`
	SnapshotID    string     `json:"snapshot_id"`
	LogicalBytes  int64      `json:"logical_bytes"`
	DirtyBytes    int64      `json:"dirty_bytes"`
	ReadBytes     int64      `json:"read_bytes"`
	UploadedBytes int64      `json:"uploaded_bytes"`
	FullRead      bool       `json:"full_read"`
	DurationMs    int64      `json:"duration_ms"`
	PreviousID    string     `json:"previous_snapshot_id"`
	Source        string     `json:"source"`
	VerifyState   string     `json:"verify_state"`
	VerifiedAt    *time.Time `json:"verified_at"`
}

// GET /snapshot-stats returns what each backup run recorded (the counters the UI shows per snapshot),
// the base snapshot and the latest verification, for the whole repository in one call.
func TestSnapshotStatsRoute(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	k := masterKeys()

	// an empty repository answers an empty list, not null
	code, body := c.do("GET", "/api/v1/snapshot-stats", nil, nil)
	if code != 200 || body != "[]\n" {
		t.Fatalf("empty repository -> %d %q", code, body)
	}

	s1, s2 := "20261001120000-00000001", "20261002120000-00000002"
	backupWith(t, n, k, s1, "", n.Now, api.CommitRequest{DirtyBytes: 11, ReadBytes: 11, UploadedBytes: 9, FullRead: true, DurationMs: 1500}, "first", "shared")
	backupWith(t, n, k, s2, s1, n.Now.Add(time.Hour), api.CommitRequest{DirtyBytes: 4, ReadBytes: 6, UploadedBytes: 3, DurationMs: 250}, "secnd", "shared")

	var rows []statsRow
	c.must("GET", "/api/v1/snapshot-stats", nil, &rows)
	if len(rows) != 2 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	a, b := rows[0], rows[1]
	if a.SnapshotID != s1 || b.SnapshotID != s2 || a.GroupID != group {
		t.Fatalf("order/ids: %+v", rows)
	}
	if a.DirtyBytes != 11 || a.ReadBytes != 11 || a.UploadedBytes != 9 || !a.FullRead || a.DurationMs != 1500 || a.LogicalBytes != 11 || a.PreviousID != "" {
		t.Fatalf("first run counters: %+v", a)
	}
	if b.DirtyBytes != 4 || b.ReadBytes != 6 || b.UploadedBytes != 3 || b.FullRead || b.DurationMs != 250 || b.PreviousID != s1 {
		t.Fatalf("second run counters: %+v", b)
	}
	if a.Source != "client" || a.VerifyState != "" || a.VerifiedAt != nil {
		t.Fatalf("never verified yet: %+v", a)
	}

	// a verification shows up as state + time
	c.must("POST", "/api/v1/verify?wait=1", map[string]any{"mode": "all"}, nil)
	c.must("GET", "/api/v1/snapshot-stats", nil, &rows)
	for _, r := range rows {
		if r.VerifyState != "ok" || r.VerifiedAt == nil || r.VerifiedAt.Location() != time.UTC {
			t.Fatalf("after verify: %+v", r)
		}
	}

	// ?group= narrows (several groups comma separated); an unknown group is an empty list
	c.must("GET", "/api/v1/snapshot-stats?group=ffffffffffffffffffffffffffffffff,"+group, nil, &rows)
	if len(rows) != 2 {
		t.Fatalf("group filter: %d rows", len(rows))
	}
	c.must("GET", "/api/v1/snapshot-stats?group=ffffffffffffffffffffffffffffffff", nil, &rows)
	if len(rows) != 0 {
		t.Fatalf("unknown group: %d rows", len(rows))
	}
	if code, _ := c.do("GET", "/api/v1/snapshot-stats?repository=nope", nil, nil); code != 404 {
		t.Fatalf("unknown repository -> %d", code)
	}
}

// POST /restore-tests/run must carry the project: jobspec requires it for restore_test, and the
// route used to build params without it, so every request failed with 400.
func TestRunRestoreTestRouteCarriesTheProject(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	k := masterKeys()
	backupWith(t, n, k, "20261001120000-00000001", "", n.Now, api.CommitRequest{}, "x")

	var job struct {
		ID     string          `json:"id"`
		Type   string          `json:"type"`
		Host   string          `json:"host"`
		State  string          `json:"state"`
		Params json.RawMessage `json:"params"`
	}
	c.must("POST", "/api/v1/restore-tests/run", map[string]any{"host": "benfica", "project": "default", "object": "volume/data", "mode": "extract"}, &job)
	var p struct{ Project, Object, Mode, Snapshot string }
	if err := json.Unmarshal(job.Params, &p); err != nil {
		t.Fatal(err)
	}
	if job.Type != "restore_test" || job.Host != "benfica" || job.State != "queued" || p.Project != "default" || p.Object != "volume/data" || p.Mode != "extract" {
		t.Fatalf("job %+v params %+v", job, p)
	}
	// the mode defaults to deep, a missing project is a 400 that says so
	c.must("POST", "/api/v1/restore-tests/run", map[string]any{"host": "benfica", "project": "default", "object": "volume/data"}, &job)
	_ = json.Unmarshal(job.Params, &p)
	if p.Mode != "deep" {
		t.Fatalf("default mode %q", p.Mode)
	}
	if code, body := c.do("POST", "/api/v1/restore-tests/run", map[string]any{"host": "benfica", "object": "volume/data"}, nil); code != 400 {
		t.Fatalf("missing project -> %d %s", code, body)
	}
}
