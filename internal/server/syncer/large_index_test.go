package syncer_test

import (
	"context"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
)

// A 4 TiB disk has an index of a million entries (40 MB, 53 MB as base64 in the JSON of the
// sync snapshot PUT). It must synchronise like any other snapshot: the route's body limit is
// derived from the index limit, not from the small-JSON limit.
func TestSyncCarriesAnIndexOfAMillionEntries(t *testing.T) {
	a, b := newNode(t, "cikarang"), newNode(t, "amsterdam")
	ctx := context.Background()
	k := keys()
	c := a.enrol("benfica", api.ScopeBackup)
	c.Domain = k.DomainID
	snap := snapID(1)
	s, err := c.Begin(ctx, api.SessionRequest{GroupID: group, SnapshotID: snap, Created: a.now, Policy: "p", Project: "custody", Object: "huge/root", Kind: "block"})
	if err != nil {
		t.Fatal(err)
	}
	// four distinct chunks, repeated: a million entries without a million uploads
	const entries = 1 << 20
	var ids []format.ID
	for _, p := range []string{"alpha", "beta", "gamma", "delta"} {
		id, blob, _ := k.EncodeChunk([]byte(p+"-xxxxxxxxxxxxxxxxxxxxxxxxxxx"), format.EncodeOpts{})
		if err := s.PutChunk(ctx, id.String(), blob); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	const unit = 32 // every part above is 32 bytes
	x := &format.Index{Kind: format.IndexDynamic, Size: entries * unit, Entries: make([]format.Entry, entries)}
	for i := range x.Entries {
		x.Entries[i] = format.Entry{End: uint64(i+1) * unit, ID: ids[i%len(ids)]}
	}
	idx, err := x.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutIndex(ctx, idx); err != nil {
		t.Fatal(err)
	}
	man, _ := k.SealManifest(&format.Manifest{SnapshotID: snap, Created: a.now, Object: format.ObjectInfo{Name: "huge/root", Kind: "block", Size: x.Size}, IndexDigest: format.IndexDigest(idx)}, group, nil)
	if err := s.PutManifest(ctx, man); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, api.CommitRequest{LogicalBytes: int64(x.Size)}); err != nil {
		t.Fatal(err)
	}
	m, _, _ := addJob(t, a, b, "push", "amsterdam")
	res, err := m.Run(ctx, "custody", "amsterdam")
	if err != nil || res.Synced != 1 {
		t.Fatalf("a 1M-entry index (%d MB) did not synchronise: %v %+v", len(idx)>>20, err, res)
	}
	got, err := b.svc.RepoIndex(ctx, b.repo, group, snap)
	if err != nil || format.IndexDigest(got) != format.IndexDigest(idx) {
		t.Fatalf("the destination index differs: %v", err)
	}
}
