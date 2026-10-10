package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
)

// "Offline refuses every access to the datastore": also when the store is opened for the first time
// after the mode was set (a daemon restart while the storage is taken away). Hosts must see the
// maintenance error (503, retry later), not "not found".
func TestAuditOfflineRepositoryIsNotOpenedAfterARestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	snap := snapID(e.now, 1)
	if err := e.upload(h, keys(1), group, snap, e.now, "nightly", "chunk-a"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "offline", "disk swap"); err != nil {
		t.Fatal(err)
	}
	e.svc.DropStoresForTest()                                           // the daemon restarts
	if err := os.Rename(e.repo.Path, e.repo.Path+".away"); err != nil { // the storage is taken away
		t.Fatal(err)
	}
	if _, err := e.svc.ReadIndex(ctx, h, group, snap); !errors.Is(err, datastore.ErrOffline) {
		t.Fatalf("read of an offline repository after a restart: %v (want the offline error)", err)
	}
	if err := os.Rename(e.repo.Path+".away", e.repo.Path); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "online", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReadIndex(ctx, h, group, snap); err != nil {
		t.Fatalf("back online: %v", err)
	}
}

// A read-only repository opened for the first time must not be written by the ownership handshake.
func TestAuditReadOnlyRepositoryHandshakeDoesNotWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	snap := snapID(e.now, 1)
	if err := e.upload(h, keys(1), group, snap, e.now, "nightly", "chunk-a"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "read-only", "snapshotting the disk"); err != nil {
		t.Fatal(err)
	}
	e.svc.DropStoresForTest()
	owner := filepath.Join(e.repo.Path, "owner.json")
	if err := os.Remove(owner); err != nil { // a repository from before owner.json existed
		t.Fatal(err)
	}
	if _, err := e.svc.ReadIndex(ctx, h, group, snap); err != nil {
		t.Fatalf("restore read in read-only mode: %v", err)
	}
	if _, err := os.Stat(owner); err == nil {
		t.Fatal("opening a read-only repository wrote owner.json")
	}
}
