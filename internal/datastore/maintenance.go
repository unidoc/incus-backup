package datastore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/progress"
)

// GCStats reports a garbage collection run.
type GCStats struct {
	Scanned          int   `json:"scanned"`
	Live             int   `json:"live"`
	KeptYoung        int   `json:"kept_young"`
	KeptPinned       int   `json:"kept_pinned"`
	Removed          int   `json:"removed"`
	BytesReclaimed   int64 `json:"bytes_reclaimed"`
	QuarantinePurged int   `json:"quarantine_purged"`
	OrphansRemoved   int   `json:"orphans_removed"`
	DryRun           bool  `json:"dry_run"`
}

// ErrGCRunning is returned when a garbage collection is already in progress.
var ErrGCRunning = errors.New("garbage collection already running")

func (s *Store) beginGC() error {
	if !atomic.CompareAndSwapInt32(&s.gcRunning, 0, 1) {
		return ErrGCRunning
	}
	s.pinMu.Lock()
	s.gcActive = true
	s.gcShadow = map[uint64]struct{}{}
	s.pinMu.Unlock()
	return nil
}

func (s *Store) endGC() {
	s.pinMu.Lock()
	s.gcActive = false
	s.gcShadow = nil
	s.pinMu.Unlock()
	atomic.StoreInt32(&s.gcRunning, 0)
}

// pinned reports whether a running session relies on the chunk, or a session that
// ended since the GC started did.
func (s *Store) pinned(id format.ID) bool {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	key := liveKey(id)
	if _, ok := s.gcShadow[key]; ok {
		return true
	}
	for x := range s.open {
		x.mu.Lock()
		_, k := x.known[key]
		x.mu.Unlock()
		if k {
			return true
		}
	}
	return false
}

// gcSweepBatch is how many sweep candidates GC collects before deleting them.
var gcSweepBatch = 4096

// liveKey is the key of a chunk id in the GC live set and in session pins (its first 8 bytes).
func liveKey(id format.ID) uint64 { return binary.LittleEndian.Uint64(id[:8]) }

// GC removes chunks that no committed snapshot references. Mark and sweep:
//
//  1. Mark: read every committed index; their chunks are live. An unreadable or corrupt
//     index aborts the whole run (never sweep on a guess).
//  2. Candidates: chunks that are not live and older than grace (the age of a chunk is the
//     time it was written; nothing ever rewrites or touches a chunk object).
//  3. Sweep: each candidate is re-checked under the write lock and skipped when a running
//     session relies on it (it said "present" or uploaded it) or a session that ended
//     since the GC began did, so a long-running backup can outlive the grace period and a
//     snapshot committed after the mark phase keeps the chunks it references.
//
// grace still protects uploads whose session was lost (server restart) and syncs.
func (s *Store) GC(ctx context.Context, grace time.Duration, dryRun bool) (*GCStats, error) {
	if err := s.beginGC(); err != nil {
		return nil, err
	}
	defer s.endGC()
	st := &GCStats{DryRun: dryRun}
	cutoff := s.now().Add(-grace)
	progress.Set(ctx, 0, "mark: reading every snapshot index")
	live, err := s.markLive(ctx)
	if err != nil {
		return nil, err
	}
	progress.Logf(ctx, progress.Info, "mark: %d chunks are referenced by committed snapshots", len(live))
	progress.Set(ctx, 30, "listing chunk objects")

	// Candidates are swept in batches of gcSweepBatch while the listing goes on, so neither the
	// listing nor the garbage of a large prune is ever held whole.
	type cand struct {
		id   format.ID
		key  string // only when the object is not at its canonical key (never, in practice)
		size int64
	}
	old := make([]cand, 0, min(gcSweepBatch, 1<<16))
	marked := false
	// Sweep in batches: the write lock is taken once per batch, every candidate of the batch is
	// re-checked (pins, then its age on the backend) and deleted under it, the requests of the
	// batch in parallel. The lock is held for about two round trips per batch instead of two per
	// candidate, and nothing a session relies on can be swept (sessions take the read side).
	par := s.parallelism()
	type outcome struct {
		removed, young bool
		size           int64
	}
	sweep := func() error {
		if !marked {
			if err := s.at("gc:after-mark"); err != nil {
				return err
			}
			marked = true
		}
		for i := 0; i < len(old); i += par {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch := old[i:min(i+par, len(old))]
			progress.Set(ctx, progress.Pct(int64(i), int64(len(old)), 50, 95), "sweep: %d of %d candidates checked", i, len(old))
			res := make([]outcome, len(batch))
			s.gcMu.Lock()
			pinned := make([]bool, len(batch))
			for j, c := range batch {
				pinned[j] = s.pinned(c.id)
			}
			derr := forEach(par, len(batch), func(j int) error {
				if pinned[j] {
					return nil
				}
				c := batch[j]
				key := c.key
				if key == "" {
					key = chunkKey(c.id)
				}
				info, err := s.b.Stat(key)
				if err != nil || info.ModTime.After(cutoff) {
					res[j].young = err == nil
					return nil
				}
				if !dryRun {
					// forget it BEFORE it goes: no Missing may answer "present" for it from here on
					s.presence.remove(c.id)
					derr := s.b.Delete(key)
					// and again after it: a re-list that started in between may have listed it
					s.presence.remove(c.id)
					if derr != nil {
						return derr
					}
				}
				res[j] = outcome{removed: true, size: info.Size}
				return nil
			})
			s.gcMu.Unlock()
			for j := range batch {
				switch {
				case pinned[j]:
					st.KeptPinned++
				case res[j].young:
					st.KeptYoung++
				case res[j].removed:
					st.Removed++
					st.BytesReclaimed += res[j].size
				}
			}
			if derr != nil {
				return derr
			}
		}
		old = old[:0]
		return nil
	}
	err = s.listChunkObjects(func(key string, info Info) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, perr := format.ParseID(key[strings.LastIndex(key, "/")+1:])
		if perr != nil {
			return nil // not ours; leave it
		}
		st.Scanned++
		if st.Scanned%10000 == 0 {
			progress.Set(ctx, 30, "listing chunk objects: %d scanned", st.Scanned)
		}
		if _, ok := live[liveKey(id)]; ok {
			st.Live++
			return nil
		}
		if info.ModTime.After(cutoff) {
			st.KeptYoung++
			return nil
		}
		c := cand{id: id, size: info.Size}
		if key != chunkKey(id) {
			c.key = key
		}
		old = append(old, c)
		if len(old) >= gcSweepBatch {
			return sweep()
		}
		return nil
	})
	if err == nil {
		err = sweep()
	}
	if err != nil {
		return st, err
	}
	progress.Logf(ctx, progress.Info, "listing: %d chunk objects, %d live, %d younger than the grace period", st.Scanned, st.Live, st.KeptYoung)
	if !dryRun {
		st.QuarantinePurged, _ = s.PurgeQuarantine(24 * time.Hour)
		// leftovers of uploads that never committed and of interrupted deletes, and filesystem temp files
		st.OrphansRemoved, _ = s.CleanOrphans(grace)
		_, _ = s.CleanTmp(grace)
	}
	return st, nil
}

// markLive is the mark phase of GC: the chunk ids of every committed snapshot. The groups are
// listed and the indexes read in parallel; an unreadable or corrupt index aborts the whole run
// (never sweep on a guess). A snapshot deleted while marking is skipped when its index is gone
// already, and otherwise marked (marking too much is safe, too little is not), so the marker
// itself need not be read again.
//
// The live set keys on the first 8 bytes of each id (liveKey): 8 bytes per live chunk instead of
// 32 plus map overhead. A collision can only make a garbage chunk look live and stay, never
// delete one. The indexes held at once are bounded by MarkIndexBudget.
func (s *Store) markLive(ctx context.Context) (map[uint64]struct{}, error) {
	groups, err := s.ListGroups()
	if err != nil {
		return nil, err
	}
	type ref struct{ group, snap string }
	var (
		mu   sync.Mutex
		refs []ref
	)
	par := s.parallelism()
	if err := forEach(par, len(groups), func(i int) error {
		snaps, err := s.committedSnapshots(groups[i])
		if err != nil {
			return err
		}
		mu.Lock()
		for _, sn := range snaps {
			refs = append(refs, ref{groups[i], sn})
		}
		mu.Unlock()
		return nil
	}); err != nil {
		return nil, err
	}
	live := map[uint64]struct{}{}
	budget := newByteBudget(MarkIndexBudget)
	err = forEach(par, len(refs), func(i int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := refs[i]
		held := budget.reserve()
		defer func() { budget.release(held) }()
		b, err := s.b.Get(snapPrefix(r.group, r.snap) + "index")
		held = budget.settle(held, 2*int64(len(b))) // the file and its parsed entries
		if errors.Is(err, ErrNotFound) {
			return nil // forgotten while we were marking: its chunks need no protection
		}
		if err != nil {
			return fmt.Errorf("gc aborted: reading index of %s/%s: %w", r.group, r.snap, err)
		}
		idx, err := format.ParseIndex(b)
		if err != nil {
			return fmt.Errorf("gc aborted: corrupt index %s/%s: %w", r.group, r.snap, err)
		}
		mu.Lock()
		for _, e := range idx.Entries {
			if !e.ID.IsZero() {
				live[liveKey(e.ID)] = struct{}{}
			}
		}
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return live, nil
}

// VerifyCacheMax bounds the chunk states a verification remembers (about 70-90 bytes each).
var VerifyCacheMax = 1 << 20

// stateCache remembers chunk states in two generations of at most limit/2 entries each.
type stateCache struct {
	cur, prev map[format.ID]chunkState
	half      int
}

func newStateCache(limit int) *stateCache {
	h := max(limit/2, 1)
	return &stateCache{cur: map[format.ID]chunkState{}, prev: map[format.ID]chunkState{}, half: h}
}

func (c *stateCache) get(id format.ID) (chunkState, bool) {
	if st, ok := c.cur[id]; ok {
		return st, true
	}
	st, ok := c.prev[id]
	return st, ok
}

func (c *stateCache) put(id format.ID, st chunkState) {
	if len(c.cur) >= c.half {
		c.prev, c.cur = c.cur, make(map[format.ID]chunkState, c.half)
	}
	c.cur[id] = st
}

// Problem is something Verify found wrong.
type Problem struct {
	Kind   string `json:"kind"` // "chunk-corrupt", "chunk-missing", "index-corrupt", "manifest-corrupt", "partial-snapshot"
	Object string `json:"object"`
	Detail string `json:"detail"`
}

// SnapshotResult is the outcome for one snapshot.
type SnapshotResult struct {
	Group    string   `json:"group"`
	Snapshot string   `json:"snapshot"`
	State    string   `json:"state"` // VerifyOK | VerifyFailed
	Chunks   int      `json:"chunks"`
	Problems []string `json:"problems"`
}

// VerifyStats reports a verification run.
type VerifyStats struct {
	Chunks      int              `json:"chunks"` // chunk objects read (each once per run, unless VerifyCacheMax is exceeded)
	Snapshots   int              `json:"snapshots"`
	Skipped     int              `json:"skipped"` // snapshots excluded by Select
	Quarantined int              `json:"quarantined"`
	Problems    []Problem        `json:"problems"`
	Results     []SnapshotResult `json:"results"`
}

// VerifyOpts tunes a verification job.
type VerifyOpts struct {
	// Quarantine moves corrupt chunks aside (see Quarantine) so they stop satisfying dedup checks.
	Quarantine bool
	// Record writes verify.json for every snapshot checked.
	Record bool
	// AllChunks also reads chunk objects no snapshot references (a full repository scrub).
	AllChunks bool
	// Now stamps the recorded events (default: the store's clock).
	Now func() time.Time
	// Select picks snapshots to check; nil checks all. It receives the stored record (nil = never verified).
	Select func(si SnapshotInfo, rec *VerifyRecord) bool
}

// Verify checks every chunk blob trailer and every committed snapshot (index and
// manifest match the commit marker's digests, all referenced chunks exist). It needs no key.
func (s *Store) Verify(ctx context.Context) (*VerifyStats, error) {
	return s.VerifyWith(ctx, VerifyOpts{AllChunks: true})
}

type chunkState int

const (
	chunkOK chunkState = iota + 1
	chunkBad
	chunkGone
)

// VerifyWith runs a verification job. Within one job a chunk is read at most once, however
// many snapshots reference it (deduplicated snapshots share almost all of their chunks).
//
// Chunk blobs are read ahead in parallel (at most DefaultVerifyParallelism blobs in memory at a
// time, in batches of verifyBatch ids) and the indexes and manifests of a few snapshots are read
// ahead, but every result is recorded in the same order a sequential job would record it, so the
// problems, results and counters do not depend on timing. A backend error is not evidence of
// damage: it aborts the job at the point a sequential job would have met it.
func (s *Store) VerifyWith(ctx context.Context, o VerifyOpts) (*VerifyStats, error) {
	vs := &VerifyStats{}
	add := func(k, obj, d string) {
		vs.Problems = append(vs.Problems, Problem{k, obj, d})
		if len(vs.Problems) <= 200 {
			progress.Logf(ctx, progress.Warn, "%s %s: %s", k, obj, d)
		}
	}
	// Chunks found corrupt are remembered exactly (they are few). The others are remembered in a
	// bounded two-generation cache: successive snapshots of one object share almost all chunks, so
	// each is read once; beyond VerifyCacheMax the oldest generation is forgotten and a chunk seen
	// again is read again (extra I/O, never a skipped check).
	badSet := map[format.ID]bool{}
	cache := newStateCache(VerifyCacheMax)
	lookup := func(id format.ID) (chunkState, bool) {
		if badSet[id] {
			return chunkBad, true
		}
		return cache.get(id)
	}
	type fetched struct {
		st   chunkState
		verr error // why the blob is corrupt
		err  error // backend error
	}
	fetch := func(id format.ID) fetched {
		b, err := s.b.Get(chunkKey(id))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fetched{st: chunkGone}
			}
			return fetched{err: err}
		}
		if verr := format.VerifyBlob(b); verr != nil {
			return fetched{st: chunkBad, verr: verr}
		}
		return fetched{st: chunkOK}
	}
	pending := map[format.ID]fetched{}
	vpar := min(DefaultVerifyParallelism, s.parallelism())
	// prefetch reads the given chunks (those not yet known) in parallel.
	prefetch := func(ids []format.ID) {
		var todo []format.ID
		for _, id := range ids {
			if _, ok := lookup(id); ok {
				continue
			}
			if _, ok := pending[id]; ok {
				continue
			}
			pending[id] = fetched{} // reserve: duplicates in ids are read once
			todo = append(todo, id)
		}
		res := make([]fetched, len(todo))
		_ = forEach(vpar, len(todo), func(i int) error {
			if ctx.Err() != nil {
				res[i] = fetched{err: ctx.Err()}
				return nil
			}
			res[i] = fetch(todo[i])
			return nil
		})
		for i, id := range todo {
			pending[id] = res[i]
		}
	}
	// checkR records the state of one chunk; remember=false for the all-chunks scrub, which lists
	// every object once and has nothing to remember it for.
	checkR := func(id format.ID, remember bool) (chunkState, error) {
		if st, ok := lookup(id); ok {
			return st, nil
		}
		r, ok := pending[id]
		if !ok || (r.st == 0 && r.err == nil) {
			r = fetch(id)
		}
		delete(pending, id)
		if r.err != nil {
			return 0, r.err
		}
		if r.st == chunkGone {
			if remember {
				cache.put(id, chunkGone)
			}
			return chunkGone, nil
		}
		vs.Chunks++
		if r.st == chunkBad {
			badSet[id] = true
			add("chunk-corrupt", id.String(), r.verr.Error())
			if o.Quarantine {
				if qerr := s.Quarantine(id); qerr != nil {
					return 0, qerr
				}
				vs.Quarantined++
			}
			return chunkBad, nil
		}
		if remember {
			cache.put(id, chunkOK)
		}
		return chunkOK, nil
	}
	check := func(id format.ID) (chunkState, error) { return checkR(id, true) }
	now := s.now
	if o.Now != nil {
		now = o.Now
	}
	par := s.parallelism()

	groups, err := s.ListGroups()
	if err != nil {
		return vs, err
	}
	snapHi := 95
	if o.AllChunks {
		snapHi = 60
	}
	for gi, g := range groups {
		progress.Set(ctx, progress.Pct(int64(gi), int64(len(groups)), 0, snapHi), "snapshots: object %d of %d, %d snapshots and %d chunks checked", gi+1, len(groups), vs.Snapshots, vs.Chunks)
		snaps, err := s.ListSnapshots(g)
		if err != nil {
			return vs, err
		}
		if cerr := ctx.Err(); cerr != nil {
			return vs, cerr
		}
		// selection: the stored records are read in parallel (only where one exists), Select is
		// called in order
		var sel []SnapshotInfo
		if o.Select != nil {
			recs := make([]*VerifyRecord, len(snaps))
			_ = forEach(par, len(snaps), func(i int) error {
				if snaps[i].HasVerify {
					recs[i], _ = s.ReadVerify(g, snaps[i].Snapshot)
				}
				return nil
			})
			for i, sn := range snaps {
				if !o.Select(sn, recs[i]) {
					vs.Skipped++
					continue
				}
				sel = append(sel, sn)
			}
		} else {
			sel = snaps
		}
		for w := 0; w < len(sel); w += verifySnapWindow {
			win := sel[w:min(w+verifySnapWindow, len(sel))]
			files := make([]snapFiles, len(win))
			_ = forEach(par, len(win), func(i int) error {
				p := snapPrefix(g, win[i].Snapshot)
				files[i].ib, files[i].ibErr = s.b.Get(p + "index")
				files[i].mb, files[i].mbErr = s.b.Get(p + "manifest")
				return nil
			})
			type rec struct {
				snap string
				ev   VerifyEvent
			}
			var recs []rec
			// records are written after the window (in parallel), and also when the job stops early,
			// so every snapshot that was checked has its record, as with a sequential job
			flushRecs := func() {
				_ = forEach(par, len(recs), func(i int) error {
					_ = s.RecordVerify(g, recs[i].snap, recs[i].ev)
					return nil
				})
				recs = nil
			}
			for i, sn := range win {
				if cerr := ctx.Err(); cerr != nil {
					flushRecs()
					return vs, cerr
				}
				vs.Snapshots++
				res := SnapshotResult{Group: g, Snapshot: sn.Snapshot, State: VerifyOK}
				obj := g + "/" + sn.Snapshot
				fail := func(kind, detail string) {
					res.State = VerifyFailed
					res.Problems = append(res.Problems, kind+": "+detail)
					add(kind, obj, detail)
				}
				var abort error
				s.verifySnapshot(sn, obj, files[i], prefetch, check, &res, fail, &abort)
				files[i] = snapFiles{}
				if abort != nil {
					flushRecs()
					return vs, abort
				}
				vs.Results = append(vs.Results, res)
				if o.Record {
					recs = append(recs, rec{sn.Snapshot, VerifyEvent{At: now().UTC(), State: res.State, Chunks: res.Chunks, Problems: res.Problems}})
				}
			}
			flushRecs()
		}
	}
	// A chunk found corrupt breaks EVERY snapshot that references it, not only the ones this job was
	// asked to check: mark them too, or an older "ok" snapshot would still be taken as a base.
	bad := badSet
	if len(bad) > 0 {
		done := map[string]bool{}
		for _, r := range vs.Results {
			done[r.Group+"/"+r.Snapshot] = true
		}
		for _, g := range groups {
			snaps, err := s.ListSnapshots(g)
			if err != nil {
				return vs, err
			}
			for _, sn := range snaps {
				if done[g+"/"+sn.Snapshot] {
					continue
				}
				ids, err := s.ChunkIDsOf(g, sn.Snapshot)
				if err != nil {
					continue
				}
				var probs []string
				for _, id := range ids {
					if bad[id] {
						probs = append(probs, "chunk-corrupt: "+id.String())
						break
					}
				}
				if len(probs) == 0 {
					continue
				}
				res := SnapshotResult{Group: g, Snapshot: sn.Snapshot, State: VerifyFailed, Problems: probs}
				vs.Results = append(vs.Results, res)
				if o.Record {
					_ = s.RecordVerify(g, sn.Snapshot, VerifyEvent{At: now().UTC(), State: VerifyFailed, Problems: probs})
				}
			}
		}
	}
	if o.AllChunks {
		progress.Logf(ctx, progress.Info, "snapshots done (%d checked); scrubbing every chunk object", vs.Snapshots)
		// in batches: read ahead in parallel, then record in listing order
		var names []string
		flush := func() error {
			ids := make([]format.ID, 0, len(names))
			for _, n := range names {
				if id, perr := format.ParseID(n); perr == nil {
					ids = append(ids, id)
				}
			}
			prefetch(ids)
			for _, n := range names {
				id, perr := format.ParseID(n)
				if perr != nil {
					add("chunk-corrupt", n, "object name is not a chunk id")
					continue
				}
				if _, err := checkR(id, false); err != nil {
					return err
				}
			}
			names = names[:0]
			progress.Set(ctx, 60, "scrub: %d chunk objects read", vs.Chunks)
			return nil
		}
		err = s.listChunkObjects(func(key string, _ Info) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			names = append(names, key[strings.LastIndex(key, "/")+1:])
			if len(names) >= verifyBatch {
				return flush()
			}
			return nil
		})
		if err == nil {
			err = flush()
		}
		if err != nil {
			return vs, err
		}
	}
	return vs, nil
}

// verifySnapWindow is how many snapshots' index and manifest a verification reads ahead.
const verifySnapWindow = 8

// snapFiles are the index and manifest of a snapshot, read ahead.
type snapFiles struct {
	ib, mb       []byte
	ibErr, mbErr error
}

func (s *Store) verifySnapshot(sn SnapshotInfo, obj string, f snapFiles, prefetch func([]format.ID), check func(format.ID) (chunkState, error), res *SnapshotResult, fail func(kind, detail string), abort *error) {
	ib, err := f.ib, f.ibErr
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			*abort = fmt.Errorf("reading the index of %s: %w", obj, err)
			return
		}
		fail("partial-snapshot", "index unreadable: "+err.Error())
		return
	}
	if sn.Receipt.IndexDigest != "" && sn.Receipt.IndexDigest != format.IndexDigest(ib) {
		fail("index-corrupt", "index differs from the commit marker")
		return
	}
	idx, err := format.ParseIndex(ib)
	if err != nil {
		fail("index-corrupt", err.Error())
		return
	}
	mb, err := f.mb, f.mbErr
	if err != nil && !errors.Is(err, ErrNotFound) {
		*abort = fmt.Errorf("reading the manifest of %s: %w", obj, err)
		return
	}
	switch {
	case err != nil:
		fail("partial-snapshot", "manifest unreadable: "+err.Error())
	case len(mb) < 32 || trailerOK(mb) != nil:
		fail("manifest-corrupt", "trailer mismatch")
	case sn.Receipt.ManifestDigest != "" && sn.Receipt.ManifestDigest != format.IndexDigest(mb):
		fail("manifest-corrupt", "manifest differs from the commit marker")
	}
	ids := idx.ChunkIDs() // distinct already
	for b := 0; b < len(ids); b += verifyBatch {
		batch := ids[b:min(b+verifyBatch, len(ids))]
		prefetch(batch)
		for _, id := range batch {
			res.Chunks++
			st, err := check(id)
			if err != nil {
				// a backend error is not evidence of damage: do not mark the snapshot failed for it
				*abort = fmt.Errorf("reading chunk %s: %w", id, err)
				return
			}
			switch st {
			case chunkGone:
				fail("chunk-missing", id.String())
			case chunkBad:
				res.State = VerifyFailed
				res.Problems = append(res.Problems, "chunk-corrupt: "+id.String())
			}
		}
	}
}

// Usage lists the chunk objects and returns their total size and count.
func (s *Store) Usage() (bytes int64, chunks int64, err error) {
	err = s.listChunkObjects(func(_ string, info Info) error {
		bytes += info.Size
		chunks++
		return nil
	})
	return
}

// CheckInvariants returns an error if a VISIBLE snapshot is incomplete: every
// committed snapshot must have a parseable index, a manifest and all its chunks.
// (An upload without a commit marker is invisible and is not a violation.)
func (s *Store) CheckInvariants() error {
	groups, err := s.ListGroups()
	if err != nil {
		return err
	}
	for _, g := range groups {
		snaps, err := s.ListSnapshots(g)
		if err != nil {
			return err
		}
		for _, sn := range snaps {
			for _, f := range []string{"index", "manifest"} {
				if _, err := s.b.Stat(snapPrefix(g, sn.Snapshot) + f); err != nil {
					return fmt.Errorf("snapshot %s/%s is visible but its %s is missing", g, sn.Snapshot, f)
				}
			}
			ib, _ := s.b.Get(snapPrefix(g, sn.Snapshot) + "index")
			idx, err := format.ParseIndex(ib)
			if err != nil {
				return fmt.Errorf("snapshot %s/%s has a bad index: %w", g, sn.Snapshot, err)
			}
			for _, id := range idx.ChunkIDs() {
				if !s.HasChunk(id) {
					return fmt.Errorf("snapshot %s/%s references missing chunk %s", g, sn.Snapshot, id)
				}
			}
		}
	}
	return nil
}
