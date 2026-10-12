package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/service"
)

func both(t *testing.T, fn func(t *testing.T, e *env)) {
	t.Run("filesystem", func(t *testing.T) { fn(t, newEnv(t)) })
	t.Run("s3", func(t *testing.T) { fn(t, newEnvS3(t)) })
}

func begin(e *env, h *service.Host, k *format.Keys, grp, snap, prev string) (*api.SessionResponse, error) {
	return e.svc.BeginSession(context.Background(), h, k.DomainID, api.SessionRequest{GroupID: grp, SnapshotID: snap, Created: e.now, Kind: "block", Project: "p", Object: "vm/root", PreviousID: prev})
}

// The database itself admits one writer per group: concurrent sessions for the same object
// conflict, other objects are unaffected, and the slot frees when the session ends.
func TestExactlyOneActiveWriterPerGroup(t *testing.T) {
	both(t, func(t *testing.T, e *env) {
		h := e.enrol("benfica", api.ScopeBackup)
		k := keys(1)
		const other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		var wg sync.WaitGroup
		results := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := begin(e, h, k, group, snapID(e.now, i), "")
				results <- err
			}(i)
		}
		wg.Wait()
		close(results)
		ok, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, service.ErrConflict) && strings.Contains(err.Error(), "already running"):
				conflicts++
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		if ok != 1 || conflicts != 7 {
			t.Fatalf("writers=%d conflicts=%d", ok, conflicts)
		}
		if _, err := begin(e, h, k, other, snapID(e.now, 50), ""); err != nil {
			t.Fatalf("a different object must not be blocked: %v", err)
		}
		var open int
		_ = e.svc.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE state='open' AND group_id=$1`, group).Scan(&open)
		if open != 1 {
			t.Fatalf("%d open rows for the group", open)
		}
	})
}

func TestWriterSlotFreesOnAbortCommitAndExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	r, err := begin(e, h, k, group, snapID(e.now, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := begin(e, h, k, group, snapID(e.now, 2), ""); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("second writer: %v", err)
	}
	if err := e.svc.SessionAbort(ctx, h, r.Session); err != nil {
		t.Fatal(err)
	}
	r, err = begin(e, h, k, group, snapID(e.now, 3), "")
	if err != nil {
		t.Fatalf("after abort: %v", err)
	}
	// a dead client: its lease runs out and the next backup reclaims the slot
	e.now = e.now.Add(service.DefaultLease + time.Minute)
	if _, err := begin(e, h, k, group, snapID(e.now, 4), ""); err != nil {
		t.Fatalf("an expired session must not block the object: %v", err)
	}
	if _, err := e.svc.SessionHeartbeat(ctx, h, r.Session); !errors.Is(err, service.ErrNotFound) && !errors.Is(err, service.ErrConflict) {
		t.Fatalf("the dead session is still usable: %v", err)
	}
}

// A long backup never expires while it is active; one that goes silent does.
func TestLeaseRenewsOnActivityAndHeartbeat(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	r, err := begin(e, h, k, group, snapID(e.now, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Lease != service.DefaultLease {
		t.Fatalf("lease %v", r.Lease)
	}
	// 10 hours of work, a heartbeat every 10 minutes: far beyond the old fixed 6 h limit
	for i := 0; i < 60; i++ {
		e.now = e.now.Add(10 * time.Minute)
		if _, err := e.svc.SessionHeartbeat(ctx, h, r.Session); err != nil {
			t.Fatalf("heartbeat %d (%s into the session): %v", i, time.Duration(i+1)*10*time.Minute, err)
		}
	}
	// the same long session can still do real work and commit
	id, blob, _ := k.EncodeChunk([]byte("late"), format.EncodeOpts{})
	if err := e.svc.SessionPutChunk(ctx, h, r.Session, id.String(), blob); err != nil {
		t.Fatal(err)
	}
	// silence for more than a lease: gone
	e.now = e.now.Add(service.DefaultLease + time.Second)
	if _, err := e.svc.SessionHeartbeat(ctx, h, r.Session); err == nil {
		t.Fatal("a silent session survived its lease")
	}
}

// A long-running session pins its chunks: GC keeps them, however old, until it ends.
func TestGCNeverSweepsChunksOfAnActiveLongSession(t *testing.T) {
	both(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		h := e.enrol("benfica", api.ScopeBackup)
		k := keys(1)
		r, err := begin(e, h, k, group, snapID(e.now, 1), "")
		if err != nil {
			t.Fatal(err)
		}
		id, blob, _ := k.EncodeChunk([]byte("uploaded-hours-ago"), format.EncodeOpts{})
		if err := e.svc.SessionPutChunk(ctx, h, r.Session, id.String(), blob); err != nil {
			t.Fatal(err)
		}
		// grace of 12h is the minimum: nothing is old enough to be swept right now, so make the store believe it is
		st, _ := e.svc.Store(e.repo)
		st.SetClockForTest(func() time.Time { return time.Now().Add(72 * time.Hour) })
		gs, err := e.svc.GC(ctx, "custody", service.DefaultGCGrace, false)
		if err != nil || gs.Removed != 0 || gs.KeptPinned != 1 {
			t.Fatalf("GC swept an active session's chunk: %v %+v", err, gs)
		}
		_ = e.svc.SessionAbort(ctx, h, r.Session)
		gs, err = e.svc.GC(ctx, "custody", service.DefaultGCGrace, false)
		if err != nil || gs.Removed != 1 {
			t.Fatalf("after the session ended the orphan must go: %v %+v", err, gs)
		}
	})
}

// Prune/forget cannot remove the snapshot a running backup builds on (the gc pin).
func TestBaseSnapshotIsPinnedWhileASessionBuildsOnIt(t *testing.T) {
	both(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		h := e.enrol("benfica", api.ScopeBackup)
		k := keys(1)
		_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "tiny", KeepLast: 1})
		var snaps []string
		for i := 0; i < 3; i++ {
			ts := e.now.Add(time.Duration(i) * time.Hour)
			sn := snapID(ts, i)
			snaps = append(snaps, sn)
			if err := e.upload(h, k, group, sn, ts, "tiny", fmt.Sprintf("v%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		// the client builds the next snapshot on the OLDEST one (lagging target): pinned
		e.now = e.now.Add(5 * time.Hour)
		r, err := begin(e, h, k, group, snapID(e.now, 9), snaps[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.ForgetSnapshot(ctx, "custody", group, snaps[0]); !errors.Is(err, service.ErrConflict) {
			t.Fatalf("forgot a pinned base: %v", err)
		}
		pre, _ := e.svc.PrunePreview(ctx, "custody")
		for _, p := range pre.Plan {
			if p.Snapshot == snaps[0] && (!p.Keep || p.Reason != "pinned by a running backup") {
				t.Fatalf("pinned base not protected in the plan: %+v", p)
			}
		}
		res, err := e.svc.Prune(ctx, "custody", false)
		if err != nil || res.Removed != 1 { // only the middle one goes
			t.Fatalf("%v %+v", err, res)
		}
		st, _ := e.svc.Store(e.repo)
		l, _ := st.ListSnapshots(group)
		if len(l) != 2 || l[0].Snapshot != snaps[0] {
			t.Fatalf("pinned base lost: %+v", l)
		}
		// once the session ends the pin is gone
		_ = e.svc.SessionAbort(ctx, h, r.Session)
		res, _ = e.svc.Prune(ctx, "custody", false)
		if res.Removed != 1 {
			t.Fatalf("after the session: %+v", res)
		}
	})
}

// Protection survives pruning and a PostgreSQL rebuild; notes and verify state come back too.
func TestProtectedSnapshotsNotesAndVerifyStateSurviveADatabaseRebuild(t *testing.T) {
	both(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		h := e.enrol("benfica", api.ScopeBackup)
		k := keys(1)
		_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "tiny", KeepLast: 1})
		var snaps []string
		for i := 0; i < 4; i++ {
			ts := e.now.Add(time.Duration(i) * time.Hour)
			sn := snapID(ts, i)
			snaps = append(snaps, sn)
			if err := e.upload(h, k, group, sn, ts, "tiny", fmt.Sprintf("v%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.svc.SetSnapshotProtected(ctx, "custody", group, snaps[1], true); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.SetSnapshotNotes(ctx, "custody", group, snaps[1], "before the Bosch upgrade"); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.SetGroupNotes(ctx, "custody", group, "production database VM"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Verify(ctx, "custody"); err != nil {
			t.Fatal(err)
		}
		res, err := e.svc.Prune(ctx, "custody", false)
		if err != nil || res.Removed != 2 || res.Protected != 1 { // keeps newest + the protected one
			t.Fatalf("%v %+v", err, res)
		}
		if err := e.svc.ForgetSnapshot(ctx, "custody", group, snaps[1]); !errors.Is(err, service.ErrConflict) {
			t.Fatalf("forgot a protected snapshot: %v", err)
		}
		// lose the database rows, rebuild from the datastore
		if _, err := e.svc.DB.Pool().Exec(ctx, `DELETE FROM snapshots`); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Reconcile(ctx, "custody"); err != nil {
			t.Fatal(err)
		}
		var prot bool
		var notes, vstate string
		if err := e.svc.DB.Pool().QueryRow(ctx, `SELECT protected, notes, verify_state FROM snapshots WHERE snapshot_id=$1`, snaps[1]).Scan(&prot, &notes, &vstate); err != nil {
			t.Fatal(err)
		}
		if !prot || notes != "before the Bosch upgrade" || vstate != "ok" {
			t.Fatalf("metadata lost in the rebuild: %v %q %q", prot, notes, vstate)
		}
		var gnotes string
		_ = e.svc.DB.Pool().QueryRow(ctx, `SELECT notes FROM storage_objects WHERE group_id=$1`, group).Scan(&gnotes)
		if gnotes != "production database VM" {
			t.Fatalf("group notes lost: %q", gnotes)
		}
		hist, err := e.svc.VerificationHistory(ctx, "custody", group, snaps[1])
		if err != nil || len(hist) != 1 || hist[0].State != "ok" {
			t.Fatalf("history %v %+v", err, hist)
		}
		// unprotect, then retention may take it
		_ = e.svc.SetSnapshotProtected(ctx, "custody", group, snaps[1], false)
		res, _ = e.svc.Prune(ctx, "custody", false)
		if res.Removed != 1 {
			t.Fatalf("%+v", res)
		}
	})
}

// Cumulative PBS retention in a policy time zone, and the preview equals what is applied.
func TestRetentionIsCumulativeInThePolicyTimeZoneAndPreviewMatchesApply(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	if err := e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "p", KeepLast: 3, KeepDaily: 7, Timezone: "Europe/Berlin"}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "bad", KeepLast: 1, Timezone: "Mars/Olympus"}); err == nil {
		t.Fatal("an unknown time zone was accepted")
	}
	for i := 0; i < 30; i++ {
		ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		if err := e.upload(h, k, group, snapID(ts, i), ts, "p", fmt.Sprintf("d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	pre, err := e.svc.PrunePreview(ctx, "custody")
	if err != nil || pre.Kept != 10 || pre.Removed != 20 || !pre.DryRun {
		t.Fatalf("keep-last 3 + keep-daily 7 with one backup a day keeps 10 (PBS): %v %+v", err, pre)
	}
	for _, p := range pre.Plan {
		if p.Timezone != "Europe/Berlin" || p.Policy != "p" {
			t.Fatalf("plan lacks policy/zone: %+v", p)
		}
	}
	st, _ := e.svc.Store(e.repo)
	if l, _ := st.ListSnapshots(group); len(l) != 30 {
		t.Fatal("the preview removed something")
	}
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Kept != pre.Kept || res.Removed != pre.Removed {
		t.Fatalf("apply differs from preview: %v %+v vs %+v", err, res, pre)
	}
	removed := map[string]bool{}
	for _, p := range pre.Plan {
		if !p.Keep {
			removed[p.Snapshot] = true
		}
	}
	l, _ := st.ListSnapshots(group)
	for _, in := range l {
		if removed[in.Snapshot] {
			t.Fatalf("%s was previewed as removed but survived or vice versa", in.Snapshot)
		}
	}
	if len(l) != 10 {
		t.Fatalf("%d left", len(l))
	}
}

// Corruption: verify quarantines, records affected snapshots, marks them failed; a new backup of the
// same data heals it, the record flips to healed and a re-verify clears the state.
func TestCorruptionIsQuarantinedRecordedAndHealedByTheNextBackup(t *testing.T) {
	both(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		h := e.enrol("benfica", api.ScopeBackup)
		k := keys(1)
		s1, s2 := snapID(e.now, 1), snapID(e.now.Add(time.Hour), 2)
		if err := e.upload(h, k, group, s1, e.now, "p", "stable", "will-rot"); err != nil {
			t.Fatal(err)
		}
		if err := e.upload(h, k, group, s2, e.now.Add(time.Hour), "p", "stable", "will-rot", "new"); err != nil {
			t.Fatal(err)
		}
		st, _ := e.svc.Store(e.repo)
		id := k.ChunkID([]byte("will-rot"))
		key := "chunks/" + id.String()[:2] + "/" + id.String()[2:4] + "/" + id.String()
		blob, _ := st.Backend().Get(key)
		blob[20] ^= 0xff
		if err := st.Backend().Put(key, blob, false); err != nil {
			t.Fatal(err)
		}
		vs, err := e.svc.Verify(ctx, "custody")
		if err == nil || vs.Quarantined != 1 {
			t.Fatalf("verify must fail loudly and quarantine: %v %+v", err, vs)
		}
		cc, _ := e.svc.CorruptChunks(ctx, "custody", false)
		if len(cc) != 1 || cc[0].ChunkID != id.String() || len(cc[0].Affected) != 2 || cc[0].State != "quarantined" {
			t.Fatalf("corruption record: %+v", cc)
		}
		l, _ := e.svc.AdminSnapshots(ctx, "custody", nil)
		for _, sn := range l {
			if sn.VerifyState != "failed" {
				t.Fatalf("snapshot %s should be failed: %+v", sn.SnapshotID, sn)
			}
		}
		// the corrupt chunk no longer counts as present, so a client that reads it again re-uploads it
		sid3 := snapID(e.now.Add(2*time.Hour), 3)
		if err := e.upload(h, k, group, sid3, e.now.Add(2*time.Hour), "p", "stable", "will-rot"); err != nil {
			t.Fatalf("healing backup: %v", err)
		}
		cc, _ = e.svc.CorruptChunks(ctx, "custody", true)
		if len(cc) != 1 || cc[0].State != "healed" || cc[0].HealedAt == nil {
			t.Fatalf("not healed: %+v", cc)
		}
		vs, err = e.svc.Verify(ctx, "custody")
		if err != nil || len(vs.Problems) != 0 {
			t.Fatalf("after healing: %v %+v", err, vs)
		}
		l, _ = e.svc.AdminSnapshots(ctx, "custody", nil)
		for _, sn := range l {
			if sn.VerifyState != "ok" {
				t.Fatalf("snapshot %s should be ok again: %+v", sn.SnapshotID, sn)
			}
		}
	})
}

func TestVerifyNewAndStaleModes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	for i := 0; i < 3; i++ {
		ts := e.now.Add(time.Duration(i) * time.Hour)
		if err := e.upload(h, k, group, snapID(ts, i), ts, "p", "shared", fmt.Sprintf("own%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := e.svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "new"})
	if err != nil || vs.Snapshots != 3 || vs.Chunks != 4 {
		t.Fatalf("first pass reads each of the 4 distinct chunks once: %v %+v", err, vs)
	}
	vs, _ = e.svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "new"})
	if vs.Snapshots != 0 || vs.Chunks != 0 {
		t.Fatalf("verify-new re-read verified data: %+v", vs)
	}
	vs, _ = e.svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "stale", StaleDays: 30})
	if vs.Snapshots != 0 {
		t.Fatalf("nothing is stale yet: %+v", vs)
	}
	e.now = e.now.Add(31 * 24 * time.Hour)
	vs, _ = e.svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "stale", StaleDays: 30})
	if vs.Snapshots != 3 {
		t.Fatalf("all three are stale now: %+v", vs)
	}
	if _, err := e.svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "bogus"}); err == nil {
		t.Fatal("bogus mode accepted")
	}
}

func TestEscrowBlobIsCreateOnlyBoundToTheKeyDomainAndClearedOnlyByTheAdmin(t *testing.T) {
	both(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		h := e.enrol("benfica", api.ScopeBackup)
		master := make([]byte, 32)
		master[0] = 1
		k, _ := format.DeriveKeys(master)
		// the key domain is pinned by the first backup
		if err := e.upload(h, keys(1), group, snapID(e.now, 1), e.now, "p", "x"); err != nil {
			t.Fatal(err)
		}
		kd := keys(1)
		_, pub, _ := format.GenerateEscrowKeyPair()
		blob, err := format.WrapMasterKey(func() []byte {
			m := make([]byte, 32)
			for i := range m {
				m[i] = 1 + byte(i)
			}
			return m
		}(), pub)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.PutEscrow(ctx, h, k.DomainID, blob); !errors.Is(err, service.ErrForbidden) {
			t.Fatalf("a blob for a foreign key domain was accepted: %v", err)
		}
		if err := e.svc.PutEscrow(ctx, h, kd.DomainID, []byte("garbage")); !errors.Is(err, service.ErrBadRequest) {
			t.Fatalf("garbage accepted: %v", err)
		}
		if err := e.svc.PutEscrow(ctx, h, kd.DomainID, blob); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.PutEscrow(ctx, h, kd.DomainID, blob); !errors.Is(err, service.ErrConflict) {
			t.Fatalf("a second upload replaced the blob: %v", err)
		}
		got, err := e.svc.GetEscrow(ctx, h)
		if err != nil || string(got) != string(blob) {
			t.Fatalf("%v", err)
		}
		sync := e.enrol("peer", api.ScopeSyncRead)
		if _, err := e.svc.GetEscrow(ctx, sync); !errors.Is(err, service.ErrForbidden) {
			t.Fatalf("a sync peer read the escrow blob: %v", err)
		}
		if err := e.svc.ClearEscrow(ctx, "custody"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.GetEscrow(ctx, h); !errors.Is(err, service.ErrNotFound) {
			t.Fatalf("not cleared: %v", err)
		}
	})
}

// Session pins live in one process, so destructive work needs the maintenance lock: while a daemon
// serves the repositories, no other process may run GC, retention, forget, quarantining verify or a
// credential rotation, and a second daemon cannot start.
func TestDestructiveWorkNeedsTheDaemonLockAndASecondDaemonCannotStart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	other := service.New(e.svc.DB, e.svc.StateDir, "test", nil) // another process on the same database
	release, err := e.svc.BecomeDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.BecomeDaemon(ctx); err == nil {
		t.Fatal("a second daemon started on the same database")
	}
	if _, err := other.GC(ctx, "custody", service.DefaultGCGrace, false); !errors.Is(err, service.ErrDaemonRunning) {
		t.Fatalf("GC from another process while the daemon runs: %v", err)
	}
	if _, err := other.Prune(ctx, "custody", false); !errors.Is(err, service.ErrDaemonRunning) {
		t.Fatalf("prune: %v", err)
	}
	if _, err := other.VerifyJob(ctx, "custody", service.VerifyRequest{}); !errors.Is(err, service.ErrDaemonRunning) {
		t.Fatalf("quarantining verify: %v", err)
	}
	if err := other.ForgetSnapshot(ctx, "custody", group, "20261001120000-00000001"); !errors.Is(err, service.ErrDaemonRunning) {
		t.Fatalf("forget: %v", err)
	}
	// read-only work is never blocked
	if _, err := other.GC(ctx, "custody", service.DefaultGCGrace, true); err != nil {
		t.Fatalf("dry-run GC: %v", err)
	}
	if _, err := other.Prune(ctx, "custody", true); err != nil {
		t.Fatalf("prune preview: %v", err)
	}
	if _, err := other.VerifyJob(ctx, "custody", service.VerifyRequest{NoQuarantine: true}); err != nil {
		t.Fatalf("report-only verify: %v", err)
	}
	// the daemon itself is not blocked by its own lock
	if _, err := e.svc.GC(ctx, "custody", service.DefaultGCGrace, false); err != nil {
		t.Fatalf("the daemon's own GC: %v", err)
	}
	release()
	if _, err := other.GC(ctx, "custody", service.DefaultGCGrace, false); err != nil {
		t.Fatalf("without a daemon the CLI may do it: %v", err)
	}
	// and while the CLI's operation holds the lock, a daemon cannot start; concurrent operations of one process share it
	done := make(chan struct{})
	go func() { _, _ = other.GC(ctx, "custody", service.DefaultGCGrace, false); close(done) }()
	<-done
	if rel2, err := e.svc.BecomeDaemon(ctx); err != nil {
		t.Fatalf("the lock must be free again: %v", err)
	} else {
		rel2()
	}
}

// ---- regressions found by the adversarial review ------------------------------------------------------

// A backup-scope host cannot change who owns an object, rename it, or take over another host's
// object: with them go the admin assignments that decide its retention.
func TestAHostCannotRenameOrHijackAnObjectToSteerRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.enrol("host-a", api.ScopeBackup)
	b := e.enrol("host-b", api.ScopeBackup)
	k := keys(1)
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "short", KeepLast: 1})
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "long", KeepLast: 100})
	for i := 0; i < 5; i++ {
		ts := e.now.Add(time.Duration(i) * time.Hour)
		if err := e.upload(a, k, group, snapID(ts, i), ts, "long", fmt.Sprintf("d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// the admin gave host-b the aggressive default and pinned the object to "long"
	if err := e.svc.Assign(ctx, "custody", "host-b", "", "", "short"); err != nil {
		t.Fatal(err)
	}
	// 1. host-b learns the group id (GET /v1/objects lists it) and opens + aborts a session on it
	if _, err := begin(e, b, k, group, snapID(e.now, 90), ""); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("another host opened a session on a foreign object: %v", err)
	}
	// 2. host-a renames its own object through the inventory: ignored, the identity is pinned
	if err := e.svc.UpsertInventory(ctx, a, api.Inventory{Objects: []api.ObjectInfo{{Project: "other", Name: "renamed", Kind: "block", GroupID: group, Policy: "short"}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UpsertInventory(ctx, b, api.Inventory{Objects: []api.ObjectInfo{{Project: "p", Name: "vm/root", Kind: "block", GroupID: group, Policy: "short"}}}); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("another host announced a foreign object: %v", err)
	}
	var proj, name, hostName, ret string
	_ = e.svc.DB.Pool().QueryRow(ctx, `SELECT project, name, host_name, retention_policy FROM storage_objects WHERE group_id=$1`, group).Scan(&proj, &name, &hostName, &ret)
	if proj != "p" || name != "vm/root" || hostName != "host-a" || ret != "long" {
		t.Fatalf("the object's identity changed: %s %s %s %s", proj, name, hostName, ret)
	}
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Removed != 0 {
		t.Fatalf("history was pruned by a policy the owner never chose: %v %+v", err, res)
	}
	// only the administrator moves an object
	if err := e.svc.TransferObject(ctx, "custody", group, "host-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := begin(e, b, k, group, snapID(e.now, 91), ""); err != nil {
		t.Fatalf("after the transfer host-b is the owner: %v", err)
	}
}

// An object first seen through the inventory still gets its retention pinned (it used to stay empty = never pruned).
func TestInventoryFirstStillPinsRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	_ = e.svc.SetPolicy(ctx, "custody", api.Policy{Name: "tiny", KeepLast: 1})
	if err := e.svc.UpsertInventory(ctx, h, api.Inventory{Objects: []api.ObjectInfo{{Project: "p", Name: "vm/root", Kind: "block", GroupID: group, Policy: "tiny"}}}); err != nil {
		t.Fatal(err)
	}
	k := keys(1)
	for i := 0; i < 4; i++ {
		ts := e.now.Add(time.Duration(i) * time.Hour)
		if err := e.upload(h, k, group, snapID(ts, i), ts, "tiny", fmt.Sprintf("d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.svc.Prune(ctx, "custody", false)
	if err != nil || res.Removed != 3 {
		t.Fatalf("retention must apply to an object first seen via the inventory: %v %+v", err, res)
	}
}

func TestPolicySchedulesAreValidated(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []api.Policy{{Name: "a", Schedule: "nightly-ish"}, {Name: "b", VerifySchedule: "whenever"}, {Name: "c", Schedule: "25:99"}} {
		if err := e.svc.SetPolicy(context.Background(), "custody", bad); !errors.Is(err, service.ErrBadRequest) {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
	if err := e.svc.SetPolicy(context.Background(), "custody", api.Policy{Name: "ok", Schedule: "every 4h"}); err != nil {
		t.Fatal(err)
	}
}

// A datastore cannot be attached twice; the second attach is a conflict, not a second repository.
func TestTheSameDatastoreCannotBeAttachedTwice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.AttachRepository(ctx, "again", e.repo.Path, false); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("filesystem: %v", err)
	}
	s3 := newEnvS3(t)
	var set service.S3Settings
	raw, _ := s3.svc.Repository(ctx, "custody")
	_ = json.Unmarshal(raw.S3Raw, &set)
	if _, err := s3.svc.AttachS3Repository(ctx, "again", service.S3Params{S3Settings: set, Credential: "custody-s3"}, false); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("s3: %v", err)
	}
}

// A lost secrets.key is reported, never silently replaced (that would orphan every stored credential).
func TestLostSealingKeyIsReportedNotReplaced(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	if err := os.Remove(filepath.Join(e.svc.StateDir, "secrets.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Credentials(ctx); err != nil {
		t.Fatalf("listing must not fail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.svc.StateDir, "secrets.key")); err == nil {
		t.Fatal("a read-only command minted a new sealing key")
	}
	if _, err := e.svc.CreateS3Credential(ctx, "second", service.S3Secret{AccessKey: "a", SecretKey: "b"}); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("a new credential was sealed with a fresh key although others exist: %v", err)
	}
}

// Corruption that was only REPORTED (nothing quarantined) must not be recorded as healed.
func TestReportOnlyVerifyDoesNotClaimHealing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	if err := e.upload(h, k, group, snapID(e.now, 1), e.now, "p", "alpha", "beta"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.svc.Store(e.repo)
	id := k.ChunkID([]byte("alpha"))
	key := "chunks/" + id.String()[:2] + "/" + id.String()[2:4] + "/" + id.String()
	b, _ := st.Backend().Get(key)
	b[20] ^= 0xff
	_ = st.Backend().Put(key, b, false)
	if _, err := e.svc.VerifyJob(ctx, "custody", service.VerifyRequest{NoQuarantine: true}); err == nil {
		t.Fatal("verify missed it")
	}
	cc, _ := e.svc.CorruptChunks(ctx, "custody", true)
	if len(cc) != 1 || cc[0].State == "healed" {
		t.Fatalf("a chunk that is still corrupt was recorded as healed: %+v", cc)
	}
}

// A reaper must not end a session whose commit is running: its pins protect the snapshot being written.
func TestASessionInTheMiddleOfItsCommitIsNotReaped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	st, _ := e.svc.Store(e.repo)
	reaped := make(chan int, 1)
	st.SetFault(func(p string) error {
		if p == "commit:before-marker" {
			st.SetFault(nil)
			e.now = e.now.Add(time.Hour) // the lease runs out while the commit is still going
			reaped <- e.svc.ReapSessions(ctx)
		}
		return nil
	})
	if err := e.upload(h, k, group, snapID(e.now, 1), e.now, "p", "x", "y"); err != nil {
		t.Fatal(err)
	}
	if n := <-reaped; n != 0 {
		t.Fatalf("the reaper ended %d session(s) in the middle of a commit", n)
	}
}
