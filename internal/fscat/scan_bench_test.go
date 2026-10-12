package fscat

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// Benchmarks of the filesystem scan and of the catalog codec.
//
//	INCUS_BACKUP_BENCH_FILES  number of files of the synthetic tree (default 20000)
//	INCUS_BACKUP_BENCH_DIR    where synthetic trees are kept between runs (default: the OS temp dir);
//	                          a tree is generated once per size and reused (warm page cache)
//
// Run: go test ./internal/fscat -run '^$' -bench . -benchmem

func benchFiles() int {
	if v := os.Getenv("INCUS_BACKUP_BENCH_FILES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 20000
}

// benchTree returns a synthetic tree of n regular files: directories of about 64 entries, a
// chain of deep directories, ~1% symlinks, ~0.5% hard links, ~2% files with a user xattr (when
// the filesystem supports them), sizes 0..4 KiB plus a few files above the small-file limit.
func benchTree(b *testing.B, n int) string {
	b.Helper()
	base := os.Getenv("INCUS_BACKUP_BENCH_DIR")
	if base == "" {
		base = os.TempDir()
	}
	root := filepath.Join(base, fmt.Sprintf("ib-fscat-bench-%d", n))
	if _, err := os.Stat(filepath.Join(root, ".complete")); err == nil {
		return filepath.Join(root, "t")
	}
	_ = os.RemoveAll(root)
	t := filepath.Join(root, "t")
	r := rand.New(rand.NewSource(1))
	buf := make([]byte, 300<<10)
	r.Read(buf)
	xattrOK := true
	for i := 0; i < n; i++ {
		// two-level fan-out, plus every 1000th file deep down a chain of 24 directories
		d := filepath.Join(t, fmt.Sprintf("d%03d", i/4096), fmt.Sprintf("s%02d", (i/64)%64))
		if i%1000 == 7 {
			d = t
			for k := 0; k < 24; k++ {
				d = filepath.Join(d, fmt.Sprintf("deep%d", k))
			}
			d = filepath.Join(d, fmt.Sprintf("x%d", i/1000))
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			b.Fatal(err)
		}
		p := filepath.Join(d, fmt.Sprintf("f%07d", i))
		switch {
		case i%100 == 50 && i > 0:
			if err := os.Symlink(fmt.Sprintf("../f%07d", i-1), p); err != nil {
				b.Fatal(err)
			}
			continue
		case i%200 == 99 && i > 0:
			// hard link to the previous file (same directory unless at a boundary: then a fresh file)
			prev := filepath.Join(d, fmt.Sprintf("f%07d", i-1))
			if err := os.Link(prev, p); err == nil {
				continue
			}
		}
		size := r.Intn(4097)
		if i%997 == 3 {
			size = 260 << 10 // a "large" file
		}
		if err := os.WriteFile(p, buf[:size], 0o644); err != nil {
			b.Fatal(err)
		}
		if xattrOK && i%50 == 11 {
			if err := unix.Lsetxattr(p, "user.bench", []byte("v"+strconv.Itoa(i)), 0); err != nil {
				xattrOK = false
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".complete"), nil, 0o644); err != nil {
		b.Fatal(err)
	}
	return t
}

func BenchmarkScanFull(b *testing.B) {
	root := benchTree(b, benchFiles())
	b.ReportAllocs()
	b.ResetTimer()
	var files int
	for i := 0; i < b.N; i++ {
		res, err := Scan(root, ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		files = res.Tree.Count
	}
	b.ReportMetric(float64(files)*float64(b.N)/b.Elapsed().Seconds(), "entries/s")
}

// BenchmarkIncrementalUnchanged is the metadata part of an incremental run over an unchanged tree:
// decode the previous catalog, scan comparing against it, encode the new catalog.
func BenchmarkIncrementalUnchanged(b *testing.B) {
	root := benchTree(b, benchFiles())
	first, err := Scan(root, ScanOptions{})
	if err != nil {
		b.Fatal(err)
	}
	prevBytes, err := first.Tree.Bytes()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prev, err := Decode(prevBytes)
		if err != nil {
			b.Fatal(err)
		}
		res, err := Scan(root, ScanOptions{Prev: prev, TrustCtimeInode: true})
		if err != nil {
			b.Fatal(err)
		}
		for _, r := range res.Runs {
			if !r.Reusable {
				b.Fatal("unchanged tree has a non-reusable run")
			}
		}
		if _, err := res.Tree.Bytes(); err != nil {
			b.Fatal(err)
		}
	}
}

// synthTree builds an in-memory tree of n entries (no filesystem), for codec benchmarks.
func synthTree(n int) *Tree {
	root := &Entry{Kind: KDir, Mode: 0o755}
	var off uint64
	cnt := 1
	for d := 0; cnt < n; d++ {
		dir := &Entry{Kind: KDir, Name: fmt.Sprintf("dir%06d", d), Mode: 0o755, MtimeNs: 1700000000000000000 + int64(d), CtimeNs: 1700000000000000000, Inode: uint64(cnt)}
		root.Children = append(root.Children, dir)
		cnt++
		for f := 0; f < 100 && cnt < n; f++ {
			e := &Entry{Kind: KFile, Name: fmt.Sprintf("file-%05d.dat", f), Mode: 0o644, UID: 1000, GID: 1000,
				MtimeNs: 1700000000123456789 + int64(cnt), CtimeNs: 1700000000123456789 + int64(cnt), Inode: uint64(cnt), Size: uint64(100 + cnt%4000), PayOff: off}
			e.Sum[0], e.Sum[1] = byte(cnt), byte(cnt>>8)
			off += e.Size
			if f%50 == 1 {
				e.Xattrs = []Xattr{{Name: "user.x", Value: []byte("value")}}
			}
			dir.Children = append(dir.Children, e)
			cnt++
		}
	}
	return &Tree{Root: root, Count: cnt}
}

func codecEntries() int {
	if v := os.Getenv("INCUS_BACKUP_BENCH_ENTRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1_000_000
}

func BenchmarkCatalogEncode(b *testing.B) {
	t := synthTree(codecEntries())
	b.ReportAllocs()
	b.ResetTimer()
	var size int
	for i := 0; i < b.N; i++ {
		bs, err := t.Bytes()
		if err != nil {
			b.Fatal(err)
		}
		size = len(bs)
	}
	b.SetBytes(int64(size))
}

func BenchmarkCatalogDecode(b *testing.B) {
	bs, err := synthTree(codecEntries()).Bytes()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(bs)))
	b.ReportAllocs()
	b.ResetTimer()
	var heap uint64
	for i := 0; i < b.N; i++ {
		var m0, m1 runtime.MemStats
		b.StopTimer()
		runtime.GC()
		runtime.ReadMemStats(&m0)
		b.StartTimer()
		t, err := Decode(bs)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		runtime.GC()
		runtime.ReadMemStats(&m1)
		b.StartTimer()
		heap = m1.HeapAlloc - m0.HeapAlloc
		runtime.KeepAlive(t)
	}
	b.ReportMetric(float64(heap)/(1<<20), "MiB-live")
}
