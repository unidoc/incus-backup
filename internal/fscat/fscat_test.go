//go:build !js

package fscat

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func rnd(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func write(t *testing.T, root, rel string, data []byte, mt time.Time) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)

func makeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < 40; i++ {
		write(t, root, fmt.Sprintf("etc/conf%02d", i), rnd(100+i*37, int64(i)), epoch)
	}
	for i := 0; i < 30; i++ {
		write(t, root, fmt.Sprintf("var/lib/app/data%02d", i), rnd(3000+i*113, int64(100+i)), epoch)
	}
	write(t, root, "var/db/big.bin", rnd(3<<20, 777), epoch)
	write(t, root, "var/db/medium.bin", rnd(400<<10, 778), epoch)
	write(t, root, "empty", nil, epoch)
	if err := os.Symlink("../etc/conf01", filepath.Join(root, "var/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "etc/conf02"), filepath.Join(root, "etc/conf02.hard")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "var/fifo"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "etc/conf03"), os.ModeSetuid|0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var/emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root
}

// payloadOf reads the run layout the way the engine does: the files of every run in order.
func payloadOf(t *testing.T, res *Result) []byte {
	t.Helper()
	var out []byte
	for _, r := range res.Runs {
		if uint64(len(out)) != r.Start {
			t.Fatalf("run starts at %d but the payload is %d bytes long", r.Start, len(out))
		}
		for _, f := range r.Files {
			b, err := os.ReadFile(f.Path)
			if err != nil {
				t.Fatal(err)
			}
			if uint64(len(b)) != f.Entry.Size {
				t.Fatalf("%s: %d bytes, catalog %d", f.Rel, len(b), f.Entry.Size)
			}
			f.Entry.Sum = sha256.Sum256(b)
			out = append(out, b...)
		}
		if uint64(len(out)) != r.End {
			t.Fatalf("run ends at %d but the payload is %d bytes long", r.End, len(out))
		}
	}
	return out
}

func sameTree(t *testing.T, a, b string) {
	t.Helper()
	ta, err := Scan(a, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tb, err := Scan(b, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var pa, pb []string
	ta.Tree.Walk(func(p string, _ *Entry) bool { pa = append(pa, p); return true })
	tb.Tree.Walk(func(p string, _ *Entry) bool { pb = append(pb, p); return true })
	if strings.Join(pa, "\n") != strings.Join(pb, "\n") {
		t.Fatalf("different entries:\n%v\n%v", pa, pb)
	}
	for _, p := range pa {
		ea, _ := ta.Tree.Lookup(p)
		eb, _ := tb.Tree.Lookup(p)
		if ea.Kind != eb.Kind || ea.Mode != eb.Mode || ea.MtimeNs != eb.MtimeNs || ea.Size != eb.Size || ea.Link != eb.Link || len(ea.Xattrs) != len(eb.Xattrs) {
			t.Fatalf("%s differs: %+v vs %+v", p, ea, eb)
		}
		if ea.Kind == KFile && ea.Size > 0 {
			ba, _ := os.ReadFile(filepath.Join(a, p))
			bb, _ := os.ReadFile(filepath.Join(b, p))
			if !bytes.Equal(ba, bb) {
				t.Fatalf("%s: content differs", p)
			}
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	root := makeTree(t)
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	payloadOf(t, res)
	b, err := res.Tree.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != res.Tree.Count {
		t.Fatalf("count %d vs %d", got.Count, res.Tree.Count)
	}
	b2, _ := got.Bytes()
	if !bytes.Equal(b, b2) {
		t.Fatal("encode(decode(x)) != x")
	}
	// every flipped bit anywhere is rejected: the digest covers the whole catalog
	for _, i := range []int{0, 5, 20, len(b) / 2, len(b) - 40, len(b) - 1} {
		bad := append([]byte(nil), b...)
		bad[i] ^= 0x40
		if _, err := Decode(bad); err == nil {
			t.Fatalf("a flipped bit at %d was accepted", i)
		}
	}
	if _, err := Decode(b[:len(b)-1]); err == nil {
		t.Fatal("truncated catalog accepted")
	}
}

func TestExtractReproducesTheTreeExactly(t *testing.T) {
	root := makeTree(t)
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pay := payloadOf(t, res)
	dest := t.TempDir()
	if err := res.Tree.Extract(dest, Sequential(bytes.NewReader(pay)), ExtractOptions{NoChown: true}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, root, dest)
	// hard link is a hard link again, fifo is a fifo, symlink target intact
	var s1, s2 syscall.Stat_t
	_ = syscall.Lstat(filepath.Join(dest, "etc/conf02"), &s1)
	_ = syscall.Lstat(filepath.Join(dest, "etc/conf02.hard"), &s2)
	if s1.Ino != s2.Ino {
		t.Fatal("the hard link was not restored as a hard link")
	}
	if fi, _ := os.Lstat(filepath.Join(dest, "var/fifo")); fi == nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("fifo lost")
	}
	if l, _ := os.Readlink(filepath.Join(dest, "var/link")); l != "../etc/conf01" {
		t.Fatalf("symlink %q", l)
	}
	if fi, _ := os.Stat(filepath.Join(dest, "etc/conf03")); fi.Mode()&os.ModeSetuid == 0 {
		t.Fatal("setuid bit lost")
	}
}

func TestBrowseAndPartialExtraction(t *testing.T) {
	root := makeTree(t)
	res, _ := Scan(root, ScanOptions{})
	pay := payloadOf(t, res)
	tr := res.Tree
	ents, err := tr.List("var/db")
	if err != nil || len(ents) != 2 || ents[0].Name != "big.bin" {
		t.Fatalf("%v %v", err, ents)
	}
	if _, err := tr.Lookup("../../etc/passwd"); err == nil {
		t.Fatal("escape above the root accepted")
	}
	if _, err := tr.Lookup("etc/nope"); err == nil {
		t.Fatal("missing file found")
	}
	found, _ := tr.Find("", "conf0*")
	if len(found) != 11 { // conf00..conf09 and the hard link name conf02.hard
		t.Fatalf("find: %v", found)
	}
	// extract one file and one directory; nothing else appears
	dest := t.TempDir()
	if err := tr.Extract(dest, FromReaderAt(bytes.NewReader(pay)), ExtractOptions{NoChown: true, Paths: []string{"var/db/big.bin", "/etc/conf05", "var/lib/app"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "var/db/big.bin"))
	want, _ := os.ReadFile(filepath.Join(root, "var/db/big.bin"))
	if !bytes.Equal(got, want) {
		t.Fatal("big.bin differs")
	}
	for _, p := range []string{"etc/conf05", "var/lib/app/data07"} {
		if _, err := os.Stat(filepath.Join(dest, p)); err != nil {
			t.Fatalf("%s missing: %v", p, err)
		}
	}
	for _, p := range []string{"etc/conf06", "var/db/medium.bin", "empty", "var/link"} {
		if _, err := os.Lstat(filepath.Join(dest, p)); err == nil {
			t.Fatalf("%s was extracted but not requested", p)
		}
	}
	// the parent directories carry their catalogued mode
	fi, _ := os.Stat(filepath.Join(dest, "var/lib"))
	oi, _ := os.Stat(filepath.Join(root, "var/lib"))
	if fi.Mode() != oi.Mode() {
		t.Fatalf("parent mode %v vs %v", fi.Mode(), oi.Mode())
	}
}

// A hard link whose first link is not selected is materialised as an ordinary file.
func TestExtractingOnlyTheSecondHardLinkStillGivesTheContent(t *testing.T) {
	root := makeTree(t)
	res, _ := Scan(root, ScanOptions{})
	pay := payloadOf(t, res)
	dest := t.TempDir()
	if err := res.Tree.Extract(dest, FromReaderAt(bytes.NewReader(pay)), ExtractOptions{NoChown: true, Paths: []string{"etc/conf02.hard"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "etc/conf02.hard"))
	want, _ := os.ReadFile(filepath.Join(root, "etc/conf02"))
	if !bytes.Equal(got, want) {
		t.Fatal("content of the link is wrong")
	}
}

func TestWrongPayloadIsDetectedByTheRecordedDigest(t *testing.T) {
	root := makeTree(t)
	res, _ := Scan(root, ScanOptions{})
	pay := payloadOf(t, res)
	pay[len(pay)/2] ^= 1 // the repository "returns wrong data"
	err := res.Tree.Extract(t.TempDir(), Sequential(bytes.NewReader(pay)), ExtractOptions{NoChown: true})
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt payload extracted without complaint: %v", err)
	}
	// skipping verification is explicit
	if err := res.Tree.Extract(t.TempDir(), Sequential(bytes.NewReader(pay)), ExtractOptions{NoChown: true, SkipVerify: true}); err != nil {
		t.Fatal(err)
	}
}

func TestExtractRefusesPathTraversalFromADamagedCatalog(t *testing.T) {
	tr := &Tree{Root: &Entry{Kind: KDir, Mode: 0o755}}
	tr.Root.Children = []*Entry{{Kind: KSymlink, Name: "escape", Link: "/etc", Mode: 0o777}, {Kind: KDir, Name: "d", Mode: 0o755}}
	// names with slashes cannot even be decoded
	b, _ := (&Tree{Root: &Entry{Kind: KDir, Children: []*Entry{{Kind: KFile, Name: "a/b"}}}}).Bytes()
	if _, err := Decode(b); err == nil {
		t.Fatal("a name containing a slash was decoded")
	}
	dest := t.TempDir()
	if err := tr.Extract(dest, Sequential(bytes.NewReader(nil)), ExtractOptions{NoChown: true}); err != nil {
		t.Fatal(err)
	}
	// a path that would go through the symlink is refused
	tr2 := &Tree{Root: &Entry{Kind: KDir, Children: []*Entry{{Kind: KDir, Name: "d"}}}}
	_ = tr2
	if _, err := safePath(dest, "escape/passwd"); err == nil {
		t.Fatal("traversal through a symlink was allowed")
	}
}

// ---- change detection ----------------------------------------------------------------

func scanWith(t *testing.T, root string, prev *Tree, trust bool) *Result {
	t.Helper()
	res, err := Scan(root, ScanOptions{Prev: prev, TrustCtimeInode: trust})
	if err != nil {
		t.Fatal(err)
	}
	payloadOf(t, res)
	return res
}

func reusableBytes(r *Result) (reuse, total uint64) {
	for _, run := range r.Runs {
		total += run.End - run.Start
		if run.Reusable {
			reuse += run.End - run.Start
		}
	}
	return
}

func TestUnchangedTreeIsFullyReusable(t *testing.T) {
	root := makeTree(t)
	first := scanWith(t, root, nil, true)
	if r, _ := reusableBytes(first); r != 0 {
		t.Fatal("nothing can be reusable without a previous catalog")
	}
	second := scanWith(t, root, first.Tree, true)
	r, total := reusableBytes(second)
	if r != total || total == 0 {
		t.Fatalf("unchanged tree: reusable %d of %d", r, total)
	}
	// reused runs point at the old bytes at identical offsets
	for _, run := range second.Runs {
		if run.OldStart != run.Start || run.OldEnd != run.End {
			t.Fatalf("an unchanged tree maps onto itself: %+v", run)
		}
	}
	// "full data" mode ignores the previous catalog
	res, _ := Scan(root, ScanOptions{Prev: first.Tree, TrustCtimeInode: true, NoReuse: true})
	if r, _ := reusableBytes(res); r != 0 {
		t.Fatal("NoReuse still reuses")
	}
}

func TestChangedFilesAreReadAndTheRestIsReused(t *testing.T) {
	root := makeTree(t)
	first := scanWith(t, root, nil, true)
	// a large file and one small file change
	write(t, root, "var/db/big.bin", rnd(3<<20, 999), epoch.Add(time.Hour))
	write(t, root, "var/lib/app/data05", rnd(5000, 4242), epoch.Add(time.Hour))
	second := scanWith(t, root, first.Tree, true)
	for _, run := range second.Runs {
		changed := false
		for _, f := range run.Files {
			changed = changed || f.Rel == "var/db/big.bin" || f.Rel == "var/lib/app/data05"
		}
		if run.Reusable == changed {
			t.Fatalf("run %+v: reusable=%v but changed=%v", run.Files[0].Rel, run.Reusable, changed)
		}
	}
	r, total := reusableBytes(second)
	if r == 0 || r >= total {
		t.Fatalf("reusable %d of %d", r, total)
	}
	// the new payload layout is a valid catalog too
	b, _ := second.Tree.Bytes()
	if _, err := Decode(b); err != nil {
		t.Fatal(err)
	}
}

// Content changed but size and mtime were put back: only the ctime/inode comparison notices.
func TestCtimeCatchesAChangeThatPreservedSizeAndMtime(t *testing.T) {
	root := makeTree(t)
	first := scanWith(t, root, nil, true)
	p := filepath.Join(root, "var/db/big.bin")
	data, _ := os.ReadFile(p)
	data[1<<20] ^= 0xff
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, epoch, epoch); err != nil { // forge the mtime back
		t.Fatal(err)
	}
	strict := scanWith(t, root, first.Tree, true)
	lax := scanWith(t, root, first.Tree, false)
	find := func(r *Result) Run {
		for _, run := range r.Runs {
			if run.Large && run.Files[0].Rel == "var/db/big.bin" {
				return run
			}
		}
		t.Fatal("run missing")
		return Run{}
	}
	if find(strict).Reusable {
		t.Fatal("ctime comparison failed to notice a rewritten file whose size and mtime were preserved")
	}
	if !find(lax).Reusable {
		t.Fatal("without ctime/inode the change is invisible (the documented limit of mtime-only detection): the test is supposed to show that")
	}
}

func TestMetadataOnlyChangesAreChanges(t *testing.T) {
	root := makeTree(t)
	first := scanWith(t, root, nil, true)
	if err := os.Chmod(filepath.Join(root, "var/db/medium.bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := scanWith(t, root, first.Tree, true)
	for _, run := range second.Runs {
		if run.Large && run.Files[0].Rel == "var/db/medium.bin" && run.Reusable {
			t.Fatal("a mode change must not count as unchanged")
		}
	}
}

// Packs end at names, not at byte positions: adding one small file only disturbs the packs up
// to the next name-defined boundary, the rest stays reusable.
func TestAddingASmallFileDoesNotInvalidateEveryLaterPack(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 600; i++ {
		write(t, root, fmt.Sprintf("d/f%04d", i), rnd(20<<10, int64(i)), epoch) // 12 MiB of small files
	}
	first := scanWith(t, root, nil, true)
	if len(first.Runs) < 6 {
		t.Fatalf("only %d packs", len(first.Runs))
	}
	write(t, root, "d/f0100x", rnd(20<<10, 12345), epoch.Add(time.Hour))
	second := scanWith(t, root, first.Tree, true)
	r, total := reusableBytes(second)
	if float64(r) < 0.8*float64(total) {
		t.Fatalf("one added file destroyed reuse: %d of %d bytes reusable", r, total)
	}
}

func TestZeroLengthAndSparseAreHandled(t *testing.T) {
	root := t.TempDir()
	write(t, root, "zero", nil, epoch)
	p := filepath.Join(root, "sparse")
	f, _ := os.Create(p)
	_ = f.Truncate(5 << 20)
	_, _ = f.WriteAt([]byte("tail"), 5<<20)
	f.Close()
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pay := payloadOf(t, res)
	dest := t.TempDir()
	if err := res.Tree.Extract(dest, Sequential(bytes.NewReader(pay)), ExtractOptions{NoChown: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(p)
	b, _ := os.ReadFile(filepath.Join(dest, "sparse"))
	if !bytes.Equal(a, b) {
		t.Fatal("sparse file differs")
	}
	_ = io.EOF
}

func TestExtractDoesNotFollowASymlinkThatAlreadyStandsWhereADirectoryGoes(t *testing.T) {
	victim := t.TempDir()
	_ = os.Chmod(victim, 0o700)
	dest := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(dest, "d")); err != nil {
		t.Fatal(err)
	}
	tr := &Tree{Root: &Entry{Kind: KDir, Mode: 0o755, Children: []*Entry{{Kind: KDir, Name: "d", Mode: 0o777}}}}
	if err := tr.Extract(dest, Sequential(bytes.NewReader(nil)), ExtractOptions{NoChown: true}); err == nil {
		t.Fatal("extraction went through an existing symlink")
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o700 {
		t.Fatalf("victim chmod'ed: %v", fi.Mode().Perm())
	}
}

func TestSequentialPayloadCountsSkippedBytesOnce(t *testing.T) {
	data := []byte("0123456789abcdefghij")
	p := Sequential(bytes.NewReader(data))
	r, _ := p.Open(0, 10)
	b := make([]byte, 3)
	_, _ = r.Read(b) // read only part of the first entry
	r2, err := p.Open(10, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r2)
	if string(got) != "abcde" {
		t.Fatalf("after skipping the rest of an entry the next one starts at the wrong offset: %q", got)
	}
}
