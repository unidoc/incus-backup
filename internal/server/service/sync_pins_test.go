package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// syncPut stores one chunk and commits a one-chunk snapshot for it as peer transfer ctx.
func syncSnapshotPut(t *testing.T, k *format.Keys, snap string, created, committed time.Time, content string) (format.ID, []byte, api.SyncSnapshotPut) {
	t.Helper()
	id, blob, err := k.EncodeChunk([]byte(content), format.EncodeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: uint64(len(content)), Entries: []format.Entry{{End: uint64(len(content)), ID: id}}}).Marshal()
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: created}, group, nil)
	return id, blob, api.SyncSnapshotPut{Meta: api.SnapshotRef{GroupID: group, SnapshotID: snap, Created: created, Committed: committed, Policy: "daily30", Project: "p", Object: "vm/root", Kind: "block"}, Index: idx, Manifest: man}
}

// F9: only an invalid blob is the sender's mistake (400); maintenance mode and the like stay retryable.
func TestSyncPutChunkMapsOnlyInvalidToBadRequest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := keys(1)
	w := e.enrol("peer", api.ScopeSyncWrite)
	repo, _ := e.svc.SyncHostRepo(ctx, w, api.ScopeSyncWrite)
	id, blob, _ := k.EncodeChunk([]byte("payload"), format.EncodeOpts{})

	// a blob that is not a chunk for that id: the sender's mistake
	if err := e.svc.SyncPutChunk(ctx, repo, id.String(), []byte("garbage")); !errors.Is(err, service.ErrBadRequest) {
		t.Fatalf("an invalid blob must be a bad request, got %v", err)
	}
	// read-only repository: NOT a bad request (it is retried / reported as 503 by the HTTP layer)
	if err := e.svc.SetRepositoryMode(ctx, "custody", "read-only", "moving the disk"); err != nil {
		t.Fatal(err)
	}
	err := e.svc.SyncPutChunk(ctx, repo, id.String(), blob)
	if !errors.Is(err, datastore.ErrReadOnly) || errors.Is(err, service.ErrBadRequest) {
		t.Fatalf("read-only mode must pass through as ErrReadOnly, got %v", err)
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "offline", ""); err != nil {
		t.Fatal(err)
	}
	err = e.svc.SyncPutChunk(ctx, repo, id.String(), blob)
	if err == nil || errors.Is(err, service.ErrBadRequest) {
		t.Fatalf("offline mode must not be a bad request, got %v", err)
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "online", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SyncPutChunk(ctx, repo, id.String(), blob); err != nil {
		t.Fatalf("online again: %v", err)
	}
}

// F9: one pin per transfer. A commit by transfer A must not unpin the chunks transfer B is still sending.
func TestConcurrentSyncTransfersKeepSeparatePins(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := keys(1)
	w := e.enrol("peer", api.ScopeSyncWrite)
	repo, _ := e.svc.SyncHostRepo(ctx, w, api.ScopeSyncWrite)
	a := service.WithSyncTransfer(ctx, "push-from-primary")
	b := service.WithSyncTransfer(ctx, "pull-from-offsite")

	idA, blobA, putA := syncSnapshotPut(t, k, snapID(e.now.Add(-time.Hour), 1), e.now.Add(-time.Hour), e.now.Add(-time.Hour), "chunk of transfer A")
	idB, blobB, _ := syncSnapshotPut(t, k, snapID(e.now.Add(-2*time.Hour), 2), e.now.Add(-2*time.Hour), e.now.Add(-2*time.Hour), "chunk of transfer B")
	if err := e.svc.SyncPutChunk(a, repo, idA.String(), blobA); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SyncPutChunk(b, repo, idB.String(), blobB); err != nil {
		t.Fatal(err)
	}
	// A commits: its pin is released. B's chunk is not referenced by any snapshot yet.
	if err := e.svc.SyncPutSnapshot(a, repo, k.DomainID, putA); err != nil {
		t.Fatal(err)
	}
	st, _ := e.svc.Store(e.repo)
	st.SetClockForTest(func() time.Time { return time.Now().Add(72 * time.Hour) })
	gs, err := e.svc.GC(ctx, "custody", service.DefaultGCGrace, false)
	if err != nil || gs.Removed != 0 || gs.KeptPinned != 1 {
		t.Fatalf("transfer A's commit unpinned transfer B's chunk: %v %+v", err, gs)
	}
	// B can still send more chunks on its pin (it was not closed under it)
	idB2, blobB2, _ := syncSnapshotPut(t, k, snapID(e.now.Add(-3*time.Hour), 3), e.now, e.now, "second chunk of B")
	if err := e.svc.SyncPutChunk(b, repo, idB2.String(), blobB2); err != nil {
		t.Fatalf("B's pin was closed by A's commit: %v", err)
	}
}

// F4: a commit time more than a day before the snapshot id's own time cannot be true.
func TestSyncedCommitTimeIsBoundedBySnapshotIDTime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := keys(1)
	w := e.enrol("peer", api.ScopeSyncWrite)
	repo, _ := e.svc.SyncHostRepo(ctx, w, api.ScopeSyncWrite)
	made := e.now.Add(-time.Hour)
	for _, c := range []struct {
		name      string
		committed time.Time
		ok        bool
	}{
		{"two days before the id", made.Add(-48 * time.Hour), false},
		{"a year before the id", made.AddDate(-1, 0, 0), false},
		{"23h before the id", made.Add(-23 * time.Hour), true},
		{"after the id", made.Add(10 * time.Minute), true},
	} {
		snap := snapID(made, len(c.name))
		id, blob, put := syncSnapshotPut(t, k, snap, made, c.committed, c.name)
		if err := e.svc.SyncPutChunk(ctx, repo, id.String(), blob); err != nil {
			t.Fatal(err)
		}
		err := e.svc.SyncPutSnapshot(ctx, repo, k.DomainID, put)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, service.ErrBadRequest) {
			t.Errorf("%s: want a bad request, got %v", c.name, err)
		}
	}
}
