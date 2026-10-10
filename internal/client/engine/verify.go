package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/format"
)

// VerifySeenMax bounds the set of chunk ids a deep verification remembers (about 50 bytes each).
const VerifySeenMax = 1 << 20

// VerifyReport is the result of a deep verification.
type VerifyReport struct {
	Snapshots int
	Chunks    int
	Bytes     uint64
	Problems  []string
}

// Verify downloads and decrypts every chunk of the latest `last` snapshots of every
// object on a target (all snapshots when last <= 0), recomputes each keyed chunk
// id from the plaintext and checks manifests against indexes. It needs the key,
// so it proves the data is restorable, which a keyless server check cannot.
func (e *Engine) Verify(ctx context.Context, t *Target, last int) (*VerifyReport, error) {
	rep := &VerifyReport{}
	objs, err := t.Client.Objects(ctx)
	if err != nil {
		return rep, err
	}
	for _, o := range objs {
		// Chunks are remembered per object: successive snapshots of one object share almost all
		// of them, so each is fetched once, while a chunk shared by two objects is fetched once per
		// object. The set never covers more than one object, and is restarted when it reaches
		// VerifySeenMax (re-reading is extra traffic, never a skipped check).
		seen := map[format.ID]bool{}
		l, err := t.Client.Snapshots(ctx, o.GroupID)
		if err != nil {
			return rep, err
		}
		snaps := l.Snapshots
		if last > 0 && len(snaps) > last {
			snaps = snaps[len(snaps)-last:]
		}
		for _, s := range snaps {
			rep.Snapshots++
			e.verifyOne(ctx, t, o, s, seen, rep)
		}
	}
	sort.Strings(rep.Problems)
	if len(rep.Problems) > 0 {
		return rep, fmt.Errorf("verification found %d problem(s)", len(rep.Problems))
	}
	return rep, nil
}

func (e *Engine) verifyOne(ctx context.Context, t *Target, o api.RepoObject, s api.SnapshotRef, seen map[format.ID]bool, rep *VerifyReport) {
	label := fmt.Sprintf("%s/%s@%s", o.Project, o.Name, s.SnapshotID)
	prob := func(f string, a ...any) { rep.Problems = append(rep.Problems, label+": "+fmt.Sprintf(f, a...)) }
	ib, err := t.Client.Index(ctx, o.GroupID, s.SnapshotID)
	if err != nil {
		prob("index: %v", err)
		return
	}
	mb, err := t.Client.Manifest(ctx, o.GroupID, s.SnapshotID)
	if err != nil {
		prob("manifest: %v", err)
		return
	}
	man, err := t.Keys.OpenManifest(mb, o.GroupID, s.SnapshotID)
	if err != nil {
		prob("manifest does not decrypt with this key: %v", err)
		return
	}
	if man.IndexDigest != format.IndexDigest(ib) {
		prob("index digest differs from the manifest")
		return
	}
	idx, err := format.ParseIndex(ib)
	if err != nil {
		prob("index: %v", err)
		return
	}
	for _, id := range idx.ChunkIDs() {
		if seen[id] {
			continue
		}
		blob, err := t.Client.Chunk(ctx, id.String())
		if err != nil {
			prob("chunk %s: %v", id, err)
			continue
		}
		pt, err := t.Keys.DecodeChunk(blob, id)
		if err != nil {
			prob("chunk %s: %v", id, err)
			continue
		}
		if len(seen) >= VerifySeenMax {
			clear(seen)
		}
		seen[id] = true
		rep.Chunks++
		rep.Bytes += uint64(len(pt))
	}
}
