package auth

import (
	"context"
	"time"
)

// AdminTokenInfo is one API token as the administrator's overview lists it (never the secret).
type AdminTokenInfo struct {
	TokenInfo
	Owner         string `json:"owner"`
	OwnerRole     Role   `json:"owner_role"`
	OwnerDisabled bool   `json:"owner_disabled"`
	// EffectiveRole is what the token can do now: the lower of its own role and its owner's current
	// role. With repository scopes (the owner's or the token's own restriction) it is the most the token
	// can do on any one repository; docs/permissions.md.
	// role ("" when it cannot be used at all: owner disabled or token expired).
	EffectiveRole Role `json:"effective_role"`
	Expired       bool `json:"expired"`
}

// AllTokens lists every user's API tokens, newest first.
func (s *Store) AllTokens(ctx context.Context) ([]AdminTokenInfo, error) {
	rows, err := s.Pool.Query(ctx, `SELECT t.id::text, t.name, t.role, t.created_at, t.expires_at, t.last_used_at, u.name, u.role, u.disabled, u.id::text, t.repo_restricted
		FROM api_tokens t JOIN users u ON u.id=t.user_id ORDER BY t.created_at DESC, t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.Now()
	out := []AdminTokenInfo{}
	var ownerIDs []string
	var restrictedFlags []bool
	for rows.Next() {
		ownerIDs, restrictedFlags = append(ownerIDs, ""), append(restrictedFlags, false)
		var t AdminTokenInfo
		var tr, ur string
		if err := rows.Scan(&t.ID, &t.Name, &tr, &t.Created, &t.Expires, &t.LastUsed, &t.Owner, &ur, &t.OwnerDisabled, &ownerIDs[len(out)], &restrictedFlags[len(out)]); err != nil {
			return nil, err
		}
		t.Role, t.OwnerRole = Role(tr), Role(ur)
		t.Expired = t.Expires != nil && now.After(*t.Expires)
		t.EffectiveRole = effectiveTokenRole(t.Role, t.OwnerRole, t.OwnerDisabled, t.Expires, now)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ti := make([]TokenInfo, len(out))
	for i := range out {
		ti[i] = out[i].TokenInfo
	}
	if err := s.fillTokenRepos(ctx, ti); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Repositories = ti[i].Repositories
		if out[i].EffectiveRole == "" {
			continue // unusable (disabled owner or expired)
		}
		// with scopes, the cap is what the token reaches on its best repository
		p := Principal{Kind: "token", UserID: ownerIDs[i], Role: out[i].OwnerRole}
		if err := s.loadAccess(ctx, &p, out[i].ID, out[i].Role, restrictedFlags[i]); err != nil {
			return nil, err
		}
		if p.Access != nil {
			out[i].EffectiveRole = p.MaxRole()
		}
	}
	return out, nil
}

// effectiveTokenRole mirrors AuthenticateToken: the lower of the two roles, nothing when unusable.
func effectiveTokenRole(token, owner Role, disabled bool, exp *time.Time, now time.Time) Role {
	if disabled || (exp != nil && now.After(*exp)) {
		return ""
	}
	if !owner.Allows(roleLevel[token]) {
		return owner
	}
	return token
}

// RevokeAnyToken deletes a token of any user (administrators).
func (s *Store) RevokeAnyToken(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM api_tokens WHERE id::text=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
