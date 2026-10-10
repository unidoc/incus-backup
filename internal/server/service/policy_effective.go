package service

import (
	"context"

	"github.com/unidoc/incus-backup/internal/api"
)

// EffectivePolicy says which policy governs an object's retention and why: the same decision Prune
// makes (retentionPolicyFor), exposed for the UI's assignment editor and the "what does this policy
// affect" lists.
type EffectivePolicy struct {
	GroupID string `json:"group_id"`
	Host    string `json:"host"`
	Project string `json:"project"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	// Owner is the name assignments match against ("instance/web/root" -> "web").
	Owner          string `json:"owner"`
	DeclaredPolicy string `json:"declared_policy"`
	// PinnedPolicy is the retention policy pinned when the object was first seen (or set by an operator).
	PinnedPolicy string `json:"pinned_policy"`
	// Policy governs retention; "" means none (everything is kept).
	Policy string `json:"policy"`
	// Source: "object", "project" or "host" (an assignment), "pinned", or "none".
	Source     string          `json:"source"`
	Assignment *api.Assignment `json:"assignment,omitempty"`
	Known      bool            `json:"policy_known"`
	Snapshots  int             `json:"snapshots"`
}

// explainPolicy is ResolvePolicy with the matching assignment and its rank (0 host, 1 project,
// 2 object; -1 none). It must agree with ResolvePolicy exactly (tested).
func explainPolicy(as []api.Assignment, project, object string) (api.Assignment, int) {
	var best api.Assignment
	rank := -1
	for _, a := range as {
		r := -1
		switch {
		case a.Project == "" && a.Object == "":
			r = 0
		case a.Project == project && a.Object == "":
			r = 1
		case a.Project == project && a.Object == object:
			r = 2
		}
		if r > rank {
			best, rank = a, r
		}
	}
	return best, rank
}

// EffectivePolicies lists every storage object of a repository with the policy that governs it.
func (s *Service) EffectivePolicies(ctx context.Context, repoName string) ([]EffectivePolicy, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	pols, err := s.policiesByRepoID(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, p := range pols {
		known[p.Name] = true
	}
	// assignments per host id
	byHost := map[string][]api.Assignment{}
	rows, err := s.DB.Pool().Query(ctx, `SELECT a.host_id::text, a.project, a.object, p.name FROM policy_assignments a
		JOIN policies p ON p.id=a.policy_id JOIN hosts h ON h.id=a.host_id WHERE h.repository_id=$1`, repo.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var hid string
		var a api.Assignment
		if err := rows.Scan(&hid, &a.Project, &a.Object, &a.Policy); err != nil {
			rows.Close()
			return nil, err
		}
		byHost[hid] = append(byHost[hid], a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	orows, err := s.DB.Pool().Query(ctx, `SELECT o.group_id, COALESCE(o.host_id::text, ''), COALESCE(o.host_name, ''), o.project, o.name, o.kind,
		COALESCE(o.policy_name, ''), COALESCE(o.retention_policy, ''),
		(SELECT count(*) FROM snapshots sn WHERE sn.repository_id=o.repository_id AND sn.group_id=o.group_id)
		FROM storage_objects o WHERE o.repository_id=$1 ORDER BY o.host_name, o.project, o.name`, repo.ID)
	if err != nil {
		return nil, err
	}
	defer orows.Close()
	out := []EffectivePolicy{}
	for orows.Next() {
		var e EffectivePolicy
		var hid string
		if err := orows.Scan(&e.GroupID, &hid, &e.Host, &e.Project, &e.Name, &e.Kind, &e.DeclaredPolicy, &e.PinnedPolicy, &e.Snapshots); err != nil {
			return nil, err
		}
		e.Owner = ownerName(e.Name)
		e.Source = "none"
		if hid != "" {
			if a, rank := explainPolicy(byHost[hid], e.Project, e.Owner); rank >= 0 {
				a := a
				e.Policy, e.Assignment = a.Policy, &a
				e.Source = [...]string{"host", "project", "object"}[rank]
			}
		}
		if e.Source == "none" && e.PinnedPolicy != "" {
			e.Policy, e.Source = e.PinnedPolicy, "pinned"
		}
		e.Known = e.Policy != "" && known[e.Policy]
		out = append(out, e)
	}
	return out, orows.Err()
}
