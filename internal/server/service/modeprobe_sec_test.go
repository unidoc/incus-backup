package service_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
)

// "Offline" promises that nothing touches the datastore (an administrator may have pulled the disk). The
// operator's probe route and the first open of a store after a restart used to ignore it: they re-created
// the repository directory on whatever filesystem was underneath.
func TestOfflineRepositoryIsNeverTouched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.enrol("benfica", api.ScopeBackup)
	if err := e.svc.SetRepositoryMode(ctx, "custody", "offline", "disk pulled"); err != nil {
		t.Fatal(err)
	}
	pulled := e.repo.Path + ".pulled"
	if err := os.Rename(e.repo.Path, pulled); err != nil {
		t.Fatal(err)
	}
	rs, err := e.svc.ProbeRepository(ctx, "custody")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.repo.Path); err == nil {
		t.Fatalf("the probe of an offline repository created %s (results %+v)", e.repo.Path, rs)
	}
	e.svc.DropStoresForTest() // as after a restart: the store is opened again
	if _, err := e.svc.ReadIndex(ctx, h, group, snapID(e.now, 1)); !errors.Is(err, datastore.ErrOffline) {
		t.Fatalf("read of an offline repository: %v", err)
	}
	if _, err := os.Stat(e.repo.Path); err == nil {
		t.Fatalf("opening an offline repository created %s", e.repo.Path)
	}
	// back online with the disk back: everything works
	if err := os.Rename(pulled, e.repo.Path); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetRepositoryMode(ctx, "custody", "online", ""); err != nil {
		t.Fatal(err)
	}
	if rs, err := e.svc.ProbeRepository(ctx, "custody"); err != nil || len(rs) == 0 || !rs[len(rs)-1].OK {
		t.Fatalf("probe online: %+v %v", rs, err)
	}
}

// The S3 probe writes and deletes a scratch object: it must not do that while the repository is read-only
// or offline.
func TestS3ProbeHonoursMaintenanceMode(t *testing.T) {
	e := newEnvS3(t)
	ctx := context.Background()
	for _, mode := range []string{"read-only", "offline"} {
		if err := e.svc.SetRepositoryMode(ctx, "custody", mode, "test"); err != nil {
			t.Fatal(err)
		}
		rs, err := e.svc.ProbeRepository(ctx, "custody")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if strings.Contains(r.Op, "put") || strings.Contains(r.Op, "delete") {
				t.Fatalf("%s: the probe wrote to the bucket: %+v", mode, rs)
			}
		}
	}
}
