//go:build !js

package fscat

// Regression tests for review finding F5: with NoChown the extracting user (root in the host daemon)
// owns every file, so a guest user's setuid/setgid bits and security.* xattrs must not be applied.

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/unidoc/incus-backup/internal/archive"
)

func TestNoChownAsRootNeverMakesGuestSetuidFilesSetuidRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (the host daemon runs as root): chown to uid 1000 is not possible otherwise; the non-root variant TestNoChownDropsSetIDWhenTheOwnerIsNotTheExtractingUser covers the logic")
	}
	root := t.TempDir()
	p := filepath.Join(root, "home/alice/x")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nid\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// an unprivileged guest user's own setuid file: harmless in the guest (it runs as uid 1000)
	if err := os.Chown(p, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := res.Tree.Extract(dest, Sequential(bytes.NewReader(payloadOf(t, res))), ExtractOptions{NoChown: true}); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(dest, "home/alice/x"), &st); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := archive.Write(&buf, dest); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	var hdr *tar.Header
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("x not in the delivered tar: %v", err)
		}
		if h.Name == "home/alice/x" {
			hdr = h
			break
		}
	}
	setuidRoot := st.Uid == 0 && st.Mode&syscall.S_ISUID != 0
	tarSetuidRoot := hdr.Uid == 0 && hdr.Mode&0o4000 != 0
	got := fmt.Sprintf("onHost uid=%d mode=%o | inDeliveredTar uid=%d mode=%o | setuidRoot=%v/%v", st.Uid, st.Mode&0o7777, hdr.Uid, hdr.Mode&0o7777, setuidRoot, tarSetuidRoot)
	t.Log(got)
	if setuidRoot || tarSetuidRoot {
		t.Fatalf("a uid-1000 setuid file came out setuid-root: %s", got)
	}
}

// The same logic without root: the extracting uid is injected, so a file owned by the current user
// looks like "somebody else's" file.
func TestNoChownDropsSetIDWhenTheOwnerIsNotTheExtractingUser(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, mode os.FileMode, dir bool) {
		p := filepath.Join(root, rel)
		var err error
		if dir {
			err = os.Mkdir(p, 0o755)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	mk("suid", os.ModeSetuid|0o755, false)
	mk("sgid", os.ModeSetgid|0o755, false)
	mk("sgiddir", os.ModeSetgid|0o755, true)
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pay := payloadOf(t, res)
	extract := func(uid int) string {
		old := extractingUID
		extractingUID = func() int { return uid }
		defer func() { extractingUID = old }()
		dest := t.TempDir()
		if err := res.Tree.Extract(dest, Sequential(bytes.NewReader(pay)), ExtractOptions{NoChown: true}); err != nil {
			t.Fatal(err)
		}
		return dest
	}
	mode := func(dest, rel string) os.FileMode {
		fi, err := os.Lstat(filepath.Join(dest, rel))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode()
	}
	// extracting as a different user than the catalog owner: set-id dropped, directory setgid kept
	d := extract(os.Geteuid() + 1)
	if mode(d, "suid")&os.ModeSetuid != 0 {
		t.Error("setuid survived although ownership was not restored")
	}
	if mode(d, "sgid")&os.ModeSetgid != 0 {
		t.Error("setgid file survived although ownership was not restored")
	}
	if mode(d, "sgiddir")&os.ModeSetgid == 0 {
		t.Error("a setgid directory must keep its bit")
	}
	// extracting as the owner itself keeps everything (TestExtractReproducesTheTreeExactly relies on it)
	d = extract(os.Geteuid())
	if mode(d, "suid")&os.ModeSetuid == 0 || mode(d, "sgid")&os.ModeSetgid == 0 {
		t.Error("the owner's own set-id bits must be kept")
	}
}

func TestSkipDevicesStillRestoresTheRestOfTheTree(t *testing.T) {
	root := makeTree(t) // contains a fifo; devices cannot be created without root, so only the "rest" is checked
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := res.Tree.Extract(dest, Sequential(bytes.NewReader(payloadOf(t, res))), ExtractOptions{NoChown: true, SkipDevices: true}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(dest, "var/fifo")); fi == nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("fifo lost")
	}
}
