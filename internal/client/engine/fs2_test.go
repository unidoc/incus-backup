package engine_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
)

// fsWorld is a world with one filesystem volume "docs" holding small and large files.
type fsWorld struct {
	*world
	dir string
	obj *engine.Object
}

func newFSWorld(t *testing.T, targets ...string) *fsWorld {
	t.Helper()
	w := newWorld(t, targets...)
	w.policy("prod", targets, "all")
	dir := t.TempDir()
	for i := 0; i < 60; i++ {
		writeTree(t, dir, map[string]string{fmt.Sprintf("small/s%02d.txt", i): strings.Repeat(fmt.Sprintf("file-%d-", i), 1500)})
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("big%d.bin", i)), rnd(1500<<10, int64(50+i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTree(t, dir, map[string]string{"secret-project-name/plans.txt": "launch"})
	w.inc.AddFSVolume("default", "docs", dir)
	w.inc.Vols["default/docs"].Config[engine.KeyPolicy] = "prod"
	fw := &fsWorld{world: w, dir: dir}
	fw.obj = find(w.objects(), "volume/docs")
	return fw
}

func (f *fsWorld) backup() *engine.Result {
	f.t.Helper()
	sp, err := engine.NewSpool(f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	defer sp.Close()
	res := f.eng.BackupFS(context.Background(), f.obj, sp)
	if !res.OK() {
		f.t.Fatalf("backup failed: %v %+v", res.Err, res.Targets)
	}
	return res
}

func (f *fsWorld) restore(target, snap string) map[string]string {
	f.t.Helper()
	ctx := context.Background()
	loc, err := f.eng.Locate(ctx, f.eng.Targets[target], "default", "volume/docs", snap)
	if err != nil {
		f.t.Fatal(err)
	}
	dest := f.t.TempDir()
	if err := f.eng.RestoreFS(ctx, loc, dest, true); err != nil {
		f.t.Fatal(err)
	}
	return readTree(f.t, dest)
}

func (f *fsWorld) advance(d time.Duration) { f.now = f.now.Add(d) }

func TestCatalogBackupReadsOnlyWhatChangedAndRestoresExactly(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	first := f.backup()
	if !first.Full || first.Read == 0 {
		t.Fatalf("the first run must read everything: %+v", first)
	}
	total := first.Logical
	live := readTree(t, f.dir)
	if got := f.restore("cikarang", first.Snapshot); len(got) != len(live) {
		t.Fatalf("restore has %d files, source %d", len(got), len(live))
	}

	f.advance(time.Hour)
	second := f.backup()
	if second.Full || second.Read != 0 {
		t.Fatalf("an unchanged tree must read nothing: full=%v read=%d", second.Full, second.Read)
	}
	if up := second.Targets[0].Uploaded; up > 64<<10 {
		t.Fatalf("an unchanged tree uploaded %d bytes", up)
	}
	_ = total

	// one big and one small file change
	f.advance(time.Hour)
	if err := os.WriteFile(filepath.Join(f.dir, "big1.bin"), rnd(1500<<10, 999), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTree(t, f.dir, map[string]string{"small/s07.txt": "rewritten"})
	third := f.backup()
	if third.Full {
		t.Fatal("a normal run after a full one is incremental")
	}
	if third.Read < 1500<<10 || third.Read > 1500<<10+1<<20 {
		t.Fatalf("expected about one big file plus one pack of small files to be read, got %d bytes of %d", third.Read, third.Logical)
	}
	live = readTree(t, f.dir)
	got := f.restore("cikarang", third.Snapshot)
	if len(got) != len(live) {
		t.Fatalf("%d vs %d files", len(got), len(live))
	}
	for k, v := range live {
		if got[k] != v {
			t.Fatalf("%s differs after the incremental backup", k)
		}
	}
	// the first snapshot is still intact and still the OLD content (no chain, no corruption by reuse)
	old := f.restore("cikarang", first.Snapshot)
	if old["small/s07.txt"] == "rewritten" || old["small/s07.txt"] == "" {
		t.Fatal("an older snapshot changed")
	}
}

// Rewriting a file and putting size and mtime back is exactly what metadata-only detection misses.
// ctime/inode catch it; and a periodic full-data run bounds the damage if even those were fooled.
func TestRewrittenFileWithForgedMtimeIsStillBackedUp(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	f.backup()
	p := filepath.Join(f.dir, "big0.bin")
	fi, _ := os.Stat(p)
	data, _ := os.ReadFile(p)
	data[700<<10] ^= 0xff
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Hour)
	res := f.backup()
	if res.Read < 1500<<10 {
		t.Fatalf("the forged-mtime change was not read (read %d)", res.Read)
	}
	if got := f.restore("cikarang", res.Snapshot); got["big0.bin"] != string(data) {
		t.Fatal("the new content was not backed up")
	}
}

func TestPeriodicFullDataRunReadsEverythingAgain(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	f.eng.FSFullEvery = 48 * time.Hour
	first := f.backup()
	f.advance(time.Hour)
	if r := f.backup(); r.Full || r.Read != 0 {
		t.Fatalf("within the period: %+v", r)
	}
	f.advance(50 * time.Hour)
	r := f.backup()
	if !r.Full || r.Read == 0 || r.Read < first.Read/2 {
		t.Fatalf("after the period every file must be read again: full=%v read=%d (first %d)", r.Full, r.Read, first.Read)
	}
	// and the clock restarts from this run
	f.advance(time.Hour)
	if r := f.backup(); r.Full {
		t.Fatal("the full-data clock did not restart")
	}
}

func TestFSNoTrustCtimeIgnoresCtimeAndInode(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	f.eng.FSNoTrustCtime = true
	f.backup()
	// touch the metadata that only ctime sees: a hard link added and removed bumps ctime
	p := filepath.Join(f.dir, "big2.bin")
	if err := os.Link(p, p+".tmp"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p + ".tmp")
	f.advance(time.Hour)
	if r := f.backup(); r.Read != 0 {
		t.Fatalf("with FSNoTrustCtime a ctime-only change must not cause a read (read %d)", r.Read)
	}
	f.eng.FSNoTrustCtime = false
	f.advance(time.Hour)
	_ = f.backup() // strict mode re-reads the file whose ctime moved (and then settles)
}

func TestCatalogBrowsingAndSingleFileExtractFetchLittle(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	res := f.backup()
	ctx := context.Background()
	loc, err := f.eng.Locate(ctx, f.eng.Targets["cikarang"], "default", "volume/docs", res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.eng.OpenFS(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := v.Tree.List("small")
	if err != nil || len(ents) != 60 {
		t.Fatalf("%v %d", err, len(ents))
	}
	var buf bytes.Buffer
	if err := v.Cat(&buf, "secret-project-name/plans.txt"); err != nil || buf.String() != "launch" {
		t.Fatalf("cat: %v %q", err, buf.String())
	}
	dest := t.TempDir()
	if err := v.Extract(dest, []string{"big1.bin", "small/s03.txt"}, true); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(f.dir, "big1.bin"))
	got, _ := os.ReadFile(filepath.Join(dest, "big1.bin"))
	if !bytes.Equal(got, want) {
		t.Fatal("extracted file differs")
	}
	if _, err := os.Stat(filepath.Join(dest, "big0.bin")); err == nil {
		t.Fatal("an unrequested file appeared")
	}
	// the cache keeps the fetched chunks bounded
	if n := len(v.CachedChunks()); n > 8 {
		t.Fatalf("view keeps %d chunks", n)
	}
	// a snapshot in the archive format cannot be browsed, and says why
	f.eng.FSFormat = "tar"
	f.advance(time.Hour)
	tarRes := f.backup()
	tl, _ := f.eng.Locate(ctx, f.eng.Targets["cikarang"], "default", "volume/docs", tarRes.Snapshot)
	if _, err := f.eng.OpenFS(ctx, tl); err == nil || !strings.Contains(err.Error(), "archive format") {
		t.Fatalf("browsing a tar snapshot: %v", err)
	}
	if got := f.restore("cikarang", tarRes.Snapshot); len(got) != len(readTree(t, f.dir)) {
		t.Fatal("tar snapshots must stay restorable")
	}
	// after switching back, the catalog run has no catalog base to reuse and reads everything once
	f.eng.FSFormat = ""
	f.advance(time.Hour)
	if r := f.backup(); !r.Full {
		t.Fatal("the previous snapshot is not a catalog snapshot: this run must be full")
	}
}

// The server only ever sees ciphertext: no file name from the volume appears anywhere in it.
func TestNoFileNameReachesTheServer(t *testing.T) {
	f := newFSWorld(t, "s3-amsterdam", "cikarang")
	f.backup()
	for name, n := range f.nodes {
		st, _ := n.Svc.Store(n.Repo)
		objects := 0
		scanStore(t, st.Backend(), func(key string, b []byte) {
			objects++
			if bytes.Contains(b, []byte("secret-project-name")) || bytes.Contains(b, []byte("plans.txt")) || bytes.Contains(b, []byte("small/s0")) {
				t.Errorf("%s: a file name is readable in %s", name, key)
			}
		})
		if objects < 5 {
			t.Fatalf("%s: only %d objects scanned", name, objects)
		}
	}
}

// Corrupt data on a target: the verified-failed snapshot is never a base, so the next run reads
// everything and the repository heals.
func TestAFailedSnapshotIsNeverABaseAndTheNextBackupHealsIt(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	first := f.backup()
	n := f.nodes["cikarang"]
	st, _ := n.Svc.Store(n.Repo)
	key := f.firstPayloadChunk("cikarang", first.Snapshot)
	blob, _ := st.Backend().Get(key)
	blob[30] ^= 0xff
	_ = st.Backend().Put(key, blob, false)
	if _, err := n.Svc.Verify(context.Background(), "custody"); err == nil {
		t.Fatal("verify missed the corruption")
	}
	f.advance(time.Hour)
	res := f.backup()
	if !res.Full || res.Read == 0 {
		t.Fatalf("a failed base must force a full-data run: %+v", res)
	}
	if vs, err := n.Svc.Verify(context.Background(), "custody"); err != nil && len(vs.Problems) > 0 {
		// the old snapshot references a chunk that was re-uploaded: verify must now be clean
		t.Fatalf("not healed: %v %+v", err, vs.Problems)
	}
	if got := f.restore("cikarang", first.Snapshot); len(got) != len(readTree(t, f.dir)) {
		t.Fatal("the old snapshot is not restorable after healing")
	}
}

// A reused chunk is missing on ONE target (pruned, lost): that target's session cannot commit, the
// run repeats in full-data mode and both targets end up with a good snapshot.
func TestMissingReusedChunkOnOneTargetFallsBackToAFullRun(t *testing.T) {
	f := newFSWorld(t, "cikarang", "amsterdam")
	first := f.backup()
	n := f.nodes["amsterdam"]
	st, _ := n.Svc.Store(n.Repo)
	if err := st.Backend().Delete(f.firstPayloadChunk("amsterdam", first.Snapshot)); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Hour)
	res := f.backup() // BackupFS repeats internally; backup() fails the test if the final result is not OK
	if !res.Full {
		t.Fatalf("the fallback run must be a full-data run: %+v", res)
	}
	for _, tn := range []string{"cikarang", "amsterdam"} {
		l, _ := f.eng.Targets[tn].Client.Snapshots(context.Background(), f.eng.Targets[tn].Keys.GroupID("default", "filesystem", "volume/docs"))
		if len(l.Snapshots) < 2 {
			t.Fatalf("%s holds %d snapshots", tn, len(l.Snapshots))
		}
		if got := f.restore(tn, res.Snapshot); len(got) != len(readTree(t, f.dir)) {
			t.Fatalf("%s: restore incomplete", tn)
		}
	}
}

// Two key domains (different master keys) from one scan: each domain reuses its own base, a new
// domain with no history reads on its own, and both restore correctly.
func TestTwoKeyDomainsShareOneScan(t *testing.T) {
	f := newFSWorld(t, "a", "b")
	other := masterKeys(77)
	f.eng.Targets["b"].Keys = other
	f.eng.Targets["b"].Client = f.nodes["b"].Client(f.creds["b"], other.DomainID)
	r1 := f.backup()
	f.advance(time.Hour)
	r2 := f.backup()
	if r2.Read != 0 {
		t.Fatalf("both domains hold the previous snapshot, nothing to read: %d", r2.Read)
	}
	if got := f.restore("b", r2.Snapshot); len(got) != len(readTree(t, f.dir)) {
		t.Fatal("domain b restore")
	}
	_ = r1
}

func scanStore(t *testing.T, b datastore.Backend, fn func(key string, data []byte)) {
	t.Helper()
	if err := b.List("", func(key string, _ datastore.Info) error {
		if d, err := b.Get(key); err == nil {
			fn(key, d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// firstPayloadChunk returns the key of the first chunk of a snapshot (file content, not catalog).
func (f *fsWorld) firstPayloadChunk(target, snap string) string {
	f.t.Helper()
	n := f.nodes[target]
	group := f.eng.Targets[target].Keys.GroupID("default", "filesystem", "volume/docs")
	ib, err := n.Svc.RepoIndex(context.Background(), n.Repo, group, snap)
	if err != nil {
		f.t.Fatal(err)
	}
	idx, _ := format.ParseIndex(ib)
	id := idx.Entries[0].ID.String()
	return "chunks/" + id[:2] + "/" + id[2:4] + "/" + id
}

// A container that reuses the name of an old VM: both roots are "instance/bosch/root", one block, one
// filesystem. Locate must find the right group for an explicit snapshot id, and the newest otherwise, and
// the container's backups must stay incremental.
func TestAContainerThatReusesAnOldVMsNameIsFoundAndStaysIncremental(t *testing.T) {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	_, rs := w.backupVM("bosch")
	mustOK(t, rs)
	vmRoot := find(w.objects(), "instance/bosch/root")
	vmSnap := w.latest("cikarang", vmRoot)
	delete(w.inc.VMs, "default/bosch")
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"etc/hostname": "bosch-container\n", "var/data": strings.Repeat("x", 300000)})
	w.inc.AddContainer("default", "bosch", dir, true)
	w.inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	w.now = w.now.Add(time.Hour)
	var ct *engine.Object
	for _, o := range w.objects() {
		if o.Name == "instance/bosch/root" && o.Kind == engine.KindFilesystem {
			ct = o
		}
	}
	if ct == nil {
		t.Fatal("the container root was not discovered")
	}
	sp, _ := engine.NewSpool(t.TempDir())
	defer sp.Close()
	r1 := w.eng.BackupFS(context.Background(), ct, sp)
	if !r1.OK() {
		t.Fatalf("%v %+v", r1.Err, r1.Targets)
	}
	ctx := context.Background()
	tg := w.eng.Targets["cikarang"]
	if l, err := w.eng.Locate(ctx, tg, "default", "instance/bosch/root", ""); err != nil || l.Kind != engine.KindFilesystem || l.Snapshot != r1.Snapshot {
		t.Fatalf("latest must be the container's snapshot: %v %+v", err, l)
	}
	if l, err := w.eng.Locate(ctx, tg, "default", "instance/bosch/root", vmSnap); err != nil || l.Kind != engine.KindBlock {
		t.Fatalf("an explicit old VM snapshot id must still be found: %v", err)
	}
	if l, err := w.eng.Locate(ctx, tg, "default", "instance/bosch/root", r1.Snapshot); err != nil || l.Kind != engine.KindFilesystem {
		t.Fatalf("an explicit filesystem snapshot id must be found: %v", err)
	}
	w.now = w.now.Add(time.Hour)
	sp2, _ := engine.NewSpool(t.TempDir())
	defer sp2.Close()
	if r2 := w.eng.BackupFS(ctx, ct, sp2); !r2.OK() || r2.Full || r2.Read != 0 {
		t.Fatalf("the second backup of the container must be incremental and read nothing: %+v %v", r2, r2.Err)
	}
}

// A last-full time in the future (the clock ran ahead once) must not switch the periodic full-data run off.
func TestAFullDataTimestampInTheFutureForcesAFullRun(t *testing.T) {
	f := newFSWorld(t, "cikarang")
	f.backup()
	f.advance(-48 * time.Hour) // the host clock jumps back: the previous snapshot now lies "in the future"
	f.advance(time.Hour)
	if r := f.backup(); !r.Full {
		t.Fatalf("a base dated in the future must force a full-data run: %+v", r)
	}
}
