package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/unidoc/incus-backup/internal/remote"
)

// ErrDeferred marks an object (or a target of it) that was not backed up because the repository was in
// maintenance (read-only or offline). It is not a failure: the next run backs it up.
var ErrDeferred = errors.New("deferred: the repository is in maintenance mode; the next run backs it up")

// IsDeferred reports whether err means "repository in maintenance, retry later": ErrDeferred, or the
// server's 503 answer to a session call.
func IsDeferred(err error) bool {
	return err != nil && (errors.Is(err, ErrDeferred) || remote.IsStatus(err, 503))
}

// Deferred reports whether the object missed its commit requirement ONLY because of repository
// maintenance (every error it has is a deferral).
func (r *Result) Deferred() bool {
	if r.OK() {
		return false
	}
	if r.Err != nil {
		return IsDeferred(r.Err)
	}
	n := 0
	for _, t := range r.Targets {
		if t.Committed {
			continue
		}
		if !IsDeferred(t.Err) {
			return false
		}
		n++
	}
	return n > 0
}

// Deferred counts objects deferred by repository maintenance.
func (r *Report) Deferred() int {
	n := 0
	for _, x := range r.Results {
		if x.Deferred() {
			n++
		}
	}
	return n
}

// maintenanceTargets asks every target for its repository mode (GET /v1/whoami) and returns the targets
// that will refuse a backup (read-only or offline). A target that cannot be asked is assumed available:
// the upload then reports the real error.
func (e *Engine) maintenanceTargets(ctx context.Context) map[string]string {
	out := map[string]string{}
	for name, t := range e.Targets {
		if t == nil || t.Client == nil {
			continue
		}
		w, err := t.Client.Whoami(ctx)
		if err != nil || w == nil {
			continue
		}
		if w.RepositoryMode != "" && w.RepositoryMode != "online" {
			out[name] = w.RepositoryMode
		}
	}
	return out
}

// deferUnavailable removes from sel every object whose targets are ALL in maintenance and returns a
// deferred result for each, so no disk or volume is read for a backup no server would accept.
func deferUnavailable(sel []*Object, down map[string]string) (keep []*Object, deferred []*Result) {
	if len(down) == 0 {
		return sel, nil
	}
	for _, o := range sel {
		all := len(o.Targets) > 0
		var modes []string
		for _, t := range o.Targets {
			m, ok := down[t]
			if !ok {
				all = false
				break
			}
			modes = append(modes, t+": "+m)
		}
		if !all {
			keep = append(keep, o)
			continue
		}
		deferred = append(deferred, &Result{Object: o, Err: fmt.Errorf("%w (%v)", ErrDeferred, modes)})
	}
	return keep, deferred
}
