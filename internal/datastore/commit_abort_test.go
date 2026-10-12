package datastore

// An abort (Session.Close) that races the slow part of a commit must not drop the session's pins
// while the commit goes on to write its marker: a GC after the abort would otherwise sweep chunks
// the new snapshot dedup'ed against. The abort is modelled by Close at the commit:before-marker hook
// (the latest point of the window), followed by a GC with chunks older than the grace period.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAbortDuringCommitThenGCKeepsChunks(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		// chunks that already exist and are old: a dedup hit on unreferenced data (e.g. of a pruned base)
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "dedup-a", "dedup-b")
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
		gcDone := make(chan string, 1)
		s.SetFault(func(p string) error {
			if p != "commit:before-marker" {
				return nil
			}
			s.SetFault(nil)
			go func() { // the client's Abort, then the nightly GC
				sess.Close()
				st, err := s.GC(context.Background(), 24*time.Hour, false)
				gcDone <- fmt.Sprintf("removed=%d err=%v", st.Removed, err)
			}()
			select { // let them run before the marker; a fixed commit makes Close wait, so do not block for ever
			case r := <-gcDone:
				gcDone <- r
			case <-time.After(500 * time.Millisecond):
			}
			return nil
		})
		_, cerr := sess.Commit("client", time.Time{})
		gc := <-gcDone
		visible := s.SnapshotExists(g1, snap)
		inv := "ok"
		if err := s.CheckInvariants(); err != nil {
			inv = "BROKEN"
		}
		got := fmt.Sprintf("snapshotVisible=%v | invariants=%s", visible, inv)
		t.Logf("commit err=%v; gc %s", cerr, gc)
		// safe outcomes: either the snapshot is visible WITH all its chunks, or it is not visible at all
		if got != "snapshotVisible=true | invariants=ok" && got != "snapshotVisible=false | invariants=ok" {
			t.Fatalf("\n got: %s\nwant: snapshotVisible=<any> | invariants=ok", got)
		}
	})
}
