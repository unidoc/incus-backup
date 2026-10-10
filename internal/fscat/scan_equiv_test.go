package fscat

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// equivTree builds a tree with every kind of entry the scanner distinguishes: hard links inside
// and across directories (and across the order the walk sees them), symlinks, fifos, a socket,
// xattrs (user.* where the filesystem allows them, several per file, one large), deep and wide
// directories, empty files and large files.
func equivTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	makeTreeAt(t, root)
	for i := 0; i < 300; i++ {
		write(t, root, fmt.Sprintf("wide/f%04d", i), rnd(i*13%5000, int64(i)), epoch)
	}
	d := "deep"
	for k := 0; k < 40; k++ {
		d += fmt.Sprintf("/l%02d", k)
		write(t, root, d+"/x", rnd(k*100, int64(k)), epoch)
	}
	// hard links: the later name in walk order must become the KHardlink, wherever it lives
	if err := os.Link(filepath.Join(root, "wide/f0007"), filepath.Join(root, "aaa-first-in-order")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, d, "x"), filepath.Join(root, "wide/zz-link-to-deep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("wide/f0001", filepath.Join(root, "sym")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/target", filepath.Join(root, "wide/dangling")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "wide/fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(root, "wide/sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	if err := os.MkdirAll(filepath.Join(root, "emptydir/inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("v"), 1500)
	set := func(rel, name string, val []byte) {
		if err := unix.Lsetxattr(filepath.Join(root, rel), name, val, 0); err != nil {
			t.Fatalf("this test needs user xattrs on %s: %v", root, err)
		}
	}
	set("wide/f0003", "user.b", []byte("2"))
	set("wide/f0003", "user.a", []byte("1"))
	set("wide/f0003", "user.empty", nil)
	set("wide/f0004", "user.big", big)
	set("emptydir", "user.dir", []byte("d"))
	for i := 0; i < 20; i++ {
		n := fmt.Sprintf("user.attr-with-a-long-name-%04d", i)
		set("wide/f0005", n, []byte(n))
	}
	return root
}

func makeTreeAt(t *testing.T, root string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		write(t, root, fmt.Sprintf("etc/conf%02d", i), rnd(100+i*37, int64(i)), epoch)
	}
	write(t, root, "var/db/big.bin", rnd(3<<20, 777), epoch)
	write(t, root, "var/db/medium.bin", rnd(400<<10, 778), epoch)
	write(t, root, "empty", nil, epoch)
	if err := os.Link(filepath.Join(root, "etc/conf02"), filepath.Join(root, "etc/conf02.hard")); err != nil {
		t.Fatal(err)
	}
}

func encodeOrFail(t *testing.T, tr *Tree) []byte {
	t.Helper()
	b, err := tr.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sameResult(t *testing.T, label string, want, got *Result) {
	t.Helper()
	if !bytes.Equal(encodeOrFail(t, want.Tree), encodeOrFail(t, got.Tree)) {
		t.Fatalf("%s: catalog differs from the sequential scanner's", label)
	}
	if want.Tree.Count != got.Tree.Count || want.PayloadSize != got.PayloadSize || want.Files != got.Files || want.Dirs != got.Dirs || want.Other != got.Other {
		t.Fatalf("%s: counts differ: %+v vs %+v", label, want, got)
	}
	if len(want.Runs) != len(got.Runs) {
		t.Fatalf("%s: %d runs vs %d", label, len(want.Runs), len(got.Runs))
	}
	for i := range want.Runs {
		a, b := want.Runs[i], got.Runs[i]
		if a.Start != b.Start || a.End != b.End || a.Large != b.Large || a.Reusable != b.Reusable || a.OldStart != b.OldStart || a.OldEnd != b.OldEnd || len(a.Files) != len(b.Files) {
			t.Fatalf("%s: run %d differs: %+v vs %+v", label, i, a, b)
		}
		for j := range a.Files {
			if a.Files[j].Rel != b.Files[j].Rel || a.Files[j].Path != b.Files[j].Path || !reflect.DeepEqual(*a.Files[j].Entry, *b.Files[j].Entry) {
				t.Fatalf("%s: run %d file %d differs: %+v vs %+v", label, i, j, a.Files[j], b.Files[j])
			}
		}
	}
}

// The concurrent scanner produces exactly what the sequential one did: same catalog bytes, same
// runs, same counts, for every worker count, for full and for incremental scans, and for root
// paths written in unclean forms.
func TestParallelScanEqualsSequential(t *testing.T) {
	root := equivTree(t)
	for _, size := range []int{4096, 1} {
		t.Run(fmt.Sprintf("xattr-buffer-%d", size), func(t *testing.T) {
			// a 1-byte scratch buffer forces the ERANGE path for every list and value
			old := xattrBufs
			xattrBufs = &sync.Pool{New: func() any { b := make([]byte, size); return &b }}
			defer func() { xattrBufs = old }()
			parallelEqualsSequential(t, root)
		})
	}
}

func parallelEqualsSequential(t *testing.T, root string) {
	base, err := origScan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if base.Other < 2 {
		t.Fatalf("tree lacks special entries (%d)", base.Other)
	}
	hard := 0
	base.Tree.Walk(func(_ string, e *Entry) bool {
		if e.Kind == KHardlink {
			hard++
		}
		return true
	})
	if hard != 3 {
		t.Fatalf("expected 3 hard links in the reference scan, got %d", hard)
	}
	prev, err := Decode(encodeOrFail(t, base.Tree))
	if err != nil {
		t.Fatal(err)
	}
	// change some files so an incremental scan has reusable and non-reusable runs
	write(t, root, "wide/f0100", rnd(77, 1), epoch.Add(1))
	write(t, root, "etc/new-file", rnd(500, 2), epoch)
	defer os.Remove(filepath.Join(root, "etc/new-file"))
	for _, w := range []int{1, 2, 3, 16, 64} {
		for _, r := range []string{root, root + "/", root + "/./wide/..", root + "//"} {
			for _, o := range []ScanOptions{{}, {Prev: prev, TrustCtimeInode: true}, {Prev: prev, NoReuse: true}, {SmallFile: 1000, PackMin: 2000, PackMax: 9000, PackDiv: 3}} {
				want, err := origScan(r, o)
				if err != nil {
					t.Fatal(err)
				}
				o.Workers = w
				got, err := Scan(r, o)
				if err != nil {
					t.Fatal(err)
				}
				sameResult(t, fmt.Sprintf("workers=%d root=%q prev=%v", w, r, o.Prev != nil), want, got)
			}
		}
	}
}

// Errors are reported the same way: the first failing entry in walk order wins, and a failure
// deep in the tree is not lost while workers run ahead.
func TestParallelScanReportsTheSameFirstError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable directories") // cannot make a directory unreadable for root
	}
	root := equivTree(t)
	for _, d := range []string{"deep/l00/l01/l02", "wide"} {
		if err := os.Chmod(filepath.Join(root, d), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, d), 0o755) })
	}
	_, want := origScan(root, ScanOptions{})
	if want == nil {
		t.Fatal("reference scan did not fail")
	}
	for _, w := range []int{1, 4, 32} {
		_, got := Scan(root, ScanOptions{Workers: w})
		if got == nil || got.Error() != want.Error() {
			t.Fatalf("workers=%d: error %v, sequential scanner said %v", w, got, want)
		}
	}
	if _, err := Scan(filepath.Join(root, "missing"), ScanOptions{}); err == nil {
		t.Fatal("scan of a missing root succeeded")
	}
	if _, err := Scan(filepath.Join(root, "empty"), ScanOptions{}); err == nil {
		t.Fatal("scan of a file as root succeeded")
	}
}

// origEncode is the catalog encoder as it was before (streaming through bufio and the hash), kept
// as the byte-for-byte reference of the encoding.
func origEncode(t *Tree) []byte {
	var out bytes.Buffer
	h := sha256.New()
	w := io.MultiWriter(&out, h)
	var tmp [binary.MaxVarintLen64]byte
	uv := func(v uint64) { n := binary.PutUvarint(tmp[:], v); w.Write(tmp[:n]) }
	sv := func(v int64) { n := binary.PutVarint(tmp[:], v); w.Write(tmp[:n]) }
	str := func(s string) { uv(uint64(len(s))); w.Write([]byte(s)) }
	var entry func(x *Entry)
	entry = func(x *Entry) {
		w.Write([]byte{byte(x.Kind)})
		str(x.Name)
		uv(uint64(x.Mode))
		uv(uint64(x.UID))
		uv(uint64(x.GID))
		sv(x.MtimeNs)
		sv(x.CtimeNs)
		uv(x.Inode)
		uv(uint64(len(x.Xattrs)))
		for _, a := range x.Xattrs {
			str(a.Name)
			uv(uint64(len(a.Value)))
			w.Write(a.Value)
		}
		switch x.Kind {
		case KDir:
			for _, c := range x.Children {
				entry(c)
			}
			w.Write([]byte{0})
		case KFile:
			uv(x.Size)
			if x.Size > 0 {
				uv(x.PayOff)
				w.Write(x.Sum[:])
			}
		case KSymlink, KHardlink:
			str(x.Link)
		case KChar, KBlock:
			uv(uint64(x.Major))
			uv(uint64(x.Minor))
		}
	}
	w.Write([]byte(magic))
	w.Write([]byte{version, 0, 0, 0})
	entry(t.Root)
	out.Write(h.Sum(nil))
	return out.Bytes()
}

// The encoder produces the bytes the original encoder produced, and Decode gives back a tree
// that encodes to the same bytes again (also with a wrong Count hint, and for a manual tree).
func TestCatalogEncodingIsUnchanged(t *testing.T) {
	root := equivTree(t)
	res, err := origScan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range []*Tree{res.Tree, synthTree(30000), {Root: &Entry{Kind: KDir}}, {Root: res.Tree.Root, Count: 0}, {Root: res.Tree.Root, Count: 1 << 20}} {
		want := origEncode(tr)
		got, err := tr.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatal("encoding differs from the original encoder")
		}
		var w bytes.Buffer
		if err := tr.Encode(&w); err != nil || !bytes.Equal(w.Bytes(), want) {
			t.Fatal("Encode differs from Bytes")
		}
		back, err := Decode(got)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := back.Bytes()
		if !bytes.Equal(again, want) {
			t.Fatal("decode+encode is not the identity")
		}
	}
}
