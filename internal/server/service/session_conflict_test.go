package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// An abort that arrives while the commit runs (the client gave up waiting) is refused with a conflict:
// it must not drop the session's pins under the commit. The commit then succeeds.
func TestAbortWhileCommittingIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	st, err := e.svc.Store(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.svc.BeginSession(ctx, h, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snapID(e.now, 1), Kind: "block", Created: e.now, Project: "p", Object: "vm/root"})
	if err != nil {
		t.Fatal(err)
	}
	id, blob, _ := k.EncodeChunk([]byte("abc"), format.EncodeOpts{})
	if err := e.svc.SessionPutChunk(ctx, h, resp.Session, id.String(), blob); err != nil {
		t.Fatal(err)
	}
	idx, _ := (&format.Index{Kind: format.IndexDynamic, Size: 3, Entries: []format.Entry{{End: 3, ID: id}}}).Marshal()
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snapID(e.now, 1), Created: e.now, Object: format.ObjectInfo{Name: "vm/root", Kind: "block", Size: 3}, IndexDigest: format.IndexDigest(idx)}, group, nil)
	if err := e.svc.SessionPutIndex(ctx, h, resp.Session, idx); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SessionPutManifest(ctx, h, resp.Session, man); err != nil {
		t.Fatal(err)
	}
	var abortErr error
	st.SetFault(func(p string) error {
		if p == "commit:before-marker" {
			st.SetFault(nil)
			abortErr = e.svc.SessionAbort(ctx, h, resp.Session)
		}
		return nil
	})
	if _, err := e.svc.SessionCommit(ctx, h, resp.Session, api.CommitRequest{LogicalBytes: 3, UploadedBytes: 3}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !errors.Is(abortErr, service.ErrConflict) {
		t.Fatalf("abort during commit = %v, want ErrConflict", abortErr)
	}
	if !st.SnapshotExists(group, snapID(e.now, 1)) {
		t.Fatal("the snapshot is not visible")
	}
}

// DeleteGroup holds the group's writer slot: a host cannot start a session into the group while the
// delete runs, and the slot is free again afterwards.
func TestDeleteGroupHoldsWriterSlot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	if err := e.upload(h, k, group, snapID(e.now, 1), e.now, "p", "one", "two"); err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.Store(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	var beginErr error
	st.SetFault(func(p string) error {
		if p == "group:before-purge" {
			st.SetFault(nil)
			_, beginErr = e.svc.BeginSession(ctx, h, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snapID(e.now, 2), Kind: "block", Created: e.now, Project: "p", Object: "vm/root"})
		}
		return nil
	})
	if _, err := e.svc.DeleteGroup(ctx, "custody", group, false); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(beginErr, service.ErrConflict) {
		t.Fatalf("BeginSession during DeleteGroup = %v, want ErrConflict", beginErr)
	}
	if _, err := e.svc.BeginSession(ctx, h, k.DomainID, api.SessionRequest{GroupID: group, SnapshotID: snapID(e.now, 3), Kind: "block", Created: e.now, Project: "p", Object: "vm/root"}); err != nil {
		t.Fatalf("the writer slot was not released: %v", err)
	}
}
