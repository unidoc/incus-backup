package datastore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A commit whose backend writes are slow (or hung) must not stall the store: no lock that GC,
// Missing, PutChunk, BeginSession, BeginPinSession or Close of OTHER sessions need may be held
// across the marker writes.
func TestSlowCommitDoesNotStallOtherSessions(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "c-a", "c-b")
		_, _, junk := buildSnapshot(t, s, k, g1, "20261007230001-0b0b0b0b", "junk-1", "junk-2", "junk-3")
		for _, id := range junk {
			backdate(t, s, chunkKey(id), time.Now().Add(-72*time.Hour))
		}
		for _, id := range ids {
			backdate(t, s, chunkKey(id), time.Now().Add(-72*time.Hour))
		}
		sess, err := s.BeginSession(g1, snap)
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.PutIndex(idx); err != nil {
			t.Fatal(err)
		}
		if err := sess.PutManifest(man); err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		inHook := make(chan struct{})
		gcSweeping := make(chan struct{})
		var hookOnce, gcOnce sync.Once
		// installed once, before any goroutine starts, and left in place (no concurrent writes to the hook)
		s.SetFault(func(p string) error {
			switch p {
			case "commit:before-marker":
				hookOnce.Do(func() {
					close(inHook)
					<-release // a slow/hung backend write
				})
			case "gc:after-mark":
				gcOnce.Do(func() { close(gcSweeping) })
			}
			return nil
		})
		commitDone := make(chan error, 1)
		go func() { _, err := sess.Commit("client", time.Time{}); commitDone <- err }()
		<-inHook
		gcDone := make(chan error, 1)
		go func() { _, err := s.GC(context.Background(), time.Hour, false); gcDone <- err }()
		defer func() { // never leave goroutines hung on failure
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		select { // GC has finished its mark phase and is about to take the sweep lock
		case <-gcSweeping:
		case err := <-gcDone:
			t.Fatalf("gc ended before its sweep: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("GC never reached its sweep")
		}
		within := func(what string, f func()) {
			t.Helper()
			d := make(chan struct{})
			go func() { f(); close(d) }()
			select {
			case <-d:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s STALLED while one commit's backend write is slow", what)
			}
		}
		within("other session's Missing", func() { s.BeginPinSession().Missing(ids) })
		within("other session's PutChunk", func() {
			o := s.BeginPinSession()
			_, _ = o.PutChunk(ids[0], nil)
			o.Close()
		})
		within("BeginSession+Close", func() {
			o, err := s.BeginSession(g1, "20261007230002-0c0c0c0c")
			if err == nil {
				o.Close()
			}
		})
		select { // the GC itself must finish too, and must keep the pinned chunks
		case err := <-gcDone:
			if err != nil {
				t.Fatalf("gc: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("GC STALLED while one commit's backend write is slow")
		}
		close(release)
		if err := <-commitDone; err != nil {
			t.Fatalf("commit: %v", err)
		}
		if !s.SnapshotExists(g1, snap) {
			t.Fatal("snapshot not visible after commit")
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
	})
}

// Branch (a): an abort that wins against a commit (it closes the session after the commit's pin and
// chunk checks but before the marker writes) makes the commit return ErrSessionClosed and write nothing.
func TestCloseWinsRaceAgainstCommit(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, _ := buildSnapshot(t, s, k, g1, snap, "c-a", "c-b")
		sess, err := s.BeginSession(g1, snap)
		if err != nil {
			t.Fatal(err)
		}
		_ = sess.PutIndex(idx)
		_ = sess.PutManifest(man)
		l := s.snapLock(g1, snap)
		l.Lock() // commit passes steps 1-2 then parks on the snapshot lock
		errc := make(chan error, 1)
		go func() { _, err := sess.Commit("client", time.Time{}); errc <- err }()
		time.Sleep(300 * time.Millisecond)
		sess.Close() // wins: commit has not taken commitMu yet
		l.Unlock()
		if err := <-errc; !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("commit err = %v, want ErrSessionClosed", err)
		}
		if s.SnapshotExists(g1, snap) {
			t.Fatal("snapshot visible after Close won the race")
		}
		if _, err := s.b.Stat(snapPrefix(g1, snap) + "commit.json"); err == nil {
			t.Fatal("commit.json exists")
		}
	})
}
