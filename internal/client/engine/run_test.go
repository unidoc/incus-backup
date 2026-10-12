package engine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/format"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

func buildMixed(t *testing.T, w *world) {
	t.Helper()
	w.inc.AddVM("default", "bosch", map[string]int{"root": 8 * MiB}, true)
	w.inc.AddVM("default", "bureau", map[string]int{"root": 8 * MiB}, true)
	w.inc.AttachVolume("default", "bosch", "data", "data1", 12*MiB, false)
	w.inc.AttachVolume("default", "bosch", "shared", "biggi", 4*MiB, false)
	w.inc.AttachVolume("default", "bureau", "shared", "biggi", 4*MiB, false)
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(8*MiB, 1))
	w.inc.GuestWrite("default", "bureau", "root", 0, rnd(8*MiB, 2))
	w.inc.GuestWriteVol("default", "data1", 0, rnd(12*MiB, 3))
	w.inc.GuestWriteVol("default", "biggi", 0, rnd(4*MiB, 4))
	ct := t.TempDir()
	writeTree(t, ct, map[string]string{"etc/hostname": "web\n", "var/www/index.html": strings.Repeat("<html>", 50000), "empty/.keep": ""})
	w.inc.AddContainer("default", "web", ct, true)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"a.txt": "hello", "sub/b.txt": strings.Repeat("b", 400000)})
	w.inc.AddFSVolume("default", "docs", docs)
	for _, vm := range []string{"bosch", "bureau", "web"} {
		w.inc.SetConfig("default", vm, engine.KeyPolicy, "prod")
	}
	w.inc.Vols["default/docs"].Config[engine.KeyPolicy] = "prod"
}

func TestRunMixedWorldBacksUpEveryObjectOnceAndRestoresOnAFreshHost(t *testing.T) {
	w := newWorld(t, "cikarang", "amsterdam")
	w.policy("prod", []string{"cikarang", "amsterdam"}, "all")
	buildMixed(t, w)
	objs := w.objects()
	rep := w.eng.Run(context.Background(), objs, engine.RunOptions{})
	if len(rep.Skipped) != 0 {
		for _, o := range rep.Skipped {
			t.Logf("skipped: %s", engine.Describe(o))
		}
	}
	if rep.Failed() != 0 {
		for _, r := range rep.Results {
			if !r.OK() {
				t.Errorf("%s: %v %+v", r.Object.Name, r.Err, r.Targets)
			}
		}
		t.FailNow()
	}
	// 3 instances + 3 roots + data1 + biggi (ONCE) + docs = 9 objects
	if len(rep.Results) != 3+3+1+1+1 {
		var names []string
		for _, r := range rep.Results {
			names = append(names, r.Object.Name)
		}
		t.Fatalf("results=%d %v", len(rep.Results), names)
	}
	biggi := find(objs, "volume/biggi")
	for _, tn := range []string{"cikarang", "amsterdam"} {
		if w.snapCount(tn, biggi) != 1 {
			t.Fatalf("the shared volume must be one snapshot per target, %s has %d", tn, w.snapCount(tn, biggi))
		}
		if !bytes.Equal(w.fetch(tn, biggi, w.latest(tn, biggi)), w.inc.Disk2("default", "biggi")) {
			t.Fatal("shared volume content differs")
		}
		for _, n := range []string{"instance/bosch/root", "instance/bureau/root"} {
			o := find(objs, n)
			vm := strings.Split(n, "/")[1]
			if !bytes.Equal(w.fetch(tn, o, w.latest(tn, o)), w.inc.Disk("default", vm, "root")) {
				t.Fatalf("%s on %s differs", n, tn)
			}
		}
		o := find(objs, "volume/data1")
		if !bytes.Equal(w.fetch(tn, o, w.latest(tn, o)), w.inc.Disk2("default", "data1")) {
			t.Fatal("data1 differs")
		}
	}
	// the control plane knows the operational names
	w.eng.ReportInventory(context.Background(), objs)
	list, err := w.eng.Targets["cikarang"].Client.Objects(context.Background())
	if err != nil || len(list) < 9 {
		t.Fatalf("%v %d", err, len(list))
	}
	if w.inc.Calls("CreateSnapshot") != 2 || len(w.inc.Snapshots()) != 0 {
		t.Fatalf("filesystem snapshots must be created once each and all removed, left: %v", w.inc.Snapshots())
	}

	// DISASTER: a brand new host (empty Incus) with only the keys and one server.
	fresh := fake.New(t.TempDir())
	e2 := *w.eng
	e2.Inc = fresh
	e2.Targets = map[string]*engine.Target{"amsterdam": w.eng.Targets["amsterdam"]}
	res, err := e2.RestoreInstance(context.Background(), engine.RestoreOptions{Server: "amsterdam", Project: "default", Name: "bosch", Volumes: true, NoChown: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 3 {
		t.Fatalf("expected root + data1 + biggi, got %v", res.Objects)
	}
	got := fresh.Disk("default", "bosch", "root")
	if !bytes.Equal(got, w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("restored VM root disk differs from the original")
	}
	if !bytes.Equal(fresh.Disk2("default", "data1"), w.inc.Disk2("default", "data1")) || !bytes.Equal(fresh.Disk2("default", "biggi"), w.inc.Disk2("default", "biggi")) {
		t.Fatal("restored volumes differ")
	}
	if _, err := e2.RestoreInstance(context.Background(), engine.RestoreOptions{Server: "amsterdam", Project: "default", Name: "bosch"}); err == nil {
		t.Fatal("restoring over an existing instance must be refused")
	}
	res, err = e2.RestoreInstance(context.Background(), engine.RestoreOptions{Server: "amsterdam", Project: "default", Name: "web", AsName: "web-copy", NoChown: true, Start: true})
	if err != nil {
		t.Fatal(err)
	}
	dir, _, _ := fresh.FilesystemPath(context.Background(), incusx.VolRef{Project: "default", Type: "container", Name: "web-copy"})
	want := readTree(t, w.inc.FS["default/web"])
	if gotT := readTree(t, dir); len(gotT) != len(want) || gotT["etc/hostname"] != "web\n" || gotT["var/www/index.html"] != want["var/www/index.html"] {
		t.Fatalf("restored container tree differs: %d files vs %d", len(gotT), len(want))
	}
	if len(fresh.Started) != 1 || fresh.Started[0] != "default/web-copy" {
		t.Fatalf("started=%v", fresh.Started)
	}

	// deep verification decrypts everything
	vr, err := w.eng.Verify(context.Background(), w.eng.Targets["cikarang"], 0)
	if err != nil || vr.Snapshots != 9 || vr.Chunks == 0 {
		t.Fatalf("%v %+v", err, vr)
	}
}

func TestCorruptedChunkIsDetectedByVerifyAndRefusedByRestore(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	objs := w.objects()
	rep := w.eng.Run(context.Background(), objs, engine.RunOptions{})
	if rep.Failed() != 0 {
		t.Fatal("backup failed")
	}
	root := find(objs, "instance/bosch/root")
	tg := w.eng.Targets["cikarang"]
	g := tg.Keys.GroupID("default", "block", root.Name)
	ib, _ := tg.Client.Index(context.Background(), g, w.latest("cikarang", root))
	idx, _ := format.ParseIndex(ib)
	id := idx.Entries[1].ID.String()
	path := filepath.Join(w.nodes["cikarang"].Repo.Path, "chunks", id[:2], id[2:4], id)
	b, _ := os.ReadFile(path)
	b[40] ^= 1 // inside the ciphertext; the keyless trailer would also notice
	_ = os.WriteFile(path, b, 0o640)
	if _, err := w.eng.Verify(context.Background(), tg, 0); err == nil {
		t.Fatal("verify missed a corrupted chunk")
	}
	loc, err := w.eng.Locate(context.Background(), tg, "default", root.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{b: make([]byte, 20*MiB)}
	if err := w.eng.RestoreBlock(context.Background(), loc, sink, false, nil); err == nil {
		t.Fatal("restore wrote corrupted data without complaint")
	}
}

type memSink struct{ b []byte }

func (m *memSink) WriteAt(p []byte, off int64) (int, error) { return copy(m.b[off:], p), nil }

func TestPolicyResolutionPrecedence(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.eng.Policies["prod"] = api.Policy{Name: "prod", Targets: []string{"a"}, CommitRequired: "any"}
	w.eng.Policies["cold"] = api.Policy{Name: "cold"}
	w.inc.AddVM("default", "v1", map[string]int{"root": MiB}, true) // key on instance
	w.inc.AddVM("default", "v2", map[string]int{"root": MiB}, true) // excluded
	w.inc.AddVM("default", "v3", map[string]int{"root": MiB}, true) // server assignment
	w.inc.AddVM("default", "v4", map[string]int{"root": MiB}, true) // nothing
	w.inc.AddVM("default", "v5", map[string]int{"root": MiB}, true) // unknown policy
	w.inc.SetConfig("default", "v1", engine.KeyPolicy, "prod")
	w.inc.SetConfig("default", "v2", engine.KeyPolicy, "prod")
	w.inc.SetConfig("default", "v2", engine.KeyExclude, "true")
	w.inc.SetConfig("default", "v5", engine.KeyPolicy, "typo")
	w.eng.Assignments = []api.Assignment{{Project: "default", Object: "v3", Policy: "cold"}, {Project: "", Object: "", Policy: "prod"}}
	w.inc.AttachVolume("default", "v1", "d", "shared1", MiB, false)
	w.inc.AttachVolume("default", "v2", "d", "shared1", MiB, false)
	w.inc.AttachVolume("default", "v2", "e", "only-excluded", MiB, false)
	objs := w.objects()
	check := func(name, wantPolicy, skipContains string) {
		o := find(objs, name)
		if o == nil {
			t.Fatalf("%s missing", name)
		}
		if wantPolicy != "" && o.Policy != wantPolicy {
			t.Errorf("%s: policy %q (%s), want %q", name, o.Policy, o.PolicySource, wantPolicy)
		}
		if skipContains != "" && !strings.Contains(o.Skip, skipContains) {
			t.Errorf("%s: skip=%q, want containing %q", name, o.Skip, skipContains)
		}
		if skipContains == "" && o.Skip != "" {
			t.Errorf("%s unexpectedly skipped: %s", name, o.Skip)
		}
	}
	check("instance/v1/root", "prod", "")
	check("instance/v2/root", "", "excluded")
	// the host-wide server assignment ("prod") applies to v4 because nothing more specific exists
	check("instance/v4/root", "prod", "")
	check("instance/v3/root", "cold", "")
	check("instance/v5/root", "", "unknown policy")
	// a shared volume is backed up if ANY consumer selects it
	check("volume/shared1", "prod", "")
	if o := find(objs, "instance/v1/root"); o.Commit != "any" || len(o.Targets) != 1 || o.Targets[0] != "a" {
		t.Errorf("policy targets/commit not applied: %+v", o)
	}
	if o := find(objs, "instance/v3/root"); len(o.Targets) != 2 || o.Commit != "all" {
		t.Errorf("a policy without targets must use all servers with commit=all: %+v", o)
	}
	_ = time.Now
}
