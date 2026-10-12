package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

// Single sign-on with OpenID Connect (docs/authentication.md): the Authorization Code flow with PKCE
// (S256), state and nonce, on golang.org/x/oauth2 and github.com/coreos/go-oidc/v3 (ID token signature,
// issuer, audience and expiry). Users are matched by (provider, subject) only (user_identities), never by
// e-mail alone; an administrator's explicit link by e-mail is bound to the subject at its first use.
// Roles and repository scopes come from the provider's mappings at every sign-in; the session is the
// same as after a password sign-in (openSession).

// LoginTTL bounds an authorization request (state, nonce, PKCE verifier).
const LoginTTL = 10 * time.Minute

// SecretStore holds OIDC client secrets (the service's sealed credential store, kind "oidc").
type SecretStore interface {
	CreateSecret(ctx context.Context, name, kind string, plain []byte) (string, error)
	ReadSecret(ctx context.Context, id string) ([]byte, error)
	RotateSecret(ctx context.Context, id string, plain []byte) error
	DeleteSecret(ctx context.Context, id string) error
}

// RoleMapping gives a role to members of an identity-provider group.
type RoleMapping struct {
	Group string `json:"group"`
	Role  Role   `json:"role"`
}

// ScopeMapping gives members of a group a role on one repository.
type ScopeMapping struct {
	Group      string `json:"group"`
	Repository string `json:"repository"`
	Role       Role   `json:"role"`
}

// OIDCProvider is a provider as listed (the client secret is never returned: HasSecret only).
type OIDCProvider struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	DisplayName    string         `json:"display_name"`
	Issuer         string         `json:"issuer"`
	ClientID       string         `json:"client_id"`
	HasSecret      bool           `json:"has_secret"`
	Scopes         []string       `json:"scopes"`
	AllowedDomains []string       `json:"allowed_domains"`
	GroupsClaim    string         `json:"groups_claim"`
	DefaultRole    Role           `json:"default_role"`
	RoleMappings   []RoleMapping  `json:"role_mappings"`
	ScopeMappings  []ScopeMapping `json:"scope_mappings"`
	AutoCreate     bool           `json:"auto_create"`
	Enabled        bool           `json:"enabled"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	secretID       *string
}

// OIDCProviderInput creates or replaces a provider. ClientSecret: nil or "" keeps the stored secret on
// update; ClearSecret removes it (a public client using PKCE only).
type OIDCProviderInput struct {
	Name           string         `json:"name"`
	DisplayName    string         `json:"display_name"`
	Issuer         string         `json:"issuer"`
	ClientID       string         `json:"client_id"`
	ClientSecret   string         `json:"client_secret,omitempty"`
	ClearSecret    bool           `json:"clear_secret,omitempty"`
	Scopes         []string       `json:"scopes"`
	AllowedDomains []string       `json:"allowed_domains"`
	GroupsClaim    string         `json:"groups_claim"`
	DefaultRole    Role           `json:"default_role"`
	RoleMappings   []RoleMapping  `json:"role_mappings"`
	ScopeMappings  []ScopeMapping `json:"scope_mappings"`
	AutoCreate     bool           `json:"auto_create"`
	Enabled        bool           `json:"enabled"`
}

// OIDCButton is what the sign-in page shows (no configuration details).
type OIDCButton struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Identity is a user's link to a provider (Subject "" = an administrator's link by e-mail not used yet).
type Identity struct {
	Provider  string     `json:"provider"`
	Subject   string     `json:"subject"`
	Email     string     `json:"email"`
	Pending   bool       `json:"pending"`
	CreatedAt time.Time  `json:"created_at"`
	LastLogin *time.Time `json:"last_login_at,omitempty"`
}

// DiscoveryReport is the provider test's result.
type DiscoveryReport struct {
	OK                    bool     `json:"ok"`
	Error                 string   `json:"error,omitempty"`
	Issuer                string   `json:"issuer,omitempty"`
	AuthorizationEndpoint string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string   `json:"token_endpoint,omitempty"`
	JWKSURI               string   `json:"jwks_uri,omitempty"`
	JWKSKeys              int      `json:"jwks_keys"`
	JWKSError             string   `json:"jwks_error,omitempty"`
	PKCEMethods           []string `json:"code_challenge_methods_supported,omitempty"`
	ScopesSupported       []string `json:"scopes_supported,omitempty"`
	ClaimsSupported       []string `json:"claims_supported,omitempty"`
	RedirectURI           string   `json:"redirect_uri"`
	Warnings              []string `json:"warnings"`
}

// SSOError is a failed sign-in through a provider. Code is a fixed word the sign-in page explains
// (never text from the identity provider, so nothing is reflected into the page).
type SSOError struct {
	Code   string // state, denied, unverified, domain, no_account, exists, disabled, locked, provider, local_off, failed
	Reason string // for the audit log
	User   string // the user it concerned, when known
}

func (e *SSOError) Error() string { return "single sign-on failed (" + e.Code + "): " + e.Reason }

func ssoErr(code, reason string, a ...any) *SSOError {
	return &SSOError{Code: code, Reason: fmt.Sprintf(reason, a...)}
}

// OIDC runs the flows. HTTP is the client for the identity provider (nil: 15 s timeout).
type OIDC struct {
	Store   *Store
	Secrets SecretStore
	HTTP    *http.Client

	mu    sync.Mutex
	cache map[string]cachedProvider
}

type cachedProvider struct {
	issuer string
	p      *oidc.Provider
	at     time.Time
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (o *OIDC) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (o *OIDC) ctx(ctx context.Context) context.Context { return oidc.ClientContext(ctx, o.client()) }

// validIssuer: https, or http only to a loopback address (local testing).
func validIssuer(iss string) error {
	u, err := url.Parse(iss)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: issuer must be a URL such as https://id.example.com/realms/main", ErrBad)
	}
	if u.Scheme == "https" {
		return nil
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); u.Scheme == "http" && (h == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("%w: issuer must use https", ErrBad)
}

func (in *OIDCProviderInput) normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	if !slugRe.MatchString(in.Name) {
		return fmt.Errorf("%w: name must be 1-32 lower-case letters, digits or '-' (it is part of the redirect URL)", ErrBad)
	}
	in.Issuer = strings.TrimSpace(in.Issuer) // exactly as the provider states it: go-oidc compares it with the discovery document (Authentik's ends in "/")
	if err := validIssuer(in.Issuer); err != nil {
		return err
	}
	in.ClientID = strings.TrimSpace(in.ClientID)
	if in.ClientID == "" || len(in.ClientID) > 512 {
		return fmt.Errorf("%w: client id is required", ErrBad)
	}
	if in.DisplayName = strings.TrimSpace(in.DisplayName); len(in.DisplayName) > 64 {
		return fmt.Errorf("%w: button label: at most 64 characters", ErrBad)
	}
	scopes := []string{"openid"}
	for _, sc := range in.Scopes {
		if sc = strings.TrimSpace(sc); sc != "" && !slices.Contains(scopes, sc) {
			scopes = append(scopes, sc)
		}
	}
	if len(scopes) == 1 {
		scopes = append(scopes, "email", "profile")
	}
	in.Scopes = scopes
	doms := []string{}
	for _, d := range in.AllowedDomains {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			doms = append(doms, d)
		}
	}
	in.AllowedDomains = doms
	if in.GroupsClaim = strings.TrimSpace(in.GroupsClaim); in.GroupsClaim == "" {
		in.GroupsClaim = "groups"
	}
	if in.DefaultRole == "" {
		in.DefaultRole = None
	}
	if !in.DefaultRole.Valid() {
		return fmt.Errorf("%w: default role must be operator, restore, readonly or none", ErrBad)
	}
	if in.DefaultRole == Admin {
		return fmt.Errorf("%w: the default role cannot be admin: map an identity-provider group to admin instead", ErrBad)
	}
	if in.RoleMappings == nil {
		in.RoleMappings = []RoleMapping{}
	}
	for _, m := range in.RoleMappings {
		if strings.TrimSpace(m.Group) == "" || !m.Role.Valid() || m.Role == None {
			return fmt.Errorf("%w: a role mapping needs a group and a role (admin, operator, restore or readonly)", ErrBad)
		}
	}
	if in.ScopeMappings == nil {
		in.ScopeMappings = []ScopeMapping{}
	}
	for _, m := range in.ScopeMappings {
		if strings.TrimSpace(m.Group) == "" || m.Repository == "" || !m.Role.Valid() || m.Role == None {
			return fmt.Errorf("%w: a repository mapping needs a group, a repository and a role (admin, operator, restore or readonly)", ErrBad)
		}
	}
	return nil
}

const providerCols = `id::text, name, display_name, issuer, client_id, secret_id::text, scopes, allowed_domains, groups_claim, default_role,
	role_mappings, scope_mappings, auto_create, enabled, created_at, updated_at`

func scanProvider(row pgx.Row) (*OIDCProvider, error) {
	var p OIDCProvider
	var role string
	var rm, sm []byte
	if err := row.Scan(&p.ID, &p.Name, &p.DisplayName, &p.Issuer, &p.ClientID, &p.secretID, &p.Scopes, &p.AllowedDomains, &p.GroupsClaim, &role,
		&rm, &sm, &p.AutoCreate, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.DefaultRole, p.HasSecret = Role(role), p.secretID != nil
	if err := json.Unmarshal(rm, &p.RoleMappings); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(sm, &p.ScopeMappings); err != nil {
		return nil, err
	}
	return &p, nil
}

// Providers lists every provider.
func (o *OIDC) Providers(ctx context.Context) ([]OIDCProvider, error) {
	rows, err := o.Store.Pool.Query(ctx, `SELECT `+providerCols+` FROM oidc_providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OIDCProvider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Provider returns one provider by name.
func (o *OIDC) Provider(ctx context.Context, name string) (*OIDCProvider, error) {
	return scanProvider(o.Store.Pool.QueryRow(ctx, `SELECT `+providerCols+` FROM oidc_providers WHERE name=$1`, name))
}

// Buttons lists the enabled providers for the sign-in page.
func (o *OIDC) Buttons(ctx context.Context) ([]OIDCButton, error) {
	rows, err := o.Store.Pool.Query(ctx, `SELECT name, display_name FROM oidc_providers WHERE enabled ORDER BY display_name, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OIDCButton{}
	for rows.Next() {
		var b OIDCButton
		if err := rows.Scan(&b.Name, &b.Label); err != nil {
			return nil, err
		}
		if b.Label == "" {
			b.Label = b.Name
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (o *OIDC) checkRepos(ctx context.Context, ms []ScopeMapping) error {
	for _, m := range ms {
		var ok bool
		if err := o.Store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE name=$1)`, m.Repository).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no repository named %q", ErrBad, m.Repository)
		}
	}
	return nil
}

// CreateProvider adds a provider; the client secret goes to the sealed store.
func (o *OIDC) CreateProvider(ctx context.Context, in OIDCProviderInput) (*OIDCProvider, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	if err := o.checkRepos(ctx, in.ScopeMappings); err != nil {
		return nil, err
	}
	var secretID *string
	if in.ClientSecret != "" {
		id, err := o.Secrets.CreateSecret(ctx, secretName(in.Name), "oidc", []byte(in.ClientSecret))
		if err != nil {
			return nil, err
		}
		secretID = &id
	}
	rm, _ := json.Marshal(in.RoleMappings)
	sm, _ := json.Marshal(in.ScopeMappings)
	p, err := scanProvider(o.Store.Pool.QueryRow(ctx, `INSERT INTO oidc_providers (name, display_name, issuer, client_id, secret_id, scopes, allowed_domains, groups_claim,
			default_role, role_mappings, scope_mappings, auto_create, enabled, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5::uuid,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14) RETURNING `+providerCols,
		in.Name, in.DisplayName, in.Issuer, in.ClientID, secretID, in.Scopes, in.AllowedDomains, in.GroupsClaim, string(in.DefaultRole), rm, sm, in.AutoCreate, in.Enabled, o.Store.Now()))
	if err != nil {
		if secretID != nil {
			_ = o.Secrets.DeleteSecret(ctx, *secretID)
		}
		if strings.Contains(err.Error(), "oidc_providers_name_key") {
			return nil, fmt.Errorf("%w: provider %q", ErrExists, in.Name)
		}
		return nil, err
	}
	return p, nil
}

// UpdateProvider replaces a provider's settings (the name cannot change: it is in the redirect URL).
// Disabling it while local sign-in is off is refused when no other administrator could sign in.
func (o *OIDC) UpdateProvider(ctx context.Context, name string, in OIDCProviderInput) (*OIDCProvider, error) {
	in.Name = name
	if err := in.normalize(); err != nil {
		return nil, err
	}
	if err := o.checkRepos(ctx, in.ScopeMappings); err != nil {
		return nil, err
	}
	cur, err := o.Provider(ctx, name)
	if err != nil {
		return nil, err
	}
	// the secret is written only after the settings were accepted (a refused update changes nothing)
	secretID := cur.secretID
	if in.ClearSecret && in.ClientSecret == "" {
		secretID = nil
	}
	rm, _ := json.Marshal(in.RoleMappings)
	sm, _ := json.Marshal(in.ScopeMappings)
	tx, err := o.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	p, err := scanProvider(tx.QueryRow(ctx, `UPDATE oidc_providers SET display_name=$2, issuer=$3, client_id=$4, secret_id=$5::uuid, scopes=$6, allowed_domains=$7,
			groups_claim=$8, default_role=$9, role_mappings=$10, scope_mappings=$11, auto_create=$12, enabled=$13, updated_at=$14
		WHERE name=$1 RETURNING `+providerCols,
		name, in.DisplayName, in.Issuer, in.ClientID, secretID, in.Scopes, in.AllowedDomains, in.GroupsClaim, string(in.DefaultRole), rm, sm, in.AutoCreate, in.Enabled, o.Store.Now()))
	if err != nil {
		return nil, err
	}
	if !in.Enabled {
		if err := guardLocalOff(ctx, tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	o.forget(cur.ID)
	switch {
	case in.ClientSecret != "" && cur.secretID != nil:
		if err := o.Secrets.RotateSecret(ctx, *cur.secretID, []byte(in.ClientSecret)); err != nil {
			return nil, err
		}
	case in.ClientSecret != "":
		id, err := o.Secrets.CreateSecret(ctx, secretName(name), "oidc", []byte(in.ClientSecret))
		if err != nil {
			return nil, err
		}
		if _, err := o.Store.Pool.Exec(ctx, `UPDATE oidc_providers SET secret_id=$2::uuid WHERE id::text=$1`, cur.ID, id); err != nil {
			_ = o.Secrets.DeleteSecret(ctx, id)
			return nil, err
		}
		p.secretID, p.HasSecret = &id, true
	case in.ClearSecret && cur.secretID != nil:
		_ = o.Secrets.DeleteSecret(ctx, *cur.secretID)
	}
	return p, nil
}

// secretName names a provider's sealed secret; the random part keeps a left-over row (a crash between
// two statements) from blocking the name for good.
func secretName(provider string) string {
	return "oidc-" + provider + "-" + strings.NewReplacer("-", "x", "_", "y").Replace(randToken(6))
}

// guardLocalOff: while local sign-in is off, an administrator must still be able to sign in.
func guardLocalOff(ctx context.Context, tx pgx.Tx) error {
	if settingIn(ctx, tx, keyLocalLogin) != "false" {
		return nil
	}
	if err := ssoAdminExists(ctx, tx); err != nil {
		return fmt.Errorf("%w (password sign-in is off: turn it on first in Settings → Authentication)", err)
	}
	return nil
}

// DeleteProvider removes a provider, its identities and its sealed secret.
func (o *OIDC) DeleteProvider(ctx context.Context, name string) error {
	cur, err := o.Provider(ctx, name)
	if err != nil {
		return err
	}
	tx, err := o.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM oidc_providers WHERE id::text=$1`, cur.ID); err != nil {
		return err
	}
	if err := guardLocalOff(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if cur.secretID != nil {
		_ = o.Secrets.DeleteSecret(ctx, *cur.secretID)
	}
	o.forget(cur.ID)
	return nil
}

func (o *OIDC) forget(id string) {
	o.mu.Lock()
	delete(o.cache, id)
	o.mu.Unlock()
}

// discover returns the go-oidc provider (discovery document and its key set), cached for an hour.
func (o *OIDC) discover(ctx context.Context, p *OIDCProvider) (*oidc.Provider, error) {
	o.mu.Lock()
	c, ok := o.cache[p.ID]
	o.mu.Unlock()
	if ok && c.issuer == p.Issuer && time.Since(c.at) < time.Hour {
		return c.p, nil
	}
	dctx, cancel := context.WithTimeout(o.ctx(ctx), 15*time.Second)
	defer cancel()
	// the key set fetches later with this context's client (go-oidc detaches it from cancellation)
	op, err := oidc.NewProvider(dctx, p.Issuer)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	if o.cache == nil {
		o.cache = map[string]cachedProvider{}
	}
	o.cache[p.ID] = cachedProvider{issuer: p.Issuer, p: op, at: time.Now()}
	o.mu.Unlock()
	return op, nil
}

func (o *OIDC) oauthConfig(ctx context.Context, p *OIDCProvider, op *oidc.Provider, redirect string) (*oauth2.Config, error) {
	c := &oauth2.Config{ClientID: p.ClientID, Endpoint: op.Endpoint(), RedirectURL: redirect, Scopes: p.Scopes}
	if p.secretID != nil {
		sec, err := o.Secrets.ReadSecret(ctx, *p.secretID)
		if err != nil {
			return nil, err
		}
		c.ClientSecret = string(sec)
	}
	return c, nil
}

// SafeNext keeps a post-sign-in destination on this site (a path, never another host).
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.HasPrefix(next, "/api/") ||
		strings.IndexFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 { // browsers drop tab/CR/LF before parsing a Location
		return "/"
	}
	return next
}

// StartLogin creates an authorization request and returns the identity provider's URL plus the browser
// binding value the caller must set as a cookie (only that browser can complete the sign-in).
func (o *OIDC) StartLogin(ctx context.Context, name, redirect, next string) (authURL, browser string, err error) {
	p, err := o.Provider(ctx, name)
	if err != nil || !p.Enabled {
		return "", "", ssoErr("provider", "no enabled provider named %q", name)
	}
	op, err := o.discover(ctx, p)
	if err != nil {
		return "", "", ssoErr("provider", "discovery of %s failed: %v", p.Issuer, err)
	}
	conf, err := o.oauthConfig(ctx, p, op, redirect)
	if err != nil {
		return "", "", err
	}
	state, nonce, verifier := randToken(32), randToken(24), oauth2.GenerateVerifier()
	browser = randToken(32)
	now := o.Store.Now()
	if _, err := o.Store.Pool.Exec(ctx, `INSERT INTO oidc_logins (state_hash, provider_id, browser_hash, nonce, verifier, next, created_at, expires_at)
		VALUES ($1,$2::uuid,$3,$4,$5,$6,$7,$8)`, hashToken(state), p.ID, hashToken(browser), nonce, verifier, SafeNext(next), now, now.Add(LoginTTL)); err != nil {
		return "", "", err
	}
	return conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), browser, nil
}

// claims of the ID token this server uses.
type idClaims struct {
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     any    `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// verifiedEmail parses email_verified fail-closed: only JSON true (or the string "true", which some
// providers send) counts.
func verifiedEmail(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return false
}

func groupsFrom(raw map[string]any, claim string) []string {
	var out []string
	switch v := raw[claim].(type) {
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok {
				out = append(out, s)
			}
		}
	case string:
		out = append(out, v)
	}
	return out
}

// FinishLogin completes the flow at the callback: the state (single use, unexpired, this provider,
// this browser), the code exchange with the PKCE verifier, the ID token (signature, issuer, audience,
// expiry by go-oidc; the nonce here), a verified e-mail, the account (by subject, or a pending explicit
// link, or auto-creation), its role and scopes from the claims, then the session. idpError is the
// provider's ?error= (the user cancelled or was refused there).
func (o *OIDC) FinishLogin(ctx context.Context, name, state, code, idpError, browser, redirect, ip, ua string) (*Session, string, error) {
	var provID, nonce, verifier, next string
	var bhash []byte
	err := o.Store.Pool.QueryRow(ctx, `DELETE FROM oidc_logins WHERE state_hash=$1 AND expires_at > $2 RETURNING provider_id::text, browser_hash, nonce, verifier, next`,
		hashToken(state), o.Store.Now()).Scan(&provID, &bhash, &nonce, &verifier, &next)
	if err != nil {
		return nil, "/", ssoErr("state", "unknown, used or expired state")
	}
	if browser == "" || subtle.ConstantTimeCompare(bhash, hashToken(browser)) != 1 {
		return nil, "/", ssoErr("state", "the sign-in was started in another browser (binding cookie missing or different)")
	}
	p, err := o.Provider(ctx, name)
	if err != nil || p.ID != provID {
		return nil, "/", ssoErr("state", "the state belongs to another provider")
	}
	if !p.Enabled {
		return nil, "/", ssoErr("provider", "provider %s is disabled", name)
	}
	if idpError != "" {
		return nil, "/", ssoErr("denied", "the identity provider answered %.100q", idpError)
	}
	op, err := o.discover(ctx, p)
	if err != nil {
		return nil, "/", ssoErr("provider", "discovery failed: %v", err)
	}
	conf, err := o.oauthConfig(ctx, p, op, redirect)
	if err != nil {
		return nil, "/", err
	}
	xctx, cancel := context.WithTimeout(o.ctx(ctx), 20*time.Second)
	defer cancel()
	tok, err := conf.Exchange(xctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, "/", ssoErr("failed", "code exchange: %v", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, "/", ssoErr("failed", "the token answer has no id_token")
	}
	idt, err := op.Verifier(&oidc.Config{ClientID: p.ClientID, Now: o.Store.Now}).Verify(xctx, raw)
	if err != nil {
		return nil, "/", ssoErr("failed", "ID token refused: %v", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return nil, "/", ssoErr("failed", "ID token nonce does not match the request")
	}
	var c idClaims
	all := map[string]any{}
	if err := idt.Claims(&c); err != nil {
		return nil, "/", ssoErr("failed", "claims: %v", err)
	}
	_ = idt.Claims(&all)
	if c.Subject == "" {
		return nil, "/", ssoErr("failed", "ID token has no subject")
	}
	if c.Email == "" || !verifiedEmail(c.EmailVerified) {
		return nil, "/", ssoErr("unverified", "the identity provider did not confirm the e-mail address (email_verified is not true) for subject %q", c.Subject)
	}
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if len(p.AllowedDomains) > 0 {
		_, dom, _ := strings.Cut(email, "@")
		if !slices.Contains(p.AllowedDomains, dom) {
			return nil, "/", ssoErr("domain", "e-mail domain %q is not allowed by provider %s", dom, p.Name)
		}
	}
	role, scopes := p.access(groupsFrom(all, p.GroupsClaim))
	sess, user, err := o.signIn(ctx, p, c, email, role, scopes, ip, ua)
	if err != nil {
		var se *SSOError
		if errors.As(err, &se) && se.User == "" {
			se.User = user
		}
		return nil, "/", err
	}
	return sess, next, nil
}

// access computes role and scopes from the groups (nil scopes = not restricted).
func (p *OIDCProvider) access(groups []string) (Role, map[string]Role) {
	role := p.DefaultRole
	for _, m := range p.RoleMappings {
		if slices.Contains(groups, m.Group) {
			role = maxRole(role, m.Role)
		}
	}
	if len(p.ScopeMappings) == 0 || role == Admin {
		return role, nil
	}
	scopes := map[string]Role{}
	for _, m := range p.ScopeMappings {
		if slices.Contains(groups, m.Group) {
			scopes[m.Repository] = maxRole(scopes[m.Repository], m.Role)
		}
	}
	return role, scopes
}

var nameClean = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (o *OIDC) signIn(ctx context.Context, p *OIDCProvider, c idClaims, email string, role Role, scopes map[string]Role, ip, ua string) (*Session, string, error) {
	now := o.Store.Now()
	tx, err := o.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	var userID string
	err = tx.QueryRow(ctx, `SELECT user_id::text FROM user_identities WHERE provider_id::text=$1 AND subject=$2`, p.ID, c.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		// an administrator's explicit link by this verified e-mail, bound now to the subject (once)
		err = tx.QueryRow(ctx, `UPDATE user_identities SET subject=$3 WHERE provider_id::text=$1 AND subject IS NULL AND lower(email)=$2 RETURNING user_id::text`,
			p.ID, email, c.Subject).Scan(&userID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if !p.AutoCreate {
			return nil, "", ssoErr("no_account", "no account is linked to subject %q (%s) at %s and automatic creation is off", c.Subject, email, p.Name)
		}
		name := c.PreferredUsername
		if name == "" {
			name, _, _ = strings.Cut(email, "@")
		}
		name = strings.Trim(nameClean.ReplaceAllString(name, "-"), "-")
		if len(name) > 64 {
			name = name[:64]
		}
		if name == "" {
			return nil, "", ssoErr("failed", "no usable user name in the claims")
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(name)=lower($1))`, name).Scan(&exists); err != nil {
			return nil, "", err
		}
		if exists { // never take over an existing account by name
			return nil, name, ssoErr("exists", "a user named %q already exists and is not linked to %s: link it explicitly (Users → link to provider) or rename it", name, p.Name)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO users (name, display_name, email, role, password_hash, created_at, password_changed_at) VALUES ($1,$2,$3,'none','',$4,$4) RETURNING id::text`,
			name, c.Name, email, now).Scan(&userID); err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_identities (provider_id, user_id, subject, email, created_at) VALUES ($1::uuid,$2::uuid,$3,$4,$5)`, p.ID, userID, c.Subject, email, now); err != nil {
			return nil, "", err
		}
	} else if err != nil {
		return nil, "", err
	}
	var uname string
	var disabled bool
	var locked *time.Time
	if err := tx.QueryRow(ctx, `SELECT name, disabled, locked_until FROM users WHERE id::text=$1 FOR UPDATE`, userID).Scan(&uname, &disabled, &locked); err != nil {
		return nil, "", err
	}
	if disabled {
		return nil, uname, ssoErr("disabled", "account %s is disabled", uname)
	}
	if locked != nil && locked.After(now) {
		return nil, uname, ssoErr("locked", "account %s is locked after failed sign-ins", uname)
	}
	// role and repository scopes from the claims, at every sign-in (the provider is authoritative)
	if _, err := tx.Exec(ctx, `UPDATE users SET role=$2, repo_scoped=$3, email=$4 WHERE id::text=$1`, userID, string(role), scopes != nil, email); err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_repo_scopes WHERE user_id::text=$1`, userID); err != nil {
		return nil, "", err
	}
	for repo, r := range scopes { // a repository deleted since the mapping was saved simply grants nothing
		if _, err := tx.Exec(ctx, `INSERT INTO user_repo_scopes (user_id, repository_id, role) SELECT $1::uuid, id, $3 FROM repositories WHERE name=$2`, userID, repo, string(r)); err != nil {
			return nil, "", err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE user_identities SET last_login_at=$3, email=$4 WHERE provider_id::text=$1 AND user_id::text=$2`, p.ID, userID, now, email); err != nil {
		return nil, "", err
	}
	sess, err := o.Store.openSession(ctx, tx, userID, "oidc", ip, ua, now)
	if err != nil {
		return nil, "", err
	}
	return sess, uname, tx.Commit(ctx)
}

// Test runs discovery against the provider's issuer and fetches its key set (no cache).
func (o *OIDC) Test(ctx context.Context, name, redirect string) (*DiscoveryReport, error) {
	p, err := o.Provider(ctx, name)
	if err != nil {
		return nil, err
	}
	r := &DiscoveryReport{RedirectURI: redirect, Warnings: []string{}}
	dctx, cancel := context.WithTimeout(o.ctx(ctx), 15*time.Second)
	defer cancel()
	op, err := oidc.NewProvider(dctx, p.Issuer)
	if err != nil {
		r.Error = err.Error()
		return r, nil
	}
	var meta struct {
		Issuer      string   `json:"issuer"`
		JWKSURI     string   `json:"jwks_uri"`
		PKCE        []string `json:"code_challenge_methods_supported"`
		Scopes      []string `json:"scopes_supported"`
		Claims      []string `json:"claims_supported"`
		AuthURL     string   `json:"authorization_endpoint"`
		TokenURL    string   `json:"token_endpoint"`
		ResponseTyp []string `json:"response_types_supported"`
	}
	_ = op.Claims(&meta)
	r.Issuer, r.AuthorizationEndpoint, r.TokenEndpoint, r.JWKSURI = meta.Issuer, meta.AuthURL, meta.TokenURL, meta.JWKSURI
	r.PKCEMethods, r.ScopesSupported, r.ClaimsSupported = meta.PKCE, meta.Scopes, meta.Claims
	if meta.JWKSURI != "" {
		req, _ := http.NewRequestWithContext(dctx, http.MethodGet, meta.JWKSURI, nil)
		if resp, err := o.client().Do(req); err != nil {
			r.JWKSError = err.Error()
		} else {
			var ks struct {
				Keys []json.RawMessage `json:"keys"`
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode != 200 || json.Unmarshal(b, &ks) != nil {
				r.JWKSError = fmt.Sprintf("HTTP %d, not a JSON key set", resp.StatusCode)
			}
			r.JWKSKeys = len(ks.Keys)
		}
	}
	if len(meta.PKCE) > 0 && !slices.Contains(meta.PKCE, "S256") {
		r.Warnings = append(r.Warnings, "the provider does not list S256 in code_challenge_methods_supported; PKCE is sent anyway")
	}
	if len(meta.Claims) > 0 && !slices.Contains(meta.Claims, "email_verified") {
		r.Warnings = append(r.Warnings, "the provider does not list email_verified among its claims: sign-in is refused without it")
	}
	if len(p.ScopeMappings)+len(p.RoleMappings) > 0 && len(meta.Claims) > 0 && !slices.Contains(meta.Claims, p.GroupsClaim) {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the provider does not list the %q claim; group mappings only work if the ID token carries it (often a scope or client mapper must add it)", p.GroupsClaim))
	}
	r.OK = r.JWKSError == "" && r.JWKSKeys > 0
	if r.JWKSKeys == 0 && r.JWKSError == "" {
		r.JWKSError = "the key set is empty"
	}
	return r, nil
}

// Identities lists a user's provider links (pending ones included).
func (o *OIDC) Identities(ctx context.Context, user string) ([]Identity, error) {
	rows, err := o.Store.Pool.Query(ctx, `SELECT p.name, COALESCE(i.subject,''), i.email, i.subject IS NULL, i.created_at, i.last_login_at
		FROM user_identities i JOIN oidc_providers p ON p.id=i.provider_id JOIN users u ON u.id=i.user_id WHERE lower(u.name)=lower($1) ORDER BY p.name`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var x Identity
		if err := rows.Scan(&x.Provider, &x.Subject, &x.Email, &x.Pending, &x.CreatedAt, &x.LastLogin); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// LinkIdentity is an administrator's explicit link: the first sign-in at provider with this verified
// e-mail address binds to user (once; afterwards the subject is what matches).
func (o *OIDC) LinkIdentity(ctx context.Context, user, provider, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return fmt.Errorf("%w: an e-mail address is required", ErrBad)
	}
	tag, err := o.Store.Pool.Exec(ctx, `INSERT INTO user_identities (provider_id, user_id, email, created_at)
		SELECT p.id, u.id, $3, $4 FROM oidc_providers p, users u WHERE p.name=$2 AND lower(u.name)=lower($1)`, user, provider, email, o.Store.Now())
	if err != nil {
		if strings.Contains(err.Error(), "user_identities_pkey") {
			return fmt.Errorf("%w: the user is already linked to %s (remove the link first)", ErrExists, provider)
		}
		if strings.Contains(err.Error(), "user_identities_pending") {
			return fmt.Errorf("%w: another user waits for the first sign-in of %s at %s", ErrExists, email, provider)
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UnlinkIdentity removes a user's link to a provider (refused when it would lock everyone out while
// local sign-in is off).
func (o *OIDC) UnlinkIdentity(ctx context.Context, user, provider string) error {
	tx, err := o.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM user_identities WHERE provider_id=(SELECT id FROM oidc_providers WHERE name=$2) AND user_id=(SELECT id FROM users WHERE lower(name)=lower($1))`, user, provider)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := guardLocalOff(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
