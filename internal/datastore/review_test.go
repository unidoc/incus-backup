package datastore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
)

// ---- regressions found by the adversarial review ---------------------------------------------------

// Close while requests of the same session are in flight and a GC is running must not crash
// ("concurrent map iteration and map write" kills the daemon) and must not lose a claim.
func TestClosingASessionWhileItsRequestsAreInFlightIsSafe(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		if err := s.beginGC(); err != nil {
			t.Fatal(err)
		}
		defer s.endGC()
		sess, _ := s.BeginSession(g1, "20261007230000-0a0a0a0a")
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 40; i++ {
					id, blob, _ := k.EncodeChunk([]byte(fmt.Sprintf("chunk-%d-%d", w, i)), format.EncodeOpts{})
					if _, err := sess.PutChunk(id, blob); err != nil && !errors.Is(err, ErrSessionClosed) {
						t.Error(err)
						return
					}
					_, _ = sess.Missing([]format.ID{id})
				}
			}(w)
		}
		time.Sleep(2 * time.Millisecond)
		sess.Close()
		wg.Wait()
		if _, err := sess.PutChunk(format.ID{1}, nil); !errors.Is(err, ErrSessionClosed) && err == nil {
			t.Fatal("a closed session accepted a chunk")
		}
	})
}

// A GC that starts AFTER commit began (but before the marker exists) must not sweep chunks the
// session only found by stat: the commit pins its whole index first.
func TestGCStartingDuringCommitCannotSweepTheIndexChunks(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "u-a", "u-b") // chunks stored without a session claim
		for _, id := range ids {
			backdate(t, s, chunkKey(id), time.Now().Add(-72*time.Hour))
		}
		marked := make(chan struct{})
		var wg sync.WaitGroup
		s.SetFault(func(p string) error {
			switch p {
			case "commit:before-marker":
				wg.Add(1)
				go func() { defer wg.Done(); _, _ = s.GC(context.Background(), 24*time.Hour, false) }()
				<-marked
			case "gc:after-mark":
				select {
				case <-marked:
				default:
					close(marked)
				}
			}
			return nil
		})
		if err := commit(t, s, g1, snap, idx, man); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if err := s.CheckInvariants(); err != nil {
			t.Fatalf("DATA LOSS: %v", err)
		}
	})
}

// A commit.json PUT that is retried after its response was lost answers "exists": when the marker is
// ours byte for byte, the commit succeeded.
type lostResponse struct {
	Backend
	done bool
}

func (l *lostResponse) Put(key string, data []byte, ifAbsent bool) error {
	if strings.HasSuffix(key, "/commit.json") && !l.done {
		if err := l.Backend.Put(key, data, ifAbsent); err != nil {
			return err
		}
		l.done = true
		return ErrExists // the retry of a PUT whose first attempt had already succeeded
	}
	return l.Backend.Put(key, data, ifAbsent)
}

func TestCommitSurvivesALostResponseOfItsOwnMarkerPut(t *testing.T) {
	b := NewMemBackend()
	if _, err := InitBackend(b); err != nil {
		t.Fatal(err)
	}
	s, _ := OpenBackend(&lostResponse{Backend: b})
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	idx, man, _ := buildSnapshot(t, s, k, g1, snap, "x")
	if err := commit(t, s, g1, snap, idx, man); err != nil {
		t.Fatalf("a commit whose marker PUT succeeded was reported as failed: %v", err)
	}
	// but a DIFFERENT snapshot with the same id is still refused
	other, _ := OpenBackend(b)
	idx2, man2, _ := buildSnapshot(t, other, k, g1, snap, "y")
	if err := commit(t, other, g1, snap, idx2, man2); !errors.Is(err, ErrExists) {
		t.Fatalf("a different snapshot under the same id: %v", err)
	}
}

// A chunk found corrupt by a job that checked only ONE snapshot breaks every snapshot that uses it.
func TestCorruptSharedChunkMarksEverySnapshotThatUsesItFailed(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snaps := []string{"20261007230000-0a0a0a0a", "20261007230001-0b0b0b0b", "20261007230002-0c0c0c0c"}
		var shared format.ID
		for i, sn := range snaps {
			idx, man, ids := buildSnapshot(t, s, k, g1, sn, "shared", fmt.Sprintf("own-%d", i))
			shared = ids[0]
			if err := commit(t, s, g1, sn, idx, man); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.VerifyWith(context.Background(), VerifyOpts{Record: true}); err != nil {
			t.Fatal(err)
		}
		corrupt(t, s, chunkKey(shared), 20)
		only := func(si SnapshotInfo, _ *VerifyRecord) bool { return si.Snapshot == snaps[0] }
		vs, _ := s.VerifyWith(context.Background(), VerifyOpts{Select: only, Quarantine: true, Record: true})
		if vs.Quarantined != 1 {
			t.Fatalf("%+v", vs)
		}
		for _, sn := range snaps {
			rec, _ := s.ReadVerify(g1, sn)
			if rec == nil || rec.State != VerifyFailed {
				t.Fatalf("%s must be marked failed (it uses the quarantined chunk): %+v", sn, rec)
			}
		}
	})
}

// Protection set between prune's check and the delete wins.
type protectOnDelete struct {
	Backend
	armed bool
}

func (p *protectOnDelete) Delete(key string) error {
	err := p.Backend.Delete(key)
	if p.armed && strings.HasSuffix(key, "/commit.json") {
		p.armed = false
		_ = p.Backend.Put(strings.TrimSuffix(key, "commit.json")+"protected", []byte("x"), false)
	}
	return err
}

func TestProtectionSetDuringDeleteIsNotLost(t *testing.T) {
	b := NewMemBackend()
	_, _ = InitBackend(b)
	pd := &protectOnDelete{Backend: b}
	s, _ := OpenBackend(pd)
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	idx, man, _ := buildSnapshot(t, s, k, g1, snap, "p")
	if err := commit(t, s, g1, snap, idx, man); err != nil {
		t.Fatal(err)
	}
	pd.armed = true
	if err := s.DeleteSnapshot(g1, snap); !errors.Is(err, ErrProtected) {
		t.Fatalf("delete: %v", err)
	}
	if l, _ := s.ListSnapshots(g1); len(l) != 1 || !l[0].Protected {
		t.Fatalf("the snapshot was lost although it was protected before the delete finished: %+v", l)
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// Orphan cleanup removes debris of uncommitted snapshots and NEVER the group notes.
func TestCleanOrphansKeepsGroupNotesAndRemovesDebris(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		if err := s.SetGroupNotes(g1, "important"); err != nil {
			t.Fatal(err)
		}
		debris := snapPrefix(g1, "20261007230000-0a0a0a0a")
		_ = s.b.Put(debris+"index", []byte("x"), false)
		_ = s.b.Put(debris+"notes", []byte("x"), false)
		backdate(t, s, debris+"index", time.Now().Add(-48*time.Hour))
		backdate(t, s, debris+"notes", time.Now().Add(-48*time.Hour))
		backdate(t, s, "groups/"+g1+"/notes", time.Now().Add(-48*time.Hour))
		n, err := s.CleanOrphans(24 * time.Hour)
		if err != nil || n != 2 {
			t.Fatalf("%d %v", n, err)
		}
		if got, _ := s.GroupNotes(g1); got != "important" {
			t.Fatalf("group notes were deleted: %q", got)
		}
		// GC runs it: debris older than the grace goes away
		_ = s.b.Put(debris+"manifest", []byte("x"), false)
		backdate(t, s, debris+"manifest", time.Now().Add(-48*time.Hour))
		st, err := s.GC(context.Background(), 24*time.Hour, false)
		if err != nil || st.OrphansRemoved != 1 {
			t.Fatalf("%+v %v", st, err)
		}
	})
}
