//go:build !js

package fscat

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Review finding: a root extraction WITH ownership of a guest catalog (fs_extract, restore-test) must not be
// aborted by a chown/xattr the host refuses, and must never apply the guest's security.*/trusted.* attributes.
// The syscalls are injected, so this runs without root.
func TestLenientExtractSkipsRefusedOwnershipAndGuestOnlyXattrs(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"suid", "plain"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "suid"), os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pay := payloadOf(t, res)
	e, err := res.Tree.Lookup("plain")
	if err != nil {
		t.Fatal(err)
	}
	e.Xattrs = []Xattr{{Name: "security.selinux", Value: []byte("x")}, {Name: "security.capability", Value: []byte("x")},
		{Name: "trusted.overlay.opaque", Value: []byte("y")}, {Name: "user.keep", Value: []byte("k")}, {Name: "user.refused", Value: []byte("r")},
		{Name: "system.posix_acl_access", Value: []byte("a")}}

	oldC, oldX, oldU := lchown, lsetxattr, extractingUID
	defer func() { lchown, lsetxattr, extractingUID = oldC, oldX, oldU }()
	var applied []string
	lchown = func(string, int, int) error { return unix.EPERM }
	lsetxattr = func(_ string, name string, _ []byte, _ int) error {
		if name == "user.refused" {
			return unix.EINVAL
		}
		applied = append(applied, name)
		return nil
	}
	extractingUID = func() int { return os.Geteuid() + 1 } // the files look like somebody else's

	dest := t.TempDir()
	skips := &Skips{}
	if err := res.Tree.Extract(dest, Sequential(bytes.NewReader(pay)), ExtractOptions{Lenient: true, Skips: skips}); err != nil {
		t.Fatalf("a lenient extraction must survive refused chown/xattrs: %v", err)
	}
	for _, a := range applied {
		if strings.HasPrefix(a, "security.") || strings.HasPrefix(a, "trusted.") {
			t.Errorf("guest-only attribute %s was applied on the host", a)
		}
	}
	if strings.Join(applied, ",") != "user.keep,system.posix_acl_access" {
		t.Errorf("applied = %v, want user.keep and the ACL", applied)
	}
	if skips.Count < 5 || len(skips.First) != maxSkipExamples {
		t.Errorf("skips = %+v, want a count and the first %d examples", skips, maxSkipExamples)
	}
	if s := skips.Summary(); !strings.Contains(s, "skipped") || !strings.Contains(s, "first:") {
		t.Errorf("summary %q must say what was skipped", s)
	}
	fi, err := os.Lstat(filepath.Join(dest, "suid"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Error("a file whose chown was skipped kept its setuid bit")
	}

	// the strict mode (CLI) keeps failing on a refused chown, and Lenient without Skips is refused up front
	if err := res.Tree.Extract(t.TempDir(), Sequential(bytes.NewReader(pay)), ExtractOptions{}); err == nil {
		t.Error("a non-lenient extraction must still fail on a refused chown")
	}
	if err := res.Tree.Extract(t.TempDir(), Sequential(bytes.NewReader(pay)), ExtractOptions{Lenient: true}); err == nil {
		t.Error("Lenient without Skips must be refused (it would skip silently)")
	}
}
