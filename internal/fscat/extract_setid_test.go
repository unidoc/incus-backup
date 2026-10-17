//go:build linux

package fscat

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The daemon (root) is the recorded owner of a setgid file and the host refuses the chown (a gid it cannot
// map): the file is left as the daemon's own, so the group-based set-id bit must go even though the recorded
// uid equals the extracting uid.
func TestLenientSkippedChownDropsSetgidOfDaemonOwnedFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "chage")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, os.ModeSetgid|0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pay := payloadOf(t, res)
	e, err := res.Tree.Lookup("chage")
	if err != nil {
		t.Fatal(err)
	}
	oldC, oldU := lchown, extractingUID
	defer func() { lchown, extractingUID = oldC, oldU }()
	lchown = func(string, int, int) error { return unix.EINVAL }
	extractingUID = func() int { return int(e.UID) } // the daemon is the recorded owner (uid 0 in production)

	dest := t.TempDir()
	skips := &Skips{}
	err = res.Tree.Extract(dest, Sequential(bytes.NewReader(pay)), ExtractOptions{Lenient: true, Skips: skips})
	fi, serr := os.Lstat(filepath.Join(dest, "chage"))
	if serr != nil {
		t.Fatal(serr)
	}
	got := fmt.Sprintf("err=%v | chown skipped=%v | setgid kept=%v", err, skips.Count > 0, fi.Mode()&os.ModeSetgid != 0)
	if want := "err=<nil> | chown skipped=true | setgid kept=false"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A Lenient extraction counts an ENOTSUP from lsetxattr like every other refusal.
func TestLenientCountsUnsupportedXattr(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lsetxattr(p, "user.ibtest", []byte("v"), 0); err != nil {
		t.Skipf("filesystem without user xattrs: %v", err)
	}
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pay := payloadOf(t, res)
	oldX := lsetxattr
	defer func() { lsetxattr = oldX }()
	lsetxattr = func(string, string, []byte, int) error { return unix.ENOTSUP }
	skips := &Skips{}
	if err := res.Tree.Extract(t.TempDir(), Sequential(bytes.NewReader(pay)), ExtractOptions{Lenient: true, NoChown: true, Skips: skips}); err != nil {
		t.Fatal(err)
	}
	if skips.Count == 0 {
		t.Fatal("an ENOTSUP from lsetxattr must be counted in Skips under Lenient")
	}
}
