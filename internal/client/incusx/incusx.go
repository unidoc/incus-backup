// Package incusx is the narrow view of Incus that incus-backup needs. The real
// implementation (real.go) talks to the Incus API with the official client
// library; the fake (package fake) simulates Incus well enough to test the
// engines, including dirty bitmaps and frozen NBD views.
package incusx

import (
	"context"
	"net"
	"time"
)

// Instance types.
const (
	TypeVM        = "virtual-machine"
	TypeContainer = "container"
)

// Instance is one Incus instance as the backup engine sees it.
type Instance struct {
	Project         string
	Name            string
	Type            string
	Running         bool
	Config          map[string]string
	Devices         map[string]map[string]string
	ExpandedConfig  map[string]string
	ExpandedDevices map[string]map[string]string
	Profiles        []string
	// JSON is the instance as returned by the API (used verbatim in manifests).
	JSON []byte
}

// RootPool returns the pool of the instance's root disk.
func (i Instance) RootPool() string {
	if d, ok := i.ExpandedDevices["root"]; ok {
		return d["pool"]
	}
	for _, d := range i.ExpandedDevices {
		if d["type"] == "disk" && d["path"] == "/" {
			return d["pool"]
		}
	}
	return ""
}

// Volume is a custom storage volume.
type Volume struct {
	Pool        string
	Project     string
	Name        string
	ContentType string // "block" or "filesystem"
	Config      map[string]string
	UsedBy      []string // instance names (same project) that attach it
	JSON        []byte
}

// VolRef addresses a storage volume.
type VolRef struct {
	Pool    string
	Project string
	Type    string // "custom", "virtual-machine" or "container"
	Name    string
}

// Bitmap is a dirty bitmap as reported by Incus.
type Bitmap struct {
	Name         string
	DirtyBytes   int64
	Granularity  int64
	Recording    bool
	Busy         bool
	Persistent   bool
	Inconsistent bool
}

// Info describes the Incus server.
type Info struct {
	Version    string
	Extensions []string
	Clustered  bool
}

// VolumeSpec describes a volume to create on restore.
type VolumeSpec struct {
	Pool        string
	Project     string
	Name        string
	ContentType string
	Size        int64
	Config      map[string]string
}

// InstanceSpec describes an empty instance to create on restore.
type InstanceSpec struct {
	Project  string
	Name     string
	Type     string
	Config   map[string]string
	Devices  map[string]map[string]string
	Profiles []string
	RootPool string
	RootSize int64
}

// RecoverySpec describes the disposable VM that reads an untrusted guest disk (see package recovery).
type RecoverySpec struct {
	Project string
	Name    string
	Pool    string
	// Image is "remote:alias" (remote "images" is the public simplestreams server) or a local alias/fingerprint.
	Image     string
	CloudInit string // cloud-init user-data
	Memory    string // e.g. "512MiB"
	CPU       string // e.g. "1"
	// Disk (attached as device "guestdisk") and Out (device "out") are scratch block volumes.
	Disk VolRef
	Out  VolRef
}

// API is everything the engines use from Incus.
type API interface {
	Info(ctx context.Context) (*Info, error)
	Projects(ctx context.Context) ([]string, error)
	Instances(ctx context.Context, project string) ([]Instance, error)
	Volumes(ctx context.Context, project string) ([]Volume, error)

	// Dirty bitmaps. CreateInstanceBitmap covers the root disk and disks with
	// dependent=true in one operation; every other volume needs CreateBitmap.
	CreateInstanceBitmap(ctx context.Context, project, instance, name string, persistent bool) error
	CreateBitmap(ctx context.Context, v VolRef, name string, persistent bool) error
	Bitmaps(ctx context.Context, v VolRef) ([]Bitmap, error)
	DeleteBitmap(ctx context.Context, v VolRef, name string) error

	// NBD. InstanceNBD exports every disk of a running VM under its device name;
	// the first connection (reuse=false) freezes a consistent view, further
	// connections (reuse=true) attach to the same session. VolumeNBD serves one
	// volume that no running instance uses.
	InstanceNBD(ctx context.Context, project, instance string, reuse bool) (net.Conn, error)
	VolumeNBD(ctx context.Context, v VolRef, writable bool) (net.Conn, error)

	// Snapshots for filesystem objects.
	CreateSnapshot(ctx context.Context, v VolRef, name string) error
	DeleteSnapshot(ctx context.Context, v VolRef, name string) error
	// SnapshotPath returns a directory with the read-only contents of a snapshot.
	SnapshotPath(ctx context.Context, v VolRef, name string) (path string, release func(), err error)
	// FilesystemPath returns the mounted directory of a volume (restore target).
	FilesystemPath(ctx context.Context, v VolRef) (path string, release func(), err error)

	// Restore helpers.
	CreateInstance(ctx context.Context, s InstanceSpec) error
	CreateVolume(ctx context.Context, s VolumeSpec) error
	StartInstance(ctx context.Context, project, name string) error
	StopInstance(ctx context.Context, project, name string) error
	InstanceExists(ctx context.Context, project, name string) (bool, error)
	VolumeExists(ctx context.Context, v VolRef) (bool, error)

	// Disposable recovery VM (granular file recovery from VM backups). CreateRecoveryVM creates a VM
	// with NO profiles, NO network and only the two scratch volumes; WaitStopped blocks until it has
	// powered itself off (and fails after the timeout). DeleteInstance force-stops and removes an
	// instance; DeleteVolume removes a custom volume.
	CreateRecoveryVM(ctx context.Context, s RecoverySpec) error
	WaitStopped(ctx context.Context, project, name string, timeout time.Duration) error
	DeleteInstance(ctx context.Context, project, name string) error
	DeleteVolume(ctx context.Context, v VolRef) error
}

// Required API extensions; doctor checks them.
var RequiredExtensions = []string{"instance_nbd", "storage_volume_nbd"}

// RootRef returns the volume reference of an instance's root volume.
func RootRef(i Instance) VolRef {
	t := "container"
	if i.Type == TypeVM {
		t = "virtual-machine"
	}
	return VolRef{Pool: i.RootPool(), Project: i.Project, Type: t, Name: i.Name}
}
