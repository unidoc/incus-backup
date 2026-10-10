package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/chunker"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/fscat"
)

// Manifest metadata of catalog snapshots (format "fs2").
const (
	MetaFSFormat     = "fs.format"       // "2"
	MetaFSPayload    = "fs.payload_size" // bytes of file content at the start of the index
	MetaFSCatalog    = "fs.catalog_size" // bytes of catalog at the end of the index
	MetaFSEntries    = "fs.entries"
	MetaFSMode       = "fs.mode"        // "full" | "incremental"
	MetaFSLastFull   = "fs.last_full"   // RFC3339: the last snapshot in the chain that read every file
	MetaFSBase       = "fs.base"        // snapshot the reuse was based on
	MetaFSTrust      = "fs.trust_ctime" // "1" when ctime and inode were compared
	defaultFullEvery = 7 * 24 * time.Hour
)

// BackupFS captures one filesystem object (container root or filesystem volume): an Incus
// snapshot is taken and mounted read-only, its tree is walked and compared with the previous
// catalog, only changed files are read, everything is encoded once per key domain, and the
// payload plus a catalog are uploaded to every target. With Engine.FSFormat "tar" the original
// archive format is used instead.
func (e *Engine) BackupFS(ctx context.Context, o *Object, sp *Spool) *Result {
	if e.FSFormat == "tar" {
		return e.backupFSTar(ctx, o, sp)
	}
	res := e.backupFSCatalog(ctx, o, sp, false)
	if res.Err == nil && reusedChunkMissing(res) {
		// A target lacks a chunk the incremental run reused (pruned, quarantined or never there):
		// the base was not usable on that target. Do it again reading everything; deduplication
		// keeps the upload small and the repository heals.
		e.Log.Warn("a reused chunk is missing on a target; repeating the backup in full-data mode", "object", o.Key())
		res = e.backupFSCatalog(ctx, o, sp, true)
	}
	return res
}

func reusedChunkMissing(r *Result) bool {
	for _, t := range r.Targets {
		if !t.Committed && errors.Is(t.Err, ErrLocalChunkMissing) {
			return true
		}
	}
	return false
}

// fsBase is what a key domain holds from the previous run.
type fsBase struct {
	snap     string
	idx      *format.Index
	tree     *fscat.Tree
	payload  uint64
	lastFull time.Time
}

// subIndex returns the entries of idx covering [from, to), rebased to start at 0. Both ends must be
// chunk boundaries.
func subIndex(idx *format.Index, from, to uint64) (*format.Index, error) {
	i := sort.Search(len(idx.Entries), func(i int) bool { return idx.Entries[i].End > from })
	if i >= len(idx.Entries) {
		if from == to && from == idx.Size {
			return &format.Index{Kind: format.IndexDynamic}, nil
		}
		return nil, fmt.Errorf("range [%d,%d) is outside the index", from, to)
	}
	if off, _ := idx.Span(i); off != from {
		return nil, fmt.Errorf("offset %d is not a chunk boundary", from)
	}
	out := &format.Index{Kind: format.IndexDynamic, Size: to - from}
	for ; i < len(idx.Entries); i++ {
		end := idx.Entries[i].End
		if end > to {
			return nil, fmt.Errorf("offset %d is not a chunk boundary", to)
		}
		out.Entries = append(out.Entries, format.Entry{End: end - from, ID: idx.Entries[i].ID})
		if end == to {
			return out, nil
		}
	}
	return nil, fmt.Errorf("range [%d,%d) is outside the index", from, to)
}

// loadFSBase loads the previous catalog snapshot of a key domain, or returns nil with a reason.
func (e *Engine) loadFSBase(ctx context.Context, o *Object, targets []*Target) (*fsBase, string) {
	var latest string
	var holder *Target
	for _, t := range targets {
		group := t.Keys.GroupID(o.Project, o.Kind, o.Name)
		l, err := t.Client.Snapshots(ctx, group)
		if err != nil {
			return nil, fmt.Sprintf("asking %s what it has failed: %v", t.Name, err)
		}
		if len(l.Snapshots) == 0 {
			return nil, "a target has no earlier snapshot of this object"
		}
		last := l.Snapshots[len(l.Snapshots)-1]
		if last.VerifyState == "failed" {
			return nil, "the latest snapshot on " + t.Name + " failed verification"
		}
		if latest == "" || last.SnapshotID < latest {
			latest, holder = last.SnapshotID, t
		}
	}
	if holder == nil {
		return nil, "no target"
	}
	loc, err := e.Locate(ctx, holder, o.Project, o.Name, latest)
	if err != nil {
		return nil, "the previous snapshot cannot be read: " + err.Error()
	}
	if loc.Manifest.Meta[MetaFSFormat] != "2" {
		return nil, "the previous snapshot is not a catalog snapshot"
	}
	tree, payload, err := e.loadCatalog(ctx, loc)
	if err != nil {
		return nil, "the previous catalog cannot be read: " + err.Error()
	}
	b := &fsBase{snap: latest, idx: loc.Index, tree: tree, payload: payload}
	if t, err := time.Parse(time.RFC3339, loc.Manifest.Meta[MetaFSLastFull]); err == nil {
		b.lastFull = t
	}
	return b, ""
}

// loadCatalog reads and decodes the catalog of a catalog snapshot.
func (e *Engine) loadCatalog(ctx context.Context, loc *Located) (*fscat.Tree, uint64, error) {
	pay, err := strconv.ParseUint(loc.Manifest.Meta[MetaFSPayload], 10, 64)
	if err != nil {
		return nil, 0, errors.New("manifest lacks fs.payload_size")
	}
	cat, err := strconv.ParseUint(loc.Manifest.Meta[MetaFSCatalog], 10, 64)
	if err != nil || pay+cat != loc.Index.Size {
		return nil, 0, errors.New("manifest layout does not match the index")
	}
	sub, err := subIndex(loc.Index, pay, pay+cat)
	if err != nil {
		return nil, 0, err
	}
	var buf bytes.Buffer
	if err := e.stream(ctx, loc.Target, sub, func(_ int, data []byte) error { buf.Write(data); return nil }); err != nil {
		return nil, 0, err
	}
	tree, err := fscat.Decode(buf.Bytes())
	return tree, pay, err
}

// slot is one index entry of a domain being built: either known (reused) or a chunk still being encoded.
type slot struct {
	size    uint64
	id      format.ID
	pending int // sequence number of the chunk being encoded; -1 for a known entry
}

func (e *Engine) backupFSCatalog(ctx context.Context, o *Object, sp *Spool, forceFull bool) *Result {
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

	byDomain := map[string][]*Target{}
	domKeys := map[string]*format.Keys{}
	var targets []*Target
	for _, n := range o.Targets {
		t := e.Targets[n]
		targets = append(targets, t)
		byDomain[t.Domain()] = append(byDomain[t.Domain()], t)
		domKeys[t.Domain()] = t.Keys
	}
	var domList []*format.Keys
	for _, k := range domKeys {
		domList = append(domList, k)
	}
	sort.Slice(domList, func(i, j int) bool { return domList[i].DomainID < domList[j].DomainID })

	// What does each domain already hold? The catalog used for comparison is the one of the first
	// domain that has a usable base; another domain may reuse only if its base is the very same
	// snapshot (then its catalog is identical).
	bases := map[string]*fsBase{}
	reason := ""
	var primary *fsBase
	for _, k := range domList {
		b, why := e.loadFSBase(ctx, o, byDomain[k.DomainID])
		if b == nil {
			if reason == "" {
				reason = why
			}
			continue
		}
		if primary == nil {
			primary = b
		} else if b.snap != primary.snap {
			continue
		}
		bases[k.DomainID] = b
	}
	full := forceFull || primary == nil
	switch {
	case forceFull:
		reason = "full-data run requested"
	case primary == nil && reason == "":
		reason = "no previous snapshot"
	}
	every := e.FSFullEvery
	if every <= 0 {
		every = defaultFullEvery
	}
	if !full && !primary.lastFull.IsZero() && (created.Sub(primary.lastFull) >= every || primary.lastFull.After(created.Add(time.Minute))) {
		full, reason = true, fmt.Sprintf("the last full-data run is older than %s (or dated in the future: the clock moved)", every)
	}
	if !full && primary.lastFull.IsZero() {
		full, reason = true, "the previous snapshot does not record when everything was last read"
	}
	if full {
		bases = map[string]*fsBase{}
		e.Log.Info("filesystem backup reads every file", "object", o.Key(), "why", reason)
	}
	var prevTree *fscat.Tree
	if !full {
		prevTree = primary.tree
	}

	// ---- payload ---------------------------------------------------------------------------
	type job struct {
		seq  int
		data []byte
		doms []*format.Keys
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
				for _, k := range j.doms {
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
	slots := map[string][]slot{}
	seq := 0
	var readBytes, reusedBytes uint64
	// emit hands one chunk of new data to the domains that need it (the others already hold
	// their own index entries for the same bytes).
	emit := func(data []byte, doms []*format.Keys) {
		zero := bytes.Count(data, []byte{0}) == len(data)
		for _, k := range doms {
			if zero {
				slots[k.DomainID] = append(slots[k.DomainID], slot{size: uint64(len(data)), pending: -1})
			} else {
				slots[k.DomainID] = append(slots[k.DomainID], slot{size: uint64(len(data)), pending: seq})
			}
		}
		if !zero {
			jobs <- job{seq: seq, data: data, doms: doms}
			seq++
		}
	}
	// The scan hands over each run as soon as it is laid out; the run is read, chunked and queued
	// (the bounded job queue is the backpressure on the scan, and the scan's walk is bounded by
	// fscat.ScanOptions.Ahead) and then dropped, so the run layout of the whole tree is never held.
	onRun := func(run *fscat.Run) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var readDoms []*format.Keys
		reused := map[string][]slot{}
		for _, k := range domList {
			if b := bases[k.DomainID]; b != nil && run.Reusable {
				if sl, ok := reuseSlots(b, run); ok {
					reused[k.DomainID] = sl
					continue
				}
			}
			readDoms = append(readDoms, k)
		}
		for d, sl := range reused {
			slots[d] = append(slots[d], sl...)
		}
		if len(readDoms) == 0 {
			reusedBytes += run.End - run.Start
			return nil
		}
		readBytes += run.End - run.Start
		if run.Large {
			_, err := readLarge(run.Files[0], func(chunk []byte) { emit(chunk, readDoms) })
			return err
		}
		// the files of a pack are read straight into the pack buffer (no per-file copy)
		pack := make([]byte, run.End-run.Start)
		pos := uint64(0)
		for _, f := range run.Files {
			if err := readWhole(f, pack[pos:pos+f.Entry.Size]); err != nil {
				return err
			}
			pos += f.Entry.Size
		}
		emit(pack, readDoms)
		return nil
	}
	scan, perr := fscat.Scan(root, fscat.ScanOptions{Prev: prevTree, TrustCtimeInode: !e.FSNoTrustCtime, NoReuse: full, OnRun: onRun})
	if perr != nil {
		perr = fmt.Errorf("scanning %s: %w", o.Name, perr)
	}
	// the previous catalog was only needed for the comparison
	prevTree = nil
	for _, b := range bases {
		b.tree = nil
	}
	if primary != nil {
		primary.tree = nil
	}
	// ---- catalog ---------------------------------------------------------------------------
	// encoded straight into the chunker: the encoded catalog is never held whole
	var catLen int64
	if perr == nil {
		pr, pw := io.Pipe()
		go func() {
			cw := &countingWriter{w: pw}
			err := scan.Tree.Encode(cw)
			catLen = cw.n
			_ = pw.CloseWithError(err)
		}()
		sp2, err := chunker.NewSplitter(pr, chunker.Default)
		if err != nil {
			perr = err
		} else {
			for {
				c, err := sp2.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					perr = err
					break
				}
				emit(c, domList)
			}
		}
		_ = pr.CloseWithError(errors.New("catalog encoding abandoned")) // unblocks the encoder on an early exit
	}
	close(jobs)
	wg.Wait()
	if perr == nil && firstErr != nil {
		perr = firstErr
	}
	if perr != nil {
		res.Err = perr
		return res
	}

	total := scan.PayloadSize + uint64(catLen)
	lastFull := created
	if !full && primary != nil {
		lastFull = primary.lastFull
	}
	meta := map[string]string{
		MetaFSFormat: "2", MetaFSPayload: strconv.FormatUint(scan.PayloadSize, 10), MetaFSCatalog: strconv.FormatInt(catLen, 10),
		MetaFSEntries: strconv.Itoa(scan.Tree.Count), MetaFSMode: map[bool]string{true: "full", false: "incremental"}[full],
		MetaFSLastFull: lastFull.Format(time.RFC3339), MetaFSTrust: map[bool]string{true: "0", false: "1"}[e.FSNoTrustCtime],
	}
	if !full && primary != nil {
		meta[MetaFSBase] = primary.snap
	}
	// A domain that reuses nothing really read everything: its manifest says so.
	fullMeta := map[string]string{MetaFSMode: "full", MetaFSLastFull: created.Format(time.RFC3339), MetaFSBase: ""}
	p := &Prepared{Obj: o, SnapID: snap, Created: created, Size: total, Full: full, Dirty: int64(readBytes), Read: int64(readBytes),
		ByDomain: map[string]*DomainSnapshot{}, Meta: meta}
	for _, k := range domList {
		x := &format.Index{Kind: format.IndexDynamic, Size: total}
		var off uint64
		for _, sl := range slots[k.DomainID] {
			off += sl.size
			ent := format.Entry{End: off, ID: sl.id}
			if sl.pending >= 0 {
				ent.ID = ids[k.DomainID][sl.pending]
			}
			x.Entries = append(x.Entries, ent)
		}
		if off != total {
			res.Err = fmt.Errorf("internal error: the index of domain %s covers %d bytes, expected %d", k.DomainID, off, total)
			return res
		}
		prev := ""
		if b := bases[k.DomainID]; b != nil {
			prev = b.snap
		}
		ds := &DomainSnapshot{Index: x, PrevSnap: prev, Keys: k}
		if !full && bases[k.DomainID] == nil {
			ds.Meta = fullMeta
		}
		p.ByDomain[k.DomainID] = ds
	}
	p.Duration = time.Since(start)
	res.Logical, res.Dirty, res.Read, res.Duration, res.Full = int64(total), p.Dirty, p.Read, p.Duration, full
	e.Log.Info("filesystem scanned", "object", o.Key(), "files", scan.Files, "payload", scan.PayloadSize, "read", readBytes, "reused", reusedBytes, "full", full)
	res.Targets = e.UploadAll(ctx, targets, p, sp)
	return res
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// reuseSlots maps the old payload range of a reusable run onto whole chunks of the base index.
func reuseSlots(b *fsBase, run *fscat.Run) ([]slot, bool) {
	if run.OldEnd > b.payload {
		return nil, false
	}
	sub, err := subIndex(b.idx, run.OldStart, run.OldEnd)
	if err != nil {
		return nil, false
	}
	out := make([]slot, 0, len(sub.Entries))
	var prev uint64
	for _, en := range sub.Entries {
		out = append(out, slot{size: en.End - prev, id: en.ID, pending: -1})
		prev = en.End
	}
	return out, true
}

// readWhole reads a small file into b (exactly f.Entry.Size bytes) and records its digest.
func readWhole(f fscat.RunFile, b []byte) error {
	fh, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := io.ReadFull(fh, b); err != nil {
		return fmt.Errorf("%s: %w (file changed while it was being read?)", f.Rel, err)
	}
	f.Entry.Sum = sha256.Sum256(b)
	return nil
}

// readLarge reads one large file, cuts it with content-defined chunking and hands every chunk to emit.
func readLarge(f fscat.RunFile, emit func([]byte)) (int64, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	h := sha256.New()
	r := io.TeeReader(io.LimitReader(fh, int64(f.Entry.Size)), h)
	sp, err := chunker.NewSplitter(r, chunker.Default)
	if err != nil {
		return 0, err
	}
	var n int64
	for {
		c, err := sp.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, fmt.Errorf("%s: %w", f.Rel, err)
		}
		n += int64(len(c))
		emit(c)
	}
	if uint64(n) != f.Entry.Size {
		return n, fmt.Errorf("%s: read %d bytes, expected %d (file changed while it was being read?)", f.Rel, n, f.Entry.Size)
	}
	copy(f.Entry.Sum[:], h.Sum(nil))
	return n, nil
}

// ---- reading catalog snapshots -------------------------------------------------------------

// FSView is an opened catalog snapshot: its tree, and random access to its payload through the
// repository, fetching only the chunks a request needs.
type FSView struct {
	Tree    *fscat.Tree
	Loc     *Located
	Payload uint64
	e       *Engine
	payIdx  *format.Index
	ctx     context.Context

	mu    sync.Mutex
	cache map[int][]byte
	order []int
}

// IsCatalogSnapshot reports whether a located snapshot has the catalog format.
func IsCatalogSnapshot(loc *Located) bool {
	return loc.Manifest != nil && loc.Manifest.Meta[MetaFSFormat] == "2"
}

// OpenFS opens a catalog snapshot for browsing and extraction.
func (e *Engine) OpenFS(ctx context.Context, loc *Located) (*FSView, error) {
	if !IsCatalogSnapshot(loc) {
		return nil, fmt.Errorf("%s is not a catalog snapshot: it was written in the archive format and cannot be browsed (restore it whole, or back up again with the catalog format)", loc.Snapshot)
	}
	tree, pay, err := e.loadCatalog(ctx, loc)
	if err != nil {
		return nil, err
	}
	pi, err := subIndex(loc.Index, 0, pay)
	if err != nil && pay != 0 {
		return nil, err
	}
	if pi == nil {
		pi = &format.Index{Kind: format.IndexDynamic}
	}
	return &FSView{Tree: tree, Loc: loc, Payload: pay, e: e, payIdx: pi, ctx: ctx, cache: map[int][]byte{}}, nil
}

const viewCacheChunks = 8

// CachedChunks lists the payload chunks currently held in memory (tests).
func (v *FSView) CachedChunks() []int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]int(nil), v.order...)
}

func (v *FSView) chunk(i int) ([]byte, error) {
	v.mu.Lock()
	if d, ok := v.cache[i]; ok {
		v.mu.Unlock()
		return d, nil
	}
	v.mu.Unlock()
	ent := v.payIdx.Entries[i]
	_, size := v.payIdx.Span(i)
	var data []byte
	if ent.ID.IsZero() {
		data = make([]byte, size)
	} else {
		blob, err := v.Loc.Target.Client.Chunk(v.ctx, ent.ID.String())
		if err != nil {
			return nil, fmt.Errorf("fetching chunk %s: %w", ent.ID, err)
		}
		if data, err = v.Loc.Target.Keys.DecodeChunk(blob, ent.ID); err != nil {
			return nil, fmt.Errorf("chunk %s: %w", ent.ID, err)
		}
	}
	v.mu.Lock()
	v.cache[i] = data
	v.order = append(v.order, i)
	for len(v.order) > viewCacheChunks {
		delete(v.cache, v.order[0])
		v.order = v.order[1:]
	}
	v.mu.Unlock()
	return data, nil
}

// ReadAt implements io.ReaderAt over the payload.
func (v *FSView) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || uint64(off) >= v.Payload {
		return 0, io.EOF
	}
	i := sort.Search(len(v.payIdx.Entries), func(i int) bool { return v.payIdx.Entries[i].End > uint64(off) })
	n := 0
	for n < len(p) && i < len(v.payIdx.Entries) {
		start, _ := v.payIdx.Span(i)
		d, err := v.chunk(i)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], d[uint64(off)+uint64(n)-start:])
		i++
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Extract writes the selected paths (all when empty) below dest. Only chunks that hold the
// requested files' bytes are fetched.
func (v *FSView) Extract(dest string, paths []string, noChown bool) error {
	return v.Tree.Extract(dest, fscat.FromReaderAt(v), fscat.ExtractOptions{NoChown: noChown, Paths: paths})
}

// Cat copies one regular file to w, verifying its SHA-256.
func (v *FSView) Cat(w io.Writer, path string) error {
	e, err := v.Tree.Lookup(path)
	if err != nil {
		return err
	}
	if e.Kind == fscat.KHardlink {
		if e, err = v.Tree.Lookup(e.Link); err != nil {
			return err
		}
	}
	if e.Kind != fscat.KFile {
		return fmt.Errorf("%s is a %s, not a regular file", path, e.Kind)
	}
	if e.Size == 0 {
		return nil
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), io.NewSectionReader(v, int64(e.PayOff), int64(e.Size))); err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != e.Sum {
		return fmt.Errorf("%s: content does not match its recorded SHA-256", path)
	}
	return nil
}

// restoreFSCatalog restores a whole catalog snapshot, streaming the payload in order with read-ahead.
func (e *Engine) restoreFSCatalog(ctx context.Context, loc *Located, dest string, noChown bool) error {
	tree, pay, err := e.loadCatalog(ctx, loc)
	if err != nil {
		return err
	}
	pi, err := subIndex(loc.Index, 0, pay)
	if err != nil && pay != 0 {
		return err
	}
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		errc <- tree.Extract(dest, fscat.Sequential(pr), fscat.ExtractOptions{NoChown: noChown})
		_, _ = io.Copy(io.Discard, pr)
	}()
	var serr error
	if pi != nil && len(pi.Entries) > 0 {
		serr = e.stream(ctx, loc.Target, pi, func(_ int, data []byte) error {
			_, werr := pw.Write(data)
			return werr
		})
	}
	_ = pw.CloseWithError(serr)
	xerr := <-errc
	if serr != nil {
		return serr
	}
	return xerr
}
