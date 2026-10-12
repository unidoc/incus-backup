package incusx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// Real talks to a local Incus daemon through its unix socket.
type Real struct {
	srv    incus.InstanceServer
	varDir string
	// mountDir is where read-only snapshot mounts are made (the client's own state
	// directory, never Incus's).
	mountDir string
}

// SetMountDir sets the directory for temporary snapshot mounts.
func (r *Real) SetMountDir(d string) { r.mountDir = d }

// CleanStaleMounts unmounts and removes leftovers of a previous run that was killed. Every mount
// directory is named after the process that made it ("snap-<pid>-<random>"): only those whose process no
// longer exists are touched. A client command (doctor, status, list...) started next to the running
// daemon must never unmount the daemon's live snapshot mount (the walk would see an empty directory).
func (r *Real) CleanStaleMounts() {
	es, err := os.ReadDir(r.mountDir)
	if err != nil {
		return
	}
	for _, e := range es {
		pid, ok := mountOwner(e.Name())
		if !ok || processAlive(pid) {
			continue // not ours to judge, or still in use
		}
		p := filepath.Join(r.mountDir, e.Name())
		_ = unix.Unmount(p, 0)
		_ = os.Remove(p) // only succeeds if it is an empty directory
	}
}

// mountOwner extracts the pid from "snap-<pid>-<random>".
func mountOwner(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "snap-")
	if !ok {
		return 0, false
	}
	num, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(num)
	return pid, err == nil && pid > 0
}

// processAlive reports whether a process with this pid exists (EPERM still means it does).
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}

// Sockets searched when none is configured.
var defaultSockets = []string{"/var/lib/incus/unix.socket", "/run/incus/unix.socket"}

// Connect opens the Incus unix socket. socket may be empty (auto-detect). The
// Incus data directory (needed to find snapshot contents) is INCUS_DIR, or the
// directory of the socket.
func Connect(socket string) (*Real, error) {
	cands := defaultSockets
	if s := os.Getenv("INCUS_SOCKET"); s != "" && socket == "" {
		socket = s
	}
	if socket != "" {
		cands = []string{socket}
	}
	var last error
	for _, c := range cands {
		if _, err := os.Stat(c); err != nil {
			last = err
			continue
		}
		srv, err := incus.ConnectIncusUnix(c, nil)
		if err != nil {
			last = err
			continue
		}
		dir := os.Getenv("INCUS_DIR")
		if dir == "" {
			dir = filepath.Dir(c)
			if filepath.Base(dir) == "incus" && strings.HasPrefix(dir, "/run") {
				dir = "/var/lib/incus"
			}
		}
		return &Real{srv: srv, varDir: dir, mountDir: "/var/lib/incus-backup/mnt"}, nil
	}
	return nil, fmt.Errorf("cannot reach Incus (tried %s): %w", strings.Join(cands, ", "), last)
}

func (r *Real) proj(p string) incus.InstanceServer {
	if p == "" || p == "default" {
		return r.srv.UseProject("default")
	}
	return r.srv.UseProject(p)
}

func wait(op incus.Operation, err error) error {
	if err != nil {
		return err
	}
	return op.Wait()
}

// Info returns server details.
func (r *Real) Info(ctx context.Context) (*Info, error) {
	s, _, err := r.srv.GetServer()
	if err != nil {
		return nil, err
	}
	return &Info{Version: s.Environment.ServerVersion, Extensions: s.APIExtensions, Clustered: s.Environment.ServerClustered}, nil
}

// Projects lists project names.
func (r *Real) Projects(ctx context.Context) ([]string, error) { return r.srv.GetProjectNames() }

// Instances lists the instances of a project.
func (r *Real) Instances(ctx context.Context, project string) ([]Instance, error) {
	full, err := r.proj(project).GetInstancesFull(api.InstanceTypeAny)
	if err != nil {
		return nil, err
	}
	var out []Instance
	for _, f := range full {
		inst := f.Instance
		b, err := json.Marshal(inst)
		if err != nil {
			return nil, err
		}
		out = append(out, Instance{Project: project, Name: inst.Name, Type: inst.Type, Running: inst.Status == "Running",
			Config: inst.Config, Devices: inst.Devices, ExpandedConfig: inst.ExpandedConfig, ExpandedDevices: inst.ExpandedDevices,
			Profiles: inst.Profiles, JSON: b})
	}
	return out, nil
}

// Volumes lists custom storage volumes visible in a project.
func (r *Real) Volumes(ctx context.Context, project string) ([]Volume, error) {
	pools, err := r.srv.GetStoragePools()
	if err != nil {
		return nil, err
	}
	var out []Volume
	for _, p := range pools {
		vols, err := r.proj(project).GetStoragePoolVolumes(p.Name)
		if err != nil {
			return nil, fmt.Errorf("listing volumes of pool %s: %w", p.Name, err)
		}
		for _, v := range vols {
			if v.Type != "custom" {
				continue
			}
			b, _ := json.Marshal(v)
			vol := Volume{Pool: p.Name, Project: project, Name: v.Name, ContentType: v.ContentType, Config: v.Config, JSON: b}
			for _, u := range v.UsedBy {
				if n := instanceFromURL(u); n != "" {
					vol.UsedBy = append(vol.UsedBy, n)
				}
			}
			out = append(out, vol)
		}
	}
	return out, nil
}

func instanceFromURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil || !strings.HasPrefix(parsed.Path, "/1.0/instances/") {
		return ""
	}
	return path.Base(parsed.Path)
}

// CreateInstanceBitmap creates a bitmap on the root disk and dependent disks.
func (r *Real) CreateInstanceBitmap(ctx context.Context, project, instance, name string, persistent bool) error {
	return r.proj(project).CreateInstanceBitmap(instance, api.StorageVolumeBitmapsPost{Name: name, Persistent: persistent})
}

// CreateBitmap creates a bitmap on a single volume.
func (r *Real) CreateBitmap(ctx context.Context, v VolRef, name string, persistent bool) error {
	return r.proj(v.Project).CreateStorageVolumeBitmap(v.Pool, v.Type, v.Name, api.StorageVolumeBitmapsPost{Name: name, Persistent: persistent})
}

// Bitmaps lists the bitmaps of a volume.
func (r *Real) Bitmaps(ctx context.Context, v VolRef) ([]Bitmap, error) {
	bs, err := r.proj(v.Project).GetStorageVolumeBitmaps(v.Pool, v.Type, v.Name)
	if err != nil {
		return nil, err
	}
	var out []Bitmap
	for _, b := range bs {
		out = append(out, Bitmap{Name: b.Name, DirtyBytes: int64(b.Count), Granularity: int64(b.Granularity), Recording: b.Recording, Busy: b.Busy, Persistent: b.Persistent, Inconsistent: b.Inconsistent})
	}
	return out, nil
}

// DeleteBitmap deletes a bitmap.
func (r *Real) DeleteBitmap(ctx context.Context, v VolRef, name string) error {
	return r.proj(v.Project).DeleteStorageVolumeBitmap(v.Pool, v.Type, v.Name, name)
}

// InstanceNBD opens an NBD connection to all disks of a running VM.
func (r *Real) InstanceNBD(ctx context.Context, project, instance string, reuse bool) (net.Conn, error) {
	return r.proj(project).GetInstanceNBDConn(instance, incus.InstanceNBDArgs{Reuse: reuse})
}

// VolumeNBD opens an NBD connection to one volume.
func (r *Real) VolumeNBD(ctx context.Context, v VolRef, writable bool) (net.Conn, error) {
	return r.proj(v.Project).GetStoragePoolVolumeBlockNBDConn(v.Pool, v.Type, v.Name, incus.StorageVolumeNBDPost{Writable: writable})
}

// CreateSnapshot snapshots an instance or custom volume (stateless).
func (r *Real) CreateSnapshot(ctx context.Context, v VolRef, name string) error {
	if v.Type == "custom" {
		return wait(r.proj(v.Project).CreateStoragePoolVolumeSnapshot(v.Pool, "custom", v.Name, api.StorageVolumeSnapshotsPost{Name: name}))
	}
	return wait(r.proj(v.Project).CreateInstanceSnapshot(v.Name, api.InstanceSnapshotsPost{Name: name, Stateful: false}))
}

// DeleteSnapshot deletes a snapshot created by CreateSnapshot.
func (r *Real) DeleteSnapshot(ctx context.Context, v VolRef, name string) error {
	if v.Type == "custom" {
		return wait(r.proj(v.Project).DeleteStoragePoolVolumeSnapshot(v.Pool, "custom", v.Name, name))
	}
	return wait(r.proj(v.Project).DeleteInstanceSnapshot(v.Name, name))
}

// incusName is how Incus names a project-scoped object on disk.
func incusName(project, name string) string {
	if project == "" || project == "default" {
		return name
	}
	return project + "_" + name
}

// volDir is the mount point of a volume: <var>/storage-pools/<pool>/<type>/<name>.
func (r *Real) volDir(v VolRef) string {
	return filepath.Join(r.varDir, "storage-pools", v.Pool, volTypeDir(v.Type), incusName(v.Project, v.Name))
}

// volTypeDir is the storage-driver name of a volume type.
func volTypeDir(t string) string {
	switch t {
	case "container":
		return "containers"
	case "virtual-machine":
		return "virtual-machines"
	}
	return t // "custom"
}

// zfsSnapshotDataset is the ZFS name of an Incus snapshot:
// <zfs.pool_name>/<type>/<project_name>@snapshot-<snap> [V: driver_zfs_utils.go dataset()].
func zfsSnapshotDataset(zfsPool string, v VolRef, snap string) string {
	return fmt.Sprintf("%s/%s/%s@snapshot-%s", zfsPool, volTypeDir(v.Type), incusName(v.Project, v.Name), snap)
}

// isMountPoint reports whether path is a mount point (its device differs from
// its parent's). An empty directory that merely exists must never be mistaken
// for snapshot contents: that would silently back up nothing.
func isMountPoint(path string) bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(path, &st) != nil || syscall.Stat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// SnapshotPath finds the read-only contents of a snapshot. In order:
//  1. where Incus itself mounted it: <var>/storage-pools/<pool>/<type>-snapshots/<name>/<snap>
//     (used only if it really is a mount point);
//  2. ZFS: mount the snapshot dataset read-only ourselves with mount(2), exactly
//     as Incus does ("Mount the snapshot directly (not possible through tools)",
//     driver_zfs_volumes.go), into a private temporary directory, released afterwards;
//  3. ZFS: the automounted <volume mount>/.zfs/snapshot/snapshot-<snap>.
//
// Which one works on a given host is what gate G3 records.
func (r *Real) SnapshotPath(ctx context.Context, v VolRef, name string) (string, func(), error) {
	t := volTypeDir(v.Type)
	incusMounted := filepath.Join(r.varDir, "storage-pools", v.Pool, t+"-snapshots", incusName(v.Project, v.Name), name)
	if isMountPoint(incusMounted) {
		return incusMounted, func() {}, nil
	}
	var tried []string
	pool, _, err := r.srv.GetStoragePool(v.Pool)
	if err == nil && pool.Driver == "zfs" {
		zp := pool.Config["zfs.pool_name"]
		if zp == "" {
			zp = v.Pool
		}
		ds := zfsSnapshotDataset(zp, v, name)
		_ = os.MkdirAll(r.mountDir, 0o700)
		dir, err := os.MkdirTemp(r.mountDir, fmt.Sprintf("snap-%d-", os.Getpid())) // the pid marks the owner: see CleanStaleMounts
		if err == nil {
			if err = unix.Mount(ds, dir, "zfs", unix.MS_RDONLY, ""); err == nil {
				return dir, func() {
					for i := 0; i < 20; i++ {
						if unix.Unmount(dir, 0) == nil {
							break
						}
						time.Sleep(500 * time.Millisecond)
					}
					_ = os.Remove(dir)
				}, nil
			}
			_ = os.Remove(dir)
		}
		tried = append(tried, fmt.Sprintf("mount -t zfs -o ro %s: %v", ds, err))
		ctl := filepath.Join(r.volDir(v), ".zfs", "snapshot", "snapshot-"+name)
		if _, err := os.ReadDir(ctl); err == nil && isMountPoint(ctl) { // the read triggers the automount
			return ctl, func() {}, nil
		}
		tried = append(tried, ctl+" (not mounted)")
	} else {
		tried = append(tried, incusMounted+" (not mounted by Incus and the pool is not ZFS)")
	}
	return "", nil, fmt.Errorf("cannot read the contents of snapshot %s of %s/%s: %s (block-backed filesystem volumes, zfs.block_mode, are not supported yet)",
		name, v.Project, v.Name, strings.Join(tried, "; "))
}

// FilesystemPath returns the mount point of a volume for restore.
func (r *Real) FilesystemPath(ctx context.Context, v VolRef) (string, func(), error) {
	d := r.volDir(v)
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("the volume is not mounted at %s (start the instance once or mount the volume): %v", d, err)
	}
	return d, func() {}, nil
}

// CreateInstance creates an empty instance (no image) with the given configuration.
func (r *Real) CreateInstance(ctx context.Context, s InstanceSpec) error {
	devs := map[string]map[string]string{}
	for k, v := range s.Devices {
		devs[k] = v
	}
	root := map[string]string{"type": "disk", "path": "/", "pool": s.RootPool}
	if s.Type == TypeVM && s.RootSize > 0 {
		root["size"] = fmt.Sprintf("%d", s.RootSize)
	}
	devs["root"] = root
	req := api.InstancesPost{
		Name:        s.Name,
		Type:        api.InstanceType(s.Type),
		Source:      api.InstanceSource{Type: "none"},
		InstancePut: api.InstancePut{Config: s.Config, Devices: devs, Profiles: s.Profiles},
	}
	return wait(r.proj(s.Project).CreateInstance(req))
}

// CreateVolume creates a custom volume.
func (r *Real) CreateVolume(ctx context.Context, s VolumeSpec) error {
	cfg := map[string]string{}
	for k, v := range s.Config {
		cfg[k] = v
	}
	if s.Size > 0 {
		cfg["size"] = fmt.Sprintf("%d", s.Size)
	}
	return r.proj(s.Project).CreateStoragePoolVolume(s.Pool, api.StorageVolumesPost{
		Name: s.Name, Type: "custom", ContentType: s.ContentType, StorageVolumePut: api.StorageVolumePut{Config: cfg},
	})
}

func (r *Real) state(project, name, action string) error {
	return wait(r.proj(project).UpdateInstanceState(name, api.InstanceStatePut{Action: action, Timeout: 120}, ""))
}

// StartInstance starts an instance.
func (r *Real) StartInstance(ctx context.Context, project, name string) error {
	return r.state(project, name, "start")
}

// StopInstance stops an instance.
func (r *Real) StopInstance(ctx context.Context, project, name string) error {
	return r.state(project, name, "stop")
}

// InstanceExists reports whether an instance exists.
func (r *Real) InstanceExists(ctx context.Context, project, name string) (bool, error) {
	_, _, err := r.proj(project).GetInstance(name)
	if err == nil {
		return true, nil
	}
	if api.StatusErrorCheck(err, 404) {
		return false, nil
	}
	return false, err
}

// VolumeExists reports whether a volume exists.
func (r *Real) VolumeExists(ctx context.Context, v VolRef) (bool, error) {
	_, _, err := r.proj(v.Project).GetStoragePoolVolume(v.Pool, v.Type, v.Name)
	if err == nil {
		return true, nil
	}
	if api.StatusErrorCheck(err, 404) {
		return false, nil
	}
	return false, err
}

var (
	_ API = (*Real)(nil)
	_     = errors.New
	_     = time.Second
)

// parseImage turns "remote:alias" (remote "images" = the public linuxcontainers simplestreams
// server), a 64-hex fingerprint or a plain local alias into an instance source.
func parseImage(img string) api.InstanceSource {
	src := api.InstanceSource{Type: "image"}
	if i := strings.Index(img, ":"); i > 0 {
		remote, alias := img[:i], img[i+1:]
		if remote == "images" {
			src.Server, src.Protocol, src.Alias = "https://images.linuxcontainers.org", "simplestreams", alias
			return src
		}
		src.Alias = alias
		return src
	}
	if len(img) == 64 {
		if _, err := fmt.Sscanf(img, "%x", new([]byte)); err == nil {
			src.Fingerprint = img
			return src
		}
	}
	src.Alias = img
	return src
}

// CreateRecoveryVM creates the disposable VM of granular file recovery: no profiles (so no
// network, no shared devices), a root disk from the image and exactly two scratch volumes.
func (r *Real) CreateRecoveryVM(ctx context.Context, s RecoverySpec) error {
	mem, cpu := s.Memory, s.CPU
	if mem == "" {
		mem = "512MiB"
	}
	if cpu == "" {
		cpu = "1"
	}
	devs := map[string]map[string]string{
		// the VM boots from ITS OWN root disk first, never from the guest disk
		"root":      {"type": "disk", "path": "/", "pool": s.Pool, "boot.priority": "10"},
		"guestdisk": {"type": "disk", "pool": s.Disk.Pool, "source": s.Disk.Name, "readonly": "true"},
		"out":       {"type": "disk", "pool": s.Out.Pool, "source": s.Out.Name},
		// cloud-init data arrives on a NoCloud seed disk, which needs neither the guest API nor an agent
		"cloud-init": {"type": "disk", "source": "cloud-init:config"},
	}
	req := api.InstancesPost{
		Name:   s.Name,
		Type:   api.InstanceTypeVM,
		Source: parseImage(s.Image),
		InstancePut: api.InstancePut{
			Config: map[string]string{
				"limits.memory": mem, "limits.cpu": cpu, "security.secureboot": "false", "security.guestapi": "false",
				"boot.autostart": "false", "cloud-init.user-data": s.CloudInit,
			},
			Devices:  devs,
			Profiles: []string{}, // explicitly none: the default profile would add a NIC
		},
	}
	return waitCtx(ctx)(r.proj(s.Project).CreateInstance(req))
}

// waitCtx waits for an operation but gives up when ctx ends (an image download can take long).
func waitCtx(ctx context.Context) func(incus.Operation, error) error {
	return func(op incus.Operation, err error) error {
		if err != nil {
			return err
		}
		return op.WaitContext(ctx)
	}
}

// WaitStopped blocks until the instance has stopped.
func (r *Real) WaitStopped(ctx context.Context, project, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, _, err := r.proj(project).GetInstanceState(name)
		if err != nil {
			return err
		}
		if st.Status == "Stopped" {
			return nil
		}
		if st.Status == "Error" || st.Status == "Frozen" {
			return fmt.Errorf("the recovery VM is in state %s", st.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the recovery VM did not power off within %s (it is %s)", timeout, st.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// DeleteInstance force-stops and deletes an instance.
func (r *Real) DeleteInstance(ctx context.Context, project, name string) error {
	st, _, err := r.proj(project).GetInstanceState(name)
	if err != nil {
		if api.StatusErrorCheck(err, 404) {
			return nil // already gone
		}
		return err
	}
	if st.Status != "Stopped" {
		_ = waitCtx(ctx)(r.proj(project).UpdateInstanceState(name, api.InstanceStatePut{Action: "stop", Force: true, Timeout: 30}, ""))
	}
	if err := waitCtx(ctx)(r.proj(project).DeleteInstance(name)); err != nil && !api.StatusErrorCheck(err, 404) {
		return err
	}
	return nil
}

// DeleteVolume deletes a custom volume.
func (r *Real) DeleteVolume(ctx context.Context, v VolRef) error {
	if err := r.proj(v.Project).DeleteStoragePoolVolume(v.Pool, "custom", v.Name); err != nil && !api.StatusErrorCheck(err, 404) {
		return err
	}
	return nil
}
