package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Access is a principal's per-repository rights (docs/permissions.md).
//
//   - Global is the role for routes that are not bound to a repository (users, settings,
//     notifications, certificates...). It may be None.
//   - Repos is the role on each repository the principal may see. A repository that is not
//     listed is invisible: every repository-bound route answers 403 (or 404 for ids), and lists
//     leave it out.
//
// A principal without Access (nil) has its Role on every repository, as before scopes existed.
type Access struct {
	Global Role            `json:"global"`
	Repos  map[string]Role `json:"repositories"`
}

// RepoRole is one per-repository grant.
type RepoRole struct {
	Repository string `json:"repository"`
	Role       Role   `json:"role"`
}

// UserAccess is what the administrator edits in the "Repository access" panel.
type UserAccess struct {
	Global Role `json:"global_role"`
	// Scoped: per-repository roles apply and repositories without a row are invisible.
	Scoped bool       `json:"scoped"`
	Scopes []RepoRole `json:"scopes"`
}

// Level orders roles (None lowest).
func (r Role) level() Perm {
	l, ok := roleLevel[r]
	if !ok {
		return -1
	}
	return l
}

func minRole(a, b Role) Role {
	if a.level() <= b.level() {
		return a
	}
	return b
}

func maxRole(a, b Role) Role {
	if a.level() >= b.level() {
		return a
	}
	return b
}

// Scoped reports whether per-repository roles apply to this principal.
func (p Principal) Scoped() bool { return p.Access != nil }

// RoleFor is the principal's role on a repository.
func (p Principal) RoleFor(repo string) Role {
	if p.Access == nil {
		return p.Role
	}
	if r, ok := p.Access.Repos[repo]; ok {
		return r
	}
	return None
}

// MaxRole is the highest role the principal has anywhere (global or on any repository): the gate
// for routes that list across repositories and filter per item.
func (p Principal) MaxRole() Role {
	if p.Access == nil {
		return p.Role
	}
	m := p.Access.Global
	for _, r := range p.Access.Repos {
		m = maxRole(m, r)
	}
	return m
}

// CanSee reports whether the principal may see repository repo with permission perm.
func (p Principal) CanSee(repo string, perm Perm) bool { return p.RoleFor(repo).Allows(perm) }

// loadAccess fills p.Access. p.Role must be the owner's global role on entry. For a token, tokenID is
// set: the token's role caps everything, and a restricted token only reaches its listed repositories
// and has no global rights.
func (s *Store) loadAccess(ctx context.Context, p *Principal, tokenID string, tokenRole Role, restricted bool) error {
	var scoped bool
	if err := s.Pool.QueryRow(ctx, `SELECT repo_scoped FROM users WHERE id::text=$1`, p.UserID).Scan(&scoped); err != nil {
		return err
	}
	if p.Role == Admin {
		scoped = false // an administrator reaches every repository (it could lift its own scopes anyway)
	}
	var owner *Access
	if scoped {
		owner = &Access{Global: p.Role, Repos: map[string]Role{}}
		rows, err := s.Pool.Query(ctx, `SELECT r.name, s.role FROM user_repo_scopes s JOIN repositories r ON r.id=s.repository_id WHERE s.user_id::text=$1`, p.UserID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n, role string
			if err := rows.Scan(&n, &role); err != nil {
				rows.Close()
				return err
			}
			owner.Repos[n] = Role(role)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if tokenID == "" {
		p.Access = owner
		return nil
	}
	ownerRole := func(repo string) Role {
		if owner == nil {
			return p.Role
		}
		if r, ok := owner.Repos[repo]; ok {
			return r
		}
		return None
	}
	if !restricted {
		if owner == nil {
			return nil // plain token: min(token role, owner role) everywhere, set by the caller
		}
		a := &Access{Global: minRole(tokenRole, owner.Global), Repos: map[string]Role{}}
		for n, r := range owner.Repos {
			a.Repos[n] = minRole(tokenRole, r)
		}
		p.Access = a
		return nil
	}
	a := &Access{Global: None, Repos: map[string]Role{}}
	rows, err := s.Pool.Query(ctx, `SELECT r.name FROM api_token_repos t JOIN repositories r ON r.id=t.repository_id WHERE t.token_id::text=$1`, tokenID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		if r := minRole(tokenRole, ownerRole(n)); r != None {
			a.Repos[n] = r
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.Role = None
	p.Access = a
	return nil
}

// UserAccessOf returns a user's global role and per-repository roles.
func (s *Store) UserAccessOf(ctx context.Context, name string) (*UserAccess, error) {
	var id, role string
	var ua UserAccess
	err := s.Pool.QueryRow(ctx, `SELECT id::text, role, repo_scoped FROM users WHERE lower(name)=lower($1)`, name).Scan(&id, &role, &ua.Scoped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ua.Global = Role(role)
	ua.Scopes = []RepoRole{}
	rows, err := s.Pool.Query(ctx, `SELECT r.name, s.role FROM user_repo_scopes s JOIN repositories r ON r.id=s.repository_id WHERE s.user_id::text=$1 ORDER BY r.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x RepoRole
		var r string
		if err := rows.Scan(&x.Repository, &r); err != nil {
			return nil, err
		}
		x.Role = Role(r)
		ua.Scopes = append(ua.Scopes, x)
	}
	return &ua, rows.Err()
}

// SetUserAccess replaces a user's per-repository roles. scoped=false removes every scope row: the
// global role then applies to every repository. An administrator cannot be scoped (it can manage
// users, so a scope would not bind it); give a lower global role first.
func (s *Store) SetUserAccess(ctx context.Context, name string, scoped bool, scopes []RepoRole) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id, role string
	err = tx.QueryRow(ctx, `SELECT id::text, role FROM users WHERE lower(name)=lower($1) FOR UPDATE`, name).Scan(&id, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if scoped && Role(role) == Admin {
		return fmt.Errorf("%w: an administrator always has every repository; give the user a lower global role (or none) to restrict it", ErrBad)
	}
	if !scoped && len(scopes) > 0 {
		return fmt.Errorf("%w: per-repository roles need scoped=true", ErrBad)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_repo_scopes WHERE user_id::text=$1`, id); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, sc := range scopes {
		if !sc.Role.Valid() || sc.Role == None {
			return fmt.Errorf("%w: role %q for repository %q (use admin, operator, restore or readonly; leave a repository out to deny it)", ErrBad, sc.Role, sc.Repository)
		}
		if seen[sc.Repository] {
			return fmt.Errorf("%w: repository %q listed twice", ErrBad, sc.Repository)
		}
		seen[sc.Repository] = true
		tag, err := tx.Exec(ctx, `INSERT INTO user_repo_scopes (user_id, repository_id, role) SELECT $1::uuid, id, $3 FROM repositories WHERE name=$2`, id, sc.Repository, string(sc.Role))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: no repository named %q", ErrBad, sc.Repository)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET repo_scoped=$2 WHERE id::text=$1`, id, scoped); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DefaultTokenTTL is the lifetime of a token created without one; MaxTokenTTL is the longest a token
// may live. A token never lives forever: it survives a password reset, so it must expire.
const (
	DefaultTokenTTL = 90 * 24 * time.Hour
	MaxTokenTTL     = 365 * 24 * time.Hour
)

// CreateTokenFor makes an API token for a user, optionally restricted to repositories (repos != nil;
// an empty non-nil list is refused). Its role cannot exceed the highest role the owner has anywhere;
// at use it is capped per repository by the owner's role there. The secret is returned once.
// ttl <= 0 means DefaultTokenTTL, more than MaxTokenTTL is refused. A user without a local password
// (single sign-on only) gets no tokens: the role of a long-lived local key would be the last one the
// identity provider mapped, and the provider could not take it back.
func (s *Store) CreateTokenFor(ctx context.Context, userName, name string, role Role, ttl time.Duration, repos []string) (string, *TokenInfo, error) {
	if name == "" || len(name) > 64 {
		return "", nil, fmt.Errorf("%w: token name", ErrBad)
	}
	if !role.Valid() || role == None {
		return "", nil, fmt.Errorf("%w: token role must be admin, operator, restore or readonly", ErrBad)
	}
	if repos != nil && len(repos) == 0 {
		return "", nil, fmt.Errorf("%w: a restricted token needs at least one repository", ErrBad)
	}
	u, err := s.User(ctx, userName)
	if err != nil {
		return "", nil, err
	}
	if !u.HasPassword {
		return "", nil, fmt.Errorf("%w: %v: API tokens would outlive the identity provider's role mapping", ErrBad, ErrNoLocalPassword)
	}
	if ttl > MaxTokenTTL {
		return "", nil, fmt.Errorf("%w: a token lives at most %d days", ErrBad, int(MaxTokenTTL/(24*time.Hour)))
	}
	owner := Principal{Kind: "user", UserID: u.ID, Role: u.Role}
	if err := s.loadAccess(ctx, &owner, "", "", false); err != nil {
		return "", nil, err
	}
	if !owner.MaxRole().Allows(roleLevel[role]) {
		return "", nil, fmt.Errorf("%w: a token cannot have more rights than its owner", ErrBad)
	}
	secret := tokenPrefix + randToken(32)
	now := s.Now()
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	exp := now.Add(ttl)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback(ctx)
	var ti TokenInfo
	var r string
	err = tx.QueryRow(ctx, `INSERT INTO api_tokens (user_id, name, token_hash, role, created_at, expires_at, repo_restricted) VALUES ($1::uuid,$2,$3,$4,$5,$6,$7)
		RETURNING id::text, name, role, created_at, expires_at`, u.ID, name, hashToken(secret), string(role), now, &exp, repos != nil).Scan(&ti.ID, &ti.Name, &r, &ti.Created, &ti.Expires)
	if err != nil {
		if strings.Contains(err.Error(), "api_tokens_user_id_name_key") {
			return "", nil, fmt.Errorf("%w: token %q", ErrExists, name)
		}
		return "", nil, err
	}
	ti.Role = Role(r)
	for _, n := range repos {
		tag, err := tx.Exec(ctx, `INSERT INTO api_token_repos (token_id, repository_id) SELECT $1::uuid, id FROM repositories WHERE name=$2 ON CONFLICT DO NOTHING`, ti.ID, n)
		if err != nil {
			return "", nil, err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE name=$1)`, n).Scan(&exists)
			if !exists {
				return "", nil, fmt.Errorf("%w: no repository named %q", ErrBad, n)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, err
	}
	if repos != nil {
		ti.Repositories = append([]string(nil), repos...)
		sort.Strings(ti.Repositories)
	}
	return secret, &ti, nil
}

// fillTokenRepos sets Repositories on restricted tokens (an empty, non-nil list when every listed
// repository was deleted: the token then reaches nothing).
func (s *Store) fillTokenRepos(ctx context.Context, ts []TokenInfo) error {
	if len(ts) == 0 {
		return nil
	}
	ids := make([]string, len(ts))
	for i := range ts {
		ids[i] = ts[i].ID
	}
	rows, err := s.Pool.Query(ctx, `SELECT t.id::text, r.name FROM api_tokens t LEFT JOIN api_token_repos x ON x.token_id=t.id LEFT JOIN repositories r ON r.id=x.repository_id
		WHERE t.repo_restricted AND t.id::text = ANY($1) ORDER BY r.name`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string][]string{}
	for rows.Next() {
		var id string
		var n *string
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		if by[id] == nil {
			by[id] = []string{}
		}
		if n != nil {
			by[id] = append(by[id], *n)
		}
	}
	for i := range ts {
		if l, ok := by[ts[i].ID]; ok {
			ts[i].Repositories = l
		}
	}
	return rows.Err()
}
