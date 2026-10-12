package syncer_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

func nodeOn(t *testing.T, name string, s3 bool) *node {
	if s3 {
		return newS3Node(t, name)
	}
	return newNode(t, name)
}

// Server-to-server sync works whichever side is an S3 bucket, in both directions, and
// the destination is a standalone restore source afterwards.
func TestSyncBetweenAnyMixOfBackends(t *testing.T) {
	for _, tc := range []struct {
		src, dst bool // true = S3
		dir      string
	}{
		{false, true, "push"}, {true, false, "push"}, {true, true, "push"},
		{false, true, "pull"}, {true, false, "pull"}, {true, true, "pull"},
	} {
		tc := tc
		name := fmt.Sprintf("%s-src-s3=%v-dst-s3=%v", tc.dir, tc.src, tc.dst)
		t.Run(name, func(t *testing.T) {
			src, dst := nodeOn(t, "src", tc.src), nodeOn(t, "dst", tc.dst)
			ctx := context.Background()
			k := keys()
			bk := src.enrol("benfica", api.ScopeBackup)
			backup(t, bk, k, group, snapID(1), src.now, "p", "alpha", "beta", "gamma")
			backup(t, bk, k, group, snapID(2), src.now.Add(3600e9), "p", "alpha", "beta", "delta")
			var m *syncer.Manager
			if tc.dir == "push" {
				m, _, _ = addJob(t, src, dst, "push", "j")
			} else {
				m, _, _ = addJob(t, dst, src, "pull", "j")
			}
			res, err := m.Run(ctx, "custody", "j")
			if err != nil || res.Synced != 2 || res.ChunksTransferred != 4 || res.ChunksReused != 2 {
				t.Fatalf("%v %+v", err, res)
			}
			if n := decodeAll(t, dst, k); n != 6 {
				t.Fatalf("decoded %d chunk refs", n)
			}
			// idempotent
			if res, err = m.Run(ctx, "custody", "j"); err != nil || res.ChunksTransferred != 0 {
				t.Fatalf("second run: %v %+v", err, res)
			}
			// the destination verifies clean, whatever it is stored on
			vs, err := dst.svc.Verify(ctx, "custody")
			if err != nil || len(vs.Problems) != 0 {
				t.Fatalf("verify: %v %+v", err, vs)
			}
		})
	}
}

// An interrupted sync INTO a bucket never exposes a partial snapshot and resumes.
func TestInterruptedSyncIntoS3Resumes(t *testing.T) {
	a, b := newNode(t, "a"), newS3Node(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "c1", "c2", "c3", "c4", "c5", "c6")
	src := &syncer.LocalRepo{Svc: a.svc, Repo: a.repo}
	real := &syncer.LocalRepo{Svc: b.svc, Repo: b.repo}
	res, err := syncer.Run(ctx, src, &failingDest{Dest: real, failAfter: 3}, syncer.Options{Concurrency: 1})
	if err == nil || res.Synced != 0 {
		t.Fatalf("expected failure: %v %+v", err, res)
	}
	if len(list(t, b)) != 0 {
		t.Fatal("partial snapshot visible in the bucket")
	}
	st, _ := b.svc.Store(b.repo)
	if err := st.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	res, err = syncer.Run(ctx, src, real, syncer.Options{})
	if err != nil || res.Synced != 1 || res.ChunksTransferred != 3 || res.ChunksReused != 3 {
		t.Fatalf("resume: %v %+v", err, res)
	}
	if decodeAll(t, b, k) != 6 {
		t.Fatal("not restorable")
	}
}

// Silent corruption inside the bucket (a flipped bit at rest) is caught keylessly by
// verify and is never replicated to another server.
func TestCorruptObjectInBucketIsDetectedAndNotReplicated(t *testing.T) {
	a, b := newS3Node(t, "a"), newNode(t, "b")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "only")
	ib, _ := a.svc.RepoIndex(ctx, a.repo, group, snapID(1))
	idx, _ := format.ParseIndex(ib)
	id := idx.Entries[0].ID.String()
	st, _ := a.svc.Store(a.repo)
	key := "chunks/" + id[:2] + "/" + id[2:4] + "/" + id
	blob, _ := st.Backend().Get(key)
	blob[15] ^= 0xff
	_ = st.Backend().Put(key, blob, false)
	// not verified yet: the keyless blob check at transfer time is what stops it
	res, err := syncer.Run(ctx, &syncer.LocalRepo{Svc: a.svc, Repo: a.repo}, &syncer.LocalRepo{Svc: b.svc, Repo: b.repo}, syncer.Options{})
	if err == nil || res.Synced != 0 || len(list(t, b)) != 0 {
		t.Fatalf("corruption replicated: %v %+v", err, res)
	}
	vs, err := a.svc.Verify(ctx, "custody") // reports problems as an error plus stats
	if err == nil || vs == nil || len(vs.Problems) != 1 || vs.Problems[0].Kind != "chunk-corrupt" {
		t.Fatalf("verify missed the corruption: %v %+v", err, vs)
	}
	// once verify has marked it failed, the snapshot is skipped outright
	res, err = syncer.Run(ctx, &syncer.LocalRepo{Svc: a.svc, Repo: a.repo}, &syncer.LocalRepo{Svc: b.svc, Repo: b.repo}, syncer.Options{})
	if err != nil || res.Synced != 0 || res.SkippedCorrupt != 1 || len(list(t, b)) != 0 {
		t.Fatalf("a failed snapshot was not skipped: %v %+v", err, res)
	}
}
