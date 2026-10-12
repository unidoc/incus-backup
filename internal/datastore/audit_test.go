package datastore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/s3test"
)

// Production stores are always wrapped in a ModeBackend (service.openStore). The optional fast paths
// that are detected by type assertion on s.b must still be taken through the wrapper, and the
// wrapper must still enforce the mode on them.
func TestAuditFastPathsSurviveTheModeWrapper(t *testing.T) {
	s3b, err := NewS3Backend(S3Config{Endpoint: "127.0.0.1:1", Bucket: "b", AccessKey: "a", SecretKey: "s", NoTLS: true, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if statBeforePut(s3b) {
		t.Fatal("control: a bare S3 backend must not HEAD before a conditional PUT")
	}
	if statBeforePut(NewModeBackend(s3b, ModeOnline)) {
		t.Fatal("through the maintenance-mode wrapper an S3 store still HEADs before every PUT")
	}
	mb := NewMemBackend()
	if _, ok := Backend(mb).(shallowLister); !ok {
		t.Fatal("control: the memory backend lists shallowly")
	}
	if shallowOf(NewModeBackend(mb, ModeOnline)) == nil {
		t.Fatal("through the maintenance-mode wrapper the chunk listing is not sharded")
	}
}

// The sharded listing through the wrapper must honour the offline mode.
func TestAuditShardedListingHonoursOfflineMode(t *testing.T) {
	mem := NewMemBackend()
	if _, err := InitBackend(mem); err != nil {
		t.Fatal(err)
	}
	mw := NewModeBackend(mem, ModeOnline)
	s, err := OpenBackend(mw)
	if err != nil {
		t.Fatal(err)
	}
	putChunks(t, s, testKeys(t), "off", 10)
	n := 0
	if err := s.listChunkObjects(func(string, Info) error { n++; return nil }); err != nil || n != 10 {
		t.Fatalf("online: %d %v", n, err)
	}
	mw.SetMode(ModeOffline)
	if err := s.listChunkObjects(func(string, Info) error { return nil }); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline listing: %v", err)
	}
}

// CleanTmp must reach the filesystem backend through the wrapper (and is a write: refused when read-only).
func TestAuditCleanTmpThroughModeWrapper(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ds")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	fb, err := NewFSBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	mw := NewModeBackend(fb, ModeOnline)
	s, err := OpenBackend(mw)
	if err != nil {
		t.Fatal(err)
	}
	// a crash leftover the way FSBackend.Put names its temp files
	tmp := filepath.Join(root, "tmp")
	_ = os.MkdirAll(tmp, 0o700)
	f := filepath.Join(tmp, ".w-leftover")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(f, old, old)
	before, _ := fb.CleanTmp(time.Hour) // what the bare backend would remove
	if before != 1 {
		t.Fatalf("control: the bare backend removed %d leftovers", before)
	}
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(f, old, old)
	mw.SetMode(ModeReadOnly)
	if _, err := s.CleanTmp(time.Hour); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only CleanTmp: %v", err)
	}
	mw.SetMode(ModeOnline)
	if n, err := s.CleanTmp(time.Hour); err != nil || n != 1 {
		t.Fatalf("CleanTmp through the wrapper removed %d (%v), the bare backend removes 1", n, err)
	}
}

// blockingDelete pauses the Delete of one key until released.
type blockingDelete struct {
	Backend
	key     string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingDelete) Delete(key string) error {
	if key == b.key {
		b.once.Do(func() { close(b.reached); <-b.release })
	}
	return b.Backend.Delete(key)
}

func (b *blockingDelete) listShallow(prefix string) ([]listedObject, []string, error) {
	return b.Backend.(shallowLister).listShallow(prefix)
}

// GC forgets a chunk BEFORE deleting it. A presence re-list that starts between that and the delete
// (and lists the chunk's prefix before the object is gone) brings the swept chunk back as a positive.
// Commit still catches it (it never trusts the set), so this is a stale positive, not data loss.
func TestAuditGCSweepVersusPresenceRebuild(t *testing.T) {
	mem := NewMemBackend()
	bd := &blockingDelete{Backend: mem, reached: make(chan struct{}), release: make(chan struct{})}
	if _, err := InitBackend(bd); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(bd)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPresenceCache(true)
	k := testKeys(t)
	ids := putChunks(t, s, k, "gcrace", 1)
	victim := ids[0]
	bd.key = chunkKey(victim)
	mem.SetModTime(chunkKey(victim), time.Now().Add(-48*time.Hour))
	if err := s.RebuildPresence(); err != nil {
		t.Fatal(err)
	}
	gcDone := make(chan error)
	go func() {
		_, err := s.GC(context.Background(), time.Hour, false)
		gcDone <- err
	}()
	<-bd.reached // presence.remove(victim) done, the object is not deleted yet
	if err := s.RebuildPresence(); err != nil {
		t.Fatal(err)
	}
	close(bd.release)
	if err := <-gcDone; err != nil {
		t.Fatal(err)
	}
	if s.HasChunk(victim) {
		t.Fatal("GC did not delete the victim")
	}
	stale := s.presence.has(victim)
	miss, err := s.Missing(ids)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("presence holds the swept chunk: %v; Missing reports %d missing", stale, len(miss))
	if stale || len(miss) != 1 {
		t.Fatalf("a chunk this process swept is still answered present (stale=%v, missing=%v)", stale, miss)
	}
}

// CleanOrphans decides from ONE listing and deletes much later. A snapshot whose earlier commit
// attempt died before the marker (index and manifest left behind, older than the grace period) and
// that is committed again under the same id (a server-to-server sync retries the same snapshot id)
// while CleanOrphans runs, loses the index and manifest it has just written: a VISIBLE snapshot
// that cannot be read, whose chunks the next GC sweeps (markLive skips a snapshot without index).
func TestAuditCleanOrphansVersusRecommitOfTheSameSnapshot(t *testing.T) {
	mem := NewMemBackend()
	snap := "20261007230000-0a0a0a0a"
	bd := &blockingDelete{Backend: mem, key: snapPrefix(g1, snap) + "index", reached: make(chan struct{}), release: make(chan struct{})}
	if _, err := InitBackend(bd); err != nil {
		t.Fatal(err)
	}
	s, err := OpenBackend(bd)
	if err != nil {
		t.Fatal(err)
	}
	k := testKeys(t)
	idx, man, _ := buildSnapshot(t, s, k, g1, snap, "o-a", "o-b")
	// attempt 1 dies between the manifest and the marker
	s.SetFault(func(p string) error {
		if p == "commit:before-marker" {
			return errors.New("crash")
		}
		return nil
	})
	if err := commit(t, s, g1, snap, idx, man); err == nil {
		t.Fatal("the injected crash did not happen")
	}
	s.SetFault(nil)
	for _, f := range []string{"index", "manifest"} {
		mem.SetModTime(snapPrefix(g1, snap)+f, time.Now().Add(-48*time.Hour))
	}
	cleaned := make(chan error)
	go func() { _, err := s.CleanOrphans(24 * time.Hour); cleaned <- err }()
	<-bd.reached // CleanOrphans listed the debris and is about to delete it
	committed := make(chan error)
	go func() { committed <- commit(t, s, g1, snap, idx, man) }() // attempt 2, same snapshot id
	var commitErr error
	select {
	case commitErr = <-committed:
		close(bd.release)
	case <-time.After(300 * time.Millisecond): // (with a lock the commit waits for the cleaner)
		close(bd.release)
		commitErr = <-committed
	}
	if err := <-cleaned; err != nil {
		t.Fatal(err)
	}
	if commitErr != nil {
		t.Fatal(commitErr)
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatalf("DATA LOSS: %v", err)
	}
}

// On real MinIO, through the maintenance-mode wrapper exactly as the daemon opens it: the HEAD-less
// conditional PUT answers "exists" for a stored chunk, the sharded listing sees exactly what one
// listing sees, verify and GC work, and offline mode stops the sharded listing.
func TestAuditS3FastPathsThroughTheModeWrapper(t *testing.T) {
	svc, bucket := s3test.NewBucket(t)
	b, err := NewS3Backend(S3Config{Endpoint: svc.Endpoint, Bucket: bucket, Prefix: "repo", AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, NoTLS: svc.NoTLS, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitBackend(b); err != nil {
		t.Fatal(err)
	}
	mw := NewModeBackend(b, ModeOnline)
	s, err := OpenBackend(mw)
	if err != nil {
		t.Fatal(err)
	}
	if statBeforePut(s.b) || shallowOf(s.b) == nil {
		t.Fatal("fast paths not selected through the wrapper")
	}
	k := testKeys(t)
	snap := "20261007230000-0a0a0a0a"
	idx, man, ids := buildSnapshot(t, s, k, g1, snap, "s3-a", "s3-b", "s3-c")
	if err := commit(t, s, g1, snap, idx, man); err != nil {
		t.Fatal(err)
	}
	_, blob, _ := k.EncodeChunk([]byte("s3-a"), format.EncodeOpts{})
	if created, err := s.PutChunk(ids[0], blob); err != nil || created {
		t.Fatalf("HEAD-less PUT of an existing chunk: created=%v err=%v", created, err)
	}
	var sharded, plain []string
	if err := s.listChunkObjects(func(k string, _ Info) error { sharded = append(sharded, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := b.List("chunks/", func(k string, _ Info) error { plain = append(plain, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sharded) != fmt.Sprint(plain) || len(plain) != 3 {
		t.Fatalf("sharded %v vs plain %v", sharded, plain)
	}
	if vs, err := s.Verify(context.Background()); err != nil || len(vs.Problems) != 0 || vs.Chunks != 3 {
		t.Fatalf("verify: %+v %v", vs, err)
	}
	if st, err := s.GC(context.Background(), time.Hour, true); err != nil || st.Scanned != 3 || st.Live != 3 {
		t.Fatalf("gc: %+v %v", st, err)
	}
	if err := s.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	mw.SetMode(ModeOffline)
	if _, _, err := s.Usage(); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline usage: %v", err)
	}
}
