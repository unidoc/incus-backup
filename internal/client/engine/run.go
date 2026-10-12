package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/remote"
)

// RunOptions narrow a run.
type RunOptions struct {
	// Policies restricts the run to objects whose policy is in the list (empty = all).
	Policies []string
	// Names restricts to objects whose owner name or logical name is listed.
	Names []string
	// DryRun only reports the plan.
	DryRun bool
}

// Report is the outcome of a run.
type Report struct {
	Started, Finished time.Time
	Results           []*Result
	Skipped           []*Object
}

// Failed counts objects that did not meet their commit requirement, except those deferred by
// repository maintenance (see Deferred).
func (r *Report) Failed() int {
	n := 0
	for _, x := range r.Results {
		if !x.OK() && !x.Deferred() {
			n++
		}
	}
	return n
}

func (o *Object) matches(opt RunOptions) bool {
	if len(opt.Policies) > 0 {
		ok := false
		for _, p := range opt.Policies {
			ok = ok || p == o.Policy
		}
		if !ok {
			return false
		}
	}
	if len(opt.Names) > 0 {
		ok := false
		for _, n := range opt.Names {
			ok = ok || n == o.Name || n == o.OwnerName()
		}
		if !ok {
			return false
		}
	}
	return true
}

// Run backs up every selected object. Shared volumes are captured once; VM disks
// of one VM are captured under one freeze; instance manifests are written last.
func (e *Engine) Run(ctx context.Context, objs []*Object, opt RunOptions) *Report {
	rep := &Report{Started: e.Now()}
	var sel []*Object
	for _, o := range objs {
		if o.Skip != "" {
			rep.Skipped = append(rep.Skipped, o)
			continue
		}
		if !o.matches(opt) {
			continue
		}
		sel = append(sel, o)
	}
	if opt.DryRun {
		rep.Finished = e.Now()
		for _, o := range sel {
			rep.Results = append(rep.Results, &Result{Object: o})
		}
		return rep
	}
	if err := os.MkdirAll(e.SpoolDir, 0o700); err != nil {
		for _, o := range sel {
			rep.Results = append(rep.Results, &Result{Object: o, Err: fmt.Errorf("spool directory: %w", err)})
		}
		rep.Finished = e.Now()
		return rep
	}
	// withSpool gives every batch (one VM, or one filesystem object) its own spool that is
	// deleted as soon as the batch is uploaded, so the spool never holds a whole run.
	withSpool := func(fn func(sp *Spool)) error {
		sp, err := NewSpool(e.SpoolDir)
		if err != nil {
			return err
		}
		defer sp.Close()
		fn(sp)
		return nil
	}

	// a repository in maintenance refuses backups: do not read what no target would accept
	var deferred []*Result
	sel, deferred = deferUnavailable(sel, e.maintenanceTargets(ctx))
	rep.Results = append(rep.Results, deferred...)
	runs := e.startRuns(ctx, len(sel)+len(deferred))
	instances := map[string]incusx.Instance{}
	for _, o := range objs {
		if o.Kind == KindInstance {
			instances[o.Project+"/"+o.Instance.Name] = *o.Instance
		}
	}

	// group block disks by the VM through which they are read
	type batch struct {
		inst  incusx.Instance
		disks []Disk
	}
	batches := map[string]*batch{}
	var fsObjs, instObjs, volOnly []*Object
	for _, o := range sel {
		switch {
		case o.Kind == KindInstance:
			instObjs = append(instObjs, o)
		case o.Kind == KindFilesystem:
			fsObjs = append(fsObjs, o)
		case o.Instance != nil: // VM root disk
			b := batches[o.Project+"/"+o.Instance.Name]
			if b == nil {
				b = &batch{inst: *o.Instance}
				batches[o.Project+"/"+o.Instance.Name] = b
			}
			b.disks = append(b.disks, Disk{Obj: o, Device: o.Device})
		default: // custom block volume
			volOnly = append(volOnly, o)
		}
	}
	for _, o := range volOnly {
		// read through a running attached VM when there is one (the only
		// consistent view of a volume a guest is writing)
		var via *Attachment
		var viaInst incusx.Instance
		atts := append([]Attachment(nil), o.Attached...)
		sort.Slice(atts, func(i, j int) bool { return atts[i].VM < atts[j].VM })
		for i := range atts {
			if in, ok := instances[o.Project+"/"+atts[i].VM]; ok && in.Running {
				via, viaInst = &atts[i], in
				break
			}
		}
		if via == nil {
			b := batches[o.Project+"//volumes"]
			if b == nil {
				b = &batch{inst: incusx.Instance{Project: o.Project, Name: "(volumes)", Running: false}}
				batches[o.Project+"//volumes"] = b
			}
			b.disks = append(b.disks, Disk{Obj: o, Device: ""})
			continue
		}
		k := o.Project + "/" + via.VM
		b := batches[k]
		if b == nil {
			b = &batch{inst: viaInst}
			batches[k] = b
		}
		b.disks = append(b.disks, Disk{Obj: o, Device: via.Device})
	}
	keys := make([]string, 0, len(batches))
	for k := range batches {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	snapOf := map[string]string{} // project/instance -> snapshot id used for its disks
	for _, k := range keys {
		b := batches[k]
		var snap string
		var results []*Result
		if err := withSpool(func(sp *Spool) { snap, results = e.BackupVM(ctx, b.inst, b.disks, sp) }); err != nil {
			for _, d := range b.disks {
				results = append(results, &Result{Object: d.Obj, Err: err})
			}
		}
		rep.Results = append(rep.Results, results...)
		if b.inst.Name != "(volumes)" {
			snapOf[b.inst.Project+"/"+b.inst.Name] = snap
		}
	}
	for _, o := range fsObjs {
		o := o
		if err := withSpool(func(sp *Spool) { rep.Results = append(rep.Results, e.BackupFS(ctx, o, sp)) }); err != nil {
			rep.Results = append(rep.Results, &Result{Object: o, Err: err})
		}
	}
	bySnap := map[string]string{}
	for _, r := range rep.Results {
		if r.Err == nil && r.Snapshot != "" {
			bySnap[r.Object.Project+"/"+r.Object.Name] = r.Snapshot
		}
	}
	for _, o := range instObjs {
		refs := map[string]string{}
		for _, ref := range o.Refs {
			if id, ok := bySnap[o.Project+"/"+ref]; ok {
				refs[ref] = id
			}
		}
		o := o
		if err := withSpool(func(sp *Spool) {
			rep.Results = append(rep.Results, e.BackupInstance(ctx, o, snapOf[o.Project+"/"+o.Instance.Name], refs, sp))
		}); err != nil {
			rep.Results = append(rep.Results, &Result{Object: o, Err: err})
		}
	}
	rep.Finished = e.Now()
	e.finishRuns(ctx, runs, rep)
	return rep
}

type runIDs map[string]string // target -> run id

func (e *Engine) startRuns(ctx context.Context, n int) runIDs {
	ids := runIDs{}
	for name, t := range e.Targets {
		id, err := t.Client.StartRun(ctx, api.RunStart{Started: e.Now(), ObjectsTotal: n})
		if err != nil {
			e.Log.Warn("could not record the run on a server", "target", name, "err", err)
			continue
		}
		ids[name] = id
	}
	return ids
}

func (e *Engine) finishRuns(ctx context.Context, ids runIDs, rep *Report) {
	for name, id := range ids {
		var ok, failed, deferred int
		var logical, up int64
		var msgs []string
		for _, r := range rep.Results {
			if !targetsInclude(r.Object, name) {
				continue // not backed up to this server: not part of its run
			}
			good := r.OK()
			late := r.Deferred()
			for _, tr := range r.Targets {
				if tr.Target != name {
					continue
				}
				good = tr.Committed
				late = !tr.Committed && IsDeferred(tr.Err)
				up += tr.Uploaded
				if tr.Err != nil {
					msgs = append(msgs, r.Object.Name+": "+tr.Err.Error())
				}
			}
			if r.Err != nil {
				msgs = append(msgs, r.Object.Name+": "+r.Err.Error())
			}
			switch {
			case good:
				ok++
				logical += r.Logical
			case late:
				deferred++
			default:
				failed++
			}
		}
		// any real failure decides; a run whose only misses are maintenance deferrals is "deferred"
		status := "ok"
		switch {
		case failed > 0 && ok == 0 && deferred == 0:
			status = "failed"
		case failed > 0:
			status = "partial"
		case deferred > 0:
			status = "deferred"
		}
		msg := strings.Join(msgs, "; ")
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		f := api.RunFinish{Status: status, ObjectsOK: ok, ObjectsFailed: failed, ObjectsDeferred: deferred, LogicalBytes: logical, UploadedBytes: up, Error: msg}
		if err := e.Targets[name].Client.FinishRun(ctx, id, f); err != nil && status == "deferred" && remote.IsStatus(err, 400) {
			// an older server does not know "deferred": report the deferred objects as not backed up
			f.Status, f.ObjectsFailed, f.ObjectsDeferred = "partial", failed+deferred, 0
			if ok == 0 {
				f.Status = "failed"
			}
			_ = e.Targets[name].Client.FinishRun(ctx, id, f)
		}
	}
}

// ReportInventory sends the storage-object graph (names, kinds, group ids,
// sharing) to every target so its control plane can show operational names.
func (e *Engine) ReportInventory(ctx context.Context, objs []*Object) {
	for name, t := range e.Targets {
		inv := api.Inventory{}
		for _, o := range objs {
			if o.Kind == "" {
				continue
			}
			inv.Objects = append(inv.Objects, api.ObjectInfo{Project: o.Project, Name: o.Name, Kind: o.Kind,
				GroupID: t.Keys.GroupID(o.Project, o.Kind, o.Name), SharedBy: o.SharedBy, Policy: o.Policy})
		}
		if err := t.Client.PutInventory(ctx, inv); err != nil {
			e.Log.Warn("could not report the inventory", "target", name, "err", err)
		}
	}
}

// Describe renders the plan for humans.
func Describe(o *Object) string {
	if o.Skip != "" {
		return fmt.Sprintf("%-8s %-40s skipped: %s", o.Kind, o.Project+"/"+o.Name, o.Skip)
	}
	return fmt.Sprintf("%-8s %-40s policy %s (%s) -> %s [%s]", o.Kind, o.Project+"/"+o.Name, o.Policy, o.PolicySource, strings.Join(o.Targets, ","), o.Commit)
}

// targetsInclude reports whether the object is backed up to the target (an object without a resolved
// target list counts everywhere, as before).
func targetsInclude(o *Object, target string) bool {
	if o == nil || len(o.Targets) == 0 {
		return true
	}
	for _, t := range o.Targets {
		if t == target {
			return true
		}
	}
	return false
}
