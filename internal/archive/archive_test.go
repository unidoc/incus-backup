package archive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"archive/tar"

	"golang.org/x/sys/unix"
)

func build(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "etc", "conf.d"), 0o755))
	must(os.WriteFile(filepath.Join(root, "etc", "hostname"), []byte("benfica\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "etc", "conf.d", "a"), bytes.Repeat([]byte("x"), 100000), 0o600))
	must(os.WriteFile(filepath.Join(root, "empty"), nil, 0o640))
	must(os.Symlink("etc/hostname", filepath.Join(root, "link")))
	must(os.Symlink("/absolute/target", filepath.Join(root, "abs")))
	must(os.Link(filepath.Join(root, "etc", "hostname"), filepath.Join(root, "hardlink")))
	must(unix.Mkfifo(filepath.Join(root, "fifo"), 0o600))
	must(os.Chmod(filepath.Join(root, "etc", "conf.d"), 0o750))
	old := time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)
	must(os.Chtimes(filepath.Join(root, "etc", "hostname"), old, old))
	// a socket must be skipped, not fail the backup
	if l, err := listenUnix(filepath.Join(root, "sock")); err == nil {
		t.Cleanup(func() { l() })
	}
	return root
}

func listenUnix(path string) (func(), error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return func() { syscall.Close(fd) }, nil
}

func TestRoundTripPreservesTree(t *testing.T) {
	src := build(t)
	var buf bytes.Buffer
	st, err := Write(&buf, src)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 || st.Links != 1 || st.Symlinks != 2 || st.Special != 1 {
		t.Fatalf("%+v", st)
	}
	dst := t.TempDir()
	if err := Extract(bytes.NewReader(buf.Bytes()), dst, ExtractOptions{NoChown: os.Getuid() != 0}); err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	if _, err := Write(&a, src); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(&b, dst); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("restored tree archives differently from the original (something was not preserved)")
	}
	fi, _ := os.Lstat(filepath.Join(dst, "etc", "hostname"))
	if !fi.ModTime().Equal(time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)) {
		t.Fatalf("nanosecond mtime lost: %v", fi.ModTime())
	}
	h1, _ := os.Stat(filepath.Join(dst, "etc", "hostname"))
	h2, _ := os.Stat(filepath.Join(dst, "hardlink"))
	if !os.SameFile(h1, h2) {
		t.Fatal("hard link not restored as a hard link")
	}
	if l, _ := os.Readlink(filepath.Join(dst, "abs")); l != "/absolute/target" {
		t.Fatalf("symlink %q", l)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "etc", "conf.d")); fi.Mode().Perm() != 0o750 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
}

func TestDeterministic(t *testing.T) {
	src := build(t)
	var a, b bytes.Buffer
	_, _ = Write(&a, src)
	// reading files must not change the stream (atime must not be recorded)
	_, _ = os.ReadFile(filepath.Join(src, "etc", "hostname"))
	_, _ = Write(&b, src)
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("archive of an unchanged tree is not byte-identical")
	}
}

func TestXattrsRoundTrip(t *testing.T) {
	src := t.TempDir()
	f := filepath.Join(src, "f")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if err := unix.Lsetxattr(f, "user.test", []byte("value\x00with-nul"), 0); err != nil {
		t.Skipf("filesystem does not support user xattrs: %v", err)
	}
	var buf bytes.Buffer
	if _, err := Write(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Extract(&buf, dst, ExtractOptions{NoChown: os.Getuid() != 0}); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 64)
	n, err := unix.Lgetxattr(filepath.Join(dst, "f"), "user.test", got)
	if err != nil || string(got[:n]) != "value\x00with-nul" {
		t.Fatalf("xattr: %v %q", err, got[:n])
	}
}

func evil(t *testing.T, hdrs ...*tar.Header) *bytes.Buffer {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte("p"), int(h.Size)))
		}
	}
	_ = tw.Close()
	return &b
}

func TestExtractRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	cases := map[string]*bytes.Buffer{
		"dotdot":   evil(t, &tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}),
		"absolute": evil(t, &tar.Header{Name: "/tmp/escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}),
		"symlink-parent": evil(t,
			&tar.Header{Name: "d", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
			&tar.Header{Name: "d/file", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}),
		"hardlink-out": evil(t, &tar.Header{Name: "l", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}),
		"unknown-type": evil(t, &tar.Header{Name: "x", Typeflag: tar.TypeXGlobalHeader}),
	}
	for name, b := range cases {
		dst := t.TempDir()
		if err := Extract(b, dst, ExtractOptions{NoChown: true}); err == nil {
			t.Errorf("%s: extraction of a hostile archive succeeded", name)
		}
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("a file was written outside the destination: %v", es)
	}
}

// A hostile tar names a directory after a symlink it just created: the later chmod/chtimes must not
// act on the symlink's target (it would make an arbitrary host directory world-writable).
func TestUntrustedDirectoryEntryCannotActThroughAnExistingSymlink(t *testing.T) {
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: "inside", Mode: 0o777})
	_ = tw.WriteHeader(&tar.Header{Name: "x/", Typeflag: tar.TypeDir, Mode: 0o777})
	_ = tw.Close()
	dest := t.TempDir()
	err := Extract(&b, dest, ExtractOptions{Untrusted: true})
	if err == nil {
		t.Fatal("the symlink-then-directory trick was accepted")
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o700 {
		t.Fatalf("the victim directory was chmod'ed to %v", fi.Mode().Perm())
	}
	// "." would change the destination itself
	var c bytes.Buffer
	tw = tar.NewWriter(&c)
	_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o777})
	_ = tw.Close()
	if err := Extract(&c, dest, ExtractOptions{Untrusted: true}); err == nil {
		t.Fatal("an entry for the destination itself was accepted")
	}
}

// paxRec encodes one PAX record ("<len> k=v\n").
func paxRec(k, v string) string {
	s := " " + k + "=" + v + "\n"
	n := len(s) + 1
	for {
		t := fmt.Sprintf("%d%s", n, s)
		if len(t) == n {
			return t
		}
		n = len(t)
	}
}

func tarBlock(name string, typ byte, size int64) []byte {
	b := make([]byte, 512)
	copy(b, name)
	copy(b[100:], "0000644\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	copy(b[124:], fmt.Sprintf("%011o\x00", size))
	copy(b[136:], "00000000000\x00")
	b[156] = typ
	copy(b[257:], "ustar\x0000")
	for i := 148; i < 156; i++ {
		b[i] = ' '
	}
	var sum int
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

func padBlock(b []byte) []byte {
	for len(b)%512 != 0 {
		b = append(b, 0)
	}
	return b
}

// sparseTar is a few KiB tar holding one PAX sparse 1.0 entry that archive/tar expands to
// realSize bytes of zeros (review finding F10).
func sparseTar(realSize int64) []byte {
	pax := paxRec("GNU.sparse.major", "1") + paxRec("GNU.sparse.minor", "0") + paxRec("GNU.sparse.name", "big") + paxRec("GNU.sparse.realsize", fmt.Sprint(realSize))
	var out []byte
	out = append(out, tarBlock("PaxHeaders/big", 'x', int64(len(pax)))...)
	out = append(out, padBlock([]byte(pax))...)
	data := padBlock([]byte("1\n0\n1\n"))
	data = append(data, 'X')
	out = append(out, tarBlock("GNUSparseFile.0/big", '0', int64(len(data)))...)
	out = append(out, padBlock(data)...)
	return append(out, make([]byte, 1024)...)
}

func TestUntrustedSparseEntryIsRefusedBeforeExtracting(t *testing.T) {
	raw := sparseTar(64 << 30)
	// premise: the standard reader really reports a 64 GiB regular file for these few KiB
	h, err := tar.NewReader(bytes.NewReader(raw)).Next()
	if err != nil || h.Typeflag != tar.TypeReg || h.Size != 64<<30 {
		t.Fatalf("premise: header %+v err %v", h, err)
	}
	dest := t.TempDir()
	rep := &ExtractReport{}
	err = Extract(bytes.NewReader(raw), dest, ExtractOptions{Untrusted: true, MaxBytes: 1 << 20, Report: rep})
	if err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("a 64 GiB sparse entry must be refused, got %v", err)
	}
	if _, serr := os.Lstat(filepath.Join(dest, "big")); serr == nil {
		t.Fatal("the oversized entry must be refused BEFORE a file is created")
	}
	if rep.Bytes != 0 {
		t.Fatalf("bytes written: %d", rep.Bytes)
	}
}

func TestUntrustedEntryCountIsCapped(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i < 10; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("d%d", i), Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	if err := Extract(bytes.NewReader(b.Bytes()), t.TempDir(), ExtractOptions{Untrusted: true, MaxEntries: 5}); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("10 entries with a cap of 5 must be refused, got %v", err)
	}
	if err := Extract(bytes.NewReader(b.Bytes()), t.TempDir(), ExtractOptions{Untrusted: true, MaxEntries: 10}); err != nil {
		t.Fatalf("exactly the cap must pass: %v", err)
	}
}

func TestUntrustedEntryCapCanBeRaisedButNotPastTheCeiling(t *testing.T) {
	if DefaultMaxEntries != 1_000_000 || MaxEntriesCeiling != 10_000_000 {
		t.Fatalf("documented limits changed: %d / %d", DefaultMaxEntries, MaxEntriesCeiling)
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i < 3; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("d%d", i), Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	// a value above the ceiling is clamped, not unlimited: nothing to refuse here, but it must not error out
	if err := Extract(bytes.NewReader(b.Bytes()), t.TempDir(), ExtractOptions{Untrusted: true, MaxEntries: MaxEntriesCeiling * 100}); err != nil {
		t.Fatalf("a raised cap must work: %v", err)
	}
}

// An untrusted archive's symlinks may only lead downwards: absolute targets and any ".." are not
// created (they are reported as skipped), relative downward ones are kept; nothing is ever written
// through a link.
func TestUntrustedSymlinkTargetsMustStayInside(t *testing.T) {
	outside := t.TempDir()
	bad := []string{outside, "/etc/shadow", "../up", "a/../../up", "a/../b", ".."}
	for _, l := range bad {
		dest := t.TempDir()
		var rep ExtractReport
		err := Extract(evil(t, &tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: l, Mode: 0o777}), dest,
			ExtractOptions{Untrusted: true, Report: &rep})
		if err != nil {
			t.Errorf("%q: %v", l, err)
		}
		if _, err := os.Lstat(filepath.Join(dest, "l")); err == nil || len(rep.Skipped) != 1 {
			t.Errorf("link to %q was created (skipped=%v)", l, rep.Skipped)
		}
	}
	dest := t.TempDir()
	if err := Extract(evil(t, &tar.Header{Name: "ok", Typeflag: tar.TypeSymlink, Linkname: "sub/file", Mode: 0o777}), dest, ExtractOptions{Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(dest, "ok")); got != "sub/file" {
		t.Errorf("the downward link was not kept: %q", got)
	}
	// a write through a link planted earlier in the same archive is refused, and nothing lands outside
	dest = t.TempDir()
	err := Extract(evil(t,
		&tar.Header{Name: "d", Typeflag: tar.TypeSymlink, Linkname: "inside", Mode: 0o777},
		&tar.Header{Name: "d/pwn", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}), dest, ExtractOptions{Untrusted: true})
	if err == nil {
		t.Error("a write through a symlink was accepted")
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("something was written outside: %v", es)
	}
}
