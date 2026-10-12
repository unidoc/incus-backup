package datastore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/s3test"
)

func TestMain(m *testing.M) { s3test.Main(m) }

// skewS3 lets tests make objects look old: S3 cannot be told a LastModified, so the
// age is subtracted when the repository reads it back (a Put makes it young again).
type skewS3 struct {
	*S3Backend
	mu  sync.Mutex
	age map[string]time.Duration
}

func (b *skewS3) skew(key string, info Info) Info {
	b.mu.Lock()
	defer b.mu.Unlock()
	info.ModTime = info.ModTime.Add(-b.age[key])
	return info
}
func (b *skewS3) Stat(key string) (Info, error) {
	i, err := b.S3Backend.Stat(key)
	return b.skew(key, i), err
}
func (b *skewS3) List(prefix string, fn func(string, Info) error) error {
	return b.S3Backend.List(prefix, func(k string, i Info) error { return fn(k, b.skew(k, i)) })
}
func (b *skewS3) Put(key string, data []byte, ifAbsent bool) error {
	err := b.S3Backend.Put(key, data, ifAbsent)
	if err == nil {
		b.mu.Lock()
		delete(b.age, key)
		b.mu.Unlock()
	}
	return err
}

const g1 = "0123456789abcdef0123456789abcdef"

func testKeys(t *testing.T) *format.Keys {
	t.Helper()
	m := make([]byte, 32)
	for i := range m {
		m[i] = byte(100 + i)
	}
	k, err := format.DeriveKeys(m)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// backends is every Backend the whole suite runs against. The in-memory one has
// strict object-store semantics (no rename, no directories): it is what keeps the
// repository logic free of POSIX assumptions.
func backends(t *testing.T) map[string]func(t *testing.T) *Store {
	return map[string]func(*testing.T) *Store{
		"fs": func(t *testing.T) *Store {
			root := filepath.Join(t.TempDir(), "ds")
			if _, err := Init(root); err != nil {
				t.Fatal(err)
			}
			s, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"mem": func(t *testing.T) *Store {
			b := NewMemBackend()
			if _, err := InitBackend(b); err != nil {
				t.Fatal(err)
			}
			s, err := OpenBackend(b)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		// the in-memory backend with the presence set switched on (as on S3), loaded eagerly
		"mem-presence": func(t *testing.T) *Store {
			b := NewMemBackend()
			if _, err := InitBackend(b); err != nil {
				t.Fatal(err)
			}
			s, err := OpenBackend(b)
			if err != nil {
				t.Fatal(err)
			}
			s.SetPresenceCache(true)
			if err := s.RebuildPresence(); err != nil {
				t.Fatal(err)
			}
			return s
		},
		"s3": func(t *testing.T) *Store {
			svc, bucket := s3test.NewBucket(t)
			b, err := NewS3Backend(S3Config{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: "repo", AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: svc.NoTLS, PathStyle: true})
			if err != nil {
				t.Fatal(err)
			}
			sb := &skewS3{S3Backend: b, age: map[string]time.Duration{}}
			if _, err := InitBackend(sb); err != nil {
				t.Fatal(err)
			}
			s, err := OpenBackend(sb)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
}

// each runs fn once per backend.
func each(t *testing.T, fn func(t *testing.T, s *Store)) {
	for name, mk := range backends(t) {
		mk := mk
		t.Run(name, func(t *testing.T) { fn(t, mk(t)) })
	}
}

// backdate makes an object look old (GC grace).
func backdate(t *testing.T, s *Store, key string, at time.Time) {
	t.Helper()
	switch b := s.b.(type) {
	case *FSBackend:
		p, _ := b.path(key)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	case *MemBackend:
		b.SetModTime(key, at)
	case *skewS3:
		b.mu.Lock()
		b.age[key] = time.Since(at)
		b.mu.Unlock()
	default:
		t.Fatalf("cannot backdate on %T", b)
	}
}

func corrupt(t *testing.T, s *Store, key string, at int) {
	t.Helper()
	data, err := s.b.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	data[at] ^= 0xff
	if err := s.b.Put(key, data, false); err != nil {
		t.Fatal(err)
	}
}

func buildSnapshot(t *testing.T, s *Store, k *format.Keys, group, snap string, parts ...string) (idx, man []byte, ids []format.ID) {
	t.Helper()
	x := &format.Index{Kind: format.IndexDynamic}
	var off uint64
	for _, p := range parts {
		id, blob, err := k.EncodeChunk([]byte(p), format.EncodeOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutChunk(id, blob); err != nil {
			t.Fatal(err)
		}
		off += uint64(len(p))
		x.Entries = append(x.Entries, format.Entry{End: off, ID: id})
		ids = append(ids, id)
	}
	x.Size = off
	idx, err := x.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m := &format.Manifest{SnapshotID: snap, Created: time.Now().UTC(), Object: format.ObjectInfo{Name: "o", Kind: "filesystem", Size: off}, IndexDigest: format.IndexDigest(idx)}
	man, err = k.SealManifest(m, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	return idx, man, ids
}

func commit(t *testing.T, s *Store, group, snap string, idx, man []byte) error {
	t.Helper()
	sess, err := s.BeginSession(group, snap)
	if err != nil {
		return err
	}
	if err := sess.PutIndex(idx); err != nil {
		return err
	}
	if err := sess.PutManifest(man); err != nil {
		return err
	}
	_, err = sess.Commit("client", time.Time{})
	return err
}

func chunkExists(s *Store, id format.ID) bool { return s.HasChunk(id) }

func TestPutChunkVerifiesAndDedups(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		id, blob, _ := k.EncodeChunk([]byte("hello"), format.EncodeOpts{})
		created, err := s.PutChunk(id, blob)
		if err != nil || !created {
			t.Fatalf("first put: %v %v", created, err)
		}
		created, err = s.PutChunk(id, blob)
		if err != nil || created {
			t.Fatalf("second put should be a no-op: %v %v", created, err)
		}
		bad := append([]byte{}, blob...)
		bad[12] ^= 1
		id2 := k.ChunkID([]byte("other"))
		if _, err := s.PutChunk(id2, bad); err == nil {
			t.Fatal("corrupt blob accepted")
		}
		if chunkExists(s, id2) {
			t.Fatal("corrupt blob left behind")
		}
		if _, err := s.PutChunk(format.ZeroID, blob); err == nil {
			t.Fatal("zero id accepted")
		}
		got, err := s.GetChunk(id)
		if err != nil || !bytes.Equal(got, blob) {
			t.Fatal("get mismatch")
		}
		miss, _ := s.Missing([]format.ID{id, id2, format.ZeroID})
		if len(miss) != 1 || miss[0] != id2 {
			t.Fatalf("missing=%v", miss)
		}
	})
}

func TestCommitListRead(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, _ := buildSnapshot(t, s, k, g1, snap, "aaa", "bbb")
		if err := commit(t, s, g1, snap, idx, man); err != nil {
			t.Fatal(err)
		}
		l, err := s.ListSnapshots(g1)
		if err != nil || len(l) != 1 || l[0].Snapshot != snap || l[0].Receipt.ChunkCount != 2 || l[0].Receipt.IndexDigest != format.IndexDigest(idx) {
			t.Fatalf("list: %v %+v", err, l)
		}
		gi, _ := s.ReadIndex(g1, snap)
		gm, _ := s.ReadManifest(g1, snap)
		if !bytes.Equal(gi, idx) || !bytes.Equal(gm, man) {
			t.Fatal("stored files differ")
		}
		if err := commit(t, s, g1, snap, idx, man); !errors.Is(err, ErrExists) {
			t.Fatalf("duplicate commit: %v", err)
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginSession("../../etc", snap); err == nil {
			t.Fatal("path traversal in group accepted")
		}
		if _, err := s.ReadIndex(g1, "../x"); err == nil {
			t.Fatal("path traversal in snapshot accepted")
		}
		if gs, _ := s.ListGroups(); len(gs) != 1 || gs[0] != g1 {
			t.Fatalf("groups %v", gs)
		}
	})
}

func TestCommitRefusesMissingChunks(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "aaa", "bbb")
		if err := s.b.Delete(chunkKey(ids[1])); err != nil {
			t.Fatal(err)
		}
		if err := commit(t, s, g1, snap, idx, man); !errors.Is(err, ErrMissingChunks) {
			t.Fatalf("want ErrMissingChunks, got %v", err)
		}
		if l, _ := s.ListSnapshots(g1); len(l) != 0 {
			t.Fatal("snapshot visible despite refusal")
		}
		if _, err := s.ReadIndex(g1, snap); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an uncommitted snapshot must not be readable: %v", err)
		}
	})
}

// Crash at every named point. A restarted store must satisfy the invariants and show
// either nothing or the whole snapshot; a retry must then succeed.
func TestCrashInjectionEveryCommitPoint(t *testing.T) {
	for _, point := range []string{"chunk:before-write", "commit:before-marker", "commit:after-marker"} {
		point := point
		t.Run(point, func(t *testing.T) {
			each(t, func(t *testing.T, s *Store) {
				k := testKeys(t)
				snap := "20261007230000-0a0a0a0a"
				s.SetFault(func(p string) error {
					if p == point {
						return errors.New("injected crash at " + p)
					}
					return nil
				})
				id, blob, _ := k.EncodeChunk([]byte("payload"), format.EncodeOpts{})
				x := &format.Index{Kind: format.IndexDynamic, Size: 7, Entries: []format.Entry{{End: 7, ID: id}}}
				idx, _ := x.Marshal()
				m := &format.Manifest{SnapshotID: snap, Created: time.Now().UTC(), Object: format.ObjectInfo{Name: "o", Kind: "filesystem", Size: 7}, IndexDigest: format.IndexDigest(idx)}
				man, _ := k.SealManifest(m, g1, nil)
				failed := false
				if _, err := s.PutChunk(id, blob); err != nil {
					failed = true
				} else if err := commit(t, s, g1, snap, idx, man); err != nil {
					failed = true
				}
				if !failed {
					t.Fatalf("fault %s did not trigger", point)
				}
				s.SetFault(nil) // "restart"
				if err := s.CheckInvariants(); err != nil {
					t.Fatalf("invariants broken after crash at %s: %v", point, err)
				}
				l, _ := s.ListSnapshots(g1)
				if point == "commit:after-marker" {
					if len(l) != 1 {
						t.Fatalf("the marker was written: the snapshot must be visible, got %d", len(l))
					}
					return
				}
				if len(l) != 0 {
					t.Fatalf("partial snapshot visible after crash at %s", point)
				}
				if _, err := s.PutChunk(id, blob); err != nil {
					t.Fatal(err)
				}
				if err := commit(t, s, g1, snap, idx, man); err != nil {
					t.Fatalf("retry after crash: %v", err)
				}
				if l, _ := s.ListSnapshots(g1); len(l) != 1 {
					t.Fatal("retry did not publish the snapshot")
				}
			})
		})
	}
}

// Negative control: the invariant checker catches a visible-but-incomplete snapshot,
// and uncommitted debris is invisible, never listed and cleaned up later.
func TestInvariantCheckerAndOrphans(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		prefix := snapPrefix(g1, "20261007230000-0a0a0a0a")
		_ = s.b.Put(prefix+"index", []byte("junk"), false) // debris of a crashed upload
		if err := s.CheckInvariants(); err != nil {
			t.Fatalf("uncommitted debris must be invisible, not a violation: %v", err)
		}
		if l, _ := s.ListSnapshots(g1); len(l) != 0 {
			t.Fatal("debris listed as a snapshot")
		}
		vs, _ := s.Verify(context.Background())
		if len(vs.Problems) != 0 {
			t.Fatalf("debris must not be reported as a corrupt snapshot: %+v", vs.Problems)
		}
		// a visible snapshot that lacks its manifest IS a violation
		_ = s.b.Put(prefix+"commit.json", []byte(`{"committed":"2026-10-07T23:00:00Z"}`), true)
		if err := s.CheckInvariants(); err == nil {
			t.Fatal("checker accepted a visible snapshot without a valid index/manifest")
		}
		if vs, _ := s.Verify(context.Background()); len(vs.Problems) == 0 {
			t.Fatal("verify missed it")
		}
	})
	each(t, func(t *testing.T, s *Store) {
		prefix := snapPrefix(g1, "20261007230001-0b0b0b0b")
		_ = s.b.Put(prefix+"index", []byte("debris"), false)
		backdate(t, s, prefix+"index", time.Now().Add(-48*time.Hour))
		if n, err := s.CleanOrphans(24 * time.Hour); err != nil || n != 1 {
			t.Fatalf("orphan cleanup: %d %v", n, err)
		}
		if _, err := s.b.Stat(prefix + "index"); !errors.Is(err, ErrNotFound) {
			t.Fatal("orphan not removed")
		}
	})
}

func TestGCRemovesOnlyUnreferencedOldChunks(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "keep-a", "keep-b")
		if err := commit(t, s, g1, snap, idx, man); err != nil {
			t.Fatal(err)
		}
		oid, oblob, _ := k.EncodeChunk([]byte("orphan-old"), format.EncodeOpts{})
		_, _ = s.PutChunk(oid, oblob)
		yid, yblob, _ := k.EncodeChunk([]byte("orphan-young"), format.EncodeOpts{})
		_, _ = s.PutChunk(yid, yblob)
		old := time.Now().Add(-48 * time.Hour)
		backdate(t, s, chunkKey(oid), old)
		for _, id := range ids {
			backdate(t, s, chunkKey(id), old)
		}
		dry, err := s.GC(context.Background(), 24*time.Hour, true)
		if err != nil || dry.Removed != 1 || !chunkExists(s, oid) {
			t.Fatalf("dry run: %v %+v", err, dry)
		}
		st, err := s.GC(context.Background(), 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		if st.Removed != 1 || st.KeptYoung != 1 || st.Live != 2 {
			t.Fatalf("stats %+v", st)
		}
		if chunkExists(s, oid) || !chunkExists(s, yid) || !chunkExists(s, ids[0]) || !chunkExists(s, ids[1]) {
			t.Fatal("GC removed the wrong chunks")
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
	})
}

// A chunk an open session relies on ("present" from Missing, or uploaded) is pinned: GC keeps
// it however old, with no write to the object (no touch, no COPY).
func TestGCPinsChunksOfOpenSessions(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		id, blob, _ := k.EncodeChunk([]byte("reused"), format.EncodeOpts{})
		_, _ = s.PutChunk(id, blob)
		backdate(t, s, chunkKey(id), time.Now().Add(-72*time.Hour))
		sess, err := s.BeginSession(g1, "20261007230000-0a0a0a0a")
		if err != nil {
			t.Fatal(err)
		}
		if miss, _ := sess.Missing([]format.ID{id}); len(miss) != 0 {
			t.Fatal("chunk should be present")
		}
		st, err := s.GC(context.Background(), 24*time.Hour, false)
		if err != nil || st.Removed != 0 || st.KeptPinned != 1 || !chunkExists(s, id) {
			t.Fatalf("GC swept a chunk a running session relies on: %+v %v", st, err)
		}
		// session ends without committing: the pin is gone, the old orphan is collectable
		sess.Close()
		st, err = s.GC(context.Background(), 24*time.Hour, false)
		if err != nil || st.Removed != 1 || chunkExists(s, id) {
			t.Fatalf("after the session ended: %+v %v", st, err)
		}
	})
}

// Race: a snapshot commits AFTER GC marked, referencing chunks that were not pinned by any
// session (they were only stat'ed at commit). GC must not sweep them.
func TestGCDoesNotSweepChunksOfASnapshotCommittedAfterTheMark(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "late-a", "late-b")
		old := time.Now().Add(-72 * time.Hour)
		for _, id := range ids {
			backdate(t, s, chunkKey(id), old)
		}
		var cerr error
		s.SetFault(func(p string) error {
			if p == "gc:after-mark" {
				s.SetFault(nil)
				cerr = commit(t, s, g1, snap, idx, man) // lands between mark and sweep
			}
			return nil
		})
		st, err := s.GC(context.Background(), 24*time.Hour, false)
		if err != nil || cerr != nil {
			t.Fatalf("gc=%v commit=%v", err, cerr)
		}
		if st.Removed != 0 || st.KeptPinned != 2 {
			t.Fatalf("GC swept chunks of a snapshot committed during the run: %+v", st)
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOnlyOneGCRunsAtATime(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		if err := s.beginGC(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GC(context.Background(), time.Hour, true); !errors.Is(err, ErrGCRunning) {
			t.Fatalf("second GC: %v", err)
		}
		s.endGC()
		if _, err := s.GC(context.Background(), time.Hour, true); err != nil {
			t.Fatal(err)
		}
	})
}

// Chunks of an upload that has not committed yet (no marker) are protected by the grace
// period, not by being referenced.
func TestGCKeepsChunksOfUncommittedUploadWithinGrace(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		_, _, ids := buildSnapshot(t, s, k, g1, "20261007230000-0a0a0a0a", "inflight")
		st, err := s.GC(context.Background(), 24*time.Hour, false)
		if err != nil || st.Removed != 0 || !chunkExists(s, ids[0]) {
			t.Fatalf("GC removed a chunk of an in-flight upload: %+v %v", st, err)
		}
	})
}

func TestGCAbortsOnCorruptCommittedIndex(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "x")
		_ = commit(t, s, g1, snap, idx, man)
		backdate(t, s, chunkKey(ids[0]), time.Now().Add(-72*time.Hour))
		_ = s.b.Put(snapPrefix(g1, snap)+"index", []byte("corrupt"), false)
		if _, err := s.GC(context.Background(), time.Hour, false); err == nil {
			t.Fatal("GC must abort rather than sweep when it cannot read a live index")
		}
		if !chunkExists(s, ids[0]) {
			t.Fatal("chunk lost")
		}
	})
}

func TestVerifyFindsCorruptionAndMissing(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "one", "two")
		_ = commit(t, s, g1, snap, idx, man)
		vs, err := s.Verify(context.Background())
		if err != nil || len(vs.Problems) != 0 || vs.Chunks != 2 || vs.Snapshots != 1 {
			t.Fatalf("clean store: %v %+v", err, vs)
		}
		corrupt(t, s, chunkKey(ids[0]), 20)
		_ = s.b.Delete(chunkKey(ids[1]))
		vs, _ = s.Verify(context.Background())
		kinds := map[string]bool{}
		for _, p := range vs.Problems {
			kinds[p.Kind] = true
		}
		if !kinds["chunk-corrupt"] || !kinds["chunk-missing"] {
			t.Fatalf("problems=%+v", vs.Problems)
		}
	})
}

// The commit marker carries the digests of index and manifest: swapping either afterwards is detected.
func TestVerifyDetectsReplacedIndexOrManifest(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, _ := buildSnapshot(t, s, k, g1, snap, "a")
		_ = commit(t, s, g1, snap, idx, man)
		idx2, _, _ := buildSnapshot(t, s, k, g1, "20261007230001-0b0b0b0b", "different")
		_ = s.b.Put(snapPrefix(g1, snap)+"index", idx2, false)
		vs, _ := s.Verify(context.Background())
		found := false
		for _, p := range vs.Problems {
			found = found || p.Kind == "index-corrupt"
		}
		if !found {
			t.Fatalf("a replaced index went unnoticed: %+v", vs.Problems)
		}
	})
}

func TestDeleteSnapshotAndUsage(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, _ := buildSnapshot(t, s, k, g1, snap, "z")
		_ = commit(t, s, g1, snap, idx, man)
		if b, c, err := s.Usage(); err != nil || c != 1 || b == 0 {
			t.Fatalf("usage %d %d %v", b, c, err)
		}
		if err := s.DeleteSnapshot(g1, snap); err != nil {
			t.Fatal(err)
		}
		if l, _ := s.ListSnapshots(g1); len(l) != 0 {
			t.Fatal("still listed")
		}
		if err := s.DeleteSnapshot(g1, snap); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
		// the same snapshot id can be committed again (e.g. a re-sync)
		if err := commit(t, s, g1, snap, idx, man); err != nil {
			t.Fatalf("recommit: %v", err)
		}
	})
}

func TestBackendKeyValidation(t *testing.T) {
	for _, bad := range []string{"", "/abs", "a//b", "a/../b", "../x", "a/./b"} {
		if validKey(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	each(t, func(t *testing.T, s *Store) {
		if err := s.b.Put("../escape", []byte("x"), false); err == nil {
			t.Fatal("a path-traversal key was accepted by a backend")
		}
	})
}

// Concurrent commits of the same snapshot: exactly one wins, the loser gets ErrExists.
func TestExactlyOneConcurrentCommitWins(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, _ := buildSnapshot(t, s, k, g1, snap, "race")
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			go func() { errs <- commit(t, s, g1, snap, idx, man) }()
		}
		ok, exists := 0, 0
		for i := 0; i < 8; i++ {
			switch err := <-errs; {
			case err == nil:
				ok++
			case errors.Is(err, ErrExists):
				exists++
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		if ok != 1 || exists != 7 {
			t.Fatalf("winners=%d losers=%d", ok, exists)
		}
	})
}

// Corrupt chunks are quarantined: they stop answering "present", the next upload of the same
// plaintext heals them, and the bad blob is kept aside until it is purged.
func TestQuarantineStopsDedupAndAllowsHealing(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, "heal-me", "fine")
		if err := commit(t, s, g1, snap, idx, man); err != nil {
			t.Fatal(err)
		}
		corrupt(t, s, chunkKey(ids[0]), 20)
		vs, err := s.VerifyWith(context.Background(), VerifyOpts{Quarantine: true, Record: true, AllChunks: true})
		if err != nil || vs.Quarantined != 1 {
			t.Fatalf("%v %+v", err, vs)
		}
		if chunkExists(s, ids[0]) {
			t.Fatal("corrupt chunk still answers present")
		}
		if miss, _ := s.Missing(ids); len(miss) != 1 || miss[0] != ids[0] {
			t.Fatalf("dedup check must now report the chunk missing: %v", miss)
		}
		q, _ := s.ListQuarantine()
		if len(q) != 1 || q[0].ID != ids[0] || q[0].Healed {
			t.Fatalf("quarantine: %+v", q)
		}
		rec, _ := s.ReadVerify(g1, snap)
		if rec == nil || rec.State != VerifyFailed {
			t.Fatalf("snapshot must be recorded as failed: %+v", rec)
		}
		// heal: the same plaintext is uploaded again
		_, blob, _ := k.EncodeChunk([]byte("heal-me"), format.EncodeOpts{})
		if created, err := s.PutChunk(ids[0], blob); err != nil || !created {
			t.Fatalf("heal put: %v %v", created, err)
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
		q, _ = s.ListQuarantine()
		if len(q) != 1 || !q[0].Healed {
			t.Fatalf("quarantine entry should be marked healed: %+v", q)
		}
		if n, _ := s.PurgeQuarantine(-time.Hour); n != 1 {
			t.Fatalf("purge removed %d", n)
		}
		// a re-verify clears the failed state
		vs, _ = s.VerifyWith(context.Background(), VerifyOpts{Record: true})
		rec, _ = s.ReadVerify(g1, snap)
		if len(vs.Problems) != 0 || rec.State != VerifyOK || len(rec.History) != 2 {
			t.Fatalf("after healing: %+v %+v", vs, rec)
		}
	})
}

// Quarantine must never remove a chunk that has been healed concurrently.
func TestQuarantineLeavesAHealthyChunkAlone(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		id, blob, _ := k.EncodeChunk([]byte("ok"), format.EncodeOpts{})
		_, _ = s.PutChunk(id, blob)
		if err := s.Quarantine(id); err != nil {
			t.Fatal(err)
		}
		if !chunkExists(s, id) {
			t.Fatal("a healthy chunk was quarantined")
		}
	})
}

// A verify job reads each chunk once, however many snapshots share it.
func TestVerifyReadsEachChunkOncePerJobAndSelectsSnapshots(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		for i, snap := range []string{"20261007230000-0a0a0a0a", "20261007230001-0b0b0b0b", "20261007230002-0c0c0c0c"} {
			idx, man, _ := buildSnapshot(t, s, k, g1, snap, "shared-1", "shared-2", fmt.Sprintf("own-%d", i))
			if err := commit(t, s, g1, snap, idx, man); err != nil {
				t.Fatal(err)
			}
		}
		vs, err := s.VerifyWith(context.Background(), VerifyOpts{Record: true})
		if err != nil || vs.Snapshots != 3 || vs.Chunks != 5 {
			t.Fatalf("5 distinct chunks must be read once each: %v %+v", err, vs)
		}
		// verify-new: nothing is unverified any more
		onlyNew := func(si SnapshotInfo, rec *VerifyRecord) bool { return rec == nil }
		vs, _ = s.VerifyWith(context.Background(), VerifyOpts{Select: onlyNew, Record: true})
		if vs.Snapshots != 0 || vs.Skipped != 3 || vs.Chunks != 0 {
			t.Fatalf("verify-new re-read verified snapshots: %+v", vs)
		}
		// re-verify after N days: age the newest record
		reverify := func(si SnapshotInfo, rec *VerifyRecord) bool {
			return rec == nil || s.now().Sub(rec.At) > 30*24*time.Hour
		}
		s.now = func() time.Time { return time.Now().Add(40 * 24 * time.Hour) }
		vs, _ = s.VerifyWith(context.Background(), VerifyOpts{Select: reverify, Record: true})
		if vs.Snapshots != 3 {
			t.Fatalf("snapshots older than 30 days must be re-verified: %+v", vs)
		}
	})
}

func TestProtectedSnapshotsNotesAndVerifyStateLiveInTheRepository(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		idx, man, _ := buildSnapshot(t, s, k, g1, snap, "p")
		_ = commit(t, s, g1, snap, idx, man)
		if err := s.SetProtected(g1, snap, true); err != nil {
			t.Fatal(err)
		}
		if err := s.SetNotes(g1, snap, "before the upgrade"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetGroupNotes(g1, "bosch"); err != nil {
			t.Fatal(err)
		}
		_ = s.RecordVerify(g1, snap, VerifyEvent{At: time.Now(), State: VerifyOK, Chunks: 1})
		l, _ := s.ListSnapshots(g1)
		if len(l) != 1 || !l[0].Protected || !l[0].HasNotes || !l[0].HasVerify {
			t.Fatalf("%+v", l)
		}
		if n, _ := s.Notes(g1, snap); n != "before the upgrade" {
			t.Fatal(n)
		}
		if err := s.DeleteSnapshot(g1, snap); !errors.Is(err, ErrProtected) {
			t.Fatalf("a protected snapshot was deleted: %v", err)
		}
		if err := s.SetProtected(g1, snap, false); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSnapshot(g1, snap); err != nil {
			t.Fatal(err)
		}
		// nothing of it is left behind
		if err := s.b.List("groups/"+g1+"/"+snap+"/", func(k string, _ Info) error { t.Fatalf("leftover %s", k); return nil }); err != nil {
			t.Fatal(err)
		}
		if err := s.SetProtected(g1, snap, true); !errors.Is(err, ErrNotFound) {
			t.Fatalf("protecting a missing snapshot: %v", err)
		}
	})
}

func TestOwnershipStopsASecondServerAdoptingTheRepository(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		b := s.Backend()
		if err := ClaimOwner(b, "server-A", "cikarang", false); err != nil {
			t.Fatal(err)
		}
		if err := ClaimOwner(b, "server-A", "cikarang", false); err != nil {
			t.Fatalf("the owner reopening: %v", err)
		}
		if _, err := OpenOwned(b, "server-B", "amsterdam", false); !errors.Is(err, ErrForeignOwner) {
			t.Fatalf("a foreign server opened the repository: %v", err)
		}
		if _, err := OpenOwned(b, "server-B", "amsterdam", true); err != nil {
			t.Fatalf("explicit adoption: %v", err)
		}
		o, _ := ReadOwner(b)
		if o.ServerID != "server-B" || o.Previous != "server-A" {
			t.Fatalf("%+v", o)
		}
		if _, err := OpenOwned(b, "server-A", "cikarang", false); !errors.Is(err, ErrForeignOwner) {
			t.Fatal("the old owner must now be refused")
		}
	})
}

// Two servers racing to claim a fresh repository: exactly one wins.
func TestOwnershipClaimRaceHasOneWinner(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		errs := make(chan error, 6)
		for i := 0; i < 6; i++ {
			id := fmt.Sprintf("server-%d", i)
			go func() { errs <- ClaimOwner(s.Backend(), id, "h", false) }()
		}
		won := 0
		for i := 0; i < 6; i++ {
			if err := <-errs; err == nil {
				won++
			} else if !errors.Is(err, ErrForeignOwner) {
				t.Fatal(err)
			}
		}
		if won != 1 {
			t.Fatalf("%d servers own the repository", won)
		}
	})
}
