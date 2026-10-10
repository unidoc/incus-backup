package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
)

// ErrLocalChunkMissing means the server lacks a chunk the client reused from an
// older index and has no copy of it locally: the base is unusable and the next
// run must read the whole object.
var ErrLocalChunkMissing = errors.New("a reused chunk is missing on the server and not available locally")

// Prepared is everything captured for one storage object in one run, per key
// domain. It contains no network state.
type Prepared struct {
	Obj      *Object
	SnapID   string
	Created  time.Time
	Size     uint64
	Full     bool
	Dirty    int64
	Read     int64
	Duration time.Duration
	ByDomain map[string]*DomainSnapshot
	Config   []byte            // instance configuration (instance objects)
	Refs     []string          // referenced storage objects (instance objects)
	RefSnaps map[string]string // referenced object -> snapshot id captured in the same run
	Meta     map[string]string // extra manifest metadata (filesystem catalog layout)
}

// DomainSnapshot is the index (and base) of the snapshot in one key domain.
type DomainSnapshot struct {
	Index    *format.Index
	PrevSnap string
	Keys     *format.Keys
	// Meta overrides entries of Prepared.Meta for this key domain's manifest.
	Meta map[string]string
}

// Defaults of the worker pools (see docs/performance.md for the measurements behind them).
const (
	// DefaultEncodeWorkers bounds the chunks encoded at once. Encoding is CPU bound and runs on
	// the Incus host next to its guests, so it is capped rather than set to every core.
	DefaultEncodeWorkers = 4
	// DefaultUploadWorkers bounds the chunk uploads in flight to one target. Uploading is bound by
	// the network round trip and the server's per-chunk durability work, not by this host's CPU:
	// measured (1 GiB, 4 MiB chunks) 16 workers beat 8 by 5-30 % at 0-80 ms RTT and cost 16 x 4 MiB.
	DefaultUploadWorkers = 16
	// missingBatch is how many ids one "which chunks are missing" request carries.
	missingBatch = 2000
	// missingInFlight bounds the "which chunks are missing" requests one upload has open at once
	// (the server checks each with up to 64 parallel requests to its datastore).
	missingInFlight = 2
)

func (e *Engine) workers() int {
	if e.Workers > 0 {
		return e.Workers
	}
	return DefaultEncodeWorkers
}

func (e *Engine) uploadWorkers() int {
	if e.UploadWorkers > 0 {
		return e.UploadWorkers
	}
	return DefaultUploadWorkers
}

// Upload sends one prepared snapshot to one target and commits it. It aborts the
// session on any error. The commit is the only point at which the snapshot
// becomes visible on that target.
func (e *Engine) Upload(ctx context.Context, t *Target, p *Prepared, sp *Spool) (uploaded int64, err error) {
	ds := p.ByDomain[t.Domain()]
	if ds == nil {
		return 0, fmt.Errorf("no data prepared for key domain %s", t.Domain())
	}
	group := t.Keys.GroupID(p.Obj.Project, p.Obj.Kind, p.Obj.Name)
	start := time.Now()
	sess, err := t.Client.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: p.SnapID, Created: p.Created, Policy: p.Obj.Policy,
		Project: p.Obj.Project, Object: p.Obj.Name, Kind: p.Obj.Kind, PreviousID: ds.PrevSnap})
	if err != nil {
		return 0, fmt.Errorf("opening session: %w", err)
	}
	// Keep the lease alive even if there is a long stretch with nothing to send.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	if every := sess.Lease / 3; every > 0 {
		go func() {
			tk := time.NewTicker(every)
			defer tk.Stop()
			for {
				select {
				case <-hbCtx.Done():
					return
				case <-tk.C:
					if err := sess.Heartbeat(hbCtx); err != nil && e.Log != nil {
						e.Log.Warn("session heartbeat failed", "target", t.Name, "err", err)
					}
				}
			}
		}()
	}
	committed := false
	defer func() {
		if !committed {
			actx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = sess.Abort(actx)
		}
	}()

	ids := ds.Index.ChunkIDs()
	var sent int64
	// push asks which chunks the server lacks and uploads exactly those from the spool. It is run again
	// when a commit is refused for missing chunks (a quarantine or a GC between the questions and the commit).
	// The questions are pipelined with the uploads: up to missingInFlight batches are asked at once
	// (each is answered by parallel checks on the server) and the missing chunks of a batch are
	// uploaded as soon as its answer is in, in batch order, while the next answers arrive. A batch
	// counts as in flight until its answer is CONSUMED, so answers never pile up ahead of slow
	// uploads, and the hex ids of a batch exist only while it is asked: memory does not grow with
	// the object.
	push := func() error {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		type answer struct {
			missing []string
			err     error
			held    bool // the question held a slot of msem
		}
		nb := (len(ids) + missingBatch - 1) / missingBatch
		answers := make([]chan answer, nb)
		for i := range answers {
			answers[i] = make(chan answer, 1)
		}
		msem := make(chan struct{}, missingInFlight)
		go func() {
			for b := 0; b < nb; b++ {
				select {
				case msem <- struct{}{}:
				case <-cctx.Done():
					answers[b] <- answer{err: cctx.Err()}
					continue
				}
				go func(b int) {
					part := ids[b*missingBatch : min((b+1)*missingBatch, len(ids))]
					hex := make([]string, len(part))
					for i, id := range part {
						hex[i] = id.String()
					}
					m, err := sess.Missing(cctx, hex)
					answers[b] <- answer{m, err, true}
				}(b)
			}
		}()
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			first error
			sem   = make(chan struct{}, e.uploadWorkers())
		)
		fail := func(err error) {
			mu.Lock()
			if first == nil {
				first = err
				cancel()
			}
			mu.Unlock()
		}
	batches:
		for b := 0; b < nb; b++ {
			a := <-answers[b]
			answers[b] = nil
			if a.held {
				<-msem // consumed: the next question may go out
			}
			if a.err != nil {
				fail(fmt.Errorf("asking which chunks are missing: %w", a.err))
				break
			}
			for _, h := range a.missing {
				id, perr := format.ParseID(h)
				if perr != nil {
					fail(perr)
					break batches
				}
				if !sp.Has(t.Domain(), id) {
					fail(fmt.Errorf("%w: %s", ErrLocalChunkMissing, h))
					break batches
				}
				sem <- struct{}{}
				wg.Add(1)
				go func(h string, id format.ID) {
					defer wg.Done()
					defer func() { <-sem }()
					if cctx.Err() != nil {
						return
					}
					blob, err := sp.Get(t.Domain(), id)
					if err == nil {
						err = sess.PutChunk(cctx, h, blob)
					}
					if err != nil {
						fail(fmt.Errorf("uploading chunk %s: %w", h, err))
						return
					}
					atomic.AddInt64(&sent, int64(len(blob)))
				}(h, id)
			}
		}
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return first
	}
	if err := push(); err != nil {
		return sent, err
	}
	idx, err := ds.Index.Marshal()
	if err != nil {
		return sent, err
	}
	if err := sess.PutIndex(ctx, idx); err != nil {
		return sent, fmt.Errorf("uploading index: %w", err)
	}
	man := &format.Manifest{
		SnapshotID: p.SnapID, PreviousID: ds.PrevSnap, Created: p.Created, Host: e.Host, Project: p.Obj.Project, Policy: p.Obj.Policy,
		Object:      format.ObjectInfo{Name: p.Obj.Name, Kind: p.Obj.Kind, Size: p.Size},
		IndexDigest: format.IndexDigest(idx), Refs: p.Refs,
		Stats: format.Stats{LogicalBytes: p.Size, DirtyBytes: uint64(p.Dirty), ReadBytes: uint64(p.Read), UploadedBytes: uint64(sent), FullRead: p.Full, DurationMs: p.Duration.Milliseconds() + time.Since(start).Milliseconds()},
	}
	if p.Config != nil {
		man.Config = p.Config
	}
	if len(p.RefSnaps) > 0 || len(p.Meta) > 0 {
		man.Meta = map[string]string{}
		for k, v := range p.RefSnaps {
			man.Meta["snap:"+k] = v
		}
		for k, v := range p.Meta {
			man.Meta[k] = v
		}
		for k, v := range ds.Meta {
			if v == "" {
				delete(man.Meta, k)
			} else {
				man.Meta[k] = v
			}
		}
	}
	mb, err := t.Keys.SealManifest(man, group, nil)
	if err != nil {
		return sent, err
	}
	if err := sess.PutManifest(ctx, mb); err != nil {
		return sent, fmt.Errorf("uploading manifest: %w", err)
	}
	cr := api.CommitRequest{LogicalBytes: int64(p.Size), DirtyBytes: p.Dirty, ReadBytes: p.Read, UploadedBytes: sent, FullRead: p.Full,
		DurationMs: p.Duration.Milliseconds()}
	for attempt := 0; ; attempt++ {
		_, err := sess.Commit(ctx, cr)
		if err == nil {
			break
		}
		// refused because chunks vanished since we asked: upload them again and retry, twice at most
		if remote.IsStatus(err, 409) && strings.Contains(err.Error(), "missing") && attempt < 2 {
			if perr := push(); perr != nil {
				return sent, fmt.Errorf("committing: %w", perr)
			}
			continue
		}
		if remote.IsStatus(err, 409) && strings.Contains(err.Error(), "missing") {
			// still missing and not re-uploadable: the reused chunks are gone from this target
			return sent, fmt.Errorf("%w: committing: %v", ErrLocalChunkMissing, err)
		}
		return sent, fmt.Errorf("committing: %w", err)
	}
	committed = true
	return sent, nil
}

// UploadAll uploads p to every target in parallel and reports each outcome.
func (e *Engine) UploadAll(ctx context.Context, targets []*Target, p *Prepared, sp *Spool) []TargetResult {
	res := make([]TargetResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t *Target) {
			defer wg.Done()
			n, err := e.Upload(ctx, t, p, sp)
			res[i] = TargetResult{Target: t.Name, Committed: err == nil, Snapshot: p.SnapID, Uploaded: n, Err: err}
		}(i, t)
	}
	wg.Wait()
	return res
}
