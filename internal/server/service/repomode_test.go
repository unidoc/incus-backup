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

func TestMaintenanceModesEnforcedBelowTheObjectModel(t *testing.T) { modeScenario(t, newEnv(t)) }
func TestMaintenanceModesOnS3(t *testing.T)                        { modeScenario(t, newEnvS3(t)) }

func modeScenario(t *testing.T, e *env) {
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	k := keys(1)
	snap := snapID(e.now, 1)
	if err := e.upload(h, k, group, snap, e.now, "nightly", "chunk-a"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "sideways", ""); err == nil {
		t.Fatal("unknown mode accepted")
	}

	// read-only: restore reads work; every write path is refused
	if err := e.svc.SetRepositoryMode(ctx, "custody", "read-only", "moving the disk"); err != nil {
		t.Fatal(err)
	}
	idx, err := e.svc.ReadIndex(ctx, h, group, snap)
	if err != nil {
		t.Fatalf("read in read-only mode: %v", err)
	}
	pi, _ := format.ParseIndex(idx)
	if _, err := e.svc.ReadChunk(ctx, h, pi.Entries[0].ID.String()); err != nil {
		t.Fatalf("restore read in read-only mode: %v", err)
	}
	snap2 := snapID(e.now.Add(time.Hour), 2)
	if err := e.upload(h, k, group, snap2, e.now.Add(time.Hour), "nightly", "chunk-new"); !errors.Is(err, datastore.ErrReadOnly) {
		t.Fatalf("a backup was accepted in read-only mode: %v", err)
	}
	if err := e.svc.SetSnapshotProtected(ctx, "custody", group, snap, true); !errors.Is(err, datastore.ErrReadOnly) {
		t.Fatalf("protect in read-only: %v", err)
	}
	if err := e.svc.ForgetSnapshot(ctx, "custody", group, snap); err == nil {
		t.Fatal("forget in read-only mode")
	}
	if _, err := e.svc.GC(ctx, "custody", 24*time.Hour, false); !errors.Is(err, datastore.ErrReadOnly) {
		t.Fatalf("GC in read-only mode: %v", err)
	}
	if _, err := e.svc.Prune(ctx, "custody", false); !errors.Is(err, datastore.ErrReadOnly) {
		t.Fatalf("prune in read-only mode: %v", err)
	}
	if _, err := e.svc.VerifyJob(ctx, "custody", service.VerifyRequest{Mode: "all"}); !errors.Is(err, datastore.ErrReadOnly) {
		t.Fatalf("verify in read-only mode: %v", err)
	}
	rs, _ := e.svc.Repositories(ctx)
	if rs[0].Mode != "read-only" || rs[0].ModeReason != "moving the disk" {
		t.Fatalf("mode not reported: %+v", rs[0])
	}

	// offline: nothing at all touches the datastore
	if err := e.svc.SetRepositoryMode(ctx, "custody", "offline", "disk swap"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReadIndex(ctx, h, group, snap); !errors.Is(err, datastore.ErrOffline) {
		t.Fatalf("read while offline: %v", err)
	}

	// back online: everything works again, and the failed attempts left nothing behind
	if err := e.svc.SetRepositoryMode(ctx, "custody", "online", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.upload(h, k, group, snap2, e.now.Add(time.Hour), "nightly", "chunk-new"); err != nil {
		t.Fatalf("backup after leaving maintenance: %v", err)
	}
	if err := e.svc.SetSnapshotProtected(ctx, "custody", group, snap, true); err != nil {
		t.Fatal(err)
	}
}

// The maintenance-mode wrapper must not hide the S3 backend from the datastore: the presence set and the
// other S3-only optimisations are chosen by looking at the backend type.
func TestServiceStoreStillRecognisesS3BehindTheModeWrapper(t *testing.T) {
	e := newEnvS3(t)
	st, err := e.svc.Store(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.PresenceStats().On {
		t.Fatal("the presence set is off for an S3 repository opened through the service")
	}
	e2 := newEnv(t)
	st2, err := e2.svc.Store(e2.repo)
	if err != nil {
		t.Fatal(err)
	}
	if st2.PresenceStats().On {
		t.Fatal("the presence set is on for a filesystem repository")
	}
}
