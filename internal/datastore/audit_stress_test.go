package datastore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
)

// Concurrent backups (Missing / PutChunk / Commit with the client's retry), snapshot deletion,
// aggressive GC (every unreferenced chunk is past the grace period), quarantine-free presence
// rebuilds and parallel requests, all at once. Whatever the interleaving, no committed snapshot may
// ever reference a missing chunk.
func TestAuditStressGCVersusBackupsNeverLosesAChunk(t *testing.T) {
	for _, name := range []string{"mem", "mem-presence", "fs"} {
		t.Run(name, func(t *testing.T) {
			s := backends(t)[name](t)
			if os.Getenv("AUDIT_ALL_OLD") == "" {
				s.b = &oldChunks{s.b} // every CHUNK is past the grace period; snapshot objects keep their real age
			} else {
				s.now = func() time.Time { return time.Now().Add(72 * time.Hour) } // everything is "old"
			}
			s.SetRequestParallelism(8)
			base := time.Now()
			k := testKeys(t)
			const pool = 300
			blobs := make([][]byte, pool)
			ids := make([]format.ID, pool)
			for i := range blobs {
				id, b, err := k.EncodeChunk([]byte(fmt.Sprintf("content-%d", i)), format.EncodeOpts{})
				if err != nil {
					t.Fatal(err)
				}
				ids[i], blobs[i] = id, b
			}
			groups := []string{g1, "fedcba9876543210fedcba9876543210", "00112233445566778899aabbccddeeff"}
			var (
				stop      atomic.Bool
				wg        sync.WaitGroup
				mu        sync.Mutex
				committed = map[string][]string{} // group -> snaps
				commits   atomic.Int64
				refused   atomic.Int64
				seq       atomic.Int64
				failure   atomic.Value
				swept     atomic.Int64
				kept      atomic.Int64
			)
			fail := func(err error) { failure.CompareAndSwap(nil, err); stop.Store(true) }
			writer := func(gi int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(gi)))
				g := groups[gi%len(groups)]
				for !stop.Load() {
					n := 1 + rng.Intn(8)
					x := &format.Index{Kind: format.IndexDynamic}
					var off uint64
					picks := make([]int, n)
					for i := range picks {
						picks[i] = rng.Intn(pool)
						off += 10
						x.Entries = append(x.Entries, format.Entry{End: off, ID: ids[picks[i]]})
					}
					x.Size = off
					idx, _ := x.Marshal()
					snap := fmt.Sprintf("%s-%08x", base.Add(time.Duration(seq.Add(1))*time.Second).UTC().Format("20060102150405"), gi)
					man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: base, Object: format.ObjectInfo{Name: "o", Kind: "filesystem", Size: off}, IndexDigest: format.IndexDigest(idx)}, g, nil)
					sess, err := s.BeginSession(g, snap)
					if err != nil {
						fail(err)
						return
					}
					push := func() error {
						miss, err := sess.Missing(x.ChunkIDs())
						if err != nil {
							return err
						}
						for _, m := range miss {
							for i := range ids {
								if ids[i] == m {
									if _, err := sess.PutChunk(m, blobs[i]); err != nil {
										return err
									}
								}
							}
						}
						return nil
					}
					if err := push(); err != nil {
						fail(err)
						return
					}
					_ = sess.PutIndex(idx)
					_ = sess.PutManifest(man)
					ok := false
					for attempt := 0; attempt < 4; attempt++ {
						_, err := sess.Commit("client", time.Time{})
						if err == nil {
							ok = true
							break
						}
						if !errors.Is(err, ErrMissingChunks) {
							fail(err)
							return
						}
						refused.Add(1)
						if err := push(); err != nil {
							fail(err)
							return
						}
					}
					sess.Close()
					if ok {
						commits.Add(1)
						mu.Lock()
						committed[g] = append(committed[g], snap)
						mu.Unlock()
					}
				}
			}
			for i := 0; i < 6; i++ {
				wg.Add(1)
				go writer(i)
			}
			deleter := func(seed int64) { // retention: delete random committed snapshots
				defer wg.Done()
				rng := rand.New(rand.NewSource(seed))
				for !stop.Load() {
					mu.Lock()
					g := groups[rng.Intn(len(groups))]
					var victim string
					if l := committed[g]; len(l) > 2 {
						i := rng.Intn(len(l))
						victim = l[i]
						committed[g] = append(l[:i:i], l[i+1:]...)
					}
					mu.Unlock()
					if victim != "" {
						if err := s.DeleteSnapshot(g, victim); err != nil {
							fail(err)
							return
						}
					} else {
						time.Sleep(time.Millisecond)
					}
				}
			}
			for d := 0; d < 3; d++ {
				wg.Add(1)
				go deleter(int64(d))
			}
			wg.Add(1)
			go func() { // GC as often as it can
				defer wg.Done()
				for !stop.Load() {
					st, err := s.GC(context.Background(), time.Hour, false)
					if err != nil && !errors.Is(err, ErrGCRunning) {
						fail(err)
						return
					}
					if st != nil {
						swept.Add(int64(st.Removed))
						kept.Add(int64(st.KeptPinned))
					}
				}
			}()
			wg.Add(1)
			go func() { // presence re-lists (no-op when the set is off)
				defer wg.Done()
				for !stop.Load() {
					if err := s.RebuildPresence(); err != nil {
						fail(err)
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
			}()
			time.Sleep(2 * time.Second)
			stop.Store(true)
			wg.Wait()
			if f := failure.Load(); f != nil {
				t.Fatal(f)
			}
			if err := s.CheckInvariants(); err != nil {
				t.Fatalf("after %d commits (%d refused for missing chunks): %v", commits.Load(), refused.Load(), err)
			}
			t.Logf("%d commits, %d refused and retried, GC swept %d chunks and kept %d pinned ones", commits.Load(), refused.Load(), swept.Load(), kept.Load())
			if commits.Load() < 20 || swept.Load() < 20 {
				t.Fatalf("too few commits to mean anything: %d", commits.Load())
			}
		})
	}
}

// oldChunks makes every chunk object look days old (GC may sweep any unreferenced chunk at once).
type oldChunks struct{ Backend }

func ageInfo(key string, i Info) Info {
	if strings.HasPrefix(key, "chunks/") {
		i.ModTime = i.ModTime.Add(-72 * time.Hour)
	}
	return i
}
func (o *oldChunks) Stat(key string) (Info, error) {
	i, err := o.Backend.Stat(key)
	return ageInfo(key, i), err
}
func (o *oldChunks) List(prefix string, fn func(string, Info) error) error {
	return o.Backend.List(prefix, func(k string, i Info) error { return fn(k, ageInfo(k, i)) })
}
func (o *oldChunks) listShallow(prefix string) ([]listedObject, []string, error) {
	objs, dirs, err := o.Backend.(shallowLister).listShallow(prefix)
	for i := range objs {
		objs[i].info = ageInfo(objs[i].key, objs[i].info)
	}
	return objs, dirs, err
}
