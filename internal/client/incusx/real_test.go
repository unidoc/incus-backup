package incusx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZFSSnapshotDatasetNames(t *testing.T) {
	cases := []struct {
		v    VolRef
		snap string
		want string
	}{
		{VolRef{Project: "default", Type: "container", Name: "web"}, "ibk-1", "tank/incus/containers/web@snapshot-ibk-1"},
		{VolRef{Project: "custody", Type: "container", Name: "web"}, "s", "tank/incus/containers/custody_web@snapshot-s"},
		{VolRef{Project: "custody", Type: "custom", Name: "biggi"}, "s", "tank/incus/custom/custody_biggi@snapshot-s"},
		{VolRef{Project: "default", Type: "virtual-machine", Name: "vm"}, "s", "tank/incus/virtual-machines/vm@snapshot-s"},
	}
	for _, c := range cases {
		if got := zfsSnapshotDataset("tank/incus", c.v, c.snap); got != c.want {
			t.Errorf("%+v: %s want %s", c.v, got, c.want)
		}
	}
}

// An existing but empty directory must not be taken for snapshot contents.
func TestIsMountPointRejectsPlainDirectories(t *testing.T) {
	d := filepath.Join(t.TempDir(), "snap")
	_ = os.Mkdir(d, 0o755)
	if isMountPoint(d) {
		t.Fatal("a plain directory was reported as a mount point")
	}
	if isMountPoint(filepath.Join(d, "missing")) {
		t.Fatal("a missing path was reported as a mount point")
	}
	if !isMountPoint("/proc") {
		t.Skip("/proc is not a separate mount here")
	}
}

func TestIncusNameAndVolDir(t *testing.T) {
	if incusName("default", "a") != "a" || incusName("", "a") != "a" || incusName("p", "a") != "p_a" {
		t.Fatal("project prefix")
	}
	r := &Real{varDir: "/var/lib/incus"}
	if got := r.volDir(VolRef{Pool: "default", Project: "custody", Type: "container", Name: "web"}); got != "/var/lib/incus/storage-pools/default/containers/custody_web" {
		t.Fatal(got)
	}
}
