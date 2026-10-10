package engine_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/recovery"
)

// guestPaths pulls the requested paths back out of the cloud-init user-data, the way the guest script does.
func guestPaths(userData string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^      path=(.*)$`).FindAllStringSubmatch(userData, -1) {
		out = append(out, m[1])
	}
	return out
}

// tarOf archives the requested paths of a directory that stands in for the guest filesystem.
func tarOf(t *testing.T, root string, paths []string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, p := range paths {
		base := filepath.Join(root, strings.TrimPrefix(p, "/"))
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			fi, _ := d.Info()
			hdr, _ := tar.FileInfoHeader(fi, "")
			hdr.Name = rel
			if d.IsDir() {
				hdr.Name += "/"
			}
			_ = tw.WriteHeader(hdr)
			if fi.Mode().IsRegular() {
				b, _ := os.ReadFile(path)
				_, _ = tw.Write(b)
			}
			return nil
		})
	}
	_ = tw.Close()
	return buf.Bytes()
}

func okOutput(t *testing.T, root, userData string) []byte {
	paths := guestPaths(userData)
	h := recovery.Header{Status: "ok", Partition: "/dev/sdb1", FS: "ext4", Paths: map[string]string{}}
	for _, p := range paths {
		h.Paths[p] = "ok"
	}
	return append(recovery.EncodeHeader(h), tarOf(t, root, paths)...)
}

func vmWorld(t *testing.T) (*world, *engine.Object, string) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	_, rs := w.backupVM("bosch")
	mustOK(t, rs)
	guest := t.TempDir()
	writeTree(t, guest, map[string]string{
		"etc/nginx/nginx.conf": "worker_processes 4;\n",
		"etc/hostname":         "bosch\n",
		"home/anna/notes.txt":  "remember the milk\n",
		"var/log/syslog":       strings.Repeat("log line\n", 1000),
	})
	return w, find(w.objects(), "instance/bosch/root"), guest
}

func guestOpts(dest string) engine.GuestFilesOptions {
	return engine.GuestFilesOptions{Project: "default", Name: "instance/bosch/root", Paths: []string{"/etc/nginx", "/home/anna/notes.txt"},
		Dest: dest, Image: "images:alpine/edge/cloud", Pool: "default", MaxBytes: 64 << 20}
}

func TestSingleFilesAreRecoveredFromAVMDiskThroughADisposableVM(t *testing.T) {
	w, _, guest := vmWorld(t)
	var sawDisk []byte
	w.inc.Guest = func(disk []byte, userData string) []byte {
		sawDisk = disk
		return okOutput(t, guest, userData)
	}
	dest := t.TempDir()
	res, err := w.eng.RestoreGuestFiles(context.Background(), guestOpts(dest))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "etc/nginx/nginx.conf")); string(b) != "worker_processes 4;\n" {
		t.Fatalf("nginx.conf = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "home/anna/notes.txt")); string(b) != "remember the milk\n" {
		t.Fatalf("notes = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dest, "var")); err == nil {
		t.Fatal("something that was not requested was extracted")
	}
	if res.Header.FS != "ext4" || res.Report.Files != 2 {
		t.Fatalf("%+v %+v", res.Header, res.Report)
	}
	// the guest saw exactly the backed up disk
	if !bytes.Equal(sawDisk, w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("the recovery VM did not get the disk as it was backed up")
	}
	// the VM was built to be harmless
	runs := w.inc.RecoveryRuns()
	if len(runs) != 1 || runs[0].Image != "images:alpine/edge/cloud" || !strings.HasPrefix(runs[0].Name, "ibk-rec-") {
		t.Fatalf("%+v", runs)
	}
	if !strings.Contains(runs[0].CloudInit, "path=/etc/nginx") || !strings.Contains(runs[0].CloudInit, "poweroff -f") {
		t.Fatalf("user-data: %s", runs[0].CloudInit)
	}
	if left := w.inc.Leftovers(); len(left) != 0 {
		t.Fatalf("recovery left things behind: %v", left)
	}
}

// Everything the guest filesystem could try, via the tar it produces: it all stays inside dest.
func TestHostileGuestOutputCannotEscapeOrCreateDangerousFiles(t *testing.T) {
	w, _, _ := vmWorld(t)
	w.inc.Guest = func(_ []byte, userData string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		add := func(h *tar.Header, body string) {
			h.Size = int64(len(body))
			_ = tw.WriteHeader(h)
			_, _ = tw.Write([]byte(body))
		}
		add(&tar.Header{Name: "ok/file.txt", Mode: 0o4755, Typeflag: tar.TypeReg, Uid: 0}, "fine") // setuid must be dropped
		add(&tar.Header{Name: "../../escape.txt", Mode: 0o644, Typeflag: tar.TypeReg}, "bad")
		add(&tar.Header{Name: "/abs.txt", Mode: 0o644, Typeflag: tar.TypeReg}, "bad")
		add(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, "")
		add(&tar.Header{Name: "link/passwd", Mode: 0o644, Typeflag: tar.TypeReg}, "through the link")
		add(&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3, Mode: 0o666}, "")
		add(&tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "ok/file.txt"}, "")
		_ = tw.Close()
		return append(recovery.EncodeHeader(recovery.Header{Status: "ok"}), buf.Bytes()...)
	}
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	_ = os.Mkdir(dest, 0o755)
	_, err := w.eng.RestoreGuestFiles(context.Background(), guestOpts(dest))
	if err == nil {
		t.Fatal("hostile entries were accepted without complaint")
	}
	for _, p := range []string{filepath.Join(parent, "escape.txt"), "/abs.txt", "/etc/passwd.ibk"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s was written outside the destination", p)
		}
	}
	if fi, err := os.Stat(filepath.Join(dest, "ok/file.txt")); err == nil && fi.Mode()&os.ModeSetuid != 0 {
		t.Fatal("setuid survived")
	}
	if _, err := os.Lstat(filepath.Join(dest, "dev")); err == nil {
		t.Fatal("a device node was created")
	}
	if left := w.inc.Leftovers(); len(left) != 0 {
		t.Fatalf("leftovers after a hostile guest: %v", left)
	}
}

func TestRecoveryFailuresAreReportedAndNeverLeaveScratchObjects(t *testing.T) {
	w, _, guest := vmWorld(t)
	ctx := context.Background()
	dest := t.TempDir()
	// the guest never writes a result (wrong image, cloud-init missing...)
	w.inc.Guest = nil
	if _, err := w.eng.RestoreGuestFiles(ctx, guestOpts(dest)); err == nil || !strings.Contains(err.Error(), "wrote no result") {
		t.Fatalf("silent guest: %v", err)
	}
	// the guest reports an error
	w.inc.Guest = func(_ []byte, _ string) []byte {
		return recovery.EncodeHeader(recovery.Header{Status: "error", Error: "no filesystem on the guest disk holds /etc/nginx"})
	}
	if _, err := w.eng.RestoreGuestFiles(ctx, guestOpts(dest)); err == nil || !strings.Contains(err.Error(), "no filesystem on the guest disk") {
		t.Fatalf("guest error: %v", err)
	}
	// the size limit
	w.inc.Guest = func(_ []byte, userData string) []byte { return okOutput(t, guest, userData) }
	o := guestOpts(dest)
	o.MaxBytes = 5
	if _, err := w.eng.RestoreGuestFiles(ctx, o); err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("limit: %v", err)
	}
	// Incus failing at each stage
	for _, op := range []string{"CreateVolume", "CreateRecoveryVM", "WaitStopped", "DeleteInstance", "VolumeNBD"} {
		w.inc.FailNext(op, os.ErrClosed)
		if _, err := w.eng.RestoreGuestFiles(ctx, guestOpts(t.TempDir())); err == nil {
			t.Fatalf("a failure in %s went unnoticed", op)
		}
		if left := w.inc.Leftovers(); len(left) != 0 {
			t.Fatalf("after a failure in %s: %v", op, left)
		}
	}
	// input validation never reaches Incus
	before := len(w.inc.RecoveryRuns())
	for _, bad := range [][]string{{"/etc/../../x"}, {"/etc\nrm"}, {}, {""}} {
		o := guestOpts(dest)
		o.Paths = bad
		if _, err := w.eng.RestoreGuestFiles(ctx, o); err == nil {
			t.Fatalf("paths %q accepted", bad)
		}
	}
	o = guestOpts(dest)
	o.Image = ""
	if _, err := w.eng.RestoreGuestFiles(ctx, o); err == nil {
		t.Fatal("no image accepted")
	}
	if len(w.inc.RecoveryRuns()) != before {
		t.Fatal("invalid requests created recovery VMs")
	}
}

func TestGuestFilesRefusesFilesystemObjects(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	f.backup()
	o := guestOpts(t.TempDir())
	o.Name = "volume/docs"
	if _, err := f.eng.RestoreGuestFiles(context.Background(), o); err == nil || !strings.Contains(err.Error(), "fs extract") {
		t.Fatalf("%v", err)
	}
}
