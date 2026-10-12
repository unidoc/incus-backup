package datastore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/memprobe"
)

// fastFS is the filesystem backend without fsync, to fabricate large repositories quickly. The
// layout and every read path are the real FSBackend's.
type fastFS struct{ *FSBackend }

func (b fastFS) Put(key string, data []byte, ifAbsent bool) error {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	if ifAbsent {
		if _, err := os.Stat(p); err == nil {
			return ErrExists
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o640)
}

// fabricate builds a repository of `chunks` small real chunk blobs: 80 % are referenced by
// snapshots of 4 groups (1000 entries each, consecutive snapshots of a group share half their
// chunks, as successive backups of one object do), 20 % are garbage. It returns the store and the
// number of snapshots and of referenced chunks.
func fabricate(t *testing.T, chunks int) (*Store, int, int) {
	t.Helper()
	fb, err := NewFSBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := fastFS{fb}
	if _, err := InitBackend(b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(b)
	if err != nil {
		t.Fatal(err)
	}
	k := testKeys(t)
	ids := make([]format.ID, chunks)
	var pt [16]byte
	for i := range ids {
		binary.LittleEndian.PutUint64(pt[:], uint64(i))
		id, blob, err := k.EncodeChunk(pt[:], format.EncodeOpts{NoCompress: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutChunk(id, blob); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	groups := 4
	per := chunks * 8 / 10 / groups
	live := ids[:per*groups]
	snaps := 0
	for g := 0; g < groups; g++ {
		group := fmt.Sprintf("%032x", g+1)
		mine := live[g*per : (g+1)*per]
		for start, n := 0, 0; start < len(mine); start, n = start+500, n+1 {
			end := min(start+1000, len(mine))
			x := &format.Index{Kind: format.IndexDynamic}
			for i, id := range mine[start:end] {
				x.Entries = append(x.Entries, format.Entry{End: uint64(i+1) * 16, ID: id})
			}
			x.Size = uint64(len(x.Entries)) * 16
			ib, err := x.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			snap := fmt.Sprintf("2026%010d-%08x", n, g)
			m := &format.Manifest{SnapshotID: snap, Created: time.Now().UTC(), Object: format.ObjectInfo{Name: "o", Kind: "filesystem", Size: x.Size}, IndexDigest: format.IndexDigest(ib)}
			mb, err := k.SealManifest(m, group, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := commit(t, s, group, snap, ib, mb); err != nil {
				t.Fatal(err)
			}
			snaps++
			if end == len(mine) {
				break
			}
		}
	}
	return s, snaps, len(live)
}

// TestMaintenanceMemoryPerChunk measures GC and verification of fabricated repositories at
// several sizes; the cost per chunk is the slope between the sizes (fixed costs cancel out). GC
// keeps 8 bytes per live chunk (plus map overhead) and sweeps in batches; verification keeps
// every chunk state up to VerifyCacheMax (then two bounded generations).
func TestMaintenanceMemoryPerChunk(t *testing.T) {
	sizes := memprobe.Sizes("IB_MEMBENCH_CHUNKS", []float64{20000, 80000})
	ctx := context.Background()
	type m struct{ gc, verify, pins memprobe.Result }
	var got []m
	var rows []string
	for _, sz := range sizes {
		n := int(sz)
		s, snaps, nlive := fabricate(t, n)
		s.now = func() time.Time { return time.Now().Add(72 * time.Hour) } // every chunk is past the grace
		label := fmt.Sprintf("%d chunks, %d snapshots", n, snaps)

		// a backup session that asks about every chunk pins all of them until it ends
		var all []format.ID
		_ = s.b.List("chunks/", func(key string, _ Info) error {
			id, err := format.ParseID(key[strings.LastIndex(key, "/")+1:])
			if err == nil {
				all = append(all, id)
			}
			return nil
		})
		sess, err := s.BeginSession(fmt.Sprintf("%032x", 99), "20261008000000-00000000")
		if err != nil {
			t.Fatal(err)
		}
		p := memprobe.StartTight("session pins")
		for i := 0; i < len(all); i += 2000 {
			if _, err := sess.Missing(all[i:min(i+2000, len(all))]); err != nil {
				t.Fatal(err)
			}
		}
		rp := p.Stop()
		sess.Abort()
		all = nil

		p = memprobe.StartTight("verify (all chunks)")
		vs, err := s.VerifyWith(ctx, VerifyOpts{AllChunks: true})
		rv := p.Stop()
		if err != nil || len(vs.Problems) != 0 || vs.Snapshots != snaps {
			t.Fatalf("verify: %v %d problems, %d of %d snapshots", err, len(vs.Problems), vs.Snapshots, snaps)
		}
		p = memprobe.StartTight("gc")
		st, err := s.GC(ctx, 24*time.Hour, false)
		rg := p.Stop()
		if err != nil {
			t.Fatal(err)
		}
		if want := n - nlive; st.Removed != want || st.Live != nlive {
			t.Fatalf("gc removed %d (want %d), live %d: %+v", st.Removed, want, st.Live, st)
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
		got = append(got, m{rg, rv, rp})
		rows = append(rows, rp.Row("repository", label), rv.Row("repository", label), rg.Row("repository", label))
	}
	if len(got) >= 2 {
		a, b := got[0], got[len(got)-1]
		dn := sizes[len(sizes)-1] - sizes[0]
		for _, x := range []struct {
			name   string
			r0, r1 memprobe.Result
			bound  float64
		}{{"gc", a.gc, b.gc, 120}, {"verify (all chunks)", a.verify, b.verify, 180}, {"session pins", a.pins, b.pins, 120}} {
			per := (float64(x.r1.HeapDelta) - float64(x.r0.HeapDelta)) / dn
			rows = append(rows, fmt.Sprintf("| repository | slope %g..%g chunks | %s | | %.0f B/chunk | | | | |", sizes[0], sizes[len(sizes)-1], x.name, per))
			if per > x.bound {
				t.Errorf("%s keeps %.0f bytes per chunk, bound %.0f", x.name, per, x.bound)
			}
		}
	}
	t.Log("\n" + memprobe.Header + "\n" + strings.Join(rows, "\n"))
	memprobe.Report(rows...)
}

// The GC sweep runs in batches during the listing: a repository with more garbage than one batch
// still loses exactly its garbage, and the after-mark fault point fires once, before any delete.
func TestGCSweepsInBatches(t *testing.T) {
	old := gcSweepBatch
	gcSweepBatch = 64
	defer func() { gcSweepBatch = old }()
	s, _, nlive := fabricate(t, 3*gcSweepBatch)
	s.now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	marks := 0
	s.SetFault(func(p string) error {
		if p == "gc:after-mark" {
			marks++
		}
		return nil
	})
	st, err := s.GC(context.Background(), 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 3 * gcSweepBatch
	if st.Removed != n-nlive || st.Live != nlive || marks != 1 {
		t.Fatalf("stats %+v, after-mark fired %d times", st, marks)
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	// an error at the fault point stops GC before anything is deleted
	s2, _, _ := fabricate(t, 2*gcSweepBatch)
	s2.now = s.now
	stop := errors.New("stop")
	s2.SetFault(func(p string) error {
		if p == "gc:after-mark" {
			return stop
		}
		return nil
	})
	if _, err := s2.GC(context.Background(), 24*time.Hour, false); !errors.Is(err, stop) {
		t.Fatalf("gc: %v", err)
	}
	_, chunks, _ := s2.Usage()
	if chunks != int64(2*gcSweepBatch) {
		t.Fatalf("%d chunks left, nothing may be deleted before the mark completes", chunks)
	}
}

// A verification whose cache bound is far below the repository size still finds a corrupt chunk
// and marks every snapshot that references it.
func TestVerifyWithASmallCacheStillFindsCorruption(t *testing.T) {
	old := VerifyCacheMax
	VerifyCacheMax = 64
	defer func() { VerifyCacheMax = old }()
	s, snaps, _ := fabricate(t, 5000)
	ids, err := s.ChunkIDsOf(fmt.Sprintf("%032x", 1), "20260000000000-00000000")
	if err != nil || len(ids) < 11 {
		t.Fatal(err)
	}
	corrupt(t, s, chunkKey(ids[10]), 40) // referenced by the first snapshot of the group only
	vs, err := s.VerifyWith(context.Background(), VerifyOpts{AllChunks: true})
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, r := range vs.Results {
		if r.State == VerifyFailed {
			failed++
		}
	}
	if len(vs.Results) != snaps || failed != 1 {
		t.Fatalf("%d results (want %d), %d failed (want 1): %+v", len(vs.Results), snaps, failed, vs.Problems)
	}
	corruptSeen := 0
	for _, p := range vs.Problems {
		if p.Kind == "chunk-corrupt" {
			corruptSeen++
		}
	}
	if corruptSeen != 1 {
		t.Fatalf("the corrupt chunk was reported %d times: %+v", corruptSeen, vs.Problems)
	}
}

// The batched sweep deletes while the listing goes on: on every backend (the S3 listing is
// paginated, the in-memory one has strict object-store semantics) exactly the garbage goes, and
// every chunk a committed snapshot references stays.
func TestGCSweepsInBatchesOnEveryBackend(t *testing.T) {
	old := gcSweepBatch
	gcSweepBatch = 4
	defer func() { gcSweepBatch = old }()
	each(t, func(t *testing.T, s *Store) {
		k := testKeys(t)
		snap := "20261007230000-0a0a0a0a"
		var parts []string
		for i := 0; i < 10; i++ {
			parts = append(parts, fmt.Sprintf("live-%02d", i))
		}
		idx, man, ids := buildSnapshot(t, s, k, g1, snap, parts...)
		if err := commit(t, s, g1, snap, idx, man); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-72 * time.Hour)
		for _, id := range ids {
			backdate(t, s, chunkKey(id), at)
		}
		var garbage []format.ID
		for i := 0; i < 21; i++ {
			id, blob, _ := k.EncodeChunk([]byte(fmt.Sprintf("garbage-%02d", i)), format.EncodeOpts{})
			if _, err := s.PutChunk(id, blob); err != nil {
				t.Fatal(err)
			}
			backdate(t, s, chunkKey(id), at)
			garbage = append(garbage, id)
		}
		st, err := s.GC(context.Background(), 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		if st.Removed != len(garbage) || st.Live != len(ids) {
			t.Fatalf("stats %+v", st)
		}
		for _, id := range garbage {
			if s.HasChunk(id) {
				t.Fatalf("garbage chunk %s survived", id)
			}
		}
		if err := s.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
	})
}
