// Package engine implements the client side of incus-backup: discovery of the
// storage-object graph, policy resolution, the VM block engine (frozen NBD view,
// dirty-bitmap generations, several targets from one capture), the filesystem
// engine, upload, restore and verification.
package engine

import (
	"log/slog"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/remote"
)

// Kinds of storage object.
const (
	KindBlock      = "block"
	KindFilesystem = "filesystem"
	KindInstance   = "instance"
)

// Target is one backup server (a complete, independent repository).
type Target struct {
	Name   string
	Client *remote.Client
	Keys   *format.Keys
}

// Domain identifies the key domain of a target (targets with the same master key
// share chunks byte for byte).
func (t *Target) Domain() string { return t.Keys.DomainID }

// Engine holds everything a backup or restore run needs.
type Engine struct {
	Inc       incusx.API
	Targets   map[string]*Target
	Host      string
	SpoolDir  string
	MaxFreeze time.Duration
	MaxLag    time.Duration
	// Workers encode chunks (CPU bound: zstd, encryption, hashing); 0 = DefaultEncodeWorkers.
	Workers int
	// UploadWorkers send chunks to one target concurrently (network bound); 0 = DefaultUploadWorkers.
	UploadWorkers int
	Now           func() time.Time
	Log           *slog.Logger
	// Policies known locally (config) and from the control server.
	Policies map[string]api.Policy
	// ServerAssignments from the control server (desired state).
	Assignments   []api.Assignment
	DefaultPolicy string
	Projects      []string // restrict to these projects (empty = all)
	Hooks         Hooks
	// FSFormat selects the format of filesystem snapshots: "" or "catalog" (default: file catalog,
	// metadata change detection, browsing, single-file restore) or "tar" (the original single
	// archive stream, kept as the correctness fallback).
	FSFormat string
	// FSFullEvery forces a run that reads every file again (full-data mode) when the last one is older
	// than this (0 = 7 days). Metadata comparison is a very strong hint, never proof of identical
	// content, so this bounds how long a missed change could survive.
	FSFullEvery time.Duration
	// FSNoTrustCtime compares only size, mtime, mode, owner and xattrs, not ctime and inode (for
	// sources where those are not stable between runs).
	FSNoTrustCtime bool
	// Memory remembers, per object and target, the last snapshot a target is known to
	// hold, so generations are still managed correctly while a target is unreachable.
	Memory Memory
}

// Memory is the engine's small persistent cache of "what does this target hold".
// The servers stay the source of truth; the cache only keeps bitmap bookkeeping
// bounded and correct while a server cannot be reached.
type Memory interface {
	Last(object, target string) string
	SetLast(object, target, snapshot string)
}

func (e *Engine) remembered(object, target string) string {
	if e.Memory == nil {
		return ""
	}
	return e.Memory.Last(object, target)
}

func (e *Engine) remember(object, target, snap string) {
	if e.Memory != nil && snap != "" {
		e.Memory.SetLast(object, target, snap)
	}
}

// Object is one storage object to back up: the unit of backup.
type Object struct {
	Project string
	Name    string // logical name, e.g. "instance/vm1/root", "volume/biggi"
	Kind    string
	Ref     incusx.VolRef
	// For VM disks: the NBD export (device) name and the owning instance.
	Device   string
	Instance *incusx.Instance
	// Shared volumes list every consumer; Attached lists the VMs that attach it
	// together with the device names they use.
	SharedBy []string
	Attached []Attachment
	// Refs of an instance object: names of the storage objects it uses.
	Refs []string
	// Policy resolution result.
	Policy       string
	PolicySource string
	Targets      []string
	Commit       string
	Skip         string // non-empty: reason this object is not backed up
	volCfg       map[string]string
}

// Attachment is one VM attaching a block volume.
type Attachment struct {
	VM     string
	Device string
}

// Key is the stable identifier of the object (state, logs).
func (o *Object) Key() string { return o.Project + "/" + o.Kind + "/" + o.Name }

// OwnerName is the instance or volume name users know (policy assignments and
// Incus config keys use it).
func (o *Object) OwnerName() string {
	if o.Instance != nil {
		return o.Instance.Name
	}
	return o.Ref.Name
}

// TargetResult is the outcome for one target.
type TargetResult struct {
	Target    string
	Committed bool
	Snapshot  string
	Uploaded  int64
	Err       error
}

// Result is the outcome for one object.
type Result struct {
	Object   *Object
	Snapshot string
	Full     bool
	Logical  int64
	Dirty    int64
	Read     int64
	Targets  []TargetResult
	Err      error
	Duration time.Duration
}

// OK reports whether the object satisfied its commit requirement.
func (r *Result) OK() bool {
	if r.Err != nil {
		return false
	}
	n := 0
	for _, t := range r.Targets {
		if t.Committed {
			n++
		}
	}
	switch r.Object.Commit {
	case "any":
		return n >= 1
	default:
		return n == len(r.Targets) && n > 0
	}
}
