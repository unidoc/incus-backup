package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/tasks"
)

func init() {
	perms["DELETE /api/v1/repositories/{name}"] = ad
	perms["DELETE /api/v1/objects/{group}"] = ad
}

// auditDone writes an explicit audit entry when a destructive task ends (the request's own entry only
// says it was accepted).
func (s *Server) auditDone(r *http.Request, action, target string, detail any, err error) {
	if s.Auth == nil {
		return
	}
	p := PrincipalFrom(r.Context())
	d, _ := json.Marshal(map[string]any{"result": detail, "error": errString(err)})
	_ = s.Auth.Audit(context.WithoutCancel(r.Context()), auth.AuditEntry{Actor: p.Name(), Kind: p.Kind, Action: action, Target: target, Detail: d, IP: clientIP(r), OK: err == nil})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// lifecycleRoutes: retiring a repository (detach or destroy) and deleting a whole backup group.
func (s *Server) lifecycleRoutes(h routeFn) {
	h("DELETE /api/v1/repositories/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode string `json:"mode"` // detach | destroy
			service.RemoveRequest
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		name := r.PathValue("name")
		switch req.Mode {
		case "detach":
			res, err := s.Svc.DetachRepository(r.Context(), name, req.RemoveRequest)
			s.auditDone(r, "repository detach", name, res, err)
			if err == nil {
				s.Hub.Publish("repository", map[string]string{"name": name, "removed": "detach"})
			}
			s.reply(w, r, res, err)
		case "destroy":
			rp, err := s.Svc.RemovePreflight(r.Context(), name, req.RemoveRequest, true)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			rr := req.RemoveRequest
			s.runTask(w, r, tasks.Spec{Kind: "repository_destroy", RepositoryID: rp.ID, Repository: name, Exclusive: true,
				Params: map[string]any{"force_hosts": rr.ForceHosts}},
				func(ctx context.Context) (any, error) {
					res, err := s.Svc.DestroyRepository(ctx, name, rr)
					s.auditDone(r, "repository destroy", name, res, err)
					if err == nil {
						s.Hub.Publish("repository", map[string]string{"name": name, "removed": "destroy"})
					}
					return res, err
				},
				func(res any, err error) { s.reply(w, r, res, err) })
		default:
			s.fail(w, r, fmt.Errorf("%w: mode must be detach (forget it, keep the data) or destroy (delete every backup in it)", service.ErrBadRequest))
		}
	})
	h("DELETE /api/v1/objects/{group}", s.withRepo(func(w http.ResponseWriter, r *http.Request, repo string) {
		var req struct {
			ForceProtected bool `json:"force_protected"`
		}
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		group := r.PathValue("group")
		rp, err := s.Svc.DeleteGroupPreflight(r.Context(), repo, group, req.ForceProtected)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.runTask(w, r, tasks.Spec{Kind: "group_delete", RepositoryID: rp.ID, Repository: repo, Exclusive: true,
			Params: map[string]any{"group": group, "force_protected": req.ForceProtected}},
			func(ctx context.Context) (any, error) {
				res, err := s.Svc.DeleteGroup(ctx, repo, group, req.ForceProtected)
				s.auditDone(r, "object delete", repo+"/"+group, res, err)
				return res, err
			},
			func(res any, err error) { s.reply(w, r, res, err) })
	}))
}
