package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// A peer chooses its transfer ids: it may hold at most MaxSyncPinsPerPeer pins per repository. Beyond
// that it is refused (releasing the oldest could unprotect a transfer that is still sending), other
// peers and local transfers are unaffected, the end of a snapshot put or the reaper frees a slot.
func TestSyncPinsPerPeerAreCapped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := keys(1)
	w := e.enrol("peer", api.ScopeSyncWrite)
	other := e.enrol("peer2", api.ScopeSyncWrite)
	repo, _ := e.svc.SyncHostRepo(ctx, w, api.ScopeSyncWrite)
	as := func(h *service.Host, id string) context.Context {
		return service.WithSyncPeer(service.WithSyncTransfer(ctx, h.ID+"/"+id), h.ID)
	}
	missing := func(c context.Context) error {
		id, _, _ := k.EncodeChunk([]byte("m"), format.EncodeOpts{})
		_, err := e.svc.SyncMissing(c, repo, []string{id.String()})
		return err
	}
	for i := 0; i < service.MaxSyncPinsPerPeer; i++ {
		if err := missing(as(w, fmt.Sprintf("%02x", i))); err != nil {
			t.Fatalf("transfer %d: %v", i, err)
		}
	}
	// an existing transfer keeps working at the cap; a new id is refused
	if err := missing(as(w, "00")); err != nil {
		t.Fatalf("an open transfer must keep working at the cap: %v", err)
	}
	if err := missing(as(w, "ff")); !errors.Is(err, service.ErrTooManyRequests) {
		t.Fatalf("a %dth transfer id must be refused with ErrTooManyRequests, got %v", service.MaxSyncPinsPerPeer+1, err)
	}
	if err := missing(as(other, "ff")); err != nil {
		t.Fatalf("another peer must not be affected: %v", err)
	}
	if err := missing(service.WithSyncTransfer(ctx, "run/1")); err != nil {
		t.Fatalf("a local transfer (no peer) is not capped: %v", err)
	}
	// a commit releases that transfer's pin: its slot is free again
	_, blob, put := syncSnapshotPut(t, k, snapID(e.now.Add(-time.Hour), 7), e.now.Add(-time.Hour), e.now.Add(-time.Hour), "slot chunk")
	id, _, _ := k.EncodeChunk([]byte("slot chunk"), format.EncodeOpts{})
	if err := e.svc.SyncPutChunk(as(w, "00"), repo, id.String(), blob); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SyncPutSnapshot(as(w, "00"), repo, k.DomainID, put); err != nil {
		t.Fatal(err)
	}
	if err := missing(as(w, "ff")); err != nil {
		t.Fatalf("the slot of a committed transfer must be free: %v", err)
	}
	// silence for two hours frees the rest
	e.now = e.now.Add(3 * time.Hour)
	e.svc.ReapSessions(ctx)
	for i := 0; i < service.MaxSyncPinsPerPeer; i++ {
		if err := missing(as(w, fmt.Sprintf("a%d", i))); err != nil {
			t.Fatalf("after reaping, transfer %d: %v", i, err)
		}
	}
}

// The writer-slot conflict names who really holds it: a host by name, a hostless holder (a delete, a sync
// commit) as "the server", never as "sync".
func TestWriterConflictNamesTheHolder(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := keys(1)
	b := e.enrol("benfica", api.ScopeBackup)
	req := func(n int) api.SessionRequest {
		return api.SessionRequest{GroupID: group, SnapshotID: snapID(e.now, n), Kind: "block", Created: e.now, Project: "p", Object: "vm/root"}
	}
	_, err := e.svc.BeginSession(ctx, b, k.DomainID, req(1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.BeginSession(ctx, b, k.DomainID, req(2))
	if err == nil || !strings.Contains(err.Error(), "started by benfica") {
		t.Fatalf("a host holder must be named: %v", err)
	}
	if _, err := e.svc.DB.Pool().Exec(ctx, `UPDATE sessions SET host_id=NULL WHERE state='open' AND group_id=$1`, group); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.BeginSession(ctx, b, k.DomainID, req(3))
	if err == nil || strings.Contains(err.Error(), "by sync") || !strings.Contains(err.Error(), "the server") {
		t.Fatalf("a hostless holder must not be called sync: %v", err)
	}
}

// One sync run is one fresh transfer id. A run that ends on a repair (the snapshot is already there) or on a
// refusal must not keep its pin for two hours, or an ordinary peer locks itself out after
// MaxSyncPinsPerPeer such runs.
func TestSyncPutSnapshotReleasesPinOnEveryOutcome(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := keys(1)
	w := e.enrol("peer", api.ScopeSyncWrite)
	repo, _ := e.svc.SyncHostRepo(ctx, w, api.ScopeSyncWrite)
	run := func(i int) context.Context {
		return service.WithSyncPeer(service.WithSyncTransfer(ctx, w.ID+"/"+fmt.Sprintf("%024x", i)), w.ID)
	}
	short := func(err error) string {
		switch {
		case err == nil:
			return "ok"
		case errors.Is(err, service.ErrTooManyRequests):
			return "429"
		case errors.Is(err, service.ErrBadRequest):
			return "400"
		}
		return err.Error()
	}
	send := func(c context.Context, idHex string, blob []byte, put api.SyncSnapshotPut) error {
		if _, err := e.svc.SyncMissing(c, repo, []string{idHex}); err != nil {
			return err
		}
		if err := e.svc.SyncPutChunk(c, repo, idHex, blob); err != nil {
			return err
		}
		return e.svc.SyncPutSnapshot(c, repo, k.DomainID, put)
	}
	at := e.now.Add(-time.Hour)
	id, blob, put := syncSnapshotPut(t, k, snapID(at, 1), at, at, "repaired chunk")
	if err := send(run(0), id.String(), blob, put); err != nil {
		t.Fatalf("first sync of the snapshot: %v", err)
	}
	var repair, refused []string
	for i := 1; i <= 4; i++ { // repair runs: the same snapshot again, accepted as identical
		repair = append(repair, short(send(run(i), id.String(), blob, put)))
	}
	bid, bblob, bad := syncSnapshotPut(t, k, snapID(at, 2), at, at.Add(-48*time.Hour), "refused chunk")
	for i := 5; i <= 8; i++ { // runs whose snapshot the server refuses (commit time before the id's time)
		refused = append(refused, short(send(run(i), bid.String(), bblob, bad)))
	}
	nid, nblob, nput := syncSnapshotPut(t, k, snapID(at, 3), at, at, "new chunk")
	next := short(send(run(9), nid.String(), nblob, nput))
	got := fmt.Sprintf("repair runs %v | refused runs %v | next healthy run %s", repair, refused, next)
	if want := "repair runs [ok ok ok ok] | refused runs [400 400 400 400] | next healthy run ok"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
