// Package syncer implements repository-aware server-to-server synchronisation:
// compare committed snapshots, transfer only the missing encrypted chunks, then
// atomically commit each snapshot on the destination. Push and pull are the same
// engine with the roles of local and remote swapped; the initiator really
// initiates (it holds the credentials and opens every connection).
//
// The engine never decrypts anything. Blob integrity is checked with the keyless
// SHA-256 trailer on every transfer.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
)

// Source is where snapshots come from.
type Source interface {
	Domain(ctx context.Context) (string, error) // key-domain id of the repository
	List(ctx context.Context) ([]api.SnapshotRef, error)
	Index(ctx context.Context, group, snap string) ([]byte, error)
	Manifest(ctx context.Context, group, snap string) ([]byte, error)
	Chunk(ctx context.Context, id string) ([]byte, error)
}

// Dest is where snapshots go.
type Dest interface {
	List(ctx context.Context) ([]api.SnapshotRef, error)
	Missing(ctx context.Context, ids []string) ([]string, error)
	PutChunk(ctx context.Context, id string, blob []byte) error
	PutSnapshot(ctx context.Context, domain string, put api.SyncSnapshotPut) error
}

// Options tune a run.
type Options struct {
	GroupFilter []string
	Concurrency int // parallel chunk transfers (default 4)
	// VerifiedOnly transfers only snapshots whose last verification on the source is "ok".
	// (Snapshots that FAILED verification are never transferred, whatever this says: corrupt
	// content must not travel as if it were healthy.)
	VerifiedOnly bool
	// ResyncCorrupt also repairs snapshots the destination holds but has marked as failed
	// verification, by fetching their quarantined chunks from the source (when the source's copy
	// has not failed itself).
	ResyncCorrupt bool
	// TransferLast limits a run to the newest N snapshots of each group on the source (0 = all).
	TransferLast int
	// RateLimit caps blob traffic in bytes per second (0 = unlimited).
	RateLimit int64
	// OnState is called as snapshots move through pending/synced/failed.
	OnState func(group, snap, state, errMsg string)
}

// Result summarises a run.
type Result struct {
	Pending           int      `json:"pending"`
	Synced            int      `json:"synced"`
	Failed            int      `json:"failed"`
	SkippedUnverified int      `json:"skipped_unverified"` // not transferred because VerifiedOnly and not verified-ok
	SkippedCorrupt    int      `json:"skipped_corrupt"`    // not transferred because the source marks them as failed verification
	Repaired          int      `json:"repaired"`           // destination snapshots healed by ResyncCorrupt
	RepairedList      []string `json:"repaired_list"`      // "group/snapshot" of each
	BytesTransferred  int64    `json:"bytes_transferred"`
	ChunksTransferred int64    `json:"chunks_transferred"`
	ChunksReused      int64    `json:"chunks_reused"`
	Errors            []string `json:"errors"`
}

const missingBatch = 2000

// Run synchronises every snapshot the destination lacks. A failure of one
// snapshot is recorded and the others still proceed; the returned error is
// non-nil if any failed. A partially transferred snapshot is never committed.
func Run(ctx context.Context, src Source, dst Dest, opt Options) (*Result, error) {
	if opt.Concurrency <= 0 {
		opt.Concurrency = 4
	}
	notify := func(g, s, st, e string) {
		if opt.OnState != nil {
			opt.OnState(g, s, st, e)
		}
	}
	res := &Result{}
	srcList, err := src.List(ctx)
	if err != nil {
		return res, fmt.Errorf("listing source: %w", err)
	}
	dstList, err := dst.List(ctx)
	if err != nil {
		return res, fmt.Errorf("listing destination: %w", err)
	}
	have := map[string]api.SnapshotRef{}
	for _, d := range dstList {
		have[d.GroupID+"/"+d.SnapshotID] = d
	}
	filter := map[string]bool{}
	for _, g := range opt.GroupFilter {
		filter[g] = true
	}
	// per group, newest N on the source (transfer-last), considering only what passes the filter
	candidates := map[string][]api.SnapshotRef{}
	var groups []string
	for _, r := range srcList {
		if len(filter) > 0 && !filter[r.GroupID] {
			continue
		}
		if _, ok := candidates[r.GroupID]; !ok {
			groups = append(groups, r.GroupID)
		}
		candidates[r.GroupID] = append(candidates[r.GroupID], r)
	}
	var pending, repair []api.SnapshotRef
	for _, g := range groups {
		list := candidates[g]
		sort.Slice(list, func(i, j int) bool { return list[i].SnapshotID < list[j].SnapshotID })
		if opt.TransferLast > 0 && len(list) > opt.TransferLast {
			list = list[len(list)-opt.TransferLast:]
		}
		for _, r := range list {
			cur, present := have[r.GroupID+"/"+r.SnapshotID]
			if present {
				if opt.ResyncCorrupt && cur.VerifyState == "failed" && r.VerifyState != "failed" {
					repair = append(repair, r)
				}
				continue
			}
			if r.VerifyState == "failed" {
				res.SkippedCorrupt++
				continue
			}
			if opt.VerifiedOnly && r.VerifyState != "ok" {
				res.SkippedUnverified++
				continue
			}
			pending = append(pending, r)
		}
	}
	var lim *limiter
	if opt.RateLimit > 0 {
		lim = newLimiter(opt.RateLimit)
	}
	res.Pending = len(pending)
	if len(pending) == 0 && len(repair) == 0 {
		return res, nil
	}
	domain, err := src.Domain(ctx)
	if err != nil {
		return res, fmt.Errorf("reading source key domain: %w", err)
	}
	if domain == "" {
		return res, errors.New("source repository has no key domain yet (nothing was ever backed up)")
	}
	for _, p := range pending {
		notify(p.GroupID, p.SnapshotID, "pending", "")
	}
	for _, p := range repair {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := syncOne(ctx, src, dst, domain, p, opt.Concurrency, lim, res); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("repairing %s/%s: %v", p.GroupID, p.SnapshotID, err))
			notify(p.GroupID, p.SnapshotID, "failed", "repair: "+err.Error())
			continue
		}
		res.Repaired++
		res.RepairedList = append(res.RepairedList, p.GroupID+"/"+p.SnapshotID)
	}
	for _, p := range pending {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := syncOne(ctx, src, dst, domain, p, opt.Concurrency, lim, res); err != nil {
			res.Failed++
			msg := fmt.Sprintf("%s/%s: %v", p.GroupID, p.SnapshotID, err)
			res.Errors = append(res.Errors, msg)
			notify(p.GroupID, p.SnapshotID, "failed", err.Error())
			continue
		}
		res.Synced++
		notify(p.GroupID, p.SnapshotID, "synced", "")
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("%d snapshot(s) failed to synchronise (of %d pending, %d to repair)", res.Failed, res.Pending, len(repair))
	}
	return res, nil
}

func syncOne(ctx context.Context, src Source, dst Dest, domain string, ref api.SnapshotRef, conc int, lim *limiter, res *Result) error {
	idxBytes, err := src.Index(ctx, ref.GroupID, ref.SnapshotID)
	if err != nil {
		return fmt.Errorf("reading index: %w", err)
	}
	idx, err := format.ParseIndex(idxBytes)
	if err != nil {
		return fmt.Errorf("source index is corrupt: %w", err)
	}
	if ref.IndexDigest != "" && ref.IndexDigest != format.IndexDigest(idxBytes) {
		return errors.New("source index does not match its recorded digest")
	}
	manBytes, err := src.Manifest(ctx, ref.GroupID, ref.SnapshotID)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	ids := idx.ChunkIDs()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		first   error
		sem     = make(chan struct{}, conc)
		moved   int64
		movedBy int64
		reused  int64
	)
	failed := func() error {
		mu.Lock()
		defer mu.Unlock()
		return first
	}
	defer wg.Wait()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// one batch at a time: ask the destination which ids it lacks, transfer those, next batch, so
	// neither the question nor the answer is ever held for the whole snapshot
	batch := make([]string, 0, missingBatch)
	for i := 0; i < len(ids) && failed() == nil; i += missingBatch {
		end := min(i+missingBatch, len(ids))
		batch = batch[:0]
		for _, id := range ids[i:end] {
			batch = append(batch, id.String())
		}
		missing, err := dst.Missing(cctx, batch)
		if err != nil {
			if f := failed(); f != nil {
				break
			}
			cancel()
			wg.Wait()
			return fmt.Errorf("asking destination for missing chunks: %w", err)
		}
		reused += int64(len(batch) - len(missing))
		for _, id := range missing {
			sem <- struct{}{}
			if failed() != nil {
				<-sem
				break
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				defer func() { <-sem }()
				if cctx.Err() != nil {
					return
				}
				blob, err := src.Chunk(cctx, id)
				if err == nil {
					err = format.VerifyBlob(blob) // keyless integrity check of what we are about to copy
				}
				if err == nil && lim != nil {
					err = lim.wait(cctx, len(blob))
				}
				if err == nil {
					err = dst.PutChunk(cctx, id, blob)
				}
				if err != nil {
					mu.Lock()
					if first == nil {
						first = fmt.Errorf("chunk %s: %w", id, err)
						cancel()
					}
					mu.Unlock()
					return
				}
				atomic.AddInt64(&moved, 1)
				atomic.AddInt64(&movedBy, int64(len(blob)))
			}(id)
		}
	}
	atomic.AddInt64(&res.ChunksReused, reused)
	wg.Wait()
	atomic.AddInt64(&res.ChunksTransferred, moved)
	atomic.AddInt64(&res.BytesTransferred, movedBy)
	if first != nil {
		return first
	}
	return dst.PutSnapshot(ctx, domain, api.SyncSnapshotPut{Meta: ref, Index: idxBytes, Manifest: manBytes})
}
