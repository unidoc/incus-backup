package auth

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Sign-in policy (docs/authentication.md). The values live in server_settings like every other
// setting, but they are NOT in the generic PUT /settings table: changing them goes through
// SetPolicy, which applies the lock-out guard (local_login) and validation that needs the database.
const (
	keyRequire2FA = "require_2fa"
	keyLocalLogin = "local_login"
	keyRPID       = "webauthn_rp_id"
	keyOrigins    = "webauthn_origins"
)

// Require2FA values.
const (
	Require2FANone  = "none"
	Require2FAAdmin = "admin"
	Require2FAAll   = "all"
)

// Policy is the sign-in policy.
type Policy struct {
	// Require2FA: none, admin (users whose global role is admin) or all: a second factor (TOTP or a
	// security key) is required at password sign-in; a user without one gets an enrolment-only session.
	Require2FA string `json:"require_2fa"`
	// LocalLogin: false = only single sign-on is accepted on the web listener (no password, no passkey).
	LocalLogin bool `json:"local_login"`
	// WebAuthnRPID and WebAuthnOrigins override what base_url implies ("" = derived).
	WebAuthnRPID    string   `json:"webauthn_rp_id"`
	WebAuthnOrigins []string `json:"webauthn_origins"`
}

// PolicyUpdate changes some policy values (nil = unchanged).
type PolicyUpdate struct {
	Require2FA      *string   `json:"require_2fa,omitempty"`
	LocalLogin      *bool     `json:"local_login,omitempty"`
	WebAuthnRPID    *string   `json:"webauthn_rp_id,omitempty"`
	WebAuthnOrigins *[]string `json:"webauthn_origins,omitempty"`
}

func policyCovers(require2FA string, role Role) bool {
	switch require2FA {
	case Require2FAAll:
		return true
	case Require2FAAdmin:
		return role == Admin
	}
	return false
}

func settingIn(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string) string {
	var v string
	_ = q.QueryRow(ctx, `SELECT value FROM server_settings WHERE key=$1`, key).Scan(&v)
	return v
}

// Policy returns the current sign-in policy.
func (s *Store) Policy(ctx context.Context) (Policy, error) {
	p := Policy{Require2FA: Require2FANone, LocalLogin: true, WebAuthnOrigins: []string{}}
	rows, err := s.Pool.Query(ctx, `SELECT key, value FROM server_settings WHERE key = ANY($1)`, []string{keyRequire2FA, keyLocalLogin, keyRPID, keyOrigins})
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return p, err
		}
		switch k {
		case keyRequire2FA:
			if v != "" {
				p.Require2FA = v
			}
		case keyLocalLogin:
			p.LocalLogin = v != "false"
		case keyRPID:
			p.WebAuthnRPID = v
		case keyOrigins:
			p.WebAuthnOrigins = splitList(v)
		}
	}
	return p, rows.Err()
}

func splitList(v string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		out = append(out, f)
	}
	return out
}

// SetPolicy validates and stores policy changes atomically. Turning local sign-in off is refused unless
// an enabled administrator can sign in through an enabled identity provider (otherwise nobody could
// manage the server from the web: the unix-socket CLI stays the break-glass path either way).
func (s *Store) SetPolicy(ctx context.Context, u PolicyUpdate) error {
	kv := map[string]string{}
	if u.Require2FA != nil {
		switch *u.Require2FA {
		case Require2FANone, Require2FAAdmin, Require2FAAll:
			kv[keyRequire2FA] = *u.Require2FA
		default:
			return fmt.Errorf("%w: require_2fa must be none, admin or all", ErrBad)
		}
	}
	if u.LocalLogin != nil {
		kv[keyLocalLogin] = fmt.Sprint(*u.LocalLogin)
	}
	if u.WebAuthnRPID != nil {
		id := strings.ToLower(strings.TrimSpace(*u.WebAuthnRPID))
		if id != "" && (strings.ContainsAny(id, ":/ ") || net.ParseIP(id) != nil || !strings.Contains(id, ".") && id != "localhost") {
			return fmt.Errorf("%w: webauthn_rp_id must be a domain name such as backup.example.com (no scheme, port or IP address)", ErrBad)
		}
		kv[keyRPID] = id
	}
	if u.WebAuthnOrigins != nil {
		var clean []string
		for _, o := range *u.WebAuthnOrigins {
			o = strings.TrimRight(strings.TrimSpace(o), "/")
			if o == "" {
				continue
			}
			if err := validOrigin(o); err != nil {
				return err
			}
			clean = append(clean, o)
		}
		kv[keyOrigins] = strings.Join(clean, ",")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE server_settings IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	for k, v := range kv {
		if _, err := tx.Exec(ctx, `INSERT INTO server_settings (key, value, updated_at) VALUES ($1,$2,$3)
			ON CONFLICT (key) DO UPDATE SET value=$2, updated_at=$3`, k, v, s.Now()); err != nil {
			return err
		}
	}
	// the WebAuthn relying party must be consistent with its origins (checked on the stored result)
	if _, _, err := rpFrom(ctx, tx); err != nil && (u.WebAuthnRPID != nil || u.WebAuthnOrigins != nil) {
		if !strings.Contains(err.Error(), "not configured") {
			return fmt.Errorf("%w: %v", ErrBad, err)
		}
	}
	if u.LocalLogin != nil && !*u.LocalLogin {
		if err := ssoAdminExists(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SSOAdmins lists the enabled administrators who signed in through an enabled identity provider (the
// people the local_login guard counts).
func (s *Store) SSOAdmins(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT u.name FROM users u JOIN user_identities i ON i.user_id=u.id JOIN oidc_providers p ON p.id=i.provider_id
		WHERE u.role='admin' AND NOT u.disabled AND p.enabled AND i.subject IS NOT NULL ORDER BY u.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ssoAdminExists is the local_login lock-out guard.
func ssoAdminExists(ctx context.Context, tx pgx.Tx) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users u JOIN user_identities i ON i.user_id=u.id JOIN oidc_providers p ON p.id=i.provider_id
		WHERE u.role='admin' AND NOT u.disabled AND p.enabled AND i.subject IS NOT NULL`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: no enabled administrator has signed in through an enabled identity provider yet: sign in once with single sign-on as an administrator first, or nobody could manage this server from the web", ErrBad)
	}
	return nil
}

func validOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return fmt.Errorf("%w: origin %q must look like https://backup.example.com[:port]", ErrBad, o)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "localhost") {
		return fmt.Errorf("%w: origin %q: passkeys need https (or http://localhost)", ErrBad, o)
	}
	return nil
}

// rpFrom derives the WebAuthn relying party: webauthn_rp_id / webauthn_origins when set, else the
// host and origin of base_url. The RP ID must be the origins' host or a parent domain of it.
func rpFrom(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (string, []string, error) {
	rpID := settingIn(ctx, q, keyRPID)
	origins := splitList(settingIn(ctx, q, keyOrigins))
	if base := settingIn(ctx, q, "base_url"); len(origins) == 0 && base != "" {
		if u, err := url.Parse(base); err == nil && u.Host != "" {
			origins = []string{u.Scheme + "://" + u.Host}
		}
	}
	if len(origins) == 0 {
		return "", nil, fmt.Errorf("passkeys are not configured: set the console address (base_url, Settings → Server) or webauthn_origins")
	}
	if rpID == "" {
		u, _ := url.Parse(origins[0])
		rpID = u.Hostname()
	}
	if net.ParseIP(rpID) != nil {
		return "", nil, fmt.Errorf("passkeys need a domain name, not an IP address (%s): open the console by its DNS name and set base_url to it", rpID)
	}
	for _, o := range origins {
		if err := validOrigin(o); err != nil {
			return "", nil, err
		}
		u, _ := url.Parse(o)
		h := u.Hostname()
		if h != rpID && !strings.HasSuffix(h, "."+rpID) {
			return "", nil, fmt.Errorf("the passkey domain %q does not cover the origin %s: set webauthn_rp_id to %s or a parent domain of it", rpID, o, h)
		}
	}
	return rpID, origins, nil
}

// WebAuthnRP is the effective relying party ID and origins, or why passkeys cannot work yet.
func (s *Store) WebAuthnRP(ctx context.Context) (rpID string, origins []string, err error) {
	return rpFrom(ctx, s.Pool)
}
