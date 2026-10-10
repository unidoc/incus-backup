package engine_test

// Memory tests: backup, restore and verification must stream with bounded buffers whatever the
// size of the object. They run real executions (fake Incus, procedural NBD export, real test
// server with PostgreSQL) and sample the Go heap and the process RSS per phase. The suite runs
// small sizes; scripts/membench.sh runs the large ones (same code, sizes from the environment).

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/memprobe"
	"github.com/unidoc/incus-backup/internal/nbd"
)

// procDisk is a disk whose content is a function of the offset: 16 equal segments, the first
// dataPerSeg bytes of each are pseudo-random (incompressible) data, the rest is a hole. Nothing
// of it is held in memory.
type procDisk struct {
	size, seg, dataPerSeg uint64
	seed                  uint64
}

func newProcDisk(size, data uint64, seed uint64) *procDisk {
	const align = 4 << 20
	seg := size / 16
	dps := data / 16 / align * align
	if dps == 0 && data > 0 {
		dps = align
	}
	return &procDisk{size: size, seg: seg, dataPerSeg: dps, seed: seed}
}

func (d *procDisk) isData(off uint64) bool { return off%d.seg < d.dataPerSeg }

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// fill writes the content of [off, off+len(p)) into p. Data/hole boundaries are 4 MiB aligned.
func (d *procDisk) fill(p []byte, off uint64) {
	for len(p) > 0 {
		// up to the next 4 MiB boundary everything is either data or hole
		n := uint64(4<<20) - off%(4<<20)
		if n > uint64(len(p)) {
			n = uint64(len(p))
		}
		q := p[:n]
		if !d.isData(off) {
			clear(q)
		} else if off%8 == 0 && n%8 == 0 {
			w := off / 8
			for i := 0; i < len(q); i += 8 {
				binary.LittleEndian.PutUint64(q[i:], splitmix(w^d.seed))
				w++
			}
		} else {
			for i := range q {
				o := off + uint64(i)
				var b [8]byte
				binary.LittleEndian.PutUint64(b[:], splitmix(o/8^d.seed))
				q[i] = b[o%8]
			}
		}
		p, off = p[n:], off+n
	}
}

func (d *procDisk) export() *nbd.Export {
	return &nbd.Export{Content: d.fill, Size: d.size, Granule: 65536, Zero: func(off, n uint64) bool { return !d.isData(off) }}
}

// procIncus is the fake Incus with one stopped VM whose root disk is a procDisk.
type procIncus struct {
	*fake.Incus
	vm   string
	disk *procDisk
}

func (p *procIncus) VolumeNBD(ctx context.Context, v incusx.VolRef, writable bool) (net.Conn, error) {
	if v.Name == p.vm && v.Type == "virtual-machine" && !writable {
		srv := &nbd.FakeServer{Exports: map[string]*nbd.Export{"": p.disk.export()}}
		return srv.Pipe(ctx), nil
	}
	return p.Incus.VolumeNBD(ctx, v, writable)
}

// checkSink is the restore destination: it compares every write with the procedural content
// and checks that the ranges never written (zero chunks on a fresh volume) are holes.
type checkSink struct {
	d       *procDisk
	mu      sync.Mutex
	buf     []byte
	next    uint64
	written uint64
	err     error
}

func (s *checkSink) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := uint64(off)
	if o < s.next {
		s.err = fmt.Errorf("write out of order at %d", o)
		return 0, s.err
	}
	s.gap(o)
	if cap(s.buf) < len(p) {
		s.buf = make([]byte, len(p))
	}
	b := s.buf[:len(p)]
	s.d.fill(b, o)
	if string(b) != string(p) {
		s.err = fmt.Errorf("restored bytes at %d differ from the source", o)
		return 0, s.err
	}
	s.next = o + uint64(len(p))
	s.written += uint64(len(p))
	return len(p), nil
}

// gap checks that [s.next, to) is a hole of the source.
func (s *checkSink) gap(to uint64) {
	for o := s.next; o < to; o += 4 << 20 {
		if s.d.isData(o) && s.err == nil {
			s.err = fmt.Errorf("data at %d was never written", o)
		}
	}
}

type memRow struct {
	size string
	res  memprobe.Result
}

// blockScenario backs up, restores and verifies one procedural VM disk and returns the
// per-phase measurements.
func blockScenario(t *testing.T, size, data uint64) []memprobe.Result {
	w := newWorld(t, "cikarang")
	w.eng.Workers = 0 // production default
	w.policy("prod", []string{"cikarang"}, "all")
	pi := &procIncus{Incus: w.inc, vm: "big", disk: newProcDisk(size, data, size)}
	w.eng.Inc = pi
	w.inc.AddVM("default", "big", map[string]int{"root": 4096}, false)
	w.inc.SetConfig("default", "big", engine.KeyPolicy, "prod")
	ctx := context.Background()
	var out []memprobe.Result

	p := memprobe.Start("backup")
	_, rs := w.backupVM("big")
	out = append(out, p.Stop())
	mustOK(t, rs)
	if rs[0].Logical != int64(size) || rs[0].Read != int64(pi.disk.dataPerSeg*16) {
		t.Fatalf("backup read %d of %d bytes, want %d of data", rs[0].Read, rs[0].Logical, pi.disk.dataPerSeg*16)
	}

	tg := w.eng.Targets["cikarang"]
	loc, err := w.eng.Locate(ctx, tg, "default", "instance/big/root", "")
	if err != nil {
		t.Fatal(err)
	}
	sink := &checkSink{d: pi.disk}
	p = memprobe.Start("restore")
	err = w.eng.RestoreBlock(ctx, loc, sink, true, nil)
	out = append(out, p.Stop())
	if err != nil || sink.err != nil {
		t.Fatalf("restore: %v / %v", err, sink.err)
	}
	sink.gap(size)
	if sink.err != nil || sink.written != pi.disk.dataPerSeg*16 {
		t.Fatalf("restore wrote %d bytes (want %d): %v", sink.written, pi.disk.dataPerSeg*16, sink.err)
	}
	loc = nil

	p = memprobe.Start("verify")
	vr, err := w.eng.Verify(ctx, tg, 0)
	out = append(out, p.Stop())
	if err != nil || vr.Bytes != pi.disk.dataPerSeg*16 {
		t.Fatalf("verify: %v (%d bytes)", err, vr.Bytes)
	}

	p = memprobe.Start("restore-test deep")
	rt, err := w.eng.RestoreTest(ctx, engine.RestoreTestOptions{Project: "default", Name: "instance/big/root", Mode: jobspec.ModeDeep})
	out = append(out, p.Stop())
	if err != nil || !rt.Passed {
		t.Fatalf("deep restore test: %v", err)
	}
	return out
}

func gib(f float64) uint64 { return uint64(math.Round(f*1024)) << 20 }

func sizeLabel(b uint64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%g GiB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", b>>20)
}

// Bounds for the default settings (4 encode workers, 16 upload workers): the capture holds a
// read-ahead batch of 8 chunks, 2 x workers queued and one being encoded per worker; the upload
// one spooled blob per upload worker on the client and one request body per upload on the
// server (plus the HTTP/2 windows); restore a read-ahead window of 8 chunks. Server and client
// run in this one process, so the numbers are client + server (measured: live <= ~175 MiB,
// heap in use <= ~400 MiB with the collector's headroom).
const (
	maxBlockHeapDelta = 640 << 20
	maxBlockLiveDelta = 320 << 20
	// the live heap a phase needs may not grow with the object: at most this much from the
	// smallest to the largest size measured
	maxBlockGrowth = 96 << 20
)

// TestBlockBackupRestoreVerifyMemoryIsFlat backs up a VM disk with 25 % data and 75 % holes,
// restores and verifies it at several sizes and requires the peak heap of every phase to stay
// under a fixed bound that does not grow with the disk.
func TestBlockBackupRestoreVerifyMemoryIsFlat(t *testing.T) {
	sizes := memprobe.Sizes("IB_MEMBENCH_DISK_GIB", []float64{0.25, 1})
	byPhase := map[string][]memprobe.Result{}
	var rows []string
	for _, s := range sizes {
		size := gib(s)
		res := blockScenario(t, size, size/4)
		for _, r := range res {
			byPhase[r.Phase] = append(byPhase[r.Phase], r)
			rows = append(rows, r.Row("block 25% data", sizeLabel(size)))
		}
	}
	t.Log("\n" + memprobe.Header + "\n" + strings.Join(rows, "\n"))
	memprobe.Report(rows...)
	checkFlat(t, byPhase, maxBlockHeapDelta, maxBlockLiveDelta, maxBlockGrowth)
}

// TestSparseDiskMemoryPerEntry measures the per-chunk-position costs (index entries, capture
// bookkeeping, restore read-ahead) on disks that are almost entirely holes, so the number of
// index entries grows while the data stays the same. These costs ARE linear in the entries (the
// index is held: 40 bytes per entry on each side, plus capture bookkeeping). The slope per entry
// is reported (at suite sizes it is inside the sampling noise; scripts/membench.sh measures it at
// 1M entries); the test bounds the total and that a sparse restore allocates nothing per hole.
func TestSparseDiskMemoryPerEntry(t *testing.T) {
	sizes := memprobe.Sizes("IB_MEMBENCH_SPARSE_GIB", []float64{64, 1024})
	var rows []string
	var runs [][]memprobe.Result
	for _, s := range sizes {
		size := gib(s)
		res := blockScenario(t, size, 64<<20)
		runs = append(runs, res)
		for _, r := range res {
			rows = append(rows, r.Row("block sparse (64 MiB data)", sizeLabel(size)))
			if r.HeapDelta > maxBlockHeapDelta {
				t.Errorf("%s at %s: heap in use grew by %s MiB, bound %s MiB", r.Phase, sizeLabel(size), memprobe.MiB(r.HeapDelta), memprobe.MiB(maxBlockHeapDelta))
			}
			if r.Phase == "restore" && r.TotalAlloc > 1<<30 {
				t.Errorf("restoring a sparse disk allocated %s MiB: zero chunks must not be materialised", memprobe.MiB(r.TotalAlloc))
			}
		}
	}
	if len(sizes) >= 2 {
		e0, e1 := float64(gib(sizes[0])/format.FixedChunkSize), float64(gib(sizes[len(sizes)-1])/format.FixedChunkSize)
		for i, r1 := range runs[len(runs)-1] {
			r0 := runs[0][i]
			per := (float64(r1.LiveDelta) - float64(r0.LiveDelta)) / (e1 - e0)
			rows = append(rows, fmt.Sprintf("| block sparse | slope %g..%g GiB | %s | | | %.0f B/entry | | %.0f B/entry | |", sizes[0], sizes[len(sizes)-1], r1.Phase, per,
				(float64(r1.TotalAlloc)-float64(r0.TotalAlloc))/(e1-e0)))
		}
	}
	t.Log("\n" + memprobe.Header + "\n" + strings.Join(rows, "\n"))
	memprobe.Report(rows...)
}

// checkFlat requires every phase to stay under the heap bound (heap in use, including the
// garbage the collector lets accumulate), under the live bound (what is really retained) and the
// live heap not to grow by more than growth from the smallest to the largest size.
func checkFlat(t *testing.T, byPhase map[string][]memprobe.Result, heapBound, liveBound, growth uint64) {
	t.Helper()
	for phase, rs := range byPhase {
		lo, hi := rs[0].LiveDelta, rs[0].LiveDelta
		for _, r := range rs {
			if r.HeapDelta > heapBound {
				t.Errorf("%s: heap in use grew by %s MiB, bound %s MiB", phase, memprobe.MiB(r.HeapDelta), memprobe.MiB(heapBound))
			}
			if r.LiveDelta > liveBound {
				t.Errorf("%s: live heap grew by %s MiB, bound %s MiB", phase, memprobe.MiB(r.LiveDelta), memprobe.MiB(liveBound))
			}
			lo, hi = min(lo, r.LiveDelta), max(hi, r.LiveDelta)
		}
		if hi-lo > growth {
			t.Errorf("%s: live heap is not flat in the object size (%s .. %s MiB)", phase, memprobe.MiB(lo), memprobe.MiB(hi))
		}
	}
}

// ---- filesystem -----------------------------------------------------------------------------

func makeTree(t *testing.T, root string, files int) {
	t.Helper()
	var b [512]byte
	for i := 0; i < files; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", i/1000), fmt.Sprintf("e%02d", i%1000/50))
		if i%50 == 0 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		binary.LittleEndian.PutUint64(b[:], splitmix(uint64(i)))
		n := 64 + int(splitmix(uint64(i))%448)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%07d.txt", i)), b[:n], 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fsScenario(t *testing.T, files int) []memprobe.Result {
	w := newWorld(t, "cikarang")
	w.eng.Workers = 0
	w.policy("prod", []string{"cikarang"}, "all")
	dir := t.TempDir()
	makeTree(t, dir, files)
	w.inc.AddFSVolume("default", "many", dir)
	w.inc.Vols["default/many"].Config[engine.KeyPolicy] = "prod"
	obj := find(w.objects(), "volume/many")
	ctx := context.Background()
	backup := func(phase string) memprobe.Result {
		sp, err := engine.NewSpool(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Close()
		p := memprobe.StartTight(phase)
		res := w.eng.BackupFS(ctx, obj, sp)
		r := p.Stop()
		if !res.OK() {
			t.Fatalf("%s: %v %+v", phase, res.Err, res.Targets)
		}
		return r
	}
	var out []memprobe.Result
	out = append(out, backup("backup (full)"))
	w.now = w.now.Add(3600e9)
	out = append(out, backup("backup (incremental)"))

	tg := w.eng.Targets["cikarang"]
	loc, err := w.eng.Locate(ctx, tg, "default", "volume/many", "")
	if err != nil {
		t.Fatal(err)
	}
	p := memprobe.StartTight("open + ls")
	v, err := w.eng.OpenFS(ctx, loc)
	if err == nil {
		_, err = v.Tree.List("d000/e00")
	}
	out = append(out, p.Stop())
	if err != nil {
		t.Fatal(err)
	}
	v = nil
	dest := t.TempDir()
	p = memprobe.StartTight("extract (restore)")
	err = w.eng.RestoreFS(ctx, loc, dest, true)
	out = append(out, p.Stop())
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := countFiles(dest); n != files {
		t.Fatalf("restored %d files, want %d", n, files)
	}
	return out
}

func countFiles(dir string) (files, dirs int, err error) {
	err = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files++
		} else if d.IsDir() {
			dirs++
		}
		return nil
	})
	return
}

// Catalog snapshots hold the tree in memory (the catalog of a tree is linear in its entries).
// The cost per entry is the slope between two tree sizes (fixed buffers cancel out), measured
// with a tight collector; this bounds it so a regression (a second copy of the tree, per-file
// buffers kept, the run layout held whole) fails.
const maxFSBytesPerEntry = 1500

// TestFilesystemCatalogMemoryPerEntry backs up (full and incremental), browses and extracts trees
// of many small files and reports the heap per catalog entry.
func TestFilesystemCatalogMemoryPerEntry(t *testing.T) {
	sizes := memprobe.Sizes("IB_MEMBENCH_FILES", []float64{10000, 40000})
	var rows []string
	var runs [][]memprobe.Result
	for _, s := range sizes {
		files := int(s)
		res := fsScenario(t, files)
		runs = append(runs, res)
		for _, r := range res {
			rows = append(rows, r.Row("fs catalog", fmt.Sprintf("%d files", files)))
		}
	}
	if len(sizes) >= 2 {
		n0, n1 := sizes[0], sizes[len(sizes)-1]
		for i, r1 := range runs[len(runs)-1] {
			r0 := runs[0][i]
			per := (float64(r1.HeapDelta) - float64(r0.HeapDelta)) / (n1 - n0)
			rows = append(rows, fmt.Sprintf("| fs catalog | slope %g..%g files | %s | | %.0f B/entry | | | %.0f B/entry | |", n0, n1, r1.Phase, per,
				(float64(r1.TotalAlloc)-float64(r0.TotalAlloc))/(n1-n0)))
			if per > maxFSBytesPerEntry {
				t.Errorf("%s: %.0f bytes of heap per catalog entry, bound %d", r1.Phase, per, maxFSBytesPerEntry)
			}
		}
	}
	t.Log("\n" + memprobe.Header + "\n" + strings.Join(rows, "\n"))
	memprobe.Report(rows...)
}
