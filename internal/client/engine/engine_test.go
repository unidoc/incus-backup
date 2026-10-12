package engine_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/pki"
	"github.com/unidoc/incus-backup/internal/remote"
	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) {
	remote.RetryDelay = time.Millisecond // dead-server tests would otherwise sleep through every retry
	testdb.AtExit(s3test.Start())
	testdb.Main(m)
}

const MiB = 1 << 20

type world struct {
	t     *testing.T
	inc   *fake.Incus
	eng   *engine.Engine
	nodes map[string]*testsupport.Node
	keys  *format.Keys
	sp    *engine.Spool
	now   time.Time
	creds map[string]testsupport.Credentials
}

func masterKeys(seed byte) *format.Keys {
	m := make([]byte, 32)
	for i := range m {
		m[i] = seed + byte(i)
	}
	k, _ := format.DeriveKeys(m)
	return k
}

func newWorld(t *testing.T, targets ...string) *world {
	t.Helper()
	w := &world{t: t, inc: fake.New(t.TempDir()), nodes: map[string]*testsupport.Node{}, keys: masterKeys(1), creds: map[string]testsupport.Credentials{},
		now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &engine.Engine{Inc: w.inc, Targets: map[string]*engine.Target{}, Host: "benfica", SpoolDir: t.TempDir(), MaxFreeze: time.Minute, MaxLag: 7 * 24 * time.Hour, Workers: 3,
		Log: log, Policies: map[string]api.Policy{}}
	e.Now = func() time.Time { return w.now }
	for _, name := range targets {
		// a target named s3-* is a server whose repository lives in an S3 bucket
		newNode := testsupport.NewNode
		if strings.HasPrefix(name, "s3-") {
			newNode = testsupport.NewS3Node
		}
		n := newNode(t)
		w.nodes[name] = n
		cr := n.Enrol("benfica", api.ScopeBackup)
		w.creds[name] = cr
		e.Targets[name] = &engine.Target{Name: name, Client: n.Client(cr, w.keys.DomainID), Keys: w.keys}
	}
	w.eng = e
	sp, err := engine.NewSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sp.Close)
	w.sp = sp
	return w
}

func (w *world) policy(name string, targets []string, commit string) {
	w.eng.Policies[name] = api.Policy{Name: name, Targets: targets, CommitRequired: commit}
}

func (w *world) objects() []*engine.Object {
	w.t.Helper()
	objs, err := w.eng.Discover(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	w.eng.Resolve(objs)
	return objs
}

func find(objs []*engine.Object, name string) *engine.Object {
	for _, o := range objs {
		if o.Name == name {
			return o
		}
	}
	return nil
}

func rnd(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// fetch reassembles an object's content from a target the way restore does.
func (w *world) fetch(target string, obj *engine.Object, snap string) []byte {
	w.t.Helper()
	ctx := context.Background()
	tg := w.eng.Targets[target]
	g := tg.Keys.GroupID(obj.Project, obj.Kind, obj.Name)
	ib, err := tg.Client.Index(ctx, g, snap)
	if err != nil {
		w.t.Fatalf("index of %s on %s: %v", snap, target, err)
	}
	idx, err := format.ParseIndex(ib)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []byte
	for i, e := range idx.Entries {
		_, n := idx.Span(i)
		if e.ID.IsZero() {
			out = append(out, make([]byte, n)...)
			continue
		}
		blob, err := tg.Client.Chunk(ctx, e.ID.String())
		if err != nil {
			w.t.Fatalf("chunk %s missing on %s: %v", e.ID, target, err)
		}
		pt, err := tg.Keys.DecodeChunk(blob, e.ID)
		if err != nil {
			w.t.Fatal(err)
		}
		out = append(out, pt...)
	}
	mb, err := tg.Client.Manifest(ctx, g, snap)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := tg.Keys.OpenManifest(mb, g, snap); err != nil {
		w.t.Fatalf("manifest: %v", err)
	}
	return out
}

func (w *world) latest(target string, obj *engine.Object) string {
	tg := w.eng.Targets[target]
	l, err := tg.Client.Snapshots(context.Background(), tg.Keys.GroupID(obj.Project, obj.Kind, obj.Name))
	if err != nil {
		w.t.Fatal(err)
	}
	if len(l.Snapshots) == 0 {
		return ""
	}
	return l.Snapshots[len(l.Snapshots)-1].SnapshotID
}

func (w *world) snapCount(target string, obj *engine.Object) int {
	tg := w.eng.Targets[target]
	l, _ := tg.Client.Snapshots(context.Background(), tg.Keys.GroupID(obj.Project, obj.Kind, obj.Name))
	return len(l.Snapshots)
}

func (w *world) newChunks(target string) int64 {
	var n int64
	_ = w.nodes[target].Svc.DB.Pool().QueryRow(context.Background(), `SELECT COALESCE(sum(chunks_new),0) FROM sessions`).Scan(&n)
	return n
}

func (w *world) backupVM(vm string) (string, []*engine.Result) {
	w.t.Helper()
	objs := w.objects()
	var disks []engine.Disk
	var inst incusx.Instance
	for _, o := range objs {
		if o.Instance != nil && o.Instance.Name == vm && o.Kind == engine.KindBlock {
			disks = append(disks, engine.Disk{Obj: o, Device: o.Device})
			inst = *o.Instance
		}
	}
	for _, o := range objs {
		if o.Kind == engine.KindBlock && o.Instance == nil {
			for _, a := range o.Attached {
				if a.VM == vm && len(o.SharedBy) <= 1 {
					disks = append(disks, engine.Disk{Obj: o, Device: a.Device})
				}
			}
		}
	}
	inst2, _ := w.eng.Inc.Instances(context.Background(), "default")
	for _, i := range inst2 {
		if i.Name == vm {
			inst = i
		}
	}
	sp, err := engine.NewSpool(w.t.TempDir()) // a fresh spool per run, as in production
	if err != nil {
		w.t.Fatal(err)
	}
	defer sp.Close()
	return w.eng.BackupVM(context.Background(), inst, disks, sp)
}

func mustOK(t *testing.T, rs []*engine.Result) {
	t.Helper()
	for _, r := range rs {
		if !r.OK() {
			t.Fatalf("%s failed: err=%v targets=%+v", r.Object.Name, r.Err, r.Targets)
		}
	}
}

func setupVM(w *world, seed int64) {
	w.inc.AddVM("default", "bosch", map[string]int{"root": 20 * MiB}, true)
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(20*MiB, seed))
}

func TestFullThenIncrementalSendsOnlyDirtyChunks(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")

	snap1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if !rs[0].Full || rs[0].Dirty != 20*MiB {
		t.Fatalf("first backup must be a full read: %+v", rs[0])
	}
	if !bytes.Equal(w.fetch("cikarang", root, snap1), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("full backup differs from the disk")
	}
	w.now = w.now.Add(time.Hour)
	// the guest dirties chunk 1 (offset 5 MiB) and chunk 4 (tail)
	w.inc.GuestWrite("default", "bosch", "root", 5*MiB, rnd(100000, 2))
	w.inc.GuestWrite("default", "bosch", "root", 20*MiB-4096, rnd(4096, 3))
	before := w.newChunks("cikarang")
	snap2, rs := w.backupVM("bosch")
	mustOK(t, rs)
	r := rs[0]
	if r.Full {
		t.Fatalf("second backup fell back to a full read: %v", w.inc.BitmapNames("default", "bosch", "root"))
	}
	if r.Dirty != 8*MiB {
		t.Fatalf("dirty=%d, want two 4 MiB chunks", r.Dirty)
	}
	if got := w.newChunks("cikarang") - before; got != 2 {
		t.Fatalf("uploaded %d new chunks, want 2", got)
	}
	if !bytes.Equal(w.fetch("cikarang", root, snap2), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("incremental backup differs from the disk")
	}
	if !bytes.Equal(w.fetch("cikarang", root, snap1), rnd(20*MiB, 1)) {
		t.Fatal("the first snapshot must be unchanged (no chain: each snapshot is a full image)")
	}
	// generations: only the newest bitmap remains
	if got := w.inc.BitmapNames("default", "bosch", "root"); len(got) != 1 || got[0] != "ib-"+snap2 {
		t.Fatalf("bitmaps=%v, want only ib-%s", got, snap2)
	}
	if w.inc.FrozenActive("default", "bosch") {
		t.Fatal("the NBD session was not released")
	}
}

// Writes that land while the NBD view is frozen belong to the NEXT backup, and
// must not be lost: this is the B1/B2 race the whole design rests on.
func TestWritesDuringFreezeAreNotLost(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	_, rs := w.backupVM("bosch")
	mustOK(t, rs)

	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 1*MiB, rnd(65536, 10)) // before the backup
	stateAtFreeze := append([]byte{}, w.inc.Disk("default", "bosch", "root")...)
	w.eng.Hooks.AfterFreeze = func() {
		// the guest keeps writing while we read from the frozen view
		w.inc.GuestWrite("default", "bosch", "root", 9*MiB, rnd(65536, 11))
		w.inc.GuestWrite("default", "bosch", "root", 1*MiB, rnd(100, 12))
	}
	snapB, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if !bytes.Equal(w.fetch("cikarang", root, snapB), stateAtFreeze) {
		t.Fatal("the snapshot must show the disk exactly as it was at the freeze point")
	}
	w.eng.Hooks.AfterFreeze = nil
	w.now = w.now.Add(time.Hour)
	snapC, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if rs[0].Full {
		t.Fatal("the third backup must still be incremental")
	}
	if !bytes.Equal(w.fetch("cikarang", root, snapC), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("a write made during the previous freeze was lost")
	}
}

// Negative control for the test above: if the engine created the new bitmap
// AFTER the freeze (the wrong order), writes in the gap would be lost. We emulate
// that wrong order by deleting the new generation's bitmap between freeze and the
// next run and checking the engine notices and reads in full instead of trusting it.
func TestMissingBitmapForcesFullReadNeverGuessing(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	snap1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	_ = w.inc.DeleteBitmap(context.Background(), root.Ref, "ib-"+snap1)
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 3*MiB, rnd(5000, 5))
	snap2, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if !rs[0].Full {
		t.Fatal("with the base bitmap gone the engine must read everything")
	}
	if !bytes.Equal(w.fetch("cikarang", root, snap2), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("full-read fallback produced the wrong content")
	}
	// inconsistent bitmap: same fail-closed behaviour
	w.now = w.now.Add(time.Hour)
	w.inc.CorruptBitmap("default", "bosch", "root", "ib-"+snap2)
	_, rs = w.backupVM("bosch")
	mustOK(t, rs)
	if !rs[0].Full {
		t.Fatal("an inconsistent bitmap must force a full read")
	}
}

func TestTwoTargetsOneCaptureAndLaggingTargetKeepsItsGeneration(t *testing.T) {
	w := newWorld(t, "cikarang", "amsterdam")
	w.policy("prod", []string{"cikarang", "amsterdam"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	s1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	for _, tn := range []string{"cikarang", "amsterdam"} {
		if !bytes.Equal(w.fetch(tn, root, s1), w.inc.Disk("default", "bosch", "root")) {
			t.Fatalf("%s differs", tn)
		}
	}
	nbdOpens := w.inc.Calls("InstanceNBD")
	// run 2: amsterdam is down
	w.nodes["amsterdam"].Srv.CloseClientConnections()
	w.nodes["amsterdam"].Srv.Close()
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 2*MiB, rnd(70000, 20))
	s2, rs := w.backupVM("bosch")
	if rs[0].OK() {
		t.Fatal("commit_required=all must not report success while a target is down")
	}
	var cikOK bool
	for _, tr := range rs[0].Targets {
		if tr.Target == "cikarang" && tr.Committed {
			cikOK = true
		}
	}
	if !cikOK {
		t.Fatalf("cikarang should have committed: %+v", rs[0].Targets)
	}
	if w.inc.Calls("InstanceNBD")-nbdOpens != 1 {
		t.Fatal("the VM must be captured once, not once per target")
	}
	// amsterdam's generation (ib-s1) must NOT have been discarded
	names := w.inc.BitmapNames("default", "bosch", "root")
	hasS1, hasS2 := false, false
	for _, n := range names {
		hasS1 = hasS1 || n == "ib-"+s1
		hasS2 = hasS2 || n == "ib-"+s2
	}
	if !hasS1 || !hasS2 {
		t.Fatalf("bitmaps=%v: must keep ib-%s (for the lagging target) and ib-%s", names, s1, s2)
	}
	// run 3: amsterdam is back (new server on the same data would be needed; here we only
	// check that the engine, with amsterdam unreachable again, still advances cikarang
	// and keeps both generations.)
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 12*MiB, rnd(1000, 21))
	s3, rs := w.backupVM("bosch")
	_ = s3
	if rs[0].Full {
		t.Fatal("cikarang must keep getting incremental backups while amsterdam is away")
	}
	if !bytes.Equal(w.fetch("cikarang", root, w.latest("cikarang", root)), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("cikarang content wrong")
	}
	if got := w.inc.BitmapNames("default", "bosch", "root"); len(got) < 2 {
		t.Fatalf("lagging generation dropped too early: %v", got)
	}
}

func TestLaggingTargetCatchesUpFromOldestGeneration(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.policy("prod", []string{"a", "b"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	s1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	// b misses run 2 because its session open fails once
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 2*MiB, rnd(70000, 30))
	realClient := w.eng.Targets["b"].Client
	w.eng.Targets["b"].Client = deadClient(t)
	s2, rs := w.backupVM("bosch")
	_ = s2
	if rs[0].OK() {
		t.Fatal("must not be OK with b down")
	}
	// b returns
	w.eng.Targets["b"].Client = realClient
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 14*MiB, rnd(70000, 31))
	s3, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if rs[0].Full {
		t.Fatalf("catch-up must be incremental from the oldest generation, got full read (%s)", s1)
	}
	for _, tn := range []string{"a", "b"} {
		if !bytes.Equal(w.fetch(tn, root, s3), w.inc.Disk("default", "bosch", "root")) {
			t.Fatalf("%s is not an exact copy after catch-up", tn)
		}
	}
	// the catch-up read the union of everything dirty since s1: chunks 0 (2MiB), 3 (14MiB)
	if rs[0].Dirty < 8*MiB {
		t.Fatalf("dirty=%d: must cover writes of both missed runs", rs[0].Dirty)
	}
	if got := w.inc.BitmapNames("default", "bosch", "root"); len(got) != 1 || got[0] != "ib-"+s3 {
		t.Fatalf("after both targets committed only the newest bitmap remains, got %v", got)
	}
}

func TestCustomVolumeBitmapLostOnRestartFallsBackToDedupedFullRead(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	w.inc.AddVM("default", "bosch", map[string]int{"root": 8 * MiB}, true)
	w.inc.AttachVolume("default", "bosch", "data", "data1", 12*MiB, false)
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(8*MiB, 1))
	w.inc.GuestWriteVol("default", "data1", 0, rnd(12*MiB, 2))
	vol := find(w.objects(), "volume/data1")
	_, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if len(rs) != 2 {
		t.Fatalf("root and data volume expected, got %d results", len(rs))
	}
	w.now = w.now.Add(time.Hour)
	w.inc.Restart("default", "bosch")
	w.inc.GuestWriteVol("default", "data1", 5*MiB, rnd(1000, 3))
	before := w.newChunks("cikarang")
	s2, rs := w.backupVM("bosch")
	mustOK(t, rs)
	var dataRes *engine.Result
	for _, r := range rs {
		if r.Object.Name == "volume/data1" {
			dataRes = r
		}
	}
	if !dataRes.Full {
		t.Fatal("the in-memory bitmap vanished with the restart: full read expected")
	}
	if got := w.newChunks("cikarang") - before; got != 1 {
		t.Fatalf("full read must still upload only the changed chunk, uploaded %d", got)
	}
	if !bytes.Equal(w.fetch("cikarang", vol, s2), w.inc.Disk2("default", "data1")) {
		t.Fatal("volume content wrong")
	}
	// root is persistent: it survived and stayed incremental
	for _, r := range rs {
		if r.Object.Name == "instance/bosch/root" && r.Full {
			t.Fatal("the persistent root bitmap should have survived the restart")
		}
	}
}

func TestFreezeLimitAbortsWithoutCommitting(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	s1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 6*MiB, rnd(5000, 9))
	w.eng.MaxFreeze = 50 * time.Millisecond
	w.eng.Hooks.AfterFreeze = func() { time.Sleep(200 * time.Millisecond) }
	_, rs = w.backupVM("bosch")
	if rs[0].OK() || rs[0].Err == nil {
		t.Fatalf("a run that exceeds max_freeze must fail: %+v", rs[0])
	}
	if w.snapCount("cikarang", root) != 1 {
		t.Fatal("nothing may be committed by an aborted run")
	}
	if w.inc.FrozenActive("default", "bosch") {
		t.Fatal("the freeze must be released on abort")
	}
	// the base generation survived, so the next run is still incremental
	w.eng.MaxFreeze = time.Minute
	w.eng.Hooks.AfterFreeze = nil
	w.now = w.now.Add(time.Hour)
	s3, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if rs[0].Full {
		t.Fatalf("base bitmap ib-%s must survive an aborted run", s1)
	}
	if !bytes.Equal(w.fetch("cikarang", root, s3), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("content wrong after the aborted run")
	}
}

func TestNBDSessionDyingMidReadIsNotCommitted(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	w.inc.FailNext("InstanceNBD", errors.New("incus restarted"))
	_, rs := w.backupVM("bosch")
	if rs[0].OK() {
		t.Fatal("must fail")
	}
	if w.snapCount("cikarang", root) != 0 {
		t.Fatal("something was committed")
	}
	if w.inc.FrozenActive("default", "bosch") {
		t.Fatal("session leaked")
	}
	_ = os.Stdout
}

// deadClient returns a client pointing at a closed port.
func deadClient(t *testing.T) *remote.Client {
	t.Helper()
	c, err := remote.New("127.0.0.1:1", mustCA(t), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustCA(t *testing.T) []byte {
	ca, _, err := pki.NewCA("x", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca.CertPEM
}

// An instance-wide bitmap call is one transaction: a dependent raw volume that cannot
// hold a persistent bitmap would fail it for the root disk too. The engine must fall
// back to per-disk calls so the root disk still gets its persistent generation.
func TestInstanceBitmapFailureFallsBackToPerDisk(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	w.inc.AddVM("default", "bosch", map[string]int{"root": 8 * MiB}, true)
	w.inc.AttachVolume("default", "bosch", "data", "data1", 8*MiB, true) // dependent, raw: no persistent bitmaps
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(8*MiB, 1))
	w.inc.GuestWriteVol("default", "data1", 0, rnd(8*MiB, 2))
	s1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if names := w.inc.BitmapNames("default", "bosch", "root"); len(names) != 1 || names[0] != "ib-"+s1 {
		t.Fatalf("root bitmaps %v: the root disk lost its generation", names)
	}
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 5*MiB, rnd(100, 3))
	_, rs = w.backupVM("bosch")
	mustOK(t, rs)
	for _, r := range rs {
		if r.Object.Name == "instance/bosch/root" && r.Full {
			t.Fatal("the root disk must stay incremental")
		}
	}
}

type memMap map[string]string

func (m memMap) Last(o, t string) string { return m[o+"|"+t] }
func (m memMap) SetLast(o, t, s string)  { m[o+"|"+t] = s }

func (w *world) bitmapCount() int { return len(w.inc.BitmapNames("default", "bosch", "root")) }

// A target that stays unreachable must not make bitmaps pile up: its base generation is
// kept (from the client's memory) and every other run's generation is dropped.
func TestBitmapsStayBoundedWhileATargetIsDown(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.eng.Memory = memMap{}
	w.policy("prod", []string{"a", "b"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	_, rs := w.backupVM("bosch")
	mustOK(t, rs)
	realB := w.eng.Targets["b"].Client
	w.eng.Targets["b"].Client = deadClient(t)
	for i := 0; i < 6; i++ {
		w.now = w.now.Add(time.Hour)
		w.inc.GuestWrite("default", "bosch", "root", (i%4)*MiB, rnd(2000, int64(40+i)))
		_, rs = w.backupVM("bosch")
		if rs[0].OK() {
			t.Fatal("must not be OK while b is down")
		}
		if n := w.bitmapCount(); n > 2 {
			t.Fatalf("run %d: %d bitmaps (%v), want at most 2 (b's base + a's newest)", i+1, n, w.inc.BitmapNames("default", "bosch", "root"))
		}
	}
	w.eng.Targets["b"].Client = realB
	w.now = w.now.Add(time.Hour)
	s, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if rs[0].Full {
		t.Fatal("b must catch up incrementally from its remembered base")
	}
	for _, tn := range []string{"a", "b"} {
		if !bytes.Equal(w.fetch(tn, root, s), w.inc.Disk("default", "bosch", "root")) {
			t.Fatalf("%s is not an exact copy after catching up", tn)
		}
	}
	if n := w.bitmapCount(); n != 1 {
		t.Fatalf("after both targets committed exactly one bitmap remains, got %v", w.inc.BitmapNames("default", "bosch", "root"))
	}
}

// Even without any memory (first run after an upgrade) the number of bitmaps is capped.
func TestBitmapCapWithoutMemory(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.policy("prod", []string{"a", "b"}, "all")
	setupVM(w, 1)
	_, rs := w.backupVM("bosch")
	mustOK(t, rs)
	w.eng.Targets["b"].Client = deadClient(t)
	for i := 0; i < 14; i++ {
		w.now = w.now.Add(time.Hour)
		w.inc.GuestWrite("default", "bosch", "root", 0, rnd(100, int64(i)))
		w.backupVM("bosch")
	}
	if n := w.bitmapCount(); n > 8 {
		t.Fatalf("%d bitmaps, the cap is 8: %v", n, w.inc.BitmapNames("default", "bosch", "root"))
	}
}

// A base chunk that vanished from the server must not make every later run fail the same way.
func TestMissingBaseChunkSelfHealsOnTheNextRun(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	objs := w.objects()
	root := find(objs, "instance/bosch/root")
	s1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	// delete the stored chunk of position 3 (unchanged afterwards, so the next run reuses it)
	tg := w.eng.Targets["cikarang"]
	ib, _ := tg.Client.Index(context.Background(), tg.Keys.GroupID("default", "block", root.Name), s1)
	idx, _ := format.ParseIndex(ib)
	id := idx.Entries[3].ID.String()
	if err := os.Remove(filepath.Join(w.nodes["cikarang"].Repo.Path, "chunks", id[:2], id[2:4], id)); err != nil {
		t.Fatal(err)
	}
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(1000, 7))
	_, rs = w.backupVM("bosch")
	if rs[0].OK() {
		t.Fatal("run 2 should fail: the reused chunk is gone from the server and not on this host")
	}
	w.now = w.now.Add(time.Hour)
	s3, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if !rs[0].Full {
		t.Fatal("run 3 must read the disk in full to heal the repository")
	}
	if !bytes.Equal(w.fetch("cikarang", root, s3), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("healed snapshot differs from the disk")
	}
}

// A copy restored next to the original must not inherit its MAC and IP addresses.
func TestRestoreAsCopyDropsNICIdentity(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	vm := w.inc.AddVM("default", "bosch", map[string]int{"root": 4 * MiB}, true)
	vm.Inst.JSON = []byte(`{"type":"virtual-machine","config":{"volatile.eth0.hwaddr":"x","limits.cpu":"4"},
	  "devices":{"eth0":{"type":"nic","network":"custody0","hwaddr":"BC:24:11:87:CB:A6","ipv4.address":"10.0.0.5","security.acls":"a"},"root":{"type":"disk","path":"/","pool":"default"}},
	  "expanded_devices":{"root":{"type":"disk","path":"/","pool":"default"}},"profiles":["default"]}`)
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(4*MiB, 1))
	rep := w.eng.Run(context.Background(), w.objects(), engine.RunOptions{})
	if rep.Failed() != 0 {
		t.Fatal("backup failed")
	}
	restore := func(o engine.RestoreOptions) map[string]string {
		fresh := fake.New(t.TempDir())
		e2 := *w.eng
		e2.Inc = fresh
		o.Server, o.Project, o.Name, o.NoChown = "cikarang", "default", "bosch", true
		if _, err := e2.RestoreInstance(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		return fresh.Created[0].Devices["eth0"]
	}
	copyNIC := restore(engine.RestoreOptions{AsName: "bosch-copy"})
	if _, ok := copyNIC["hwaddr"]; ok || copyNIC["ipv4.address"] != "" {
		t.Fatalf("the copy inherited the NIC identity: %v", copyNIC)
	}
	if copyNIC["network"] != "custody0" || copyNIC["security.acls"] != "a" {
		t.Fatalf("the copy lost the NIC's network or ACLs: %v", copyNIC)
	}
	if nic := restore(engine.RestoreOptions{AsName: "bosch-copy", KeepNetwork: true}); nic["hwaddr"] == "" {
		t.Fatal("--keep-network must keep the MAC")
	}
	if nic := restore(engine.RestoreOptions{}); nic["hwaddr"] == "" || nic["ipv4.address"] != "10.0.0.5" {
		t.Fatalf("a same-name disaster-recovery restore must keep the identity: %v", nic)
	}
}

func TestISOVolumesAreSkippedNotFailed(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	w.inc.AddVM("default", "v", map[string]int{"root": MiB}, true)
	w.inc.SetConfig("default", "v", engine.KeyPolicy, "prod")
	w.inc.Vols["default/installer"] = &fake.Volume{Ref: incusx.VolRef{Pool: "default", Project: "default", Type: "custom", Name: "installer"}, Content: "iso", Config: map[string]string{engine.KeyPolicy: "prod"}}
	o := find(w.objects(), "volume/installer")
	if o == nil || !strings.Contains(o.Skip, "ISO") {
		t.Fatalf("ISO volume not skipped: %+v", o)
	}
}

// One capture, two targets of different kinds (filesystem server + S3-backed server),
// incremental runs, then a restore straight from the S3-backed target of a fresh host.
func TestMixedFilesystemAndS3TargetsOneCapture(t *testing.T) {
	w := newWorld(t, "cikarang", "s3-amsterdam")
	w.policy("prod", []string{"cikarang", "s3-amsterdam"}, "all")
	vm := w.inc.AddVM("default", "bosch", map[string]int{"root": 16 * MiB}, true)
	_ = vm
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	w.inc.GuestWrite("default", "bosch", "root", 0, rnd(16*MiB, 1))
	root := find(w.objects(), "instance/bosch/root")
	s1, rs := w.backupVM("bosch")
	mustOK(t, rs)
	nbd := w.inc.Calls("InstanceNBD")
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 5*MiB, rnd(70000, 2))
	s2, rs := w.backupVM("bosch")
	mustOK(t, rs)
	if rs[0].Full {
		t.Fatal("second run must be incremental on both kinds of target")
	}
	if w.inc.Calls("InstanceNBD")-nbd != 1 {
		t.Fatal("one capture per run, not one per target")
	}
	for _, tn := range []string{"cikarang", "s3-amsterdam"} {
		if !bytes.Equal(w.fetch(tn, root, s2), w.inc.Disk("default", "bosch", "root")) {
			t.Fatalf("%s differs from the live disk", tn)
		}
		if !bytes.Equal(w.fetch(tn, root, s1), w.fetch("cikarang", root, s1)) {
			t.Fatalf("%s: first snapshot differs", tn)
		}
	}
	// the S3-backed server holds only ciphertext: no plaintext of the guest appears in any object
	st, _ := w.nodes["s3-amsterdam"].Svc.Store(w.nodes["s3-amsterdam"].Repo)
	plain := w.inc.Disk("default", "bosch", "root")[3*MiB : 3*MiB+4096]
	leak := false
	_ = st.Backend().List("", func(key string, _ datastore.Info) error {
		if b, err := st.Backend().Get(key); err == nil && bytes.Contains(b, plain) {
			leak = true
		}
		return nil
	})
	if leak {
		t.Fatal("guest plaintext found in the bucket")
	}
	// a full run also records the instance manifest on both kinds of target
	w.now = w.now.Add(time.Hour)
	if rep := w.eng.Run(context.Background(), w.objects(), engine.RunOptions{}); rep.Failed() != 0 {
		t.Fatal("full run failed")
	}
	// disaster recovery: a fresh host restores from the S3-backed target alone
	fresh := fake.New(t.TempDir())
	e2 := *w.eng
	e2.Inc = fresh
	e2.Targets = map[string]*engine.Target{"s3-amsterdam": w.eng.Targets["s3-amsterdam"]}
	if _, err := e2.RestoreInstance(context.Background(), engine.RestoreOptions{Server: "s3-amsterdam", Project: "default", Name: "bosch", NoChown: true}); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Created) != 1 {
		t.Fatal("instance not recreated")
	}
}

// Taking the S3-backed target away: the filesystem target still advances, the S3 one keeps its generation.
func TestS3TargetDownDoesNotBlockFilesystemTarget(t *testing.T) {
	w := newWorld(t, "cikarang", "s3-amsterdam")
	w.policy("prod", []string{"cikarang", "s3-amsterdam"}, "any")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	_, rs := w.backupVM("bosch")
	mustOK(t, rs)
	w.nodes["s3-amsterdam"].Srv.CloseClientConnections()
	w.nodes["s3-amsterdam"].Srv.Close()
	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 2*MiB, rnd(70000, 9))
	_, rs = w.backupVM("bosch")
	if rs[0].Full {
		t.Fatal("must stay incremental")
	}
	if !bytes.Equal(w.fetch("cikarang", root, w.latest("cikarang", root)), w.inc.Disk("default", "bosch", "root")) {
		t.Fatal("filesystem target content wrong")
	}
}
