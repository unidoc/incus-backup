package recovery

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGuestScriptIsValidShellAndNeverWritesToTheGuestDisk(t *testing.T) {
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(GuestScript())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	s := GuestScript()
	// every mount of the guest disk is read-only, and nothing writes to the guest disk device
	if strings.Contains(s, "mount -o rw") || strings.Contains(s, "of=\"$DISK\"") || strings.Contains(s, "of=$DISK") {
		t.Fatal("the guest script writes to the guest disk")
	}
	for _, opts := range []string{"ro,nosuid,nodev,noexec,noload", "ro,nosuid,nodev,noexec,norecovery", "ro,nosuid,nodev,noexec"} {
		if !strings.Contains(s, opts) {
			t.Fatalf("missing read-only mount option set %q", opts)
		}
	}
	if !strings.Contains(s, "poweroff -f") {
		t.Fatal("the guest must power itself off")
	}
}

func TestUserDataIsValidCloudConfigCarryingTheScriptAndTheRequest(t *testing.T) {
	ud, err := UserData(Request{Paths: []string{"/etc/nginx", "/home/anna/../anna/notes.txt"}, Partition: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("not a cloud-config")
	}
	var doc struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
		Runcmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, ud)
	}
	got := map[string]string{}
	for _, f := range doc.WriteFiles {
		got[f.Path] = f.Content
	}
	if strings.TrimSpace(got["/usr/local/sbin/ibk-recover"]) != strings.TrimSpace(GuestScript()) {
		t.Fatal("the script did not survive the YAML round trip")
	}
	req := got["/etc/ibk-request"]
	for _, want := range []string{"partition=2", "path=/etc/nginx", "path=/home/anna/notes.txt"} {
		if !strings.Contains(req, want) {
			t.Fatalf("request lacks %q:\n%s", want, req)
		}
	}
	if len(doc.Runcmd) != 1 || doc.Runcmd[0][0] != "/usr/local/sbin/ibk-recover" {
		t.Fatalf("runcmd %v", doc.Runcmd)
	}
}

func TestPathValidation(t *testing.T) {
	for _, bad := range [][]string{nil, {""}, {"/a\nb"}, {"/a\x00"}, {"/../x"}, {"x/../../y"}, {"/"}, {"/a/.."}} {
		if _, err := CleanPaths(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	got, err := CleanPaths([]string{"etc//nginx/", "/a/./b/../c"})
	if err != nil || got[0] != "/etc/nginx" || got[1] != "/a/c" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := UserData(Request{Paths: []string{"/x"}, Partition: -1}); err == nil {
		t.Fatal("negative partition accepted")
	}
}

func TestParseOutput(t *testing.T) {
	ok := append(EncodeHeader(Header{Status: "ok", FS: "ext4"}), []byte("tar-bytes")...)
	h, tr, err := ParseOutput(bytes.NewReader(ok))
	if err != nil || h.FS != "ext4" || tr == nil {
		t.Fatalf("%v %+v", err, h)
	}
	if _, _, err := ParseOutput(bytes.NewReader(make([]byte, 8192))); err == nil || !strings.Contains(err.Error(), "wrote no result") {
		t.Fatalf("empty output: %v", err)
	}
	if _, _, err := ParseOutput(bytes.NewReader(EncodeHeader(Header{Status: "error", Error: "boom"}))); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error status: %v", err)
	}
	bad := EncodeHeader(Header{Status: "ok"})
	copy(bad[len(headerMagic):], "{{{not json")
	if _, _, err := ParseOutput(bytes.NewReader(bad)); err == nil {
		t.Fatal("damaged header accepted")
	}
}
