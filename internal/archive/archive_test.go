package archive

import (
	"bytes"
	"os"
	"path/filepath"
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
	_ = tw.WriteHeader(&tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: victim, Mode: 0o777})
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
