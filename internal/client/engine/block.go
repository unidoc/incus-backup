package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/nbd"
)

// BitmapPrefix marks the bitmaps incus-backup owns. Nothing else is ever deleted.
const BitmapPrefix = "ib-"

// Hooks let tests act at precise moments of a backup.
type Hooks struct {
	AfterFreeze  func() // the NBD view is frozen; the guest may keep writing
	BeforeUpload func() // the freeze is released, nothing is uploaded yet
}

// Disk is one block disk captured together with the others of a VM.
type Disk struct {
	Obj    *Object
	Device string
}

type domPlan struct {
	keys     *format.Keys
	targets  []*Target
	baseSnap string
	baseIdx  *format.Index
	bitmap   string
}

type diskPlan struct {
	obj         *Object
	dev         string
	ref         incusx.VolRef
	targets     []*Target
	failed      []TargetResult // targets we could not even plan for
	lastSnap    map[string]string
	allKnown    bool
	domains     map[string]*domPlan
	full        bool
	fullReason  string
	noBitmap    bool
	newBitmapOK bool
	poison      map[string]bool // base bitmaps that must not be kept (their base chunks are gone)
}

func snapTime(id string) time.Time {
	if len(id) < 14 {
		return time.Time{}
	}
	t, _ := time.Parse("20060102150405", id[:14])
	return t
}

func bitmapName(snap string) string { return BitmapPrefix + snap }

// planDisk learns from every target what it already has and decides whether an
// incremental capture is possible. Anything doubtful becomes a full read.
func (e *Engine) planDisk(ctx context.Context, d Disk, running bool, ref incusx.VolRef, bitmaps []incusx.Bitmap) *diskPlan {
	p := &diskPlan{obj: d.Obj, dev: d.Device, ref: ref, lastSnap: map[string]string{}, allKnown: true, domains: map[string]*domPlan{}, poison: map[string]bool{}}
	if len(d.Obj.SharedBy) > 1 {
		p.noBitmap, p.full, p.fullReason = true, true, "shared by several instances: a per-VM bitmap cannot see the other writers"
	}
	if !running {
		p.noBitmap, p.full, p.fullReason = true, true, "instance is stopped: no bitmaps exist"
	}
	have := map[string]incusx.Bitmap{}
	for _, b := range bitmaps {
		have[b.Name] = b
	}
	for _, name := range d.Obj.Targets {
		t := e.Targets[name]
		group := t.Keys.GroupID(d.Obj.Project, d.Obj.Kind, d.Obj.Name)
		l, err := t.Client.Snapshots(ctx, group)
		if err != nil {
			p.failed = append(p.failed, TargetResult{Target: name, Err: fmt.Errorf("asking %s what it has: %w", name, err)})
			if last := e.remembered(d.Obj.Key(), name); last != "" {
				// unreachable, but we know the generation it holds: keep it as a base
				// (until max_target_lag) instead of pausing all bitmap housekeeping
				p.lastSnap[name] = last
			} else {
				p.allKnown = false
			}
			continue
		}
		last := ""
		if n := len(l.Snapshots); n > 0 {
			last = l.Snapshots[n-1].SnapshotID
			if l.Snapshots[n-1].VerifyState == "failed" {
				// A snapshot that failed verification is never a base: its chunks may be corrupt or
				// quarantined, and an incremental run would never read (and so never re-upload) the
				// unchanged ones. Read everything again; deduplication keeps the cost low and the
				// repository heals.
				p.full, p.fullReason = true, "the latest snapshot on "+name+" failed verification: reading everything again so the repository heals"
			}
		}
		p.lastSnap[name] = last
		e.remember(d.Obj.Key(), name, last)
		p.targets = append(p.targets, t)
		dp := p.domains[t.Domain()]
		if dp == nil {
			dp = &domPlan{keys: t.Keys}
			p.domains[t.Domain()] = dp
		}
		dp.targets = append(dp.targets, t)
		// base of this domain = the OLDEST latest-snapshot among its targets
		if last == "" {
			dp.baseSnap = "\x00none"
		} else if dp.baseSnap == "" || (dp.baseSnap != "\x00none" && last < dp.baseSnap) {
			dp.baseSnap = last
		}
	}
	for _, dp := range p.domains {
		if dp.baseSnap == "\x00none" || dp.baseSnap == "" {
			dp.baseSnap = ""
			p.full, p.fullReason = true, "a target has no earlier snapshot of this object"
			continue
		}
		dp.bitmap = bitmapName(dp.baseSnap)
		b, ok := have[dp.bitmap]
		switch {
		case p.noBitmap:
		case !ok:
			p.full, p.fullReason = true, "bitmap "+dp.bitmap+" no longer exists (VM restarted, bitmap deleted or never created)"
		case b.Inconsistent:
			p.full, p.fullReason = true, "bitmap "+dp.bitmap+" is inconsistent"
		case !b.Recording:
			p.full, p.fullReason = true, "bitmap "+dp.bitmap+" is not recording"
		}
	}
	// fetch the base index of every domain from a target that holds it
	if !p.full {
		for _, dp := range p.domains {
			var got *format.Index
			for _, t := range dp.targets {
				if p.lastSnap[t.Name] != dp.baseSnap {
					continue
				}
				group := t.Keys.GroupID(d.Obj.Project, d.Obj.Kind, d.Obj.Name)
				b, err := t.Client.Index(ctx, group, dp.baseSnap)
				if err != nil {
					continue
				}
				if x, err := format.ParseIndex(b); err == nil && x.Kind == format.IndexFixed {
					got = x
					break
				}
			}
			if got == nil {
				p.full, p.fullReason = true, "the base index of "+dp.baseSnap+" could not be read"
				break
			}
			dp.baseIdx = got
		}
	}
	return p
}

// captureReadAhead is how many chunk positions captureDisk reads with one pipelined NBD batch
// (8 x 4 MiB = 32 MiB in flight): enough to hide a millisecond-scale round trip to the NBD server.
const captureReadAhead = 8

// chunkBufs recycles the 4 MiB plaintext buffers of block capture.
var chunkBufs = sync.Pool{New: func() any { b := make([]byte, format.FixedChunkSize); return &b }}

type posResult struct {
	ids map[string][]format.ID // per domain, by position
}

// captureDisk reads the dirty (or all) chunks of one export from the frozen
// view, encodes them once per key domain and spools the blobs. It returns the
// per-domain index and statistics. conn must be at the frozen point already.
func (e *Engine) captureDisk(ctx context.Context, p *diskPlan, conn *nbd.Conn, sp *Spool, snap string, created time.Time) (*Prepared, error) {
	start := time.Now()
	size := conn.Size()
	cs := uint64(format.FixedChunkSize)
	n := int((size + cs - 1) / cs)
	prep := &Prepared{Obj: p.obj, SnapID: snap, Created: created, Size: size, ByDomain: map[string]*DomainSnapshot{}}

	// Which positions must be (re)read?
	full := p.full
	reason := p.fullReason
	if !full {
		for _, dp := range p.domains {
			if dp.baseIdx == nil || dp.baseIdx.Size != size || dp.baseIdx.ChunkSize != uint32(cs) {
				full, reason = true, "the disk size changed"
				break
			}
		}
	}
	need := make([]bool, n)
	if !full {
		for _, dp := range p.domains {
			ctxName := "qemu:dirty-bitmap:" + dp.bitmap
			if _, ok := conn.Context[ctxName]; !ok {
				full, reason = true, "the server did not grant "+ctxName
				break
			}
			ext, err := conn.Status(ctxName, 0, size)
			if err != nil {
				full, reason = true, "reading "+ctxName+": "+err.Error()
				break
			}
			for _, x := range ext {
				if x.Flags&1 == 0 {
					continue
				}
				for i := x.Offset / cs; i <= (x.Offset+x.Length-1)/cs && int(i) < n; i++ {
					need[i] = true
				}
			}
		}
	}
	if full {
		for i := range need {
			need[i] = true
		}
		prep.Full = true
		e.Log.Info("full read", "object", p.obj.Key(), "reason", reason)
	}
	// holes: skip reading regions the server says are zero
	var zero []nbd.Extent
	if full {
		if _, ok := conn.Context["base:allocation"]; ok {
			if ext, err := conn.Status("base:allocation", 0, size); err == nil {
				zero = ext
			}
		}
	}
	isHole := func(off, l uint64) bool {
		i := sort.Search(len(zero), func(i int) bool { return zero[i].Offset+zero[i].Length > off })
		for ; i < len(zero) && zero[i].Offset < off+l; i++ {
			if zero[i].Flags&2 == 0 {
				return false
			}
			if zero[i].Offset <= off && zero[i].Offset+zero[i].Length >= off+l {
				return true
			}
		}
		return false
	}

	domains := make([]*domPlan, 0, len(p.domains))
	for _, dp := range p.domains {
		domains = append(domains, dp)
	}
	ids := map[string][]format.ID{}
	for _, dp := range domains {
		ids[dp.keys.DomainID] = make([]format.ID, n)
	}

	type job struct {
		pos  int
		data []byte
		buf  *[]byte // returned to chunkBufs once every domain has encoded it
	}
	jobs := make(chan job, e.workers()*2)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}
	for w := 0; w < e.workers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				for _, dp := range domains {
					id, blob, err := dp.keys.EncodeChunk(j.data, format.EncodeOpts{})
					if err == nil {
						err = sp.Put(dp.keys.DomainID, id, blob)
					}
					if err != nil {
						setErr(err)
						continue
					}
					ids[dp.keys.DomainID][j.pos] = id
				}
				chunkBufs.Put(j.buf) // EncodeChunk keeps no reference to its input
			}
		}()
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	// Positions are read in order, captureReadAhead at a time with one pipelined NBD batch, so the
	// round trip to the NBD server is paid once per batch instead of once per chunk.
	type pending struct {
		pos  int
		off  uint64
		buf  *[]byte
		data []byte
	}
	batch := make([]pending, 0, captureReadAhead)
	segs := make([]nbd.Segment, 0, captureReadAhead)
	var readErr error
	flush := func() {
		if len(batch) == 0 {
			return
		}
		segs = segs[:0]
		for _, it := range batch {
			segs = append(segs, nbd.Segment{Off: int64(it.off), Buf: it.data})
		}
		if err := conn.ReadBatch(segs); err != nil {
			if ctx.Err() != nil {
				readErr = fmt.Errorf("the frozen view was held longer than max_freeze (%s): %w", e.MaxFreeze, ctx.Err())
			} else {
				readErr = fmt.Errorf("reading %s at %d: %w", p.obj.Name, batch[0].off, err)
			}
			for _, it := range batch {
				chunkBufs.Put(it.buf)
			}
			batch = batch[:0]
			return
		}
		for _, it := range batch {
			prep.Read += int64(len(it.data))
			if bytes.Count(it.data, []byte{0}) == len(it.data) {
				chunkBufs.Put(it.buf)
				continue
			}
			jobs <- job{pos: it.pos, data: it.data, buf: it.buf}
		}
		batch = batch[:0]
	}
	for i := 0; i < n && readErr == nil; i++ {
		if !need[i] {
			continue
		}
		off := uint64(i) * cs
		l := cs
		if off+l > size {
			l = size - off
		}
		prep.Dirty += int64(l)
		if full && isHole(off, l) {
			continue // ids[..][i] stays the zero id
		}
		if ctx.Err() != nil { // do not depend on the connection-close race: an expired freeze never reads again
			readErr = fmt.Errorf("the frozen view was held longer than max_freeze (%s): %w", e.MaxFreeze, ctx.Err())
			break
		}
		bp := chunkBufs.Get().(*[]byte)
		batch = append(batch, pending{pos: i, off: off, buf: bp, data: (*bp)[:l]})
		if len(batch) == captureReadAhead {
			flush()
		}
	}
	if readErr == nil {
		flush()
	}
	close(jobs)
	wg.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if firstErr != nil {
		return nil, firstErr
	}
	for _, dp := range domains {
		x := format.NewFixedIndex(size, uint32(cs))
		for i := range x.Entries {
			switch {
			case need[i]:
				x.Entries[i].ID = ids[dp.keys.DomainID][i]
			case dp.baseIdx != nil && i < len(dp.baseIdx.Entries):
				x.Entries[i].ID = dp.baseIdx.Entries[i].ID
			default:
				return nil, fmt.Errorf("internal error: position %d has neither new data nor a base", i)
			}
		}
		prep.ByDomain[dp.keys.DomainID] = &DomainSnapshot{Index: x, PrevSnap: dp.baseSnap, Keys: dp.keys}
	}
	prep.Duration = time.Since(start)
	return prep, nil
}

// BackupVM captures every given disk of one VM under ONE frozen NBD session (so
// the disks are consistent with each other), then uploads each disk to each of its
// targets. Disks of a stopped VM are read through the per-volume NBD endpoint.
func (e *Engine) BackupVM(ctx context.Context, inst incusx.Instance, disks []Disk, sp *Spool) (snap string, results []*Result) {
	created := e.Now().UTC()
	snap, err := format.NewSnapshotID(created, nil)
	if err != nil {
		for _, d := range disks {
			results = append(results, &Result{Object: d.Obj, Err: err})
		}
		return "", results
	}
	fail := func(d Disk, err error) *Result { return &Result{Object: d.Obj, Snapshot: snap, Err: err} }

	// 1. what exists now, and a plan per disk
	plans := make([]*diskPlan, len(disks))
	for i, d := range disks {
		var bms []incusx.Bitmap
		if inst.Running {
			if b, err := e.Inc.Bitmaps(ctx, d.Obj.Ref); err == nil {
				bms = b
			} else {
				e.Log.Warn("listing bitmaps failed; the disk will be read in full", "object", d.Obj.Key(), "err", err)
			}
		}
		plans[i] = e.planDisk(ctx, d, inst.Running, d.Obj.Ref, bms)
	}

	// 2. create the new generation BEFORE the freeze
	newBM := bitmapName(snap)
	if inst.Running {
		var wanted []Disk
		for i, d := range disks {
			if !plans[i].noBitmap {
				wanted = append(wanted, d)
			}
		}
		if len(wanted) > 0 {
			instanceWide := true
			if err := e.Inc.CreateInstanceBitmap(ctx, inst.Project, inst.Name, newBM, true); err != nil {
				// The instance-wide call is ONE transaction: a single disk that cannot hold a
				// persistent bitmap (a raw dependent volume) fails all of them. Fall back to
				// one call per disk: persistent for the root disk, in-memory for the rest.
				e.Log.Info("the instance-wide bitmap call failed; creating bitmaps per disk", "instance", inst.Name, "err", err)
				instanceWide = false
			}
			for i, d := range disks {
				if plans[i].noBitmap {
					continue
				}
				isRoot := d.Device == rootDevice(inst)
				dep := isRoot || inst.ExpandedDevices[d.Device]["dependent"] == "true"
				if !dep || !instanceWide {
					if err := e.Inc.CreateBitmap(ctx, d.Obj.Ref, newBM, isRoot); err != nil {
						e.Log.Warn("creating a bitmap failed; this disk will be read in full next time", "object", d.Obj.Key(), "err", err)
					}
				}
				if bms, err := e.Inc.Bitmaps(ctx, d.Obj.Ref); err == nil {
					for _, b := range bms {
						if b.Name == newBM && b.Recording && !b.Inconsistent {
							plans[i].newBitmapOK = true
						}
					}
				}
			}
		}
	}

	// 3. freeze, capture, release
	fctx, cancel := context.WithTimeout(ctx, e.MaxFreeze)
	prepared := make([]*Prepared, len(disks))
	errs := make([]error, len(disks))
	conns := make([]*nbd.Conn, len(disks))
	func() {
		defer cancel()
		closeAll := func() {
			for _, c := range conns {
				if c != nil {
					_ = c.Close()
				}
			}
		}
		defer closeAll()
		for i, d := range disks {
			nc, derr0 := e.dial(fctx, inst, d, i == 0)
			if derr0 != nil {
				errs[i] = derr0
				continue
			}
			ctxs := []string{"base:allocation"}
			for _, dp := range plans[i].domains {
				if dp.bitmap != "" && !plans[i].noBitmap {
					ctxs = append(ctxs, "qemu:dirty-bitmap:"+dp.bitmap)
				}
			}
			export := d.Device
			if !inst.Running {
				export = ""
			}
			c, derr := nbd.Dial(fctx, nc, export, ctxs)
			if derr != nil {
				errs[i] = fmt.Errorf("negotiating NBD export %q: %w", export, derr)
				continue
			}
			conns[i] = c
		}
		if e.Hooks.AfterFreeze != nil {
			e.Hooks.AfterFreeze()
		}
		for i := range disks {
			if conns[i] == nil {
				continue
			}
			prepared[i], errs[i] = e.captureDisk(fctx, plans[i], conns[i], sp, snap, created)
		}
	}()
	if e.Hooks.BeforeUpload != nil {
		e.Hooks.BeforeUpload()
	}

	// 4. upload to every target of every disk
	for i, d := range disks {
		if errs[i] != nil || prepared[i] == nil {
			r := fail(d, errs[i])
			if r.Err == nil {
				r.Err = errors.New("nothing captured")
			}
			results = append(results, r)
			continue
		}
		p := prepared[i]
		var targets []*Target
		for _, t := range plans[i].targets {
			targets = append(targets, t)
		}
		r := &Result{Object: d.Obj, Snapshot: snap, Full: p.Full, Logical: int64(p.Size), Dirty: p.Dirty, Read: p.Read, Duration: p.Duration}
		r.Targets = append(r.Targets, plans[i].failed...)
		r.Targets = append(r.Targets, e.UploadAll(ctx, targets, p, sp)...)
		for _, tr := range r.Targets {
			if tr.Committed {
				e.remember(d.Obj.Key(), tr.Target, snap)
			}
			if errors.Is(tr.Err, ErrLocalChunkMissing) {
				// The base this target relies on is damaged: drop that generation so the next
				// run reads the disk in full (deduplicated) instead of failing the same way forever.
				e.Log.Warn("a base chunk is gone from a target; its base generation is dropped and the next run reads this object in full", "object", d.Obj.Key(), "target", tr.Target)
				if t := e.Targets[tr.Target]; t != nil {
					if dp := plans[i].domains[t.Domain()]; dp != nil && dp.bitmap != "" {
						plans[i].poison[dp.bitmap] = true
					}
				}
			}
		}
		results = append(results, r)
	}

	// 5. bitmap generations
	if inst.Running {
		for i, d := range disks {
			e.sweepBitmaps(ctx, d, plans[i], results[i], snap)
		}
	}
	return snap, results
}

// dial opens the NBD connection for a disk: the first connection of a running VM
// freezes the view (reuse=false), later ones attach to it.
func (e *Engine) dial(ctx context.Context, inst incusx.Instance, d Disk, first bool) (conn net.Conn, err error) {
	if inst.Running {
		c, err := e.Inc.InstanceNBD(ctx, inst.Project, inst.Name, !first)
		if err != nil {
			return nil, fmt.Errorf("opening the NBD session: %w", err)
		}
		return c, nil
	}
	c, err := e.Inc.VolumeNBD(ctx, d.Obj.Ref, false)
	if err != nil {
		return nil, fmt.Errorf("opening the volume NBD: %w", err)
	}
	return c, nil
}

// sweepBitmaps keeps exactly the bitmaps that some target still needs as the
// base of its next incremental backup, plus the new generation, and deletes every
// other incus-backup bitmap. It deletes nothing when any target's state is unknown.
func (e *Engine) sweepBitmaps(ctx context.Context, d Disk, p *diskPlan, r *Result, snap string) {
	if p.noBitmap {
		return
	}
	if !p.allKnown {
		// A target's state is unknown (unreachable and never seen): delete nothing a target
		// might still need, but do not let the pile grow without bound either.
		e.capBitmaps(ctx, d, r, snap)
		return
	}
	committed := map[string]bool{}
	anyCommitted := false
	for _, tr := range r.Targets {
		committed[tr.Target] = tr.Committed
		anyCommitted = anyCommitted || tr.Committed
	}
	keep := map[string]bool{}
	// The new generation is only useful as somebody's base: if no target committed it,
	// nobody holds snapshot `snap`, and keeping it would just pile up recording bitmaps.
	if anyCommitted {
		keep[bitmapName(snap)] = true
	}
	// every target still needs the generation it last committed (or the new one if it
	// committed now); this includes runs that failed before any upload
	for target, last := range p.lastSnap {
		if committed[target] {
			last = snap
		}
		if last == "" {
			continue
		}
		if age := e.Now().Sub(snapTime(last)); last != snap && e.MaxLag > 0 && age > e.MaxLag {
			continue // too old: let it fall back to a full read
		}
		keep[bitmapName(last)] = true
	}
	for name := range p.poison {
		delete(keep, name)
	}
	// hard cap: never carry more than maxGenerations recording bitmaps (oldest dropped first)
	if len(keep) > maxGenerations {
		var names []string
		for n := range keep {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names[:len(names)-maxGenerations] {
			delete(keep, n)
		}
	}
	bms, err := e.Inc.Bitmaps(ctx, d.Obj.Ref)
	if err != nil {
		return
	}
	for _, b := range bms {
		if !strings.HasPrefix(b.Name, BitmapPrefix) || keep[b.Name] {
			continue
		}
		if err := e.Inc.DeleteBitmap(ctx, d.Obj.Ref, b.Name); err != nil {
			e.Log.Warn("deleting an old bitmap failed", "object", d.Obj.Key(), "bitmap", b.Name, "err", err)
		}
	}
}

// maxGenerations caps the incus-backup bitmaps one disk carries (each recording
// bitmap costs QEMU a little work on every guest write).
const maxGenerations = 8

// capBitmaps enforces maxGenerations when the exact set to keep is unknown: it drops the
// new generation if nothing committed it (it is nobody's base) and then the oldest ones.
func (e *Engine) capBitmaps(ctx context.Context, d Disk, r *Result, snap string) {
	bms, err := e.Inc.Bitmaps(ctx, d.Obj.Ref)
	if err != nil {
		return
	}
	any := false
	for _, tr := range r.Targets {
		any = any || tr.Committed
	}
	var names []string
	for _, b := range bms {
		if !strings.HasPrefix(b.Name, BitmapPrefix) {
			continue
		}
		if b.Name == bitmapName(snap) && !any {
			_ = e.Inc.DeleteBitmap(ctx, d.Obj.Ref, b.Name)
			continue
		}
		names = append(names, b.Name)
	}
	sort.Strings(names)
	for len(names) > maxGenerations {
		if err := e.Inc.DeleteBitmap(ctx, d.Obj.Ref, names[0]); err != nil {
			e.Log.Warn("deleting an old bitmap failed", "object", d.Obj.Key(), "bitmap", names[0], "err", err)
		}
		names = names[1:]
	}
}
