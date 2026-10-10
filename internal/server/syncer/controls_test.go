package syncer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

func eachMix(t *testing.T, fn func(t *testing.T, src, dst *node)) {
	t.Run("fs-to-fs", func(t *testing.T) { fn(t, newNode(t, "src"), newNode(t, "dst")) })
	t.Run("s3-to-fs", func(t *testing.T) { fn(t, newS3Node(t, "src"), newNode(t, "dst")) })
	t.Run("fs-to-s3", func(t *testing.T) { fn(t, newNode(t, "src"), newS3Node(t, "dst")) })
}

func TestSyncVerifiedOnlyAndNeverPropagatesFailedSnapshots(t *testing.T) {
	eachMix(t, func(t *testing.T, a, b *node) {
		ctx := context.Background()
		k := keys()
		bk := a.enrol("benfica", api.ScopeBackup)
		backup(t, bk, k, group, snapID(1), a.now, "p", "one", "shared")
		backup(t, bk, k, group, snapID(2), a.now.Add(time.Hour), "p", "two", "shared")
		if _, err := a.svc.Verify(ctx, "custody"); err != nil {
			t.Fatal(err)
		}
		backup(t, bk, k, group, snapID(3), a.now.Add(2*time.Hour), "p", "three", "shared") // never verified
		m, _, _ := addJob(t, a, b, "push", "j")
		if err := m.SetOptions(ctx, "custody", "j", opts(true, false, 0, 0)); err != nil {
			t.Fatal(err)
		}
		res, err := m.Run(ctx, "custody", "j")
		if err != nil || res.Synced != 2 || res.SkippedUnverified != 1 {
			t.Fatalf("verified-only: %v %+v", err, res)
		}
		if len(list(t, b)) != 2 {
			t.Fatal("an unverified snapshot was synced by a verified-only job")
		}
		st, _ := m.Status(ctx, "custody", "j")
		if st.SnapshotsSkipped != 1 || !st.VerifiedOnly {
			t.Fatalf("status must expose the skip: %+v", st)
		}
		// without verified-only the third goes, once verified
		_ = m.SetOptions(ctx, "custody", "j", opts(false, false, 0, 0))
		if _, err := a.svc.Verify(ctx, "custody"); err != nil {
			t.Fatal(err)
		}
		if res, err = m.Run(ctx, "custody", "j"); err != nil || res.Synced != 1 {
			t.Fatalf("%v %+v", err, res)
		}
	})
}

func TestCorruptSnapshotsAreNeverTransferred(t *testing.T) {
	eachMix(t, func(t *testing.T, a, b *node) {
		ctx := context.Background()
		k := keys()
		bk := a.enrol("benfica", api.ScopeBackup)
		backup(t, bk, k, group, snapID(1), a.now, "p", "good", "rotten")
		backup(t, bk, k, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", snapID(2), a.now, "p", "fine-other-object")
		st, _ := a.svc.Store(a.repo)
		id := k.ChunkID([]byte("rotten"))
		key := "chunks/" + id.String()[:2] + "/" + id.String()[2:4] + "/" + id.String()
		blob, _ := st.Backend().Get(key)
		blob[25] ^= 1
		_ = st.Backend().Put(key, blob, false)
		_, _ = a.svc.Verify(ctx, "custody") // quarantines, marks the snapshot failed
		m, _, _ := addJob(t, a, b, "push", "j")
		res, err := m.Run(ctx, "custody", "j")
		if err != nil || res.Synced != 1 || res.SkippedCorrupt != 1 {
			t.Fatalf("%v %+v", err, res)
		}
		for _, r := range list(t, b) {
			if r.GroupID == group {
				t.Fatal("a snapshot that failed verification was transferred")
			}
		}
	})
}

func TestTransferLastLimitsToTheNewestSnapshotsPerGroup(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	for i := 1; i <= 5; i++ {
		backup(t, bk, k, group, snapID(i), a.now.Add(time.Duration(i)*time.Hour), "p", fmt.Sprintf("v%d", i))
	}
	m, _, _ := addJob(t, a, b, "push", "j")
	if err := m.SetOptions(ctx, "custody", "j", opts(false, false, 2, 0)); err != nil {
		t.Fatal(err)
	}
	res, err := m.Run(ctx, "custody", "j")
	if err != nil || res.Synced != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	got := list(t, b)
	if len(got) != 2 || got[0].SnapshotID != snapID(4) || got[1].SnapshotID != snapID(5) {
		t.Fatalf("not the newest two: %+v", got)
	}
}

func TestSyncRateLimitIsEnforced(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	var parts []string
	for i := 0; i < 6; i++ {
		parts = append(parts, strings.Repeat(fmt.Sprintf("%c", 'a'+i), 4096)+fmt.Sprint(i)) // ~ incompressible-ish distinct chunks
	}
	backup(t, bk, k, group, snapID(1), a.now, "p", parts...)
	m, _, _ := addJob(t, a, b, "push", "j")
	_ = m.SetOptions(ctx, "custody", "j", opts(false, false, 0, 600)) // 600 B/s
	start := time.Now()
	res, err := m.Run(ctx, "custody", "j")
	el := time.Since(start)
	if err != nil || res.Synced != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	want := time.Duration(float64(res.BytesTransferred)/600*float64(time.Second)) * 7 / 10
	if el < want {
		t.Fatalf("moved %d bytes in %v at a 600 B/s limit (want at least %v)", res.BytesTransferred, el, want)
	}
}

// resync-corrupt: a destination snapshot that failed verification is repaired from the source.
func TestResyncCorruptRepairsTheDestinationFromAHealthySource(t *testing.T) {
	eachMix(t, func(t *testing.T, a, b *node) {
		ctx := context.Background()
		k := keys()
		bk := a.enrol("benfica", api.ScopeBackup)
		backup(t, bk, k, group, snapID(1), a.now, "p", "stable", "will-rot")
		// b pulls from a (so b is the local repository and can verify itself)
		m, _, _ := addJob(t, b, a, "pull", "j")
		if res, err := m.Run(ctx, "custody", "j"); err != nil || res.Synced != 1 {
			t.Fatalf("%v %+v", err, res)
		}
		st, _ := b.svc.Store(b.repo)
		id := k.ChunkID([]byte("will-rot"))
		key := "chunks/" + id.String()[:2] + "/" + id.String()[2:4] + "/" + id.String()
		blob, _ := st.Backend().Get(key)
		blob[30] ^= 1
		_ = st.Backend().Put(key, blob, false)
		if _, err := b.svc.Verify(ctx, "custody"); err == nil {
			t.Fatal("verify missed the corruption")
		}
		// without the option nothing is repaired
		if res, err := m.Run(ctx, "custody", "j"); err != nil || res.Repaired != 0 {
			t.Fatalf("%v %+v", err, res)
		}
		_ = m.SetOptions(ctx, "custody", "j", opts(false, true, 0, 0))
		res, err := m.Run(ctx, "custody", "j")
		if err != nil || res.Repaired != 1 || res.ChunksTransferred != 1 {
			t.Fatalf("repair: %v %+v", err, res)
		}
		if decodeAll(t, b, k) != 2 {
			t.Fatal("destination not restorable after the repair")
		}
		l := list(t, b)
		if len(l) != 1 || l[0].VerifyState != "ok" {
			t.Fatalf("the repaired snapshot must be re-verified ok: %+v", l)
		}
		cc, _ := b.svc.CorruptChunks(ctx, "custody", true)
		if len(cc) != 1 || cc[0].State != "healed" {
			t.Fatalf("corruption record: %+v", cc)
		}
	})
}

// If the source is corrupt too, a resync must not "repair" anything from it.
func TestResyncCorruptNeverCopiesFromACorruptSource(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "stable", "will-rot")
	m, _, _ := addJob(t, b, a, "pull", "j")
	_, _ = m.Run(ctx, "custody", "j")
	rot := func(n *node) {
		st, _ := n.svc.Store(n.repo)
		id := k.ChunkID([]byte("will-rot"))
		key := "chunks/" + id.String()[:2] + "/" + id.String()[2:4] + "/" + id.String()
		blob, _ := st.Backend().Get(key)
		blob[30] ^= 1
		_ = st.Backend().Put(key, blob, false)
	}
	rot(a)
	rot(b)
	_, _ = a.svc.Verify(ctx, "custody")
	_, _ = b.svc.Verify(ctx, "custody")
	_ = m.SetOptions(ctx, "custody", "j", opts(false, true, 0, 0))
	res, err := m.Run(ctx, "custody", "j")
	if err != nil || res.Repaired != 0 || res.ChunksTransferred != 0 {
		t.Fatalf("repaired from a corrupt source: %v %+v", err, res)
	}
	_ = format.ZeroID
	_ = syncer.Options{}
}

func opts(verifiedOnly, resyncCorrupt bool, transferLast int, bps int64) syncer.JobOptions {
	return syncer.JobOptions{VerifiedOnly: &verifiedOnly, ResyncCorrupt: &resyncCorrupt, TransferLast: &transferLast, RateLimitBps: &bps}
}

func TestSyncJobNamesFiltersAndPartialOptionChangesAreValidated(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	ctx := context.Background()
	m := &syncer.Manager{Svc: a.svc}
	tokFor := func(n string) string {
		tok, err := b.svc.CreateToken(ctx, "custody", "peer-"+n, api.ScopeSyncWrite, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	for _, bad := range []string{".", "..", "a/b", "", "x y"} {
		if err := m.AddJob(ctx, "custody", syncer.Job{Name: bad, Direction: "push", Remote: b.addr()}, tokFor("n")); err == nil {
			t.Errorf("job name %q accepted", bad)
		}
	}
	if err := m.AddJob(ctx, "custody", syncer.Job{Name: "j", Direction: "push", Remote: b.addr(), GroupFilter: []string{"not-a-group-id"}}, tokFor("g")); err == nil {
		t.Fatal("a group filter that is not a group id was accepted (the job would copy nothing and report ok)")
	}
	if err := m.AddJob(ctx, "custody", syncer.Job{Name: "j", Direction: "push", Remote: b.addr(), GroupFilter: []string{group}, TransferLast: 4}, tokFor("ok")); err != nil {
		t.Fatal(err)
	}
	// a second job with the same name must fail BEFORE it consumes the token or touches the first job's certificates
	tok := tokFor("dup")
	if err := m.AddJob(ctx, "custody", syncer.Job{Name: "j", Direction: "push", Remote: b.addr()}, tok); err == nil {
		t.Fatal("a duplicate job name was accepted")
	}
	if _, err := os.Stat(filepath.Join(a.svc.StateDir, "sync", "custody", "j", "client.key")); err != nil {
		t.Fatalf("the first job's credentials were damaged: %v", err)
	}
	// only the options given change: the group filter and transfer-last survive a rate-limit change
	bps := int64(5000)
	if err := m.SetOptions(ctx, "custody", "j", syncer.JobOptions{RateLimitBps: &bps}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := m.Jobs(ctx, "custody")
	if len(jobs[0].GroupFilter) != 1 || jobs[0].TransferLast != 4 || jobs[0].RateLimitBps != 5000 {
		t.Fatalf("a partial change reset other options: %+v", jobs[0])
	}
	badGroups := []string{"oops"}
	if err := m.SetOptions(ctx, "custody", "j", syncer.JobOptions{Groups: &badGroups}); err == nil {
		t.Fatal("bad group filter accepted by set")
	}
}
