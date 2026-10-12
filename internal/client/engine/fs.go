package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/archive"
	"github.com/unidoc/incus-backup/internal/chunker"
	"github.com/unidoc/incus-backup/internal/format"
)

// backupFSTar is the original filesystem format ("tar"): an Incus snapshot is taken, its tree
// is archived into a deterministic PAX stream, the stream is cut with content-defined chunking,
// each chunk is encoded once per key domain and spooled, and the result is uploaded to every
// target. It reads every file on every run (unchanged regions deduplicate, so only changed data
// is uploaded) and has no catalog. It is the correctness fallback of the catalog format and is
// selected with Engine.FSFormat = "tar".
func (e *Engine) backupFSTar(ctx context.Context, o *Object, sp *Spool) *Result {
	start := time.Now()
	created := e.Now().UTC()
	snap, err := format.NewSnapshotID(created, nil)
	if err != nil {
		return &Result{Object: o, Err: err}
	}
	res := &Result{Object: o, Snapshot: snap, Full: true}
	snapName := "ibk-" + snap
	if err := e.Inc.CreateSnapshot(ctx, o.Ref, snapName); err != nil {
		res.Err = fmt.Errorf("creating the Incus snapshot: %w", err)
		return res
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := e.Inc.DeleteSnapshot(dctx, o.Ref, snapName); err != nil {
			e.Log.Warn("could not delete the temporary snapshot; remove it by hand", "object", o.Key(), "snapshot", snapName, "err", err)
		}
	}()
	root, release, err := e.Inc.SnapshotPath(ctx, o.Ref, snapName)
	if err != nil {
		res.Err = fmt.Errorf("locating the snapshot contents: %w", err)
		return res
	}
	defer release()

	var targets []*Target
	domains := map[string]*format.Keys{}
	for _, n := range o.Targets {
		t := e.Targets[n]
		targets = append(targets, t)
		domains[t.Domain()] = t.Keys
	}
	var domList []*format.Keys
	for _, k := range domains {
		domList = append(domList, k)
	}
	sort.Slice(domList, func(i, j int) bool { return domList[i].DomainID < domList[j].DomainID })

	pr, pw := io.Pipe()
	var astats *archive.Stats
	archErr := make(chan error, 1)
	go func() {
		st, err := archive.Write(pw, root)
		astats = st
		_ = pw.CloseWithError(err)
		archErr <- err
	}()
	spl, err := chunker.NewSplitter(pr, chunker.Default)
	if err != nil {
		res.Err = err
		return res
	}

	type job struct {
		seq  int
		data []byte
	}
	jobs := make(chan job, e.workers()*2)
	var mu sync.Mutex
	ids := map[string]map[int]format.ID{}
	for _, k := range domList {
		ids[k.DomainID] = map[int]format.ID{}
	}
	var firstErr error
	var wg sync.WaitGroup
	for w := 0; w < e.workers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				for _, k := range domList {
					id, blob, err := k.EncodeChunk(j.data, format.EncodeOpts{})
					if err == nil {
						err = sp.Put(k.DomainID, id, blob)
					}
					mu.Lock()
					if err != nil && firstErr == nil {
						firstErr = err
					}
					ids[k.DomainID][j.seq] = id
					mu.Unlock()
				}
			}
		}()
	}
	var ends []uint64
	var total uint64
	var zeroSeq = map[int]bool{}
	seq := 0
	for {
		c, err := spl.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = pr.CloseWithError(err)
			res.Err = err
			break
		}
		total += uint64(len(c))
		ends = append(ends, total)
		if bytes.Count(c, []byte{0}) == len(c) {
			zeroSeq[seq] = true
		} else {
			jobs <- job{seq: seq, data: c}
		}
		seq++
	}
	close(jobs)
	wg.Wait()
	aerr := <-archErr
	if res.Err == nil && aerr != nil {
		res.Err = fmt.Errorf("archiving %s: %w", o.Name, aerr)
	}
	if res.Err == nil && firstErr != nil {
		res.Err = firstErr
	}
	if res.Err != nil {
		return res
	}

	p := &Prepared{Obj: o, SnapID: snap, Created: created, Size: total, Full: true, Dirty: int64(total), Read: int64(total), ByDomain: map[string]*DomainSnapshot{}}
	if astats != nil {
		p.Read = astats.Bytes
	}
	for _, k := range domList {
		x := &format.Index{Kind: format.IndexDynamic, Size: total}
		for i, end := range ends {
			ent := format.Entry{End: end}
			if !zeroSeq[i] {
				ent.ID = ids[k.DomainID][i]
			}
			x.Entries = append(x.Entries, ent)
		}
		p.ByDomain[k.DomainID] = &DomainSnapshot{Index: x, Keys: k}
	}
	p.Duration = time.Since(start)
	res.Logical, res.Dirty, res.Read, res.Duration = int64(total), p.Dirty, p.Read, p.Duration
	res.Targets = e.UploadAll(ctx, targets, p, sp)
	return res
}

// BackupInstance records the configuration of an instance and the storage
// objects it uses as a manifest-only snapshot (an empty index) on every target.
func (e *Engine) BackupInstance(ctx context.Context, o *Object, snap string, refSnaps map[string]string, sp *Spool) *Result {
	created := e.Now().UTC()
	if snap == "" {
		var err error
		if snap, err = format.NewSnapshotID(created, nil); err != nil {
			return &Result{Object: o, Err: err}
		}
	}
	p := &Prepared{Obj: o, SnapID: snap, Created: created, ByDomain: map[string]*DomainSnapshot{}, Config: o.Instance.JSON, Refs: o.Refs, RefSnaps: refSnaps}
	var targets []*Target
	for _, n := range o.Targets {
		t := e.Targets[n]
		targets = append(targets, t)
		if _, ok := p.ByDomain[t.Domain()]; !ok {
			p.ByDomain[t.Domain()] = &DomainSnapshot{Index: &format.Index{Kind: format.IndexDynamic}, Keys: t.Keys}
		}
	}
	res := &Result{Object: o, Snapshot: snap}
	res.Targets = e.UploadAll(ctx, targets, p, sp)
	return res
}
