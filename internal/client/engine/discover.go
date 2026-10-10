package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/incusx"
)

// Incus config keys that select what is backed up.
const (
	KeyPolicy  = "user.backup.policy"
	KeyExclude = "user.backup.exclude"
)

func (e *Engine) inScope(project string) bool {
	if len(e.Projects) == 0 {
		return true
	}
	for _, p := range e.Projects {
		if p == project {
			return true
		}
	}
	return false
}

func rootDevice(i incusx.Instance) string {
	for n, d := range i.ExpandedDevices {
		if d["type"] == "disk" && d["path"] == "/" {
			return n
		}
	}
	return "root"
}

// Discover reads Incus and builds the storage-object graph. Shared volumes are
// one object however many instances use them. Nothing is selected yet: call
// Resolve for that.
func (e *Engine) Discover(ctx context.Context) ([]*Object, error) {
	projects, err := e.Inc.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	sort.Strings(projects)
	var out []*Object
	for _, project := range projects {
		if !e.inScope(project) {
			continue
		}
		insts, err := e.Inc.Instances(ctx, project)
		if err != nil {
			return nil, fmt.Errorf("listing instances of %s: %w", project, err)
		}
		vols, err := e.Inc.Volumes(ctx, project)
		if err != nil {
			return nil, fmt.Errorf("listing volumes of %s: %w", project, err)
		}
		volObj := map[string]*Object{}
		for _, v := range vols {
			kind := KindFilesystem
			if v.ContentType == "block" {
				kind = KindBlock
			}
			o := &Object{Project: project, Name: "volume/" + v.Name, Kind: kind,
				Ref: incusx.VolRef{Pool: v.Pool, Project: project, Type: "custom", Name: v.Name}, SharedBy: append([]string(nil), v.UsedBy...), volCfg: v.Config}
			if v.ContentType != "block" && v.ContentType != "filesystem" {
				o.Skip = fmt.Sprintf("content type %q volumes (ISO images) are not backed up", v.ContentType)
			}
			volObj[v.Name] = o
			out = append(out, o)
		}
		for idx := range insts {
			inst := insts[idx]
			io := &Object{Project: project, Name: "instance/" + inst.Name, Kind: KindInstance, Instance: &inst}
			root := &Object{Project: project, Name: "instance/" + inst.Name + "/root", Instance: &inst, Ref: incusx.RootRef(inst), Device: rootDevice(inst)}
			if inst.Type == incusx.TypeVM {
				root.Kind = KindBlock
			} else {
				root.Kind = KindFilesystem
			}
			io.Refs = append(io.Refs, root.Name)
			var devs []string
			for d := range inst.ExpandedDevices {
				devs = append(devs, d)
			}
			sort.Strings(devs)
			for _, dn := range devs {
				d := inst.ExpandedDevices[dn]
				if d["type"] != "disk" || d["path"] == "/" || d["source"] == "" || d["pool"] == "" {
					continue
				}
				if vo, ok := volObj[d["source"]]; ok {
					io.Refs = append(io.Refs, vo.Name)
					vo.Attached = append(vo.Attached, Attachment{VM: inst.Name, Device: dn})
				}
			}
			out = append(out, root, io)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Project != out[b].Project {
			return out[a].Project < out[b].Project
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

func instanceOf(o *Object, all []*Object) []*Object {
	var users []*Object
	for _, x := range all {
		if x.Instance != nil && x.Kind == KindInstance {
			for _, r := range x.Refs {
				if r == o.Name {
					users = append(users, x)
				}
			}
		}
	}
	return users
}

// Resolve decides which policy applies to every object and which are skipped.
// Precedence (highest first): exclude, an explicit user.backup.policy on the
// object itself, an explicit key on an instance that uses it, the server's
// assignment (object > project > host), then the configured default.
func (e *Engine) Resolve(objs []*Object) {
	byName := map[string]*Object{}
	for _, o := range objs {
		byName[o.Project+"/"+o.Name] = o
	}
	instOf := func(name, project string) *Object { return byName[project+"/instance/"+name] }

	for _, o := range objs {
		var ownCfg map[string]string
		switch {
		case o.Kind == KindInstance:
			ownCfg = o.Instance.ExpandedConfig
		case o.Instance != nil: // root disk follows its instance
			ownCfg = o.Instance.ExpandedConfig
		default: // custom volume: its own config
			ownCfg = e.volumeConfig(o)
		}
		if isTrue(ownCfg[KeyExclude]) {
			o.Skip = "excluded by " + KeyExclude
			continue
		}
		pol, src := ownCfg[KeyPolicy], "incus config on the object"
		if pol == "" && o.Instance == nil {
			// a shared volume is backed up if ANY consumer selects it
			for _, a := range o.Attached {
				if in := instOf(a.VM, o.Project); in != nil {
					if isTrue(in.Instance.ExpandedConfig[KeyExclude]) {
						continue
					}
					if p := in.Instance.ExpandedConfig[KeyPolicy]; p != "" {
						pol, src = p, "incus config on instance "+a.VM
						break
					}
				}
			}
		}
		if pol == "" {
			if p := serverPolicy(e.Assignments, o); p != "" {
				pol, src = p, "server assignment"
			}
		}
		if pol == "" && e.DefaultPolicy != "" {
			pol, src = e.DefaultPolicy, "client default_policy"
		}
		if pol == "" {
			o.Skip = "no policy selected"
			continue
		}
		p, ok := e.Policies[pol]
		if !ok {
			o.Skip = fmt.Sprintf("unknown policy %q (from %s)", pol, src)
			continue
		}
		o.Policy, o.PolicySource = pol, src
		o.Commit = p.CommitRequired
		if o.Commit == "" {
			o.Commit = "all"
		}
		for _, t := range p.Targets {
			if _, ok := e.Targets[t]; ok {
				o.Targets = append(o.Targets, t)
			}
		}
		if len(o.Targets) == 0 {
			for t := range e.Targets {
				o.Targets = append(o.Targets, t)
			}
			sort.Strings(o.Targets)
		}
	}
}

func (e *Engine) volumeConfig(o *Object) map[string]string {
	return o.volCfg
}

func serverPolicy(as []api.Assignment, o *Object) string {
	best, rank := "", -1
	name := o.OwnerName()
	for _, a := range as {
		r := -1
		switch {
		case a.Project == "" && a.Object == "":
			r = 0
		case a.Project == o.Project && a.Object == "":
			r = 1
		case a.Project == o.Project && a.Object == name:
			r = 2
		}
		if r > rank {
			best, rank = a.Policy, r
		}
	}
	return best
}

func isTrue(s string) bool {
	switch strings.ToLower(s) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
