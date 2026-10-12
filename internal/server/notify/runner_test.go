package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/service"
)

func TestBackupMissedLifecycle(t *testing.T) {
	e := newEnv(t)
	if err := e.eng.SetBaseURL(e.ctx, "https://backup.example.com/"); err != nil {
		t.Fatal(err)
	}
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "ops", func(r *notify.Rule) { r.RepeatMinutes = 360 })
	e.policy("daily", "03:00")
	hid := e.host("node1", ptr(e.clock()))
	g := e.object(hid, "node1", "default", "bosch", "daily", e.clock().Add(-10*24*time.Hour))
	e.snapshot(g, e.clock().Add(-30*time.Hour))

	// 30 h < 1.5*24h + 60 min = 37 h: nothing
	e.tick()
	if as := e.alerts(notify.AlertFilter{Active: true}); len(as) != 0 {
		t.Fatalf("alerts at 30 h: %+v", as)
	}
	e.advance(8 * time.Hour) // 38 h
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	r := e.tick()
	key := "backup_missed:custody/" + g
	a := e.alertByKey(key)
	if a == nil || a.State != notify.StateFiring || a.Severity != notify.SeverityWarning || r.Fired != 1 {
		t.Fatalf("expected a firing warning, got %+v (tick %+v)", a, r)
	}
	if a.Title != "bosch: no successful backup for 38 h" || a.Host != "node1" || a.Object != "default/bosch" {
		t.Fatalf("title/where: %+v", a)
	}
	ms := rcv.messages(t)
	if len(ms) != 1 || ms[0].Kind != "firing" || ms[0].Subject != "[WARNING] bosch: no successful backup for 38 h" {
		t.Fatalf("firing message: %+v", ms)
	}
	if ms[0].Link != fmt.Sprintf("https://backup.example.com/alerts/%d", a.ID) || !strings.Contains(ms[0].Text, "What to do:") ||
		!strings.Contains(ms[0].Text, "Where: host node1, repository custody, object default/bosch") {
		t.Fatalf("body: %s / link %s", ms[0].Text, ms[0].Link)
	}

	// same state again: no new message (the state did not change)
	e.advance(time.Minute)
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	if n := len(rcv.requests()); n != 1 {
		t.Fatalf("a second tick sent again: %d", n)
	}
	// reminder after repeat_minutes (6 h)
	e.advance(6 * time.Hour)
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	ms = rcv.messages(t)
	if len(ms) != 2 || ms[1].Kind != "reminder" || !strings.HasPrefix(ms[1].Subject, "[WARNING] Still firing: bosch") {
		t.Fatalf("reminder: %+v", kinds(ms))
	}
	// acknowledged: no more reminders
	if _, err := e.eng.Acknowledge(e.ctx, a.ID, "alice", "looking"); err != nil {
		t.Fatal(err)
	}
	e.advance(7 * time.Hour)
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	if n := len(rcv.requests()); n != 2 {
		t.Fatalf("reminder although acknowledged: %v", kinds(rcv.messages(t)))
	}
	// beyond 3*24h+60min: escalated to critical (announced even though acknowledged)
	e.advance(30 * time.Hour) // 38+6+7+30 = 81 h (+1 min)
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	ms = rcv.messages(t)
	if len(ms) != 3 || ms[2].Kind != "escalation" || ms[2].Severity != notify.SeverityCritical || !strings.HasPrefix(ms[2].Subject, "[CRITICAL] bosch") {
		t.Fatalf("escalation: %+v", kinds(ms))
	}
	if !strings.Contains(ms[2].Text, "Severity raised from warning to critical") || !strings.Contains(ms[2].Text, "acknowledged by alice") {
		t.Fatalf("escalation body: %s", ms[2].Text)
	}
	// a new snapshot: resolves by itself, "resolved" sent once, acknowledgement history kept
	e.snapshot(g, e.clock())
	r = e.tick()
	a = e.alertByKey(key)
	if a.State != notify.StateResolved || a.ResolvedAt == nil || r.Resolved != 1 {
		t.Fatalf("not resolved: %+v", a)
	}
	ms = rcv.messages(t)
	if len(ms) != 4 || ms[3].Kind != "resolved" || !strings.HasPrefix(ms[3].Subject, "[RESOLVED] ") {
		t.Fatalf("resolved: %+v", kinds(ms))
	}
	e.advance(time.Hour)
	e.tick()
	if n := len(rcv.requests()); n != 4 {
		t.Fatalf("resolved sent twice: %v", kinds(rcv.messages(t)))
	}
	// fires again later: same row, new episode, count 2, acknowledgement cleared
	e.advance(40 * time.Hour)
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	a2 := e.alertByKey(key)
	if a2.ID != a.ID || a2.Episode != 2 || a2.Count != 2 || a2.State != notify.StateFiring || a2.AckedBy != "" {
		t.Fatalf("re-fire: %+v", a2)
	}
	if ms = rcv.messages(t); len(ms) != 5 || ms[4].Kind != "firing" {
		t.Fatalf("re-fire message: %v", kinds(ms))
	}
	// the delivery log and the alert timeline
	det, err := e.eng.GetAlert(e.ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(det.Deliveries) != 5 || len(det.Events) < 8 {
		t.Fatalf("detail: %d deliveries, %d events", len(det.Deliveries), len(det.Events))
	}
	for _, d := range det.Deliveries {
		if d.State != "sent" || d.Attempts != 1 || d.SentAt == nil {
			t.Fatalf("delivery: %+v", d)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestBackupMissedEdges(t *testing.T) {
	e := newEnv(t)
	e.policy("daily", "daily")
	e.policy("hand", "manual")
	e.policy("often", "every 4h")
	hid := e.host("node1", ptr(e.clock()))
	// a brand-new object is not late before first_seen + threshold
	fresh := e.object(hid, "node1", "default", "fresh", "daily", e.clock().Add(-2*time.Hour))
	// never backed up for long: fires with a "never" title
	never := e.object(hid, "node1", "web", "never", "daily", e.clock().Add(-3*24*time.Hour))
	// manual policy: never late
	e.object(hid, "node1", "default", "manual", "hand", e.clock().Add(-30*24*time.Hour))
	// every 4h: threshold 1.5*4h+1h = 7h
	often := e.object(hid, "node1", "default", "often", "often", e.clock().Add(-30*24*time.Hour))
	e.snapshot(often, e.clock().Add(-8*time.Hour))
	// stale object (not reported for > 7 days: instance deleted): retired
	stale := e.object(hid, "node1", "default", "gone", "daily", e.clock().Add(-30*24*time.Hour))
	e.exec(`UPDATE storage_objects SET last_seen_at=$2 WHERE group_id=$1`, stale, e.clock().Add(-8*24*time.Hour))
	// object without an owner host (sync destination): not checked here
	e.exec(`INSERT INTO storage_objects (repository_id, project, name, group_id, retention_policy, first_seen_at) VALUES ($1,'default','instance/replica/root',$2,'daily',$3)`,
		e.repo.ID, randHex(16), e.clock().Add(-30*24*time.Hour))
	// disabled host: not checked
	h2 := e.host("node2", ptr(e.clock()))
	e.object(h2, "node2", "default", "off", "daily", e.clock().Add(-30*24*time.Hour))
	e.exec(`UPDATE hosts SET disabled=true WHERE id=$1`, h2)
	// an admin assignment overrides the pinned policy: object pinned "daily" but assigned "hand"
	assigned := e.object(hid, "node1", "default", "assigned", "daily", e.clock().Add(-30*24*time.Hour))
	e.exec(`INSERT INTO policy_assignments (host_id, project, object, policy_id) SELECT $1,'default','assigned',id FROM policies WHERE name='hand'`, hid)

	e.tick()
	got := map[string]notify.Alert{}
	for _, a := range e.alerts(notify.AlertFilter{Active: true, Type: notify.TypeBackupMissed}) {
		got[a.Key] = a
	}
	if len(got) != 2 {
		t.Fatalf("expected never+often, got %v", got)
	}
	if a, ok := got["backup_missed:custody/"+never]; !ok || a.Title != "web/never: never backed up (known for 3 d)" || a.Object != "web/never" {
		t.Fatalf("never: %+v", a)
	}
	if a, ok := got["backup_missed:custody/"+often]; !ok || a.Title != "often: no successful backup for 8 h" {
		t.Fatalf("often: %+v", a)
	}
	_ = fresh
	_ = assigned
	// the thresholds are data: raising the grace to 2 h clears the 4-hourly one (6h + 2h = 8 h, not exceeded... exactly at 8 h is not late)
	if err := e.eng.SetCondition(e.ctx, notify.Condition{Type: notify.TypeBackupMissed, Enabled: true, Severity: notify.SeverityWarning,
		Params: json.RawMessage(`{"grace_minutes": 120}`)}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if a := e.alertByKey("backup_missed:custody/" + often); a.State != notify.StateResolved {
		t.Fatalf("still firing after raising the grace: %+v", a)
	}
	if err := e.eng.SetCondition(e.ctx, notify.Condition{Type: notify.TypeBackupMissed, Enabled: true, Severity: notify.SeverityWarning,
		Params: json.RawMessage(`{"no_such_knob": 1}`)}); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("unknown parameter accepted: %v", err)
	}
	// disabling the condition ends its alerts silently
	if err := e.eng.SetCondition(e.ctx, notify.Condition{Type: notify.TypeBackupMissed, Enabled: false, Severity: notify.SeverityWarning}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if a := e.alertByKey("backup_missed:custody/" + never); a.State != notify.StateResolved || !a.ResolvedSilently {
		t.Fatalf("disabled condition: %+v", a)
	}
}

func TestHostOfflineBackupFailedMaintenance(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "ops", nil)
	hid := e.host("node1", ptr(e.clock().Add(-3*time.Hour)))
	e.host("never", nil) // enrolled 30 days ago, never seen
	e.exec(`INSERT INTO backup_runs (repository_id, host_id, started_at, finished_at, status, error) VALUES ($1,$2,$3,$4,'failed','incus: connection refused')`,
		e.repo.ID, hid, e.clock().Add(-2*time.Hour), e.clock().Add(-2*time.Hour))
	e.exec(`INSERT INTO maintenance_runs (repository_id, kind, started_at, finished_at, status, error) VALUES ($1,'gc',$2,$2,'failed','disk full')`, e.repo.ID, e.clock().Add(-time.Hour))
	e.tick()
	off := e.alertByKey("host_offline:custody/node1")
	nev := e.alertByKey("host_offline:custody/never")
	bf := e.alertByKey("backup_failed:custody/node1")
	mf := e.alertByKey("maintenance_failed:custody/gc")
	if off == nil || off.Severity != notify.SeverityWarning || off.Title != "node1: offline for 3 h" {
		t.Fatalf("offline: %+v", off)
	}
	if nev == nil || nev.Severity != notify.SeverityCritical || !strings.Contains(nev.Title, "never contacted") {
		t.Fatalf("never seen: %+v", nev)
	}
	if bf == nil || bf.Severity != notify.SeverityCritical || !strings.Contains(bf.Detail, "incus: connection refused") {
		t.Fatalf("backup failed: %+v", bf)
	}
	if mf == nil || mf.Title != "custody: gc failed" || !strings.Contains(mf.Detail, "disk full") {
		t.Fatalf("maintenance: %+v", mf)
	}
	// a partial run replaces the failure: same key, severity drops to warning (no new message for a drop)
	e.exec(`INSERT INTO backup_runs (repository_id, host_id, started_at, finished_at, status, objects_total, objects_failed) VALUES ($1,$2,$3,$3,'partial',5,1)`,
		e.repo.ID, hid, e.clock().Add(-time.Hour))
	before := len(rcv.requests())
	e.tick()
	if bf = e.alertByKey("backup_failed:custody/node1"); bf.Severity != notify.SeverityWarning || bf.Title != "node1: backup run partly failed (1 of 5 objects)" {
		t.Fatalf("partial: %+v", bf)
	}
	if len(rcv.requests()) != before {
		t.Fatalf("a severity drop sent a message")
	}
	// recover: host seen, a good run, a good gc
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.exec(`INSERT INTO backup_runs (repository_id, host_id, started_at, finished_at, status) VALUES ($1,$2,$3,$3,'ok')`, e.repo.ID, hid, e.clock())
	e.exec(`INSERT INTO maintenance_runs (repository_id, kind, started_at, finished_at, status) VALUES ($1,'gc',$2,$2,'ok')`, e.repo.ID, e.clock())
	e.tick()
	for _, k := range []string{"host_offline:custody/node1", "host_offline:custody/never", "backup_failed:custody/node1", "maintenance_failed:custody/gc"} {
		if a := e.alertByKey(k); a.State != notify.StateResolved {
			t.Fatalf("%s not resolved: %+v", k, a)
		}
	}
	res := 0
	for _, m := range rcv.messages(t) {
		if m.Kind == "resolved" {
			res++
		}
	}
	if res != 4 {
		t.Fatalf("resolved messages: %v", kinds(rcv.messages(t)))
	}
}

func TestCorruptionSyncCapacity(t *testing.T) {
	e := newEnv(t)
	hid := e.host("node1", ptr(e.clock()))
	g := e.object(hid, "node1", "default", "db", "", e.clock())
	sid := e.snapshot(g, e.clock().Add(-time.Hour))
	e.exec(`UPDATE snapshots SET verify_state='failed', verified_at=$2 WHERE snapshot_id=$1`, sid, e.clock())
	e.exec(`INSERT INTO corrupt_chunks (repository_id, chunk_id, detected_at) VALUES ($1,'c1',$2), ($1,'c2',$2)`, e.repo.ID, e.clock())
	var job string
	if err := e.d.Pool().QueryRow(e.ctx, `INSERT INTO sync_jobs (repository_id, name, direction, remote, remote_repository, schedule, created_at, last_success_at)
		VALUES ($1,'offsite','push','https://dr.example:8443','dr','daily',$2,$3) RETURNING id`, e.repo.ID, e.clock().Add(-30*24*time.Hour), e.clock().Add(-50*time.Hour)).Scan(&job); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO sync_runs (job_id, started_at, finished_at, status, error) VALUES ($1,$2,$2,'failed','remote: 503')`, job, e.clock().Add(-time.Hour))
	// capacity: 95 % used, growing 10 GiB/day with 30 GiB free -> full in ~3 days
	gib := int64(1 << 30)
	for i := 0; i <= 7; i++ {
		at := e.clock().Add(-time.Duration(7-i) * 24 * time.Hour)
		used := int64(500)*gib + int64(i)*10*gib
		e.exec(`INSERT INTO capacity_samples (repository_id, taken_at, physical_bytes, chunk_count, snapshot_count, fs_total_bytes, fs_free_bytes) VALUES ($1,$2,$3,1,1,$4,$5)`,
			e.repo.ID, at, used, 610*gib, 610*gib-used)
	}
	e.tick()
	c := e.alertByKey("corruption:custody/" + g)
	cc := e.alertByKey("corrupt_chunks:custody")
	sf := e.alertByKey("sync_failed:custody/offsite")
	sl := e.alertByKey("sync_lag:custody/offsite")
	ca := e.alertByKey("capacity:custody")
	if c == nil || c.Severity != notify.SeverityCritical || c.Title != "db: 1 snapshot(s) failed verification" {
		t.Fatalf("corruption: %+v", c)
	}
	if cc == nil || cc.Type != notify.TypeCorruption || cc.Title != "custody: 2 corrupt chunk(s) quarantined" {
		t.Fatalf("chunks: %+v", cc)
	}
	if sf == nil || !strings.Contains(sf.Detail, "remote: 503") {
		t.Fatalf("sync failed: %+v", sf)
	}
	if sl == nil || sl.Title != "sync offsite (custody): no successful run for 50 h" {
		t.Fatalf("sync lag: %+v", sl)
	}
	if ca == nil || !strings.Contains(ca.Title, "93% used") || !strings.Contains(ca.Title, "full in about") || ca.Severity != notify.SeverityWarning {
		t.Fatalf("capacity: %+v", ca)
	}
	// heal everything
	e.exec(`UPDATE snapshots SET verify_state='ok'`)
	e.exec(`UPDATE corrupt_chunks SET state='healed'`)
	e.exec(`INSERT INTO sync_runs (job_id, started_at, finished_at, status) VALUES ($1,$2,$2,'ok')`, job, e.clock())
	e.exec(`UPDATE sync_jobs SET last_success_at=$1`, e.clock())
	e.exec(`INSERT INTO capacity_samples (repository_id, taken_at, physical_bytes, chunk_count, snapshot_count, fs_total_bytes, fs_free_bytes) VALUES ($1,$2,$3,1,1,$4,$5)`,
		e.repo.ID, e.clock().Add(time.Minute), 100*gib, 600*gib, 500*gib)
	e.advance(2 * time.Minute)
	e.tick()
	for _, a := range e.alerts(notify.AlertFilter{Limit: 100}) {
		if a.State != notify.StateResolved {
			t.Fatalf("still open after healing: %+v", a)
		}
	}
}

type fakeCerts struct {
	mu    sync.Mutex
	items []notify.CertItem
	err   error
}

func (f *fakeCerts) status(context.Context) ([]notify.CertItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.CertItem(nil), f.items...), f.err
}

type fakeRestore struct {
	mu  sync.Mutex
	res []notify.RestoreTestResult
}

func (f *fakeRestore) LatestRestoreTests(context.Context) ([]notify.RestoreTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.RestoreTestResult(nil), f.res...), nil
}

func TestCertsRestoreTestsWiring(t *testing.T) {
	e := newEnv(t)
	// not wired: nothing evaluated, no error
	e.tick()
	certs := &fakeCerts{items: []notify.CertItem{
		{Name: "server", Subject: "CN=backup", NotAfter: e.clock().Add(10 * 24 * time.Hour)},
		{Name: "ca", Subject: "CN=ca", NotAfter: e.clock().Add(400 * 24 * time.Hour)},
		{Name: "acme", Subject: "CN=x", NotAfter: e.clock().Add(-time.Hour)},
	}}
	rt := &fakeRestore{res: []notify.RestoreTestResult{{Repository: "custody", Host: "node1", Object: "default/db", GroupID: "g1", OK: false, At: e.clock(), Error: "checksum mismatch"},
		{Repository: "custody", Host: "node1", Object: "default/web", GroupID: "g2", OK: true, At: e.clock()}}}
	e.eng.CertStatus = certs.status
	e.eng.RestoreTests = rt
	e.tick()
	if a := e.alertByKey("cert_expiry:server"); a == nil || a.Severity != notify.SeverityWarning || a.Title != "certificate server expires in 10 d" {
		t.Fatalf("server cert: %+v", a)
	}
	if a := e.alertByKey("cert_expiry:acme"); a == nil || a.Severity != notify.SeverityCritical {
		t.Fatalf("expired cert: %+v", a)
	}
	if a := e.alertByKey("cert_expiry:ca"); a != nil {
		t.Fatalf("valid cert alerted: %+v", a)
	}
	if a := e.alertByKey("restore_test_failed:custody/g1"); a == nil || !strings.Contains(a.Detail, "checksum mismatch") {
		t.Fatalf("restore test: %+v", a)
	}
	// a failing source must not resolve its alerts (no false "resolved")
	certs.mu.Lock()
	certs.err = errors.New("cert store unavailable")
	certs.mu.Unlock()
	r, err := e.eng.Tick(e.ctx)
	if err != nil || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "cert store unavailable") {
		t.Fatalf("tick: %+v %v", r, err)
	}
	if a := e.alertByKey("cert_expiry:server"); a.State != notify.StateFiring {
		t.Fatalf("evaluation error resolved the alert: %+v", a)
	}
	certs.mu.Lock()
	certs.err = nil
	certs.items[0].NotAfter = e.clock().Add(300 * 24 * time.Hour) // renewed
	certs.mu.Unlock()
	rt.mu.Lock()
	rt.res[0].OK = true
	rt.mu.Unlock()
	e.tick()
	if a := e.alertByKey("cert_expiry:server"); a.State != notify.StateResolved {
		t.Fatalf("renewed cert still firing: %+v", a)
	}
	if a := e.alertByKey("restore_test_failed:custody/g1"); a.State != notify.StateResolved {
		t.Fatalf("restore test still firing: %+v", a)
	}
}

func TestRepositoryUnreachableFakeProbe(t *testing.T) {
	e := newEnv(t)
	// pretend the repository is an S3 one (the probe is faked here; the MinIO test uses the real one)
	e.exec(`UPDATE repositories SET backend='s3'`)
	var mu sync.Mutex
	fail := true
	calls := 0
	e.eng.Probe = func(ctx context.Context, name string) ([]datastore.ProbeResult, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return []datastore.ProbeResult{{Op: "list bucket", OK: false, Err: "dial tcp: connection refused"}}, nil
		}
		return []datastore.ProbeResult{{Op: "list bucket", OK: true}}, nil
	}
	e.tick() // failure 1: not yet (needs 2)
	if a := e.alertByKey("repository_unreachable:custody"); a != nil {
		t.Fatalf("alerted after one failure: %+v", a)
	}
	e.advance(5 * time.Minute)
	e.tick() // not due (every 10 min)
	if calls != 1 {
		t.Fatalf("probed %d times within the interval", calls)
	}
	e.advance(5 * time.Minute)
	e.tick() // failure 2
	a := e.alertByKey("repository_unreachable:custody")
	if a == nil || a.Severity != notify.SeverityCritical || !strings.Contains(a.Detail, "connection refused") {
		t.Fatalf("unreachable: %+v", a)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	e.advance(10 * time.Minute)
	e.tick()
	if a := e.alertByKey("repository_unreachable:custody"); a.State != notify.StateResolved {
		t.Fatalf("not resolved after a good probe: %+v", a)
	}
	// a probe that hangs is bounded by ProbeTimeout
	e.eng.ProbeTimeout = 100 * time.Millisecond
	block := make(chan struct{})
	defer close(block)
	e.eng.Probe = func(ctx context.Context, name string) ([]datastore.ProbeResult, error) { <-block; return nil, nil }
	e.advance(10 * time.Minute)
	e.tick()
	var msg string
	if err := e.d.Pool().QueryRow(e.ctx, `SELECT error FROM notify_probe_state`).Scan(&msg); err != nil || !strings.Contains(msg, "timed out") {
		t.Fatalf("hung probe: %q %v", msg, err)
	}
}

func TestEmitEvents(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "sec", func(r *notify.Rule) { r.EventTypes = []string{"login_*"} })
	if err := e.eng.Emit(e.ctx, notify.Event{Type: notify.TypeBackupMissed, Title: "x"}); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("condition type emitted: %v", err)
	}
	ev := notify.Event{Type: notify.TypeLoginSecurity, Key: "203.0.113.9", Title: "5 failed logins from 203.0.113.9", Detail: "user admin"}
	if err := e.eng.Emit(e.ctx, ev); err != nil {
		t.Fatal(err)
	}
	// Emit dispatched at once (no Tick running); Deliver sends it
	e.eng.Deliver(e.ctx)
	ms := rcv.messages(t)
	if len(ms) != 1 || ms[0].Subject != "[WARNING] 5 failed logins from 203.0.113.9" {
		t.Fatalf("event message: %+v", ms)
	}
	// the same key again: counted, not re-sent
	if err := e.eng.Emit(e.ctx, ev); err != nil {
		t.Fatal(err)
	}
	e.eng.Deliver(e.ctx)
	a := e.alertByKey("login_security:203.0.113.9")
	if a.Count != 2 || len(rcv.requests()) != 1 {
		t.Fatalf("repeat event: count %d, %d messages", a.Count, len(rcv.requests()))
	}
	// job_failed is not routed by this rule
	if err := e.eng.Emit(e.ctx, notify.Event{Type: notify.TypeJobFailed, Key: "gc", Title: "gc job failed", Severity: notify.SeverityCritical}); err != nil {
		t.Fatal(err)
	}
	e.eng.Deliver(e.ctx)
	if len(rcv.requests()) != 1 {
		t.Fatalf("rule filter ignored")
	}
	// events expire silently after event_expire_minutes (no "resolved" message)
	e.advance(25 * time.Hour)
	e.tick()
	if a := e.alertByKey("login_security:203.0.113.9"); a.State != notify.StateResolved || !a.ResolvedSilently {
		t.Fatalf("expiry: %+v", a)
	}
	if len(rcv.requests()) != 1 {
		t.Fatalf("expiry sent a message: %v", kinds(rcv.messages(t)))
	}
	// explicit resolve sends "resolved"
	ev.Key = "198.51.100.1"
	ev.Title = "brute force from 198.51.100.1"
	_ = e.eng.Emit(e.ctx, ev)
	ev.Resolve = true
	_ = e.eng.Emit(e.ctx, ev)
	e.eng.Deliver(e.ctx)
	if got := kinds(rcv.messages(t)); !reflect.DeepEqual(got, []string{"firing", "firing", "resolved"}) {
		t.Fatalf("explicit resolve: %v", got)
	}
}

func TestSilence(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "ops", nil)
	e.host("node1", ptr(e.clock().Add(-3*time.Hour)))
	e.host("node2", ptr(e.clock().Add(-3*time.Hour)))
	sil, err := e.eng.CreateSilence(e.ctx, notify.Silence{HostGlob: "node1", Until: e.clock().Add(2 * time.Hour), Reason: "maintenance window", Author: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.CreateSilence(e.ctx, notify.Silence{Until: e.clock().Add(time.Hour), Reason: "x", Author: "y"}); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("match-everything silence accepted: %v", err)
	}
	e.tick()
	a1, a2 := e.alertByKey("host_offline:custody/node1"), e.alertByKey("host_offline:custody/node2")
	if a1.State != notify.StateSilenced || a1.SilenceID == nil || *a1.SilenceID != sil.ID || a2.State != notify.StateFiring {
		t.Fatalf("silence: %+v / %+v", a1, a2)
	}
	ms := rcv.messages(t)
	if len(ms) != 1 || ms[0].Alert.Host != "node2" {
		t.Fatalf("silenced alert notified: %+v", ms)
	}
	ls, _ := e.eng.ListSilences(e.ctx, false)
	if len(ls) != 1 || !ls[0].Active {
		t.Fatalf("list: %+v", ls)
	}
	// silence expires while still firing: the alert is announced now
	e.advance(2*time.Hour + time.Minute)
	e.tick()
	if a1 = e.alertByKey("host_offline:custody/node1"); a1.State != notify.StateFiring {
		t.Fatalf("after expiry: %+v", a1)
	}
	if ms = rcv.messages(t); len(ms) != 2 || ms[1].Alert.Host != "node1" || ms[1].Kind != "firing" {
		t.Fatalf("after expiry: %v", kinds(ms))
	}
	// unsilence ends a silence now
	s2, err := e.eng.CreateSilence(e.ctx, notify.Silence{TypeGlob: "host_*", Until: e.clock().Add(24 * time.Hour), Reason: "r", Author: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if a := e.alertByKey("host_offline:custody/node2"); a.State != notify.StateSilenced {
		t.Fatalf("silence not applied at once: %+v", a)
	}
	if err := e.eng.Unsilence(e.ctx, s2.ID); err != nil {
		t.Fatal(err)
	}
	if a := e.alertByKey("host_offline:custody/node2"); a.State != notify.StateFiring {
		t.Fatalf("unsilence: %+v", a)
	}
}

func TestFlappingHeld(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "ops", nil)
	st, _ := e.eng.GetSettings(e.ctx)
	st.DedupeMinutes = 0
	if err := e.eng.SetSettings(e.ctx, *st); err != nil {
		t.Fatal(err)
	}
	e.host("node1", nil)
	offline := func(off bool) {
		if off {
			e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock().Add(-3*time.Hour))
		} else {
			e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
		}
		e.tick()
		e.advance(5 * time.Minute)
	}
	// fire, resolve, fire, resolve: announced normally (2 fires within the window < threshold 3)
	offline(true)
	offline(false)
	offline(true)
	offline(false)
	if got := kinds(rcv.messages(t)); !reflect.DeepEqual(got, []string{"firing", "resolved", "firing", "resolved"}) {
		t.Fatalf("before flapping: %v", got)
	}
	// third fire within an hour: flapping, held
	offline(true)
	a := e.alertByKey("host_offline:custody/node1")
	if !a.Flapping {
		t.Fatalf("not flapping: %+v", a)
	}
	offline(false)
	offline(true)
	offline(false)
	offline(true)
	if n := len(rcv.requests()); n != 4 {
		t.Fatalf("flapping alert notified: %v", kinds(rcv.messages(t)))
	}
	// stays firing for the whole window: released and announced once
	for i := 0; i < 13; i++ {
		offline(true)
	}
	a = e.alertByKey("host_offline:custody/node1")
	if a.Flapping || a.State != notify.StateFiring {
		t.Fatalf("not released: %+v", a)
	}
	if got := kinds(rcv.messages(t)); !reflect.DeepEqual(got, []string{"firing", "resolved", "firing", "resolved", "firing"}) {
		t.Fatalf("after release: %v", got)
	}
}

func TestQuietHours(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "ops", func(r *notify.Rule) {
		r.QuietStart, r.QuietEnd, r.Timezone = "22:00", "07:00", "Atlantic/Reykjavik"
	})
	e.set(time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC)) // Reykjavik = UTC
	e.host("node1", ptr(e.clock().Add(-3*time.Hour)))    // warning
	e.host("node2", ptr(e.clock().Add(-25*time.Hour)))   // critical: bypasses quiet hours
	e.tick()
	ms := rcv.messages(t)
	if len(ms) != 1 || ms[0].Alert.Host != "node2" || ms[0].Severity != notify.SeverityCritical {
		t.Fatalf("during quiet hours: %+v", ms)
	}
	e.set(time.Date(2026, 10, 8, 6, 59, 0, 0, time.UTC))
	e.tick()
	if len(rcv.requests()) != 1 {
		t.Fatalf("sent before the end of quiet hours")
	}
	e.set(time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC))
	e.tick()
	ms = rcv.messages(t)
	if len(ms) != 2 || ms[1].Alert.Host != "node1" {
		t.Fatalf("held message not sent after quiet hours: %+v", kinds(ms))
	}
	// an alert that fires and clears during quiet hours sends nothing at all
	e.set(time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC))
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.host("node3", ptr(e.clock().Add(-3*time.Hour)))
	e.tick() // node1/node2 resolve (resolved is held: warning node1; critical node2 bypasses), node3 fires (held)
	e.set(time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC))
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	e.set(time.Date(2026, 10, 9, 7, 30, 0, 0, time.UTC))
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	for _, m := range rcv.messages(t) {
		if m.Alert.Host == "node3" {
			t.Fatalf("node3 fired and cleared during quiet hours but was announced: %s", m.Subject)
		}
	}
	got := kinds(rcv.messages(t))
	if !reflect.DeepEqual(got, []string{"firing", "firing", "resolved", "resolved"}) {
		t.Fatalf("messages: %v", got)
	}
}

func TestDigest(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.set(time.Date(2026, 10, 7, 7, 0, 0, 0, time.UTC))
	_, rule := e.webhookChannel(rcv, "daily", func(r *notify.Rule) { r.Kind = notify.RuleDigest; r.DigestAt = "08:00" })
	_ = rule
	e.policy("daily", "03:00")
	hid := e.host("node1", ptr(e.clock()))
	g1 := e.object(hid, "node1", "default", "a", "daily", e.clock().Add(-10*24*time.Hour))
	e.object(hid, "node1", "default", "b", "daily", e.clock().Add(-10*24*time.Hour)) // never backed up: overdue
	e.snapshot(g1, e.clock().Add(-2*time.Hour))
	e.exec(`INSERT INTO backup_runs (repository_id, host_id, started_at, finished_at, status) VALUES ($1,$2,$3,$3,'ok'), ($1,$2,$4,$4,'failed')`,
		e.repo.ID, hid, e.clock().Add(-3*time.Hour), e.clock().Add(-2*time.Hour))
	e.exec(`INSERT INTO snapshot_verifications (repository_id, group_id, snapshot_id, verified_at, state) VALUES ($1,'g','s',$2,'ok')`, e.repo.ID, e.clock().Add(-time.Hour))
	e.exec(`INSERT INTO capacity_samples (repository_id, taken_at, physical_bytes, chunk_count, snapshot_count) VALUES ($1,$2,1000,1,1), ($1,$3,3048,1,1)`,
		e.repo.ID, e.clock().Add(-25*time.Hour), e.clock().Add(-time.Hour))
	e.tick() // 07:00: not due
	if n := len(rcv.requests()); n != 0 {
		t.Fatalf("digest before its time")
	}
	e.set(time.Date(2026, 10, 7, 8, 0, 30, 0, time.UTC))
	e.tick()
	reqs := rcv.requests()
	if len(reqs) != 1 {
		t.Fatalf("digest: %d messages", len(reqs))
	}
	var m struct {
		Kind    string              `json:"kind"`
		Subject string              `json:"subject"`
		Text    string              `json:"text"`
		Digest  notify.DigestReport `json:"digest"`
	}
	if err := json.Unmarshal(reqs[0].Body, &m); err != nil {
		t.Fatal(err)
	}
	d := m.Digest
	if m.Kind != "digest" || d.BackupsOK != 1 || d.BackupsFailed != 1 || d.Objects != 2 || d.ObjectsProtected != 1 || d.ProtectedPercent != 50 ||
		d.VerifiedOK != 1 || len(d.Repositories) != 1 || d.Repositories[0].Growth24h != 2048 {
		t.Fatalf("digest content: %+v", d)
	}
	if !strings.Contains(m.Text, "Protected:  50.0% (1 of 2 scheduled objects backed up on time)") || !strings.Contains(m.Subject, "1 ok, 1 failed, 50% protected") {
		t.Fatalf("digest text: %s / %s", m.Subject, m.Text)
	}
	e.advance(time.Hour)
	e.tick()
	e.set(time.Date(2026, 10, 8, 7, 59, 0, 0, time.UTC))
	e.tick()
	if n := len(rcv.requests()); n != 1 {
		t.Fatalf("digest sent twice in one day: %d", n)
	}
	e.set(time.Date(2026, 10, 8, 8, 1, 0, 0, time.UTC))
	e.tick()
	if n := len(rcv.requests()); n != 2 {
		t.Fatalf("second day's digest: %d", n)
	}
	// Digest is also callable directly (the UI's summary)
	if _, err := e.eng.Digest(e.ctx, e.clock()); err != nil {
		t.Fatal(err)
	}
}

func TestRetryBackoffDeadLetter(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	rcv.setStatus("upstream broken", 500)
	ch, _ := e.webhookChannel(rcv, "ops", nil)
	st, _ := e.eng.GetSettings(e.ctx)
	st.MaxAttempts, st.BackoffBaseSeconds, st.BackoffMaxSeconds = 4, 60, 150
	if err := e.eng.SetSettings(e.ctx, *st); err != nil {
		t.Fatal(err)
	}
	e.host("node1", ptr(e.clock().Add(-3*time.Hour)))
	r := e.tick()
	if r.Enqueued != 1 || r.Delivered != 0 || r.Failed != 1 {
		t.Fatalf("tick: %+v", r)
	}
	d := e.deliveries(notify.DeliveryFilter{ChannelID: ch.ID})[0]
	if d.State != "pending" || d.Attempts != 1 || d.LastError != "HTTP 500 Internal Server Error: upstream broken" || !d.NextAttemptAt.Equal(e.clock().Add(60*time.Second)) {
		t.Fatalf("after attempt 1: %+v", d)
	}
	e.advance(59 * time.Second)
	if s, f := e.eng.Deliver(e.ctx); s+f != 0 {
		t.Fatalf("retried before the backoff")
	}
	e.advance(time.Second)
	e.eng.Deliver(e.ctx) // attempt 2 -> next in 120 s
	d = e.deliveries(notify.DeliveryFilter{ChannelID: ch.ID})[0]
	if d.Attempts != 2 || !d.NextAttemptAt.Equal(e.clock().Add(120*time.Second)) {
		t.Fatalf("after attempt 2: %+v", d)
	}
	e.advance(120 * time.Second)
	e.eng.Deliver(e.ctx) // attempt 3 -> capped at 150 s
	d = e.deliveries(notify.DeliveryFilter{ChannelID: ch.ID})[0]
	if d.Attempts != 3 || !d.NextAttemptAt.Equal(e.clock().Add(150*time.Second)) {
		t.Fatalf("after attempt 3 (cap): %+v", d)
	}
	e.advance(150 * time.Second)
	e.eng.Deliver(e.ctx) // attempt 4 = max: dead letter
	d = e.deliveries(notify.DeliveryFilter{ChannelID: ch.ID})[0]
	if d.State != "dead" || d.Attempts != 4 {
		t.Fatalf("dead letter: %+v", d)
	}
	e.advance(time.Hour)
	e.eng.Deliver(e.ctx)
	if n := len(rcv.requests()); n != 4 {
		t.Fatalf("dead letter retried: %d requests", n)
	}
	if dl := e.deliveries(notify.DeliveryFilter{State: "dead"}); len(dl) != 1 {
		t.Fatalf("dead-letter listing: %+v", dl)
	}
	// operator fixes the receiver and retries the dead letter
	rcv.setStatus("", 200)
	if err := e.eng.RetryDelivery(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.eng.Deliver(e.ctx); s != 1 {
		t.Fatal("retry not sent")
	}
	if d = e.deliveries(notify.DeliveryFilter{ChannelID: ch.ID})[0]; d.State != "sent" || d.LastError != "" {
		t.Fatalf("after retry: %+v", d)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	ch, _ := e.webhookChannel(rcv, "ops", nil)
	if _, err := e.eng.UpdateChannel(e.ctx, ch.ID, notify.ChannelInput{RatePerHour: ptr(2)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e.host(fmt.Sprintf("node%d", i), ptr(e.clock().Add(-3*time.Hour)))
	}
	r := e.tick()
	if r.Enqueued != 5 || r.Delivered != 2 {
		t.Fatalf("tick: %+v", r)
	}
	pend := e.deliveries(notify.DeliveryFilter{State: "pending"})
	if len(pend) != 3 || !strings.Contains(pend[0].LastError, "rate limit") || pend[0].Attempts != 0 {
		t.Fatalf("held by the rate limit: %+v", pend)
	}
	e.advance(30 * time.Minute)
	e.eng.Deliver(e.ctx)
	if n := len(rcv.requests()); n != 2 {
		t.Fatalf("rate limit not enforced: %d", n)
	}
	e.advance(31 * time.Minute)
	e.eng.Deliver(e.ctx)
	if n := len(rcv.requests()); n != 4 {
		t.Fatalf("after the hour: %d", n)
	}
	e.advance(61 * time.Minute)
	e.eng.Deliver(e.ctx)
	if n := len(rcv.requests()); n != 5 {
		t.Fatalf("all delivered eventually: %d", n)
	}
}

// Two Ticks at the same time (here: two engines over two services, like two processes) must not
// both evaluate; no notification is ever queued or sent twice.
func TestConcurrentTicks(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "ops", nil)
	for i := 0; i < 20; i++ {
		e.host(fmt.Sprintf("node%02d", i), ptr(e.clock().Add(-3*time.Hour)))
	}
	other := newEnvSameDB(t, e)
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		results := make([]*notify.TickResult, 4)
		engines := []*notify.Engine{e.eng, other, e.eng, other}
		for i := range engines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := engines[i].Tick(e.ctx)
				if err != nil {
					t.Error(err)
					return
				}
				results[i] = r
			}()
		}
		wg.Wait()
		e.advance(time.Minute)
	}
	if t.Failed() {
		return
	}
	reqs := rcv.requests()
	if len(reqs) != 20 {
		t.Fatalf("expected exactly 20 notifications, got %d", len(reqs))
	}
	seen := map[string]bool{}
	for _, m := range rcv.messages(t) {
		if seen[m.Alert.Key] {
			t.Fatalf("duplicate notification for %s", m.Alert.Key)
		}
		seen[m.Alert.Key] = true
	}
	var n int
	_ = e.d.Pool().QueryRow(e.ctx, `SELECT count(*) FROM notify_deliveries`).Scan(&n)
	if n != 20 {
		t.Fatalf("%d deliveries queued", n)
	}
}

// newEnvSameDB is a second service (second "process") on the same database, state directory and clock.
func newEnvSameDB(t *testing.T, e *env, params ...string) *notify.Engine {
	t.Helper()
	u := e.url
	for _, p := range params {
		if strings.Contains(u, "?") {
			u += "&" + p
		} else {
			u += "?" + p
		}
	}
	d, err := db.Open(e.ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	s := service.New(d, e.svc.StateDir, "test", e.svc.Log)
	s.Now = e.clock
	return notify.New(s)
}

func TestConcurrentDeliverAndEmit(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "sec", nil)
	if _, err := e.eng.UpdateChannel(e.ctx, "sec", notify.ChannelInput{RatePerHour: ptr(0)}); err != nil {
		t.Fatal(err)
	}
	// two "processes" with tiny connection pools: a transaction that needed a second pool
	// connection while holding row locks would hang here
	small1 := newEnvSameDB(t, e, "pool_max_conns=2")
	small2 := newEnvSameDB(t, e, "pool_max_conns=3")
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eng := small1
			if i%2 == 1 {
				eng = small2
			}
			if err := eng.Emit(e.ctx, notify.Event{Type: notify.TypeLoginSecurity, Key: fmt.Sprintf("10.0.0.%d", i), Title: fmt.Sprintf("failed logins from 10.0.0.%d", i)}); err != nil {
				t.Error(err)
			}
			if i%5 == 0 {
				if _, err := eng.Tick(e.ctx); err != nil {
					t.Error(err)
				}
			}
			eng.Deliver(e.ctx)
		}()
	}
	wg.Wait()
	e.tick()
	e.eng.Deliver(e.ctx)
	if n := len(rcv.requests()); n != 40 {
		t.Fatalf("expected 40 messages, got %d", n)
	}
	seen := map[string]bool{}
	for _, m := range rcv.messages(t) {
		if seen[m.Alert.Key] {
			t.Fatalf("duplicate: %s", m.Alert.Key)
		}
		seen[m.Alert.Key] = true
	}
}

func TestHubNonBlocking(t *testing.T) {
	e := newEnv(t)
	sub := e.eng.Subscribe()
	slow := e.eng.Subscribe() // never read
	e.host("node1", ptr(e.clock().Add(-3*time.Hour)))
	e.tick()
	select {
	case c := <-sub:
		if c.Kind != "alert" || c.Transition != "fired" || c.Key != "host_offline:custody/node1" {
			t.Fatalf("change: %+v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("no change published")
	}
	// flood: publishing never blocks on the slow subscriber
	done := make(chan struct{})
	go func() {
		for i := 0; i < 300; i++ {
			_ = e.eng.Emit(e.ctx, notify.Event{Type: notify.TypeJobFailed, Key: fmt.Sprint(i), Title: "job failed"})
		}
		close(done)
	}()
	go func() {
		for range sub {
		}
	}()
	<-done // a blocking publish would hang here (the test binary's timeout reports it)
	if e.eng.DroppedChanges() == 0 {
		t.Fatal("expected drops for the slow subscriber")
	}
	e.eng.Unsubscribe(slow)
	e.eng.Unsubscribe(sub)
}

// The real probe against real S3 (MinIO): reachable -> no alert; endpoint broken -> alert after
// two failed probes ten minutes apart; fixed -> resolved.
func TestRepositoryUnreachableMinIO(t *testing.T) {
	e := newEnv(t)
	s3c, bucket := s3test.NewBucket(t)
	repo, err := e.svc.CreateS3Repository(e.ctx, "offsite", datastore.S3Config{Endpoint: s3c.Endpoint, Bucket: bucket, Prefix: "ib",
		AccessKey: s3c.AccessKey, SecretKey: s3c.SecretKey, NoTLS: s3c.NoTLS, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.ProbeTimeout = 30 * time.Second
	key := "repository_unreachable:offsite"
	e.tick()
	var ok bool
	if err := e.d.Pool().QueryRow(e.ctx, `SELECT ok FROM notify_probe_state WHERE repository_id=$1`, repo.ID).Scan(&ok); err != nil || !ok {
		t.Fatalf("real probe of a healthy MinIO: ok=%v %v", ok, err)
	}
	var good json.RawMessage
	_ = e.d.Pool().QueryRow(e.ctx, `SELECT s3_settings FROM repositories WHERE id=$1`, repo.ID).Scan(&good)
	e.exec(`UPDATE repositories SET s3_settings=jsonb_set(s3_settings, '{endpoint}', '"127.0.0.1:1"') WHERE id=$1`, repo.ID)
	for i := 0; i < 2; i++ {
		e.advance(10 * time.Minute)
		e.tick()
	}
	a := e.alertByKey(key)
	if a == nil || a.State != notify.StateFiring {
		t.Fatalf("no alert for a dead endpoint: %+v", a)
	}
	t.Logf("detail: %s", a.Detail)
	e.exec(`UPDATE repositories SET s3_settings=$2 WHERE id=$1`, repo.ID, good)
	e.advance(10 * time.Minute)
	e.tick()
	if a = e.alertByKey(key); a.State != notify.StateResolved {
		t.Fatalf("not resolved after the endpoint came back: %+v", a)
	}
}

// A repository in maintenance (mode read-only/offline) raises no alerts; its open alerts are kept
// as they are (not "resolved") and continue once it is back online.
func TestPausedRepositoryNoAlerts(t *testing.T) {
	e := newEnv(t)
	// the repository-mode migration lives in another patch; same definition
	e.exec(`ALTER TABLE repositories ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'online' CHECK (mode IN ('online','read-only','offline'))`)
	rcv := newReceiver(t)
	e.webhookChannel(rcv, "ops", nil)
	hid := e.host("node1", ptr(e.clock().Add(-3*time.Hour)))
	e.exec(`UPDATE repositories SET mode='offline'`)
	e.tick()
	if as := e.alerts(notify.AlertFilter{Active: true}); len(as) != 0 {
		t.Fatalf("alerts for an offline repository: %+v", as)
	}
	e.exec(`UPDATE repositories SET mode='online'`)
	e.tick()
	if a := e.alertByKey("host_offline:custody/node1"); a == nil || a.State != notify.StateFiring {
		t.Fatalf("online again: %+v", a)
	}
	e.exec(`UPDATE repositories SET mode='read-only'`)
	e.exec(`INSERT INTO backup_runs (repository_id, host_id, started_at, finished_at, status) VALUES ($1,$2,$3,$3,'failed')`, e.repo.ID, hid, e.clock())
	e.tick()
	if a := e.alertByKey("host_offline:custody/node1"); a.State != notify.StateFiring {
		t.Fatalf("maintenance resolved an open alert: %+v", a)
	}
	if a := e.alertByKey("backup_failed:custody/node1"); a != nil {
		t.Fatalf("alert during maintenance: %+v", a)
	}
	if got := kinds(rcv.messages(t)); !reflect.DeepEqual(got, []string{"firing"}) {
		t.Fatalf("messages: %v", got)
	}
}

func TestDBRestoreTestSource(t *testing.T) {
	e := newEnv(t)
	src := notify.DBRestoreTests{Pool: e.d.Pool()}
	if rs, err := src.LatestRestoreTests(e.ctx); err != nil || len(rs) != 0 {
		t.Fatalf("without the table: %v %v", rs, err)
	}
	// the jobs migration's table when it is applied, else the columns it defines (subset)
	e.exec(`CREATE TABLE IF NOT EXISTS restore_tests (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), seq bigserial NOT NULL,
		repository_id uuid NOT NULL REFERENCES repositories(id), host_name text NOT NULL, object text NOT NULL, method text NOT NULL,
		state text NOT NULL, started timestamptz NOT NULL, finished timestamptz, details jsonb NOT NULL DEFAULT '{}')`)
	ins := func(obj, state string, at time.Time, details string) {
		e.exec(`INSERT INTO restore_tests (repository_id, host_name, object, method, state, started, finished, details) VALUES ($1,'node1',$2,'deep',$3,$4,$4,$5)`,
			e.repo.ID, obj, state, at, details)
	}
	ins("default/instance/vm1/root", "passed", e.clock().Add(-2*time.Hour), `{}`)
	ins("default/instance/vm1/root", "failed", e.clock().Add(-time.Hour), `{"error":"boot check failed"}`)
	ins("default/instance/vm2/root", "failed", e.clock().Add(-2*time.Hour), `{"error":"x"}`)
	ins("default/instance/vm2/root", "passed", e.clock().Add(-time.Hour), `{}`)
	ins("default/instance/vm3/root", "running", e.clock(), `{}`)
	e.eng.RestoreTests = src
	e.tick()
	as := e.alerts(notify.AlertFilter{Active: true, Type: notify.TypeRestoreTestFailed})
	if len(as) != 1 || as[0].Object != "default/vm1" || !strings.Contains(as[0].Detail, "boot check failed") {
		t.Fatalf("restore test alerts: %+v", as)
	}
}
