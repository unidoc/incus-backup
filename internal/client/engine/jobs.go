package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/archive"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/fscat"
)

// Engine entry points used by host jobs (package client/jobs).

func extractTar(r io.Reader, dest string, noChown bool) error {
	return archive.Extract(r, dest, archive.ExtractOptions{NoChown: noChown})
}

// VerifyObjects is Verify restricted to the objects match accepts (nil = all), with progress per
// snapshot. Problems name objects, snapshots and chunk ids only.
func (e *Engine) VerifyObjects(ctx context.Context, t *Target, last int, match func(api.RepoObject) bool, progress func(done, total int)) (*VerifyReport, error) {
	rep := &VerifyReport{}
	objs, err := t.Client.Objects(ctx)
	if err != nil {
		return rep, err
	}
	type item struct {
		o api.RepoObject
		s api.SnapshotRef
	}
	var items []item
	for _, o := range objs {
		if match != nil && !match(o) {
			continue
		}
		l, err := t.Client.Snapshots(ctx, o.GroupID)
		if err != nil {
			return rep, err
		}
		snaps := l.Snapshots
		if last > 0 && len(snaps) > last {
			snaps = snaps[len(snaps)-last:]
		}
		for _, s := range snaps {
			items = append(items, item{o, s})
		}
	}
	seen := map[format.ID]bool{}
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Snapshots++
		e.verifyOne(ctx, t, it.o, it.s, seen, rep)
		if progress != nil {
			progress(i+1, len(items))
		}
	}
	sort.Strings(rep.Problems)
	if len(rep.Problems) > 0 {
		return rep, fmt.Errorf("verification found %d problem(s)", len(rep.Problems))
	}
	return rep, nil
}

// ExtractFSPaths extracts paths of a filesystem catalog snapshot into dest (an existing, empty
// directory), fetching only the chunks that hold them; every file is checked against its SHA-256.
// It returns the snapshot id.
//
// It also returns what the host refused while restoring ownership and attributes (see fscat.ExtractOptions.Lenient):
// those are skipped and reported, never fatal, and never applied silently.
func (e *Engine) ExtractFSPaths(ctx context.Context, server, project, name, snap string, paths []string, dest string) (string, *fscat.Skips, error) {
	t, err := e.PickTarget(server)
	if err != nil {
		return "", nil, err
	}
	loc, err := e.Locate(ctx, t, project, name, snap)
	if err != nil {
		return "", nil, err
	}
	if loc.Kind != KindFilesystem || !IsCatalogSnapshot(loc) {
		return loc.Snapshot, nil, &PublicError{Msg: fmt.Sprintf("%s@%s is not a filesystem catalog snapshot (single files can only be extracted from those; use guest_files for VM disks)", name, loc.Snapshot)}
	}
	v, err := e.OpenFS(ctx, loc)
	if err != nil {
		return loc.Snapshot, nil, err
	}
	// The daemon runs as root: extract WITH ownership, so the delivered tar carries the catalog uid/gid
	// and its set-id bits are faithful (extracting without ownership as root would hand every file to
	// root, and fscat then drops the set-id bits). Device nodes of a guest catalog are never created on
	// the host; dest lives in a private 0700 scratch directory. Not root: ownership cannot be restored.
	skips := &fscat.Skips{}
	err = v.Tree.Extract(dest, fscat.FromReaderAt(v), fscat.ExtractOptions{NoChown: os.Geteuid() != 0, SkipDevices: true, Paths: paths, Lenient: true, Skips: skips})
	return loc.Snapshot, skips, err
}
