package datastore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/s3test"
)

// memStore returns a store on a counting in-memory backend whose Stat/Get sleep for delay.
func memStore(t *testing.T, delay time.Duration) (*Store, *countingBackend, *MemBackend) {
	t.Helper()
	mb := NewMemBackend()
	cb := &countingBackend{Backend: mb, delay: delay}
	if _, err := InitBackend(cb); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(cb)
	if err != nil {
		t.Fatal(err)
	}
	return s, cb, mb
}

func putChunks(t *testing.T, s *Store, k *format.Keys, prefix string, n int) []format.ID {
	t.Helper()
	ids := make([]format.ID, n)
	for i := range ids {
		id, blob, err := k.EncodeChunk([]byte(fmt.Sprintf("%s-%d", prefix, i)), format.EncodeOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutChunk(id, blob); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

func randomIDs(n int, seed byte) []format.ID {
	out := make([]format.ID, n)
	for i := range out {
		out[i][0], out[i][1], out[i][2], out[i][31] = seed, byte(i>>8), byte(i), 0x5a
	}
	return out
}

// Missing asks the backend in parallel, never more than the configured bound at once, and
// answers exactly what the sequential implementation answers (order, duplicates, zero ids).
func TestParallelMissingIsBoundedAndIdenticalToSequential(t *testing.T) {
	s, cb, _ := memStore(t, 2*time.Millisecond)
	k := testKeys(t)
	present := putChunks(t, s, k, "p", 150)
	absent := randomIDs(60, 7)
	var ids []format.ID
	for i := 0; i < 150; i++ {
		ids = append(ids, present[i])
		if i%3 == 0 {
			ids = append(ids, absent[i/3%60])
		}
		if i%10 == 0 {
			ids = append(ids, format.ZeroID, present[i]) // zero ids and duplicates
		}
	}
	ids = append(ids, absent[0], absent[0])
	s.SetRequestParallelism(1)
	want, err := s.Missing(ids)
	if err != nil {
		t.Fatal(err)
	}
	if cb.maxInflight.Load() != 1 {
		t.Fatalf("sequential run had %d requests in flight", cb.maxInflight.Load())
	}
	for _, par := range []int{4, 16} {
		s.SetRequestParallelism(par)
		cb.reset()
		got, err := s.Missing(ids)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("par=%d: answer differs from the sequential one", par)
		}
		if m := cb.maxInflight.Load(); m > int64(par) || m < 2 {
			t.Fatalf("par=%d: %d requests in flight", par, m)
		}
		// each distinct non-zero id is asked once
		if n := cb.stats.Load(); n != int64(150+50) {
			t.Fatalf("par=%d: %d HEADs for 200 distinct ids", par, n)
		}
	}
}

type failingStat struct {
	Backend
	bad string
}

func (f *failingStat) Stat(k string) (Info, error) {
	if strings.HasSuffix(k, f.bad) {
		return Info{}, errors.New("injected backend failure")
	}
	return f.Backend.Stat(k)
}

// A backend error (not "not found") fails Missing and Commit: it is never taken as "present"
// or "missing".
func TestParallelChecksFailOnBackendErrors(t *testing.T) {
	mb := NewMemBackend()
	fb := &failingStat{Backend: mb}
	if _, err := InitBackend(fb); err != nil {
		t.Fatal(err)
	}
	s, _ := OpenBackend(fb)
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	var parts []string
	for i := 0; i < 100; i++ {
		parts = append(parts, fmt.Sprintf("part-%d", i))
	}
	idx, man, ids := buildSnapshot(t, s, k, g1, snap, parts...)
	fb.bad = ids[57].String()
	if _, err := s.Missing(ids); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Missing: %v", err)
	}
	err := commit(t, s, g1, snap, idx, man)
	if err == nil || errors.Is(err, ErrMissingChunks) || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Commit: %v", err)
	}
	if s.SnapshotExists(g1, snap) {
		t.Fatal("snapshot visible")
	}
	fb.bad = "no-such-suffix"
	if err := commit(t, s, g1, snap, idx, man); err != nil {
		t.Fatal(err)
	}
}

// Commit checks every chunk in parallel (bounded) and still refuses a snapshot with a missing chunk.
func TestParallelCommitIsBoundedAndRefusesMissing(t *testing.T) {
	s, cb, mb := memStore(t, time.Millisecond)
	s.SetRequestParallelism(8)
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	var parts []string
	for i := 0; i < 200; i++ {
		parts = append(parts, fmt.Sprintf("c-%d", i))
	}
	idx, man, ids := buildSnapshot(t, s, k, g1, snap, parts...)
	_ = mb.Delete(chunkKey(ids[123]))
	_ = mb.Delete(chunkKey(ids[7]))
	cb.reset()
	err := commit(t, s, g1, snap, idx, man)
	if !errors.Is(err, ErrMissingChunks) || !strings.Contains(err.Error(), ": 2 (first "+ids[7].String()) {
		t.Fatalf("commit with 2 missing chunks: %v", err)
	}
	if m := cb.maxInflight.Load(); m > 8 || m < 2 {
		t.Fatalf("%d requests in flight", m)
	}
	if s.SnapshotExists(g1, snap) {
		t.Fatal("visible")
	}
}

// GC (which takes the write side of the lock) and many parallel Missing/Commit calls (read side,
// with parallel requests under it) run together without deadlock, and GC never sweeps a chunk a
// session pinned.
func TestParallelChecksAndGCDoNotDeadlock(t *testing.T) {
	s, _, mb := memStore(t, 200*time.Microsecond)
	s.SetRequestParallelism(8)
	k := testKeys(t)
	ids := putChunks(t, s, k, "shared", 300)
	for _, id := range ids {
		mb.SetModTime(chunkKey(id), time.Now().Add(-48*time.Hour))
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	var sessErr atomic.Value
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				sess := s.BeginPinSession()
				miss, err := sess.Missing(ids)
				if err != nil {
					sessErr.Store(err)
				}
				gone := map[format.ID]bool{}
				for _, m := range miss {
					gone[m] = true
				}
				// whatever the session was told is present must stay present while it is open
				for _, id := range ids {
					if !gone[id] && !s.HasChunk(id) {
						sessErr.Store(fmt.Errorf("chunk %s swept while pinned", id))
					}
				}
				sess.Close()
			}
		}(w)
	}
	finished := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			if _, err := s.GC(context.Background(), time.Hour, false); err != nil && !errors.Is(err, ErrGCRunning) {
				sessErr.Store(err)
			}
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("GC and parallel checks deadlocked")
	}
	close(done)
	wg.Wait()
	if err, _ := sessErr.Load().(error); err != nil {
		t.Fatal(err)
	}
}

// buildMaintenanceRepo writes the same repository (same snapshots, same damage) every time.
func buildMaintenanceRepo(t *testing.T, s *Store, mb *MemBackend) {
	t.Helper()
	k := testKeys(t)
	var all []format.ID
	for g := 0; g < 3; g++ {
		group := fmt.Sprintf("%032x", g+1)
		for i := 0; i < 4; i++ {
			snap := fmt.Sprintf("2026100723000%d-0a0a0a0%d", i, g)
			var parts []string
			for j := 0; j < 40; j++ {
				parts = append(parts, fmt.Sprintf("shared-%d", j), fmt.Sprintf("own-%d-%d-%d", g, i, j))
			}
			idx, man, ids := buildSnapshot(t, s, k, group, snap, parts...)
			if err := commit(t, s, group, snap, idx, man); err != nil {
				t.Fatal(err)
			}
			all = append(all, ids...)
		}
	}
	// garbage: unreferenced old chunks, young chunks, junk names
	old := putChunks(t, s, k, "garbage", 50)
	for _, id := range old {
		mb.SetModTime(chunkKey(id), time.Now().Add(-48*time.Hour))
	}
	putChunks(t, s, k, "young", 10)
	_ = mb.Put("chunks/zz/not-a-chunk", []byte("junk"), false)
	_ = mb.Put("chunks/ab/cd/also-junk", []byte("junk"), false)
	// damage: corrupt and missing chunks, a corrupt index and a missing manifest
	sort.Slice(all, func(i, j int) bool { return all[i].String() < all[j].String() })
	mb.Corrupt(chunkKey(all[3]), 40)
	mb.Corrupt(chunkKey(all[200]), 40)
	_ = mb.Delete(chunkKey(all[100]))
	mb.Corrupt("groups/"+fmt.Sprintf("%032x", 2)+"/20261007230001-0a0a0a01/index", 60)
	_ = mb.Delete("groups/" + fmt.Sprintf("%032x", 3) + "/20261007230002-0a0a0a02/manifest")
}

// Parallel verification reports exactly what the sequential one reports (problems in the same
// order, the same results, counters and quarantine), on a repository with every kind of damage.
func TestParallelVerifyIsIdenticalToSequential(t *testing.T) {
	run := func(par int, o VerifyOpts) (*VerifyStats, []string) {
		s, _, mb := memStore(t, 0)
		mb.SetClock(func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) })
		buildMaintenanceRepo(t, s, mb)
		s.SetRequestParallelism(par)
		fixed := time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
		o.Now = func() time.Time { return fixed }
		vs, err := s.VerifyWith(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		_ = mb.List("", func(k string, _ Info) error {
			b, _ := mb.Get(k)
			if strings.HasSuffix(k, "verify.json") {
				keys = append(keys, k+"="+string(b))
			} else {
				keys = append(keys, k)
			}
			return nil
		})
		return vs, keys
	}
	for _, o := range []VerifyOpts{
		{AllChunks: true, Quarantine: true, Record: true},
		{Record: true},
		{Select: func(si SnapshotInfo, _ *VerifyRecord) bool { return si.Snapshot < "20261007230002" }, Quarantine: true},
	} {
		seq, seqKeys := run(1, o)
		if len(seq.Problems) == 0 {
			t.Fatal("the damage was not found: the comparison proves nothing")
		}
		for _, par := range []int{4, 32} {
			got, keys := run(par, o)
			if !reflect.DeepEqual(got, seq) {
				t.Fatalf("par=%d: verify differs:\n%+v\nsequential:\n%+v", par, got, seq)
			}
			if !reflect.DeepEqual(keys, seqKeys) {
				t.Fatalf("par=%d: the repository differs after verify", par)
			}
		}
	}
}

// Parallel GC removes exactly what the sequential one removes and reports the same counters.
func TestParallelGCIsIdenticalToSequential(t *testing.T) {
	run := func(par int) (*GCStats, []string) {
		s, _, mb := memStore(t, 0)
		buildMaintenanceRepo(t, s, mb)
		// the corrupt index makes a GC abort; repair that one for the sweep comparison
		group2 := fmt.Sprintf("%032x", 2)
		_ = s.DeleteSnapshot(group2, "20261007230001-0a0a0a01")
		s.SetRequestParallelism(par)
		st, err := s.GC(context.Background(), time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		_ = mb.List("chunks/", func(k string, _ Info) error { keys = append(keys, k); return nil })
		return st, keys
	}
	seq, seqKeys := run(1)
	if seq.Removed < 50 {
		t.Fatalf("sequential GC removed %d", seq.Removed)
	}
	for _, par := range []int{4, 32} {
		got, keys := run(par)
		if !reflect.DeepEqual(got, seq) || !reflect.DeepEqual(keys, seqKeys) {
			t.Fatalf("par=%d: %+v vs %+v", par, got, seq)
		}
	}
}

// A corrupt index still aborts a parallel GC before anything is swept.
func TestParallelGCAbortsOnCorruptIndex(t *testing.T) {
	s, _, mb := memStore(t, 0)
	buildMaintenanceRepo(t, s, mb)
	s.SetRequestParallelism(16)
	var before []string
	_ = mb.List("chunks/", func(k string, _ Info) error { before = append(before, k); return nil })
	if _, err := s.GC(context.Background(), time.Hour, false); err == nil || !strings.Contains(err.Error(), "corrupt index") {
		t.Fatalf("gc: %v", err)
	}
	var after []string
	_ = mb.List("chunks/", func(k string, _ Info) error { after = append(after, k); return nil })
	if !reflect.DeepEqual(before, after) {
		t.Fatal("an aborted GC swept chunks")
	}
}

// ---- presence set ------------------------------------------------------------------------------

func presenceStore(t *testing.T) (*Store, *countingBackend, *MemBackend) {
	t.Helper()
	s, cb, mb := memStore(t, 0)
	s.SetPresenceCache(true)
	return s, cb, mb
}

// Once the presence set is loaded, Missing answers present chunks without a request; absent ones
// are still asked.
func TestPresenceSetAnswersMissingWithoutRequests(t *testing.T) {
	s, cb, _ := presenceStore(t)
	k := testKeys(t)
	ids := putChunks(t, s, k, "x", 100)
	if err := s.RebuildPresence(); err != nil {
		t.Fatal(err)
	}
	if st := s.PresenceStats(); !st.Ready || st.Entries != 100 {
		t.Fatalf("%+v", st)
	}
	absent := randomIDs(10, 3)
	cb.reset()
	miss, err := s.Missing(append(append([]format.ID(nil), ids...), absent...))
	if err != nil || len(miss) != 10 {
		t.Fatalf("%v %v", miss, err)
	}
	if n := cb.stats.Load(); n != 10 {
		t.Fatalf("%d HEADs: only the 10 unknown ids should have been asked", n)
	}
	// negative control: with the set off every id is a request
	s.SetPresenceCache(false)
	cb.reset()
	if _, err := s.Missing(ids); err != nil {
		t.Fatal(err)
	}
	if n := cb.stats.Load(); n != 100 {
		t.Fatalf("%d HEADs with the set off", n)
	}
}

// The required property: a chunk deleted behind the server's back is found by Commit even though
// the presence set still says "present"; the refused commit evicts it, the client's retry of
// Missing reports it, and after the upload the commit succeeds.
func TestPresenceSetExternalDeletionIsCaughtAtCommit(t *testing.T) {
	for _, deleteIt := range []bool{true, false} {
		t.Run(fmt.Sprintf("deleted=%v", deleteIt), func(t *testing.T) {
			s, _, mb := presenceStore(t)
			k := testKeys(t)
			snap1, snap2 := "20261007230000-0a0a0a0a", "20261007230001-0b0b0b0b"
			idx, man, ids := buildSnapshot(t, s, k, g1, snap1, "a", "b", "c")
			if err := commit(t, s, g1, snap1, idx, man); err != nil {
				t.Fatal(err)
			}
			if err := s.RebuildPresence(); err != nil {
				t.Fatal(err)
			}
			if deleteIt {
				_ = mb.Delete(chunkKey(ids[1])) // an operator, a lifecycle rule...
			}
			sess, _ := s.BeginSession(g1, snap2)
			miss, err := sess.Missing(ids)
			if err != nil || len(miss) != 0 {
				t.Fatalf("Missing (served from the set, stale by design): %v %v", miss, err)
			}
			_ = sess.PutIndex(idx)
			_ = sess.PutManifest(man)
			_, err = sess.Commit("client", time.Time{})
			if !deleteIt {
				if err != nil {
					t.Fatalf("negative control: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrMissingChunks) {
				t.Fatalf("commit with an externally deleted chunk: %v", err)
			}
			if s.SnapshotExists(g1, snap2) {
				t.Fatal("a snapshot with a missing chunk became visible")
			}
			// the client's recovery: ask again, upload what is missing, commit again
			miss, err = sess.Missing(ids)
			if err != nil || len(miss) != 1 || miss[0] != ids[1] {
				t.Fatalf("Missing after the refused commit must report the deleted chunk: %v %v", miss, err)
			}
			_, blob, _ := k.EncodeChunk([]byte("b"), format.EncodeOpts{})
			if _, err := sess.PutChunk(ids[1], blob); err != nil {
				t.Fatal(err)
			}
			if _, err := sess.Commit("client", time.Time{}); err != nil {
				t.Fatalf("commit after re-upload: %v", err)
			}
			if err := s.CheckInvariants(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Deletions by this process (GC, quarantine) leave the set at once.
func TestPresenceSetForgetsWhatGCAndQuarantineDelete(t *testing.T) {
	s, _, mb := presenceStore(t)
	k := testKeys(t)
	ids := putChunks(t, s, k, "g", 5)
	if err := s.RebuildPresence(); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[:3] {
		mb.SetModTime(chunkKey(id), time.Now().Add(-48*time.Hour))
	}
	snap := "20261007230000-0a0a0a0a"
	idx, man, kept := buildSnapshot(t, s, k, g1, snap, "g-3", "g-4")
	if err := commit(t, s, g1, snap, idx, man); err != nil {
		t.Fatal(err)
	}
	if st, err := s.GC(context.Background(), time.Hour, false); err != nil || st.Removed != 3 {
		t.Fatalf("%+v %v", st, err)
	}
	miss, _ := s.Missing(ids)
	if len(miss) != 3 {
		t.Fatalf("after GC Missing reports %d of 3 swept chunks", len(miss))
	}
	corrupt(t, s, chunkKey(kept[0]), 30)
	if err := s.Quarantine(kept[0]); err != nil {
		t.Fatal(err)
	}
	if miss, _ := s.Missing(kept); len(miss) != 1 || miss[0] != kept[0] {
		t.Fatalf("after quarantine: %v", miss)
	}
}

// pausingList blocks the first listing of chunks/ after its first key until released.
type pausingList struct {
	Backend
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (p *pausingList) List(prefix string, fn func(string, Info) error) error {
	if prefix != "chunks/" || !p.armed.CompareAndSwap(true, false) {
		return p.Backend.List(prefix, fn)
	}
	first := true
	return p.Backend.List(prefix, func(k string, i Info) error {
		if first {
			first = false
			close(p.reached)
			<-p.release
		}
		return fn(k, i)
	})
}

// A deletion and an upload that happen while the set is being re-listed are applied to the new
// listing (a tombstone and an addition), so the rebuilt set neither keeps the deleted chunk nor
// loses the new one.
func TestPresenceSetRebuildHonoursConcurrentDeletesAndPuts(t *testing.T) {
	mb := NewMemBackend()
	pl := &pausingList{Backend: mb, reached: make(chan struct{}), release: make(chan struct{})}
	if _, err := InitBackend(pl); err != nil {
		t.Fatal(err)
	}
	s, _ := OpenBackend(pl)
	s.SetPresenceCache(true)
	k := testKeys(t)
	ids := putChunks(t, s, k, "r", 20)
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	victim := ids[len(ids)-1] // listed last: the listing still shows it after the delete
	corrupt(t, s, chunkKey(victim), 30)
	pl.armed.Store(true)
	done := make(chan error)
	go func() { done <- s.RebuildPresence() }()
	<-pl.reached
	if err := s.Quarantine(victim); err != nil { // deleted by this process during the listing
		t.Fatal(err)
	}
	newID, blob, _ := k.EncodeChunk([]byte("uploaded during the listing"), format.EncodeOpts{})
	if _, err := s.PutChunk(newID, blob); err != nil {
		t.Fatal(err)
	}
	close(pl.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.presence.has(victim) {
		t.Fatal("the rebuilt set still holds a chunk deleted during the listing")
	}
	if !s.presence.has(newID) {
		t.Fatal("the rebuilt set lost a chunk stored during the listing")
	}
	if !s.presence.has(ids[0]) || s.PresenceStats().Entries != 20 {
		t.Fatalf("rebuilt set: %+v", s.PresenceStats())
	}
}

// An answer from a request that was in flight when this process removed a chunk is not recorded.
func TestPresenceSetDropsAnswersOlderThanARemoval(t *testing.T) {
	p := presenceSet{on: true, ready: true, ids: map[format.ID]struct{}{}, max: 10, refresh: time.Hour, now: time.Now}
	id := randomIDs(1, 9)[0]
	ep := p.epoch()
	p.remove(id) // the delete starts after the HEAD was sent
	p.observed(id, ep)
	if p.has(id) {
		t.Fatal("a stale positive was recorded")
	}
	p.observed(id, p.epoch())
	if !p.has(id) {
		t.Fatal("a fresh positive was dropped")
	}
}

// Over the memory cap the set is not used and Missing asks the backend for everything.
func TestPresenceSetOverCapFallsBackToRequests(t *testing.T) {
	s, cb, _ := presenceStore(t)
	s.presence.max = 5
	k := testKeys(t)
	ids := putChunks(t, s, k, "cap", 20)
	if err := s.RebuildPresence(); !errors.Is(err, errPresenceFull) {
		t.Fatalf("rebuild over the cap: %v", err)
	}
	cb.reset()
	miss, err := s.Missing(ids)
	if err != nil || len(miss) != 0 || cb.stats.Load() != 20 {
		t.Fatalf("%v %v %d", miss, err, cb.stats.Load())
	}
}

// Missing starts the listing in the background and keeps asking the backend until it is complete.
func TestPresenceSetIsBuiltInTheBackground(t *testing.T) {
	s, cb, _ := presenceStore(t)
	k := testKeys(t)
	ids := putChunks(t, s, k, "bg", 30)
	if _, err := s.Missing(ids); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !s.PresenceStats().Ready {
		if time.Now().After(deadline) {
			t.Fatal("the background listing never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cb.reset()
	if miss, err := s.Missing(ids); err != nil || len(miss) != 0 || cb.stats.Load() != 0 {
		t.Fatalf("%v %v %d", miss, err, cb.stats.Load())
	}
}

// On an S3 service: the set is on for S3 repositories, Missing is served from it, and an
// object deleted with a plain S3 client behind the server's back is still caught by Commit.
func TestS3PresenceSetExternalDeletionCaughtAtCommit(t *testing.T) {
	svc, bucket := s3test.NewBucket(t)
	raw := s3BackendAt(t, svc.Endpoint, bucket, svc)
	if _, err := InitBackend(raw); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !s.PresenceStats().On {
		t.Fatal("the presence set is not on for an S3 repository")
	}
	k := testKeys(t)
	snap1, snap2 := "20261007230000-0a0a0a0a", "20261007230001-0b0b0b0b"
	idx, man, ids := buildSnapshot(t, s, k, g1, snap1, "s3-a", "s3-b", "s3-c")
	if err := commit(t, s, g1, snap1, idx, man); err != nil {
		t.Fatal(err)
	}
	if err := s.RebuildPresence(); err != nil {
		t.Fatal(err)
	}
	if st := s.PresenceStats(); !st.Ready || st.Entries != 3 {
		t.Fatalf("%+v", st)
	}
	if err := raw.cl().RemoveObject(context.Background(), bucket, "repo/"+chunkKey(ids[2]), minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	sess, _ := s.BeginSession(g1, snap2)
	if miss, _ := sess.Missing(ids); len(miss) != 0 {
		t.Fatalf("expected the stale answer from the set, got %v", miss)
	}
	_ = sess.PutIndex(idx)
	_ = sess.PutManifest(man)
	if _, err := sess.Commit("client", time.Time{}); !errors.Is(err, ErrMissingChunks) {
		t.Fatalf("commit: %v", err)
	}
	if miss, _ := sess.Missing(ids); len(miss) != 1 || miss[0] != ids[2] {
		t.Fatalf("retry Missing: %v", miss)
	}
	_, blob, _ := k.EncodeChunk([]byte("s3-c"), format.EncodeOpts{})
	if _, err := sess.PutChunk(ids[2], blob); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Commit("client", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// The sharded listing delivers exactly the keys (and sizes) of one recursive listing, in the same
// order, on every backend, including objects that sit directly below chunks/ and junk at any depth.
func TestShardedListingMatchesOneListing(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		putChunks(t, s, k, "list", 300)
		junk := []string{"chunks/zz/not-a-chunk", "chunks/0junk", "chunks/ab/cd/also-junk", "chunks/ff/x", "chunks/top"}
		if _, isMem := s.b.(*MemBackend); isMem {
			// an object and a "directory" of the same name: possible in S3, not on a filesystem (and
			// not on a service that stores objects as files, e.g. versitygw posix)
			junk = append(junk, "chunks/ab")
		}
		for _, j := range junk {
			if err := s.b.Put(j, []byte("junk"), false); err != nil {
				t.Fatal(err)
			}
		}
		type kv struct {
			k string
			n int64
		}
		var want, got []kv
		if err := s.b.List("chunks/", func(key string, i Info) error { want = append(want, kv{key, i.Size}); return nil }); err != nil {
			t.Fatal(err)
		}
		for _, par := range []int{1, 3, 32} {
			s.SetRequestParallelism(par)
			got = got[:0]
			if err := s.listChunkObjects(func(key string, i Info) error { got = append(got, kv{key, i.Size}); return nil }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("par=%d: sharded listing differs (%d vs %d keys)", par, len(got), len(want))
			}
		}
		if len(want) != 300+len(junk) {
			t.Fatalf("%d keys", len(want))
		}
		// an error from the callback stops the listing and is returned
		stopErr := errors.New("stop")
		n := 0
		if err := s.listChunkObjects(func(string, Info) error {
			n++
			if n == 10 {
				return stopErr
			}
			return nil
		}); !errors.Is(err, stopErr) || n != 10 {
			t.Fatalf("%v after %d keys", err, n)
		}
	})
}
