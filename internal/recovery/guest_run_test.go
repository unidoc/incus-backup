package recovery

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// guestEnv builds a fake recovery VM environment on the build machine: "partitions" are
// directories, mount is a stub that links them, and poweroff records that it was called. The
// real script runs unmodified against it.
type guestEnv struct {
	t     *testing.T
	root  string
	out   string
	bin   string
	env   []string
	power string
}

func newGuestEnv(t *testing.T, request string, parts map[string]map[string]string) *guestEnv {
	t.Helper()
	root := t.TempDir()
	g := &guestEnv{t: t, root: root, out: filepath.Join(root, "dev", "outdev"), bin: filepath.Join(root, "bin"), power: filepath.Join(root, "powered-off")}
	for _, d := range []string{"dev", "by-id", "bin", "tmp", "mnt"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// "partitions": directories (or plain files for an unsupported filesystem)
	for name, files := range parts {
		p := filepath.Join(root, "dev", name)
		if files == nil {
			if err := os.WriteFile(p, []byte("not a filesystem"), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		for rel, body := range files {
			full := filepath.Join(p, rel)
			_ = os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if len(files) == 0 {
			_ = os.MkdirAll(p, 0o755)
		}
	}
	// the whole-disk node and the output node
	if err := os.WriteFile(filepath.Join(root, "dev", "sdb"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.out, make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(filepath.Join(root, "dev", "sdb"), filepath.Join(root, "by-id", "scsi-0QEMU_QEMU_HARDDISK_incus_guestdisk"))
	_ = os.Symlink(g.out, filepath.Join(root, "by-id", "scsi-0QEMU_QEMU_HARDDISK_incus_out"))
	if err := os.WriteFile(filepath.Join(root, "request"), []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(g.bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// mount -o OPTS DEV MNT: directories mount, anything else is an unsupported filesystem
	stub("mount", `dev=$3; mnt=$4
[ -d "$dev" ] || { echo "mount: unknown filesystem type" >&2; exit 1; }
rmdir "$mnt" 2>/dev/null; rm -f "$mnt"; ln -s "$dev" "$mnt"
echo "$dev $mnt ext4 ro 0 0" >> "$IBK_PROCMOUNTS"
`)
	stub("umount", `rm -f "$1"; mkdir -p "$1"`)
	stub("poweroff", `: > "`+g.power+`"`)
	g.env = append(os.Environ(),
		"IBK_BYID="+filepath.Join(root, "by-id"), "IBK_REQ="+filepath.Join(root, "request"), "IBK_MNT="+filepath.Join(root, "mnt", "ibk"),
		"IBK_TMP="+filepath.Join(root, "tmp"), "IBK_PROCMOUNTS="+filepath.Join(root, "procmounts"), "IBK_TESTBLK=1",
		"PATH="+g.bin+":"+os.Getenv("PATH"))
	return g
}

func (g *guestEnv) run() (*Header, *tar.Reader, error) {
	g.t.Helper()
	script := filepath.Join(g.root, "guest.sh")
	if err := os.WriteFile(script, []byte(GuestScript()), 0o755); err != nil {
		g.t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Env = g.env
	if out, err := cmd.CombinedOutput(); err != nil {
		lg, _ := os.ReadFile(filepath.Join(g.root, "tmp", "ibk.log"))
		g.t.Logf("script exit: %v\n%s\nlog:\n%s", err, out, lg)
	}
	if _, err := os.Stat(g.power); err != nil {
		g.t.Fatal("the guest did not power itself off")
	}
	f, err := os.Open(g.out)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { f.Close() })
	return ParseOutput(f)
}

func untar(t *testing.T, tr *tar.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[strings.TrimPrefix(strings.TrimSuffix(h.Name, "/"), "./")] = string(b)
	}
}

func TestGuestScriptPicksThePartitionThatHoldsThePathAndPacksIt(t *testing.T) {
	g := newGuestEnv(t, "partition=0\npath=/etc/nginx\npath=/home/anna/notes.txt\npath=/nope\n", map[string]map[string]string{
		"sdb1": {"vmlinuz": "kernel"},
		"sdb2": {"etc/nginx/nginx.conf": "worker_processes 4;\n", "home/anna/notes.txt": "milk\n", "etc/hostname": "bosch\n"},
	})
	h, tr, err := g.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(h.Partition, "sdb2") || h.FS != "ext4" {
		t.Fatalf("%+v", h)
	}
	if h.Paths["/etc/nginx"] != "ok" || h.Paths["/nope"] != "missing" {
		t.Fatalf("%+v", h.Paths)
	}
	files := untar(t, tr)
	if files["etc/nginx/nginx.conf"] != "worker_processes 4;\n" || files["home/anna/notes.txt"] != "milk\n" {
		t.Fatalf("%v", files)
	}
	if _, bad := files["etc/hostname"]; bad {
		t.Fatal("an unrequested file was packed")
	}
}

func TestGuestScriptHonoursAnExplicitPartition(t *testing.T) {
	g := newGuestEnv(t, "partition=1\npath=/vmlinuz\n", map[string]map[string]string{
		"sdb1": {"vmlinuz": "kernel"}, "sdb2": {"vmlinuz": "other"},
	})
	h, tr, err := g.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(h.Partition, "sdb1") || untar(t, tr)["vmlinuz"] != "kernel" {
		t.Fatalf("%+v", h)
	}
}

func TestGuestScriptReportsEveryFailureInsteadOfFinishingSilently(t *testing.T) {
	cases := map[string]struct {
		req   string
		parts map[string]map[string]string
		want  string
	}{
		"path on no filesystem":  {"path=/etc/nginx\n", map[string]map[string]string{"sdb1": {"vmlinuz": "k"}}, "no filesystem on the guest disk holds"},
		"unsupported filesystem": {"path=/etc/x\n", map[string]map[string]string{"sdb1": nil}, "no filesystem on the guest disk holds"},
		"missing partition":      {"partition=9\npath=/x\n", map[string]map[string]string{"sdb1": {"x": "1"}}, "partition 9 does not exist"},
		"empty request":          {"partition=0\n", map[string]map[string]string{"sdb1": {"x": "1"}}, "no paths requested"},
	}
	for name, c := range cases {
		c := c
		t.Run(name, func(t *testing.T) {
			g := newGuestEnv(t, c.req, c.parts)
			_, _, err := g.run()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// A path that is a symlink inside the guest is archived AS a symlink, never followed to the host.
func TestGuestScriptDoesNotFollowGuestSymlinks(t *testing.T) {
	g := newGuestEnv(t, "path=/etc/passwd-link\n", map[string]map[string]string{"sdb1": {"etc/x": "1"}})
	if err := os.Symlink("/etc/passwd", filepath.Join(g.root, "dev", "sdb1", "etc", "passwd-link")); err != nil {
		t.Fatal(err)
	}
	_, tr, err := g.run()
	if err != nil {
		t.Fatal(err)
	}
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatal("the link entry is missing")
		}
		if strings.HasSuffix(h.Name, "passwd-link") {
			if h.Typeflag != tar.TypeSymlink || h.Linkname != "/etc/passwd" {
				t.Fatalf("the symlink was followed or altered: %+v", h)
			}
			return
		}
	}
}

func TestGuestScriptHandlesAwkwardPathsInOneArchive(t *testing.T) {
	g := newGuestEnv(t, "path=/home/u/My Documents/a b.txt\npath=/--checkpoint=1\npath=/it's \"quoted\"/f\\x.txt\npath=/plain\n", map[string]map[string]string{"sdb1": {
		"home/u/My Documents/a b.txt": "spaces", "--checkpoint=1": "dash", "it's \"quoted\"/f\\x.txt": "quotes", "plain": "p"}})
	h, tr, err := g.run()
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, tr)
	for name, want := range map[string]string{"home/u/My Documents/a b.txt": "spaces", "--checkpoint=1": "dash", "it's \"quoted\"/f\\x.txt": "quotes", "plain": "p"} {
		if files[strings.TrimPrefix(name, "./")] != want && files["./"+name] != want {
			t.Fatalf("%q lost or wrong: %v", name, files)
		}
	}
	for p, st := range h.Paths {
		if st != "ok" {
			t.Fatalf("%q: %s", p, st)
		}
	}
}

func TestGuestScriptManyPathsStillProduceAReadableHeader(t *testing.T) {
	var req strings.Builder
	files := map[string]string{}
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("data/some/long/directory/name/%03d/file-with-a-rather-long-name-%03d.txt", i, i)
		req.WriteString("path=/" + name + "\n")
		files[name] = "x"
	}
	g := newGuestEnv(t, req.String(), map[string]map[string]string{"sdb1": files})
	h, tr, err := g.run()
	if err != nil || !h.PathsOmitted {
		t.Fatalf("a long request must not damage the result header: %v %+v", err, h)
	}
	if n := len(untar(t, tr)); n < 300 {
		t.Fatalf("only %d entries in the archive (h=%v)", n, h != nil)
	}
}

func TestGuestScriptRefusesPathsBehindGuestSymlinksInsteadOfReadingTheRecoveryVMsOwnFiles(t *testing.T) {
	g := newGuestEnv(t, "path=/data/os-release\npath=/real/f\n", map[string]map[string]string{"sdb1": {"real/f": "guest file", "etc/x": "1"}})
	// guest: /data -> /etc (an absolute symlink: in the recovery VM that is the VM's own /etc)
	if err := os.Symlink("/etc", filepath.Join(g.root, "dev", "sdb1", "data")); err != nil {
		t.Fatal(err)
	}
	h, tr, err := g.run()
	if err == nil {
		// /data/os-release is first: the first path decides the partition, so the whole request is refused
		t.Fatalf("a request behind a symlink was served: %v %v", h, untar(t, tr))
	}
	g2 := newGuestEnv(t, "path=/real/f\npath=/data/os-release\n", map[string]map[string]string{"sdb1": {"real/f": "guest file"}})
	_ = os.Symlink("/etc", filepath.Join(g2.root, "dev", "sdb1", "data"))
	h, tr, err = g2.run()
	if err != nil {
		t.Fatal(err)
	}
	if h.Paths["/data/os-release"] != "symlink in path" || h.Paths["/real/f"] != "ok" {
		t.Fatalf("%v", h.Paths)
	}
	for name := range untar(t, tr) {
		if strings.Contains(name, "os-release") {
			t.Fatal("the recovery VM's own file was returned as guest data")
		}
	}
}

func TestGuestScriptFailsLoudlyWhenTarCannotReadAFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	g := newGuestEnv(t, "path=/etc\n", map[string]map[string]string{"sdb1": {"etc/hosts": "h", "etc/shadow": "s"}})
	if err := os.Chmod(filepath.Join(g.root, "dev", "sdb1", "etc", "shadow"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.run(); err == nil || !strings.Contains(err.Error(), "tar reported problems") {
		t.Fatalf("a file that could not be read went unnoticed: %v", err)
	}
}

func TestGuestScriptNumbersPartitionsNumerically(t *testing.T) {
	parts := map[string]map[string]string{}
	for i := 1; i <= 11; i++ {
		parts[fmt.Sprintf("sdb%d", i)] = map[string]string{"marker": fmt.Sprintf("part%d", i)}
	}
	g := newGuestEnv(t, "path=/marker\n", parts)
	h, tr, err := g.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(h.Partition, "sdb1") || untar(t, tr)["marker"] != "part1" {
		t.Fatalf("sdb10 was tried before sdb2: %+v", h)
	}
}
