package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/unidoc/incus-backup/internal/archive"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/nbd"
)

// PickTarget returns the named target, or the first one by name when empty.
func (e *Engine) PickTarget(name string) (*Target, error) {
	if name != "" {
		t, ok := e.Targets[name]
		if !ok {
			return nil, fmt.Errorf("unknown server %q", name)
		}
		return t, nil
	}
	var names []string
	for n := range e.Targets {
		names = append(names, n)
	}
	if len(names) == 0 {
		return nil, errors.New("no servers configured")
	}
	sort.Strings(names)
	return e.Targets[names[0]], nil
}

// Located is a snapshot found on a target, verified against its manifest.
type Located struct {
	Target   *Target
	Group    string
	Snapshot string
	Kind     string
	Index    *format.Index
	Manifest *format.Manifest
}

func kindsFor(name string) []string {
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 2 && parts[0] == "instance":
		return []string{KindInstance}
	default:
		return []string{KindBlock, KindFilesystem}
	}
}

// Locate finds a snapshot ("" = latest) of an object, decrypts and checks its
// manifest and parses its index.
func (e *Engine) Locate(ctx context.Context, t *Target, project, name, snap string) (*Located, error) {
	// A VM root disk and a container root are both "instance/<n>/root": the repository may hold
	// snapshots of both kinds under one name (an old VM, a new container). Pick by the snapshot id
	// when one is given, else the newest snapshot of any kind.
	type cand struct {
		kind, group, id string
	}
	var best *cand
	for _, kind := range kindsFor(name) {
		group := t.Keys.GroupID(project, kind, name)
		l, err := t.Client.Snapshots(ctx, group)
		if err != nil {
			return nil, err
		}
		if len(l.Snapshots) == 0 {
			continue
		}
		id := snap
		if id == "" {
			id = l.Snapshots[len(l.Snapshots)-1].SnapshotID
		} else {
			found := false
			for _, s := range l.Snapshots {
				found = found || s.SnapshotID == id
			}
			if !found {
				continue
			}
		}
		if best == nil || id > best.id {
			best = &cand{kind, group, id}
		}
	}
	if best == nil {
		if snap != "" {
			return nil, fmt.Errorf("%s has no snapshot %s of %s/%s (project and name are case sensitive)", t.Name, snap, project, name)
		}
		return nil, fmt.Errorf("%s holds no snapshot of %s/%s (project and name are case sensitive)", t.Name, project, name)
	}
	ib, err := t.Client.Index(ctx, best.group, best.id)
	if err != nil {
		return nil, err
	}
	mb, err := t.Client.Manifest(ctx, best.group, best.id)
	if err != nil {
		return nil, err
	}
	man, err := t.Keys.OpenManifest(mb, best.group, best.id)
	if err != nil {
		return nil, fmt.Errorf("manifest of %s: %w", best.id, err)
	}
	if man.IndexDigest != format.IndexDigest(ib) {
		return nil, fmt.Errorf("the index of %s does not match its manifest: the repository was modified", best.id)
	}
	idx, err := format.ParseIndex(ib)
	if err != nil {
		return nil, err
	}
	return &Located{Target: t, Group: best.group, Snapshot: best.id, Kind: best.kind, Index: idx, Manifest: man}, nil
}

type fetched struct {
	data []byte
	err  error
}

// stream fetches and decodes the chunks of idx in order with a read-ahead window.
func (e *Engine) stream(ctx context.Context, t *Target, idx *format.Index, fn func(i int, data []byte) error) error {
	const window = 8
	n := len(idx.Entries)
	futures := make([]chan fetched, n)
	sem := make(chan struct{}, e.workers())
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	next := 0
	launch := func(upTo int) {
		for ; next < upTo && next < n; next++ {
			i := next
			ch := make(chan fetched, 1)
			futures[i] = ch
			ent := idx.Entries[i]
			_, size := idx.Span(i)
			if ent.ID.IsZero() {
				ch <- fetched{data: nil}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if cctx.Err() != nil {
					ch <- fetched{err: cctx.Err()}
					return
				}
				blob, err := t.Client.Chunk(cctx, ent.ID.String())
				if err != nil {
					ch <- fetched{err: fmt.Errorf("fetching chunk %s: %w", ent.ID, err)}
					return
				}
				pt, err := t.Keys.DecodeChunk(blob, ent.ID)
				if err != nil {
					ch <- fetched{err: fmt.Errorf("chunk %s: %w", ent.ID, err)}
					return
				}
				if idx.Kind == format.IndexFixed && uint64(len(pt)) != size {
					ch <- fetched{err: fmt.Errorf("chunk %s has %d bytes, the index says %d", ent.ID, len(pt), size)}
					return
				}
				ch <- fetched{data: pt}
			}()
		}
	}
	defer wg.Wait()
	for i := 0; i < n; i++ {
		launch(i + window)
		f := <-futures[i]
		futures[i] = nil // a consumed slot holds nothing: the window, not the index, bounds memory
		if f.err != nil {
			cancel()
			return f.err
		}
		data := f.data
		if data == nil { // zero chunk
			_, size := idx.Span(i)
			if err := fn(i, zeros(size)); err != nil {
				cancel()
				return err
			}
			continue
		}
		if err := fn(i, data); err != nil {
			cancel()
			return err
		}
	}
	return nil
}

// zeroChunk backs every zero chunk handed to a stream callback: callbacks only read it (io.Writer
// and io.WriterAt must not modify the slice), so a sparse disk costs no allocation per hole. It
// lives in BSS: untouched pages are never resident.
var zeroChunk [2 * format.FixedChunkSize]byte

var zeroBuf = zeroChunk[:]

// zeros returns n zero bytes, shared and read-only when n fits the static zero chunk.
func zeros(n uint64) []byte {
	if n <= uint64(len(zeroChunk)) {
		return zeroChunk[:n:n]
	}
	return make([]byte, n)
}

// RestoreBlock writes a block snapshot to w. With assumeZero the destination is
// known to be zero already (a volume we just created), so zero chunks are skipped.
func (e *Engine) RestoreBlock(ctx context.Context, loc *Located, w io.WriterAt, assumeZero bool, progress func(done, total uint64)) error {
	if loc.Index.Kind != format.IndexFixed {
		return fmt.Errorf("%s is not a block snapshot", loc.Snapshot)
	}
	var done uint64
	err := e.streamBlock(ctx, loc, assumeZero, func(i int, data []byte, zero bool) error {
		off, n := loc.Index.Span(i)
		if zero && assumeZero {
			done += n
			return nil
		}
		if _, err := w.WriteAt(data, int64(off)); err != nil {
			return fmt.Errorf("writing at %d: %w", off, err)
		}
		done += n
		if progress != nil {
			progress(done, loc.Index.Size)
		}
		return nil
	})
	return err
}

func (e *Engine) streamBlock(ctx context.Context, loc *Located, avoidZeros bool, fn func(i int, data []byte, zero bool) error) error {
	idx := loc.Index
	// zero chunks never need fetching; avoid allocating them when skipped
	return e.stream(ctx, loc.Target, idx, func(i int, data []byte) error {
		return fn(i, data, idx.Entries[i].ID.IsZero())
	})
}

// RestoreFS extracts a filesystem snapshot into dest (which must exist).
func (e *Engine) RestoreFS(ctx context.Context, loc *Located, dest string, noChown bool) error {
	if loc.Index.Kind != format.IndexDynamic {
		return fmt.Errorf("%s is not a filesystem snapshot", loc.Snapshot)
	}
	if IsCatalogSnapshot(loc) {
		return e.restoreFSCatalog(ctx, loc, dest, noChown)
	}
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		errc <- archive.Extract(pr, dest, archive.ExtractOptions{NoChown: noChown})
		_, _ = io.Copy(io.Discard, pr)
	}()
	err := e.stream(ctx, loc.Target, loc.Index, func(i int, data []byte) error {
		_, werr := pw.Write(data)
		return werr
	})
	_ = pw.CloseWithError(err)
	xerr := <-errc
	if err != nil {
		return err
	}
	return xerr
}

// RestoreOptions drive a whole-instance restore.
type RestoreOptions struct {
	Server    string
	Project   string
	Name      string // instance name in the repository
	Snapshot  string // "" = latest
	AsProject string // destination project (default: same)
	AsName    string // destination name (default: same)
	Volumes   bool   // also restore referenced custom volumes that do not exist
	Start     bool
	NoChown   bool
	// KeepNetwork keeps NIC identities (hwaddr, static addresses) when restoring under a
	// different name or project. Default: drop them, so a copy next to the original
	// does not clash on MAC or IP.
	KeepNetwork bool
}

// RestoreResult reports what was restored.
type RestoreResult struct {
	Instance string
	Objects  []string
	Bytes    uint64
}

type apiInstance struct {
	Type            string                       `json:"type"`
	Config          map[string]string            `json:"config"`
	Devices         map[string]map[string]string `json:"devices"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
	Profiles        []string                     `json:"profiles"`
}

func dropVolatile(c map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range c {
		if strings.HasPrefix(k, "volatile.") {
			continue
		}
		out[k] = v
	}
	return out
}

// RestoreInstance recreates an instance from its manifest: an empty instance is
// created with the stored configuration, then its disks (or container tree) and,
// optionally, referenced custom volumes are written from the repository. It
// refuses to touch an existing instance.
func (e *Engine) RestoreInstance(ctx context.Context, o RestoreOptions) (*RestoreResult, error) {
	t, err := e.PickTarget(o.Server)
	if err != nil {
		return nil, err
	}
	dstProject, dstName := o.AsProject, o.AsName
	if dstProject == "" {
		dstProject = o.Project
	}
	if dstName == "" {
		dstName = o.Name
	}
	im, err := e.Locate(ctx, t, o.Project, "instance/"+o.Name, o.Snapshot)
	if err != nil {
		return nil, err
	}
	if ok, err := e.Inc.InstanceExists(ctx, dstProject, dstName); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("instance %s/%s already exists: restore under another name with --as, or delete it first", dstProject, dstName)
	}
	var ai apiInstance
	if err := json.Unmarshal(im.Manifest.Config, &ai); err != nil {
		return nil, fmt.Errorf("the stored instance configuration is unreadable: %w", err)
	}
	rootName := "instance/" + o.Name + "/root"
	refSnap := func(ref string) string { return im.Manifest.Meta["snap:"+ref] }
	rootLoc, err := e.Locate(ctx, t, o.Project, rootName, refSnap(rootName))
	if err != nil {
		return nil, fmt.Errorf("the root disk: %w", err)
	}
	rootPool := ""
	for _, d := range ai.ExpandedDevices {
		if d["type"] == "disk" && d["path"] == "/" {
			rootPool = d["pool"]
		}
	}
	devices := ai.Devices
	if (dstProject != o.Project || dstName != o.Name) && !o.KeepNetwork {
		devices = stripNICIdentity(devices)
	}
	spec := incusx.InstanceSpec{Project: dstProject, Name: dstName, Type: ai.Type, Config: dropVolatile(ai.Config), Devices: devices,
		Profiles: ai.Profiles, RootPool: rootPool, RootSize: int64(rootLoc.Manifest.Object.Size)}
	if err := e.Inc.CreateInstance(ctx, spec); err != nil {
		return nil, fmt.Errorf("creating the instance: %w", err)
	}
	res := &RestoreResult{Instance: dstProject + "/" + dstName}
	rootRef := incusx.VolRef{Pool: rootPool, Project: dstProject, Name: dstName, Type: "container"}
	if ai.Type == incusx.TypeVM {
		rootRef.Type = "virtual-machine"
		if err := e.writeVolume(ctx, rootRef, rootLoc, true); err != nil {
			return res, fmt.Errorf("restoring the root disk: %w (the empty instance %s was left in place)", err, res.Instance)
		}
	} else {
		dir, release, err := e.Inc.FilesystemPath(ctx, rootRef)
		if err != nil {
			return res, fmt.Errorf("locating the container root: %w", err)
		}
		defer release()
		if err := e.RestoreFS(ctx, rootLoc, dir, o.NoChown); err != nil {
			return res, fmt.Errorf("restoring the container tree: %w (the empty instance %s was left in place)", err, res.Instance)
		}
	}
	res.Objects = append(res.Objects, rootName)
	res.Bytes += rootLoc.Index.Size
	if o.Volumes {
		for _, ref := range im.Manifest.Refs {
			if !strings.HasPrefix(ref, "volume/") {
				continue
			}
			vname := strings.TrimPrefix(ref, "volume/")
			vref := incusx.VolRef{Project: dstProject, Type: "custom", Name: vname}
			if ok, _ := e.Inc.VolumeExists(ctx, vref); ok {
				continue
			}
			vloc, err := e.Locate(ctx, t, o.Project, ref, refSnap(ref))
			if err != nil {
				return res, fmt.Errorf("volume %s: %w", ref, err)
			}
			pool := rootPool
			for _, d := range ai.Devices {
				if d["source"] == vname && d["pool"] != "" {
					pool = d["pool"]
				}
			}
			vref.Pool = pool
			content := "filesystem"
			if vloc.Kind == KindBlock {
				content = "block"
			}
			if err := e.Inc.CreateVolume(ctx, incusx.VolumeSpec{Pool: pool, Project: dstProject, Name: vname, ContentType: content, Size: int64(vloc.Manifest.Object.Size)}); err != nil {
				return res, fmt.Errorf("creating volume %s: %w", vname, err)
			}
			if content == "block" {
				if err := e.writeVolume(ctx, vref, vloc, true); err != nil {
					return res, fmt.Errorf("restoring volume %s: %w", vname, err)
				}
			} else {
				dir, release, err := e.Inc.FilesystemPath(ctx, vref)
				if err != nil {
					return res, err
				}
				err = e.RestoreFS(ctx, vloc, dir, o.NoChown)
				release()
				if err != nil {
					return res, fmt.Errorf("restoring volume %s: %w", vname, err)
				}
			}
			res.Objects = append(res.Objects, ref)
			res.Bytes += vloc.Index.Size
		}
	}
	if o.Start {
		if err := e.Inc.StartInstance(ctx, dstProject, dstName); err != nil {
			return res, fmt.Errorf("starting: %w", err)
		}
	}
	return res, nil
}

// writeVolume writes a block snapshot into a stopped volume through Incus's
// per-volume NBD endpoint (writable).
func (e *Engine) writeVolume(ctx context.Context, ref incusx.VolRef, loc *Located, assumeZero bool) error {
	nc, err := e.Inc.VolumeNBD(ctx, ref, true)
	if err != nil {
		return fmt.Errorf("opening the volume for writing: %w", err)
	}
	c, err := nbd.Dial(ctx, nc, "", nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if c.Size() < loc.Index.Size {
		return fmt.Errorf("the destination volume is %d bytes, the backup is %d", c.Size(), loc.Index.Size)
	}
	if err := e.RestoreBlock(ctx, loc, c, assumeZero, nil); err != nil {
		return err
	}
	return c.Flush()
}

// RestoreVolume restores one storage object (a custom volume or a VM disk) into a
// volume of the given reference, creating it when it does not exist.
func (e *Engine) RestoreVolume(ctx context.Context, server, project, name, snap string, dst incusx.VolRef, noChown bool) (*Located, error) {
	t, err := e.PickTarget(server)
	if err != nil {
		return nil, err
	}
	loc, err := e.Locate(ctx, t, project, name, snap)
	if err != nil {
		return nil, err
	}
	exists, err := e.Inc.VolumeExists(ctx, dst)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("volume %s already exists: restore into a new name", dst.Name)
	}
	content := "filesystem"
	if loc.Kind == KindBlock {
		content = "block"
	}
	if err := e.Inc.CreateVolume(ctx, incusx.VolumeSpec{Pool: dst.Pool, Project: dst.Project, Name: dst.Name, ContentType: content, Size: int64(loc.Manifest.Object.Size)}); err != nil {
		return nil, err
	}
	if content == "block" {
		return loc, e.writeVolume(ctx, dst, loc, true)
	}
	dir, release, err := e.Inc.FilesystemPath(ctx, dst)
	if err != nil {
		return loc, err
	}
	defer release()
	return loc, e.RestoreFS(ctx, loc, dir, noChown)
}

var _ = bytes.Equal

// nicIdentityKeys identify a NIC on the network: a restored copy must not reuse them.
var nicIdentityKeys = []string{"hwaddr", "ipv4.address", "ipv6.address", "host_name"}

// stripNICIdentity returns a copy of devices without the identity keys of NICs.
func stripNICIdentity(devices map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for name, d := range devices {
		c := map[string]string{}
		for k, v := range d {
			c[k] = v
		}
		if c["type"] == "nic" {
			for _, k := range nicIdentityKeys {
				delete(c, k)
			}
		}
		out[name] = c
	}
	return out
}
