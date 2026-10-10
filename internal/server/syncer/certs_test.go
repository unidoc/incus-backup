package syncer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/remote"
)

// The sync manager renews this server's credential at the peer before a run when
// fewer than 45 days remain, and the run then uses the new certificate.
func TestSyncRunRenewsPeerCredential(t *testing.T) {
	a, b := newNode(t, "cikarang"), newNode(t, "amsterdam")
	ctx := context.Background()
	k := keys()
	bk := a.enrol("benfica", api.ScopeBackup)
	backup(t, bk, k, group, snapID(1), a.now, "p", "alpha")
	m, _, _ := addJob(t, a, b, "push", "amsterdam")
	dir := filepath.Join(a.svc.StateDir, "sync", "custody", "amsterdam")
	before, _ := os.ReadFile(filepath.Join(dir, "client.crt"))

	if res, err := m.Run(ctx, "custody", "amsterdam"); err != nil || res.Synced != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "client.crt")); string(now) != string(before) {
		t.Fatal("renewed a fresh credential")
	}
	a.now = a.now.Add(700 * 24 * time.Hour)
	b.now = a.now
	backup(t, bk, k, group, snapID(2), a.now, "p", "beta")
	res, err := m.Run(ctx, "custody", "amsterdam")
	if err != nil || res.Synced != 1 {
		t.Fatalf("run after renewal: %v %+v", err, res)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "client.crt"))
	if string(after) == string(before) {
		t.Fatal("peer credential not renewed before the run")
	}
	if st := remote.LoadRenewState(dir); st.LastSuccess.IsZero() || st.ConfirmPending {
		t.Fatalf("renew state: %+v", st)
	}
	// the peer sees one identity with one valid certificate (the old one was confirmed away)
	rb, err := b.svc.CertificateStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range rb.Hosts {
		if h.Name == "peer-amsterdam" {
			found = true
			if h.LastRenewed == nil || h.OtherValid != 0 || h.State != "ok" {
				t.Fatalf("peer identity on the remote: %+v", h)
			}
		}
	}
	if !found {
		t.Fatal("peer identity missing on the remote")
	}
	// and this server reports its credential at the peer
	ra, err := a.svc.CertificateStatus(ctx)
	if err != nil || len(ra.SyncPeers) != 1 || ra.SyncPeers[0].Job != "amsterdam" || ra.SyncPeers[0].State != "ok" || ra.SyncPeers[0].DaysLeft < 700 {
		t.Fatalf("local status of the peer credential: %v %+v", err, ra.SyncPeers)
	}
}
