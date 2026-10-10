package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/unidoc/incus-backup/internal/archive"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/nbd"
	"github.com/unidoc/incus-backup/internal/recovery"
)

// GuestFilesOptions drive the recovery of single files from a backed up VM disk.
type GuestFilesOptions struct {
	Server   string
	Project  string
	Name     string // block object: "instance/<vm>/root" or "volume/<name>"
	Snapshot string // "" = latest
	Paths    []string
	// Partition of the guest disk to read (0 = the first one that holds the first path).
	Partition int
	Dest      string // existing local directory
	// Image of the disposable recovery VM, "images:alpine/edge/cloud" or a local alias. It must
	// provide cloud-init, busybox/GNU tar and mount support for the guest's filesystem.
	Image string
	// Pool for the scratch volumes and the VM's root disk.
	Pool           string
	ScratchProject string // project for the scratch objects (default: Project)
	Memory         string
	CPU            string
	// OutSize is the size of the output volume, the upper bound of what can be recovered in one go.
	OutSize int64
	Timeout time.Duration
	// MaxBytes bounds what is extracted on this host.
	MaxBytes int64
}

// GuestFilesResult reports a recovery.
type GuestFilesResult struct {
	Snapshot string
	Header   *recovery.Header
	Report   *archive.ExtractReport
}

// RestoreGuestFiles recovers single files from a VM disk backup without mounting the guest
// filesystem on this host: see package recovery for the isolation design.
func (e *Engine) RestoreGuestFiles(ctx context.Context, o GuestFilesOptions) (*GuestFilesResult, error) {
	if o.Image == "" {
		return nil, errors.New("a recovery image is required (--image), e.g. images:alpine/edge/cloud")
	}
	if o.Pool == "" {
		return nil, errors.New("a storage pool for the scratch volumes is required (--pool)")
	}
	paths, err := recovery.CleanPaths(o.Paths)
	if err != nil {
		return nil, err
	}
	userData, err := recovery.UserData(recovery.Request{Paths: paths, Partition: o.Partition})
	if err != nil {
		return nil, err
	}
	t, err := e.PickTarget(o.Server)
	if err != nil {
		return nil, err
	}
	loc, err := e.Locate(ctx, t, o.Project, o.Name, o.Snapshot)
	if err != nil {
		return nil, err
	}
	if loc.Kind != KindBlock {
		return nil, fmt.Errorf("%s is not a block object (a VM disk or block volume); use `fs extract` for filesystem backups", o.Name)
	}
	project := o.ScratchProject
	if project == "" {
		project = o.Project
	}
	if o.OutSize <= 0 {
		o.OutSize = 1 << 30
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Minute
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	id := hex.EncodeToString(rnd[:])
	disk := incusx.VolRef{Pool: o.Pool, Project: project, Type: "custom", Name: "ibk-rec-" + id + "-disk"}
	out := incusx.VolRef{Pool: o.Pool, Project: project, Type: "custom", Name: "ibk-rec-" + id + "-out"}
	vm := "ibk-rec-" + id

	// Whatever happens, nothing of the recovery may be left behind.
	var createdVM, createdDisk, createdOut bool
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if createdVM {
			if err := e.Inc.DeleteInstance(cctx, project, vm); err != nil {
				e.Log.Warn("could not delete the recovery VM; remove it by hand", "instance", vm, "err", err)
			}
		}
		for _, c := range []struct {
			made bool
			ref  incusx.VolRef
		}{{createdDisk, disk}, {createdOut, out}} {
			if c.made {
				if err := e.Inc.DeleteVolume(cctx, c.ref); err != nil {
					e.Log.Warn("could not delete a recovery scratch volume; remove it by hand", "volume", c.ref.Name, "err", err)
				}
			}
		}
	}()

	// 1. the guest disk as it was at the snapshot, in a scratch volume
	createdDisk = true // set BEFORE the call: a call that fails half-way may still have created the object
	if err := e.Inc.CreateVolume(ctx, incusx.VolumeSpec{Pool: o.Pool, Project: project, Name: disk.Name, ContentType: "block", Size: int64(loc.Index.Size)}); err != nil {
		return nil, fmt.Errorf("creating the scratch disk: %w", err)
	}
	if err := e.writeVolume(ctx, disk, loc, true); err != nil {
		return nil, fmt.Errorf("restoring the disk into the scratch volume: %w", err)
	}
	// 2. the output volume
	createdOut = true
	if err := e.Inc.CreateVolume(ctx, incusx.VolumeSpec{Pool: o.Pool, Project: project, Name: out.Name, ContentType: "block", Size: o.OutSize}); err != nil {
		return nil, fmt.Errorf("creating the output volume: %w", err)
	}
	// 3. the disposable VM: no network, no profile, two scratch volumes
	createdVM = true
	if err := e.Inc.CreateRecoveryVM(ctx, incusx.RecoverySpec{Project: project, Name: vm, Pool: o.Pool, Image: o.Image, CloudInit: userData,
		Memory: o.Memory, CPU: o.CPU, Disk: disk, Out: out}); err != nil {
		return nil, fmt.Errorf("creating the recovery VM: %w", err)
	}
	if err := e.Inc.StartInstance(ctx, project, vm); err != nil {
		return nil, fmt.Errorf("starting the recovery VM: %w", err)
	}
	if err := e.Inc.WaitStopped(ctx, project, vm, o.Timeout); err != nil {
		return nil, err
	}
	// the VM is stopped: nothing can write to the output volume any more. Delete it before reading
	// so the volume is unused (the per-volume NBD endpoint needs that).
	if err := e.Inc.DeleteInstance(ctx, project, vm); err != nil {
		return nil, fmt.Errorf("removing the recovery VM: %w", err)
	}
	createdVM = false
	// 4. read the result through NBD; nothing is mounted on this host
	nc, err := e.Inc.VolumeNBD(ctx, out, false)
	if err != nil {
		return nil, fmt.Errorf("opening the output volume: %w", err)
	}
	c, err := nbd.Dial(ctx, nc, "", nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	hdr, tr, err := recovery.ParseOutput(c)
	res := &GuestFilesResult{Snapshot: loc.Snapshot, Header: hdr}
	if err != nil {
		return res, err
	}
	rep := &archive.ExtractReport{}
	if err := archive.ExtractTar(tr, o.Dest, archive.ExtractOptions{Untrusted: true, MaxBytes: o.MaxBytes, Report: rep}); err != nil {
		res.Report = rep
		return res, fmt.Errorf("extracting the recovered files: %w", err)
	}
	res.Report = rep
	return res, nil
}
