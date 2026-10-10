// Package fake is an in-memory Incus used by the engine tests. It models what
// matters for correctness: guest writes that mark every recording dirty bitmap,
// in-memory versus persistent bitmaps (the latter survive a VM restart), a
// frozen copy-before-write NBD view that is created by the first connection and
// ended when the last connection closes, and snapshots of filesystem volumes.
package fake

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/nbd"
)

const granule = 65536

type bitmap struct {
	b     incusx.Bitmap
	dirty map[uint64]bool
}

// Disk is one VM disk.
type Disk struct {
	Data    []byte
	bitmaps map[string]*bitmap
	// persistentCapable: root disks (qcow2 metadata layer) can hold persistent bitmaps.
	PersistentCapable bool
	Dependent         bool
}

// VM is a virtual machine.
type VM struct {
	Inst    incusx.Instance
	Running bool
	Disks   map[string]*Disk // device name -> disk ("root", ...)
	// device -> volume (custom) name for attached volumes
	Attached map[string]string
}

// Incus is the fake.
type Incus struct {
	mu          sync.Mutex
	VMs         map[string]*VM // key project/name
	Vols        map[string]*Volume
	FS          map[string]string // "project/name" -> host directory for container roots
	ProjectList []string
	Version     incusx.Info

	// session state
	frozen   map[string]*frozenSession
	snaps    map[string]string // snapshot key -> copied dir
	CallLog  []string
	failNext map[string]error
	Created  []incusx.InstanceSpec
	Started  []string
	Tmp      string
	// CopySnapshots makes a snapshot an isolated copy of the tree (new inodes and ctimes, like no
	// real snapshot). The default models a ZFS snapshot for change detection: the same files with
	// the same inode numbers and ctimes, read-only; tests change the live tree between runs.
	CopySnapshots bool
	// ShareFrozen makes a frozen NBD view share the live disk instead of copying it. Only valid when
	// nothing writes to the disk while a backup runs (the dev demo, whose multi-GiB sparse disks must
	// not be copied on every freeze).
	ShareFrozen bool

	// recovery VMs (granular file recovery)
	Guest        GuestRun
	recovery     map[string]incusx.RecoverySpec
	recoveryRuns []incusx.RecoverySpec
}

// Volume is a custom volume.
type Volume struct {
	Ref     incusx.VolRef
	Content string
	Disk    *Disk  // block volumes
	Dir     string // filesystem volumes
	UsedBy  []string
	Config  map[string]string
}

type frozenSession struct {
	server  *nbd.FakeServer
	conns   int
	exports map[string]*nbd.Export
}

// New returns an empty fake.
func New(tmp string) *Incus {
	return &Incus{VMs: map[string]*VM{}, Vols: map[string]*Volume{}, FS: map[string]string{}, frozen: map[string]*frozenSession{},
		snaps: map[string]string{}, failNext: map[string]error{}, Tmp: tmp, ProjectList: []string{"default"},
		Version: incusx.Info{Version: "7.5.1", Extensions: []string{"instance_nbd", "storage_volume_nbd"}}}
}

func key(project, name string) string { return project + "/" + name }

// AddVM registers a running VM with the given disks (device -> size).
func (f *Incus) AddVM(project, name string, disks map[string]int, running bool) *VM {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := &VM{Running: running, Disks: map[string]*Disk{}, Attached: map[string]string{}}
	devs := map[string]map[string]string{}
	for dev, size := range disks {
		d := &Disk{Data: make([]byte, size), bitmaps: map[string]*bitmap{}, PersistentCapable: dev == "root"}
		vm.Disks[dev] = d
		if dev == "root" {
			devs["root"] = map[string]string{"type": "disk", "path": "/", "pool": "default"}
		}
	}
	vm.Inst = incusx.Instance{Project: project, Name: name, Type: incusx.TypeVM, Running: running, Devices: devs, ExpandedDevices: devs,
		Config: map[string]string{}, ExpandedConfig: map[string]string{}, JSON: []byte(`{"name":"` + name + `","type":"virtual-machine"}`)}
	f.VMs[key(project, name)] = vm
	return vm
}

// AttachVolume attaches a custom block volume to a VM under a device name.
func (f *Incus) AttachVolume(project, vmName, dev, volName string, size int, dependent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := f.VMs[key(project, vmName)]
	v, ok := f.Vols[key(project, volName)]
	if !ok {
		v = &Volume{Ref: incusx.VolRef{Pool: "default", Project: project, Type: "custom", Name: volName}, Content: "block", Disk: &Disk{Data: make([]byte, size), bitmaps: map[string]*bitmap{}}, Config: map[string]string{}}
		f.Vols[key(project, volName)] = v
	}
	v.UsedBy = append(v.UsedBy, vmName)
	vm.Disks[dev] = v.Disk
	vm.Attached[dev] = volName
	d := map[string]string{"type": "disk", "pool": "default", "source": volName}
	if dependent {
		d["dependent"] = "true"
		v.Disk.Dependent = true
	}
	vm.Inst.ExpandedDevices[dev] = d
	vm.Inst.Devices[dev] = d
}

// AddFSVolume registers a custom filesystem volume backed by a host directory.
func (f *Incus) AddFSVolume(project, name, dir string, usedBy ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Vols[key(project, name)] = &Volume{Ref: incusx.VolRef{Pool: "default", Project: project, Type: "custom", Name: name}, Content: "filesystem", Dir: dir, UsedBy: usedBy, Config: map[string]string{}}
}

// AddContainer registers a container whose root filesystem is dir.
func (f *Incus) AddContainer(project, name, dir string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	devs := map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}}
	f.VMs[key(project, name)] = &VM{Running: running, Disks: map[string]*Disk{}, Inst: incusx.Instance{Project: project, Name: name, Type: incusx.TypeContainer, Running: running,
		Devices: devs, ExpandedDevices: devs, Config: map[string]string{}, ExpandedConfig: map[string]string{}, JSON: []byte(`{"name":"` + name + `","type":"container"}`)}}
	f.FS[key(project, name)] = dir
}

// SetConfig sets an instance config key.
func (f *Incus) SetConfig(project, name, k, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := f.VMs[key(project, name)]
	vm.Inst.Config[k] = v
	vm.Inst.ExpandedConfig[k] = v
}

// GuestWrite is a write by the guest: it changes the disk and marks every
// recording bitmap, and is visible to the live disk only (a frozen NBD view keeps
// the old content, as copy-before-write does).
func (f *Incus) GuestWrite(project, vm, dev string, off int, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.VMs[key(project, vm)].Disks[dev]
	copy(d.Data[off:], data)
	for g := uint64(off) / granule; g <= uint64(off+len(data)-1)/granule; g++ {
		for _, b := range d.bitmaps {
			if b.b.Recording {
				b.dirty[g*granule] = true
			}
		}
	}
}

// GuestWriteVol writes to a custom block volume.
func (f *Incus) GuestWriteVol(project, vol string, off int, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.Vols[key(project, vol)].Disk
	copy(d.Data[off:], data)
	for g := uint64(off) / granule; g <= uint64(off+len(data)-1)/granule; g++ {
		for _, b := range d.bitmaps {
			if b.b.Recording {
				b.dirty[g*granule] = true
			}
		}
	}
}

// Disk returns a VM disk's live content.
func (f *Incus) Disk(project, vm, dev string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte{}, f.VMs[key(project, vm)].Disks[dev].Data...)
}

// Restart simulates a clean VM stop and start: in-memory bitmaps vanish,
// persistent ones survive.
func (f *Incus) Restart(project, vm string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.VMs[key(project, vm)].Disks {
		for n, b := range d.bitmaps {
			if !b.b.Persistent {
				delete(d.bitmaps, n)
			}
		}
	}
}

// SetRunning changes a VM's state.
func (f *Incus) SetRunning(project, vm string, r bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.VMs[key(project, vm)].Running = r
	f.VMs[key(project, vm)].Inst.Running = r
}

// FailNext makes the next call of the named operation fail once.
func (f *Incus) FailNext(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[op] = err
}

func (f *Incus) fail(op string) error {
	f.CallLog = append(f.CallLog, op)
	if e, ok := f.failNext[op]; ok {
		delete(f.failNext, op)
		return e
	}
	return nil
}

// Calls returns how often an operation was invoked.
func (f *Incus) Calls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.CallLog {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// BitmapNames returns the sorted bitmap names on a VM disk.
func (f *Incus) BitmapNames(project, vm, dev string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.VMs[key(project, vm)].Disks[dev].bitmaps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CorruptBitmap marks a bitmap inconsistent.
func (f *Incus) CorruptBitmap(project, vm, dev, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.VMs[key(project, vm)].Disks[dev].bitmaps[name].b.Inconsistent = true
}

// ---- API ---------------------------------------------------------------------

func (f *Incus) Info(ctx context.Context) (*incusx.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.Version
	return &i, f.fail("Info")
}

func (f *Incus) Projects(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.ProjectList...), f.fail("Projects")
}

func (f *Incus) Instances(ctx context.Context, project string) ([]incusx.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Instances"); err != nil {
		return nil, err
	}
	var out []incusx.Instance
	for k, vm := range f.VMs {
		if strings.HasPrefix(k, project+"/") {
			i := vm.Inst
			i.Running = vm.Running
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func (f *Incus) Volumes(ctx context.Context, project string) ([]incusx.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []incusx.Volume
	for k, v := range f.Vols {
		if strings.HasPrefix(k, project+"/") {
			out = append(out, incusx.Volume{Pool: v.Ref.Pool, Project: project, Name: v.Ref.Name, ContentType: v.Content, Config: v.Config, UsedBy: v.UsedBy, JSON: []byte(`{"name":"` + v.Ref.Name + `"}`)})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func (f *Incus) disk(v incusx.VolRef) (*Disk, error) {
	switch v.Type {
	case "custom":
		if vol, ok := f.Vols[key(v.Project, v.Name)]; ok && vol.Disk != nil {
			return vol.Disk, nil
		}
	case "virtual-machine":
		if vm, ok := f.VMs[key(v.Project, v.Name)]; ok {
			return vm.Disks["root"], nil
		}
	}
	return nil, fmt.Errorf("fake: no such volume %+v", v)
}

func (f *Incus) running(project, vm string) bool {
	x, ok := f.VMs[key(project, vm)]
	return ok && x.Running
}

func (f *Incus) addBitmap(d *Disk, name string, persistent bool) error {
	if _, ok := d.bitmaps[name]; ok {
		return fmt.Errorf("fake: bitmap %s exists", name)
	}
	if persistent && !d.PersistentCapable {
		return errors.New("GenericError: Can't store persistent bitmaps: Not supported")
	}
	d.bitmaps[name] = &bitmap{b: incusx.Bitmap{Name: name, Granularity: granule, Recording: true, Persistent: persistent}, dirty: map[uint64]bool{}}
	return nil
}

func (f *Incus) CreateInstanceBitmap(ctx context.Context, project, instance, name string, persistent bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateInstanceBitmap"); err != nil {
		return err
	}
	vm, ok := f.VMs[key(project, instance)]
	if !ok || !vm.Running {
		return errors.New("Creating bitmaps requires the instance to be running")
	}
	// one transaction, as in QEMU: validate every disk first, change nothing on failure
	var targets []*Disk
	for dev, d := range vm.Disks {
		if dev == "root" || d.Dependent {
			if _, ok := d.bitmaps[name]; ok {
				return fmt.Errorf("fake: bitmap %s exists", name)
			}
			if persistent && !d.PersistentCapable {
				return errors.New("GenericError: Can't store persistent bitmaps: Not supported")
			}
			targets = append(targets, d)
		}
	}
	for _, d := range targets {
		if err := f.addBitmap(d, name, persistent); err != nil {
			return err
		}
	}
	return nil
}

func (f *Incus) CreateBitmap(ctx context.Context, v incusx.VolRef, name string, persistent bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateBitmap"); err != nil {
		return err
	}
	d, err := f.disk(v)
	if err != nil {
		return err
	}
	if v.Type == "custom" {
		running := false
		for _, u := range f.Vols[key(v.Project, v.Name)].UsedBy {
			running = running || f.running(v.Project, u)
		}
		if !running {
			return errors.New("Creating bitmaps requires the instance to be running")
		}
	}
	return f.addBitmap(d, name, persistent)
}

func (f *Incus) Bitmaps(ctx context.Context, v incusx.VolRef) ([]incusx.Bitmap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Bitmaps"); err != nil {
		return nil, err
	}
	d, err := f.disk(v)
	if err != nil {
		return nil, err
	}
	var out []incusx.Bitmap
	for _, b := range d.bitmaps {
		bb := b.b
		bb.DirtyBytes = int64(len(b.dirty)) * granule
		out = append(out, bb)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func (f *Incus) DeleteBitmap(ctx context.Context, v incusx.VolRef, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteBitmap"); err != nil {
		return err
	}
	d, err := f.disk(v)
	if err != nil {
		return err
	}
	if _, ok := d.bitmaps[name]; !ok {
		return fmt.Errorf("Bitmap %q not found", name)
	}
	delete(d.bitmaps, name)
	return nil
}

// InstanceNBD: the first connection takes a frozen copy of every disk and
// exposes live bitmaps; closing the last connection ends the session.
func (f *Incus) InstanceNBD(ctx context.Context, project, instance string, reuse bool) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("InstanceNBD"); err != nil {
		return nil, err
	}
	k := key(project, instance)
	vm, ok := f.VMs[k]
	if !ok || !vm.Running {
		return nil, errors.New("instance is not running")
	}
	sess := f.frozen[k]
	if reuse {
		if sess == nil {
			return nil, errors.New("No NBD session is currently active")
		}
	} else {
		if sess != nil {
			return nil, errors.New("Another NBD operation is already in progress")
		}
		sess = &frozenSession{exports: map[string]*nbd.Export{}}
		for dev, d := range vm.Disks {
			data := d.Data
			if !f.ShareFrozen {
				data = append([]byte{}, d.Data...)
			}
			e := &nbd.Export{Data: data, Dirty: map[string]func(uint64) bool{}, Granule: granule}
			d := d
			for name := range d.bitmaps {
				name := name
				e.Dirty[name] = func(o uint64) bool {
					f.mu.Lock()
					defer f.mu.Unlock()
					b := d.bitmaps[name]
					return b != nil && b.dirty[o]
				}
			}
			sess.exports[dev] = e
		}
		sess.server = &nbd.FakeServer{Exports: sess.exports}
		f.frozen[k] = sess
	}
	sess.conns++
	a, b := net.Pipe()
	go func() {
		sess.server.Serve(ctx, b)
		f.mu.Lock()
		sess.conns--
		if sess.conns == 0 && f.frozen[k] == sess {
			delete(f.frozen, k)
		}
		f.mu.Unlock()
	}()
	return a, nil
}

// FrozenActive reports whether an NBD session is open.
func (f *Incus) FrozenActive(project, vm string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frozen[key(project, vm)] != nil
}

func (f *Incus) VolumeNBD(ctx context.Context, v incusx.VolRef, writable bool) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("VolumeNBD"); err != nil {
		return nil, err
	}
	d, err := f.disk(v)
	if err != nil {
		return nil, err
	}
	if v.Type == "virtual-machine" && f.running(v.Project, v.Name) {
		return nil, errors.New("volume is in use by a running instance")
	}
	srv := &nbd.FakeServer{Exports: map[string]*nbd.Export{"": {Data: d.Data, Writes: writable, Granule: granule}}}
	// a volume NBD server has a single anonymous export
	return srv.Pipe(ctx), nil
}

func (f *Incus) fsDir(v incusx.VolRef) (string, error) {
	switch v.Type {
	case "custom":
		if vol, ok := f.Vols[key(v.Project, v.Name)]; ok && vol.Dir != "" {
			return vol.Dir, nil
		}
	case "container":
		if d, ok := f.FS[key(v.Project, v.Name)]; ok {
			return d, nil
		}
	}
	return "", fmt.Errorf("fake: no filesystem volume %+v", v)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(t, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			return os.Symlink(l, t)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(t, b, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(t, info.ModTime(), info.ModTime())
		}
	})
}

func (f *Incus) CreateSnapshot(ctx context.Context, v incusx.VolRef, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateSnapshot"); err != nil {
		return err
	}
	dir, err := f.fsDir(v)
	if err != nil {
		return err
	}
	k := v.Project + "/" + v.Type + "/" + v.Name + "@" + name
	if _, ok := f.snaps[k]; ok {
		return errors.New("snapshot exists")
	}
	if !f.CopySnapshots {
		f.snaps[k] = dir
		return nil
	}
	dst, err := os.MkdirTemp(f.Tmp, "snap-*")
	if err != nil {
		return err
	}
	if err := copyTree(dir, dst); err != nil {
		return err
	}
	f.snaps[k] = dst
	return nil
}

func (f *Incus) DeleteSnapshot(ctx context.Context, v incusx.VolRef, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteSnapshot"); err != nil {
		return err
	}
	k := v.Project + "/" + v.Type + "/" + v.Name + "@" + name
	d, ok := f.snaps[k]
	if !ok {
		return errors.New("no such snapshot")
	}
	delete(f.snaps, k)
	if !f.CopySnapshots {
		return nil // the "snapshot" is the live tree: never delete it
	}
	return os.RemoveAll(d)
}

// Snapshots returns the names of existing snapshots (for leak checks in tests).
func (f *Incus) Snapshots() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.snaps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *Incus) SnapshotPath(ctx context.Context, v incusx.VolRef, name string) (string, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.snaps[v.Project+"/"+v.Type+"/"+v.Name+"@"+name]
	if !ok {
		return "", nil, errors.New("no such snapshot")
	}
	return d, func() {}, nil
}

func (f *Incus) FilesystemPath(ctx context.Context, v incusx.VolRef) (string, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.fsDir(v)
	return d, func() {}, err
}

func (f *Incus) CreateInstance(ctx context.Context, s incusx.InstanceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateInstance"); err != nil {
		return err
	}
	if _, ok := f.VMs[key(s.Project, s.Name)]; ok {
		return errors.New("instance exists")
	}
	f.Created = append(f.Created, s)
	if s.Type == incusx.TypeVM {
		vm := f.addEmptyVM(s)
		_ = vm
	} else {
		dir, _ := os.MkdirTemp(f.Tmp, "ct-*")
		devs := map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": s.RootPool}}
		f.VMs[key(s.Project, s.Name)] = &VM{Disks: map[string]*Disk{}, Inst: incusx.Instance{Project: s.Project, Name: s.Name, Type: s.Type, Devices: devs, ExpandedDevices: devs, Config: s.Config, ExpandedConfig: s.Config}}
		f.FS[key(s.Project, s.Name)] = dir
	}
	return nil
}

func (f *Incus) addEmptyVM(s incusx.InstanceSpec) *VM {
	devs := map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": s.RootPool}}
	for k, v := range s.Devices {
		devs[k] = v
	}
	vm := &VM{Disks: map[string]*Disk{"root": {Data: make([]byte, s.RootSize), bitmaps: map[string]*bitmap{}, PersistentCapable: true}}, Attached: map[string]string{},
		Inst: incusx.Instance{Project: s.Project, Name: s.Name, Type: s.Type, Devices: devs, ExpandedDevices: devs, Config: s.Config, ExpandedConfig: s.Config}}
	f.VMs[key(s.Project, s.Name)] = vm
	return vm
}

func (f *Incus) CreateVolume(ctx context.Context, s incusx.VolumeSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateVolume"); err != nil {
		return err
	}
	k := key(s.Project, s.Name)
	if _, ok := f.Vols[k]; ok {
		return errors.New("volume exists")
	}
	v := &Volume{Ref: incusx.VolRef{Pool: s.Pool, Project: s.Project, Type: "custom", Name: s.Name}, Content: s.ContentType, Config: s.Config}
	if s.ContentType == "block" {
		v.Disk = &Disk{Data: make([]byte, s.Size), bitmaps: map[string]*bitmap{}}
	} else {
		v.Dir, _ = os.MkdirTemp(f.Tmp, "vol-*")
	}
	f.Vols[k] = v
	return nil
}

func (f *Incus) StartInstance(ctx context.Context, project, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Started = append(f.Started, project+"/"+name)
	if s, ok := f.recovery[key(project, name)]; ok {
		f.runRecovery(s) // the recovery VM boots, does its work and powers itself off
		return nil
	}
	if vm, ok := f.VMs[key(project, name)]; ok {
		vm.Running = true
		vm.Inst.Running = true
	}
	return nil
}

func (f *Incus) StopInstance(ctx context.Context, project, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.VMs[key(project, name)]; ok {
		vm.Running = false
		vm.Inst.Running = false
	}
	return nil
}

func (f *Incus) InstanceExists(ctx context.Context, project, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.VMs[key(project, name)]
	return ok, nil
}

func (f *Incus) VolumeExists(ctx context.Context, v incusx.VolRef) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v.Type == "custom" {
		_, ok := f.Vols[key(v.Project, v.Name)]
		return ok, nil
	}
	_, ok := f.VMs[key(v.Project, v.Name)]
	return ok, nil
}

var _ incusx.API = (*Incus)(nil)

// Disk2 returns a custom block volume's live content.
func (f *Incus) Disk2(project, vol string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte{}, f.Vols[key(project, vol)].Disk.Data...)
}

// ---- disposable recovery VM ------------------------------------------------------------

// GuestRun simulates what the recovery VM would do: given the bytes of the guest disk and the
// cloud-init user-data it returns the bytes to write to the start of the output volume. A nil
// GuestRun means the guest never produces output (a broken image).
type GuestRun func(disk []byte, userData string) []byte

// RecoveryRuns lists the recovery VMs ever created (the specs the engine passed).
func (f *Incus) RecoveryRuns() []incusx.RecoverySpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]incusx.RecoverySpec(nil), f.recoveryRuns...)
}

// Leftovers returns recovery instances and scratch volumes that still exist (leak checks).
func (f *Incus) Leftovers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.recovery {
		out = append(out, "instance "+k)
	}
	for k, v := range f.Vols {
		if strings.HasPrefix(v.Ref.Name, "ibk-rec-") {
			out = append(out, "volume "+k)
		}
	}
	sort.Strings(out)
	return out
}

func (f *Incus) CreateRecoveryVM(ctx context.Context, s incusx.RecoverySpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateRecoveryVM"); err != nil {
		return err
	}
	for _, v := range []incusx.VolRef{s.Disk, s.Out} {
		if _, ok := f.Vols[key(v.Project, v.Name)]; !ok {
			return fmt.Errorf("fake: recovery VM refers to missing volume %s", v.Name)
		}
	}
	if f.recovery == nil {
		f.recovery = map[string]incusx.RecoverySpec{}
	}
	f.recovery[key(s.Project, s.Name)] = s
	f.recoveryRuns = append(f.recoveryRuns, s)
	return nil
}

func (f *Incus) runRecovery(s incusx.RecoverySpec) {
	disk := f.Vols[key(s.Disk.Project, s.Disk.Name)].Disk
	out := f.Vols[key(s.Out.Project, s.Out.Name)].Disk
	if f.Guest == nil {
		return
	}
	copy(out.Data, f.Guest(append([]byte(nil), disk.Data...), s.CloudInit))
}

func (f *Incus) WaitStopped(ctx context.Context, project, name string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("WaitStopped"); err != nil {
		return err
	}
	if _, ok := f.recovery[key(project, name)]; !ok {
		return errors.New("fake: no such recovery VM")
	}
	return nil
}

func (f *Incus) DeleteInstance(ctx context.Context, project, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteInstance"); err != nil {
		return err
	}
	delete(f.recovery, key(project, name))
	delete(f.VMs, key(project, name))
	return nil
}

func (f *Incus) DeleteVolume(ctx context.Context, v incusx.VolRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteVolume"); err != nil {
		return err
	}
	delete(f.Vols, key(v.Project, v.Name))
	return nil
}
