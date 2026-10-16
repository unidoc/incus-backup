// Package auth is people-and-automation authentication for the management plane (web UI and
// /api/v1): users with argon2id passwords, four roles, TOTP two-factor with recovery codes,
// cookie sessions with idle and absolute expiry, API tokens and an audit trail.
//
// Hosts (backup agents) do not use this: they authenticate with mutual TLS.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

// Role is what a user may do.
type Role string

// Roles, from least to most privileged.
const (
	ReadOnly Role = "readonly" // look at everything
	Restore  Role = "restore"  // + browse and restore
	Operator Role = "operator" // + run jobs, edit policies/schedules, notes, protection, host tokens
	Admin    Role = "admin"    // + users, repositories, secrets, notifications, settings, destructive operations
	// None is a global role that allows nothing: a person who only has per-repository roles
	// (user_repo_scopes, docs/permissions.md). Never a token role or a scope role.
	None Role = "none"
)

// Perm is one capability a route requires.
type Perm int

// Permissions.
const (
	PermRead Perm = iota
	PermRestore
	PermOperate
	PermAdmin
)

var roleLevel = map[Role]Perm{None: -1, ReadOnly: PermRead, Restore: PermRestore, Operator: PermOperate, Admin: PermAdmin}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { _, ok := roleLevel[r]; return ok }

// Allows reports whether the role grants p.
func (r Role) Allows(p Perm) bool {
	l, ok := roleLevel[r]
	return ok && l >= p
}

// Errors.
var (
	ErrInvalidCredentials = errors.New("invalid user name, password or code")
	ErrLocked             = errors.New("too many failed attempts: try again later")
	ErrNeedTOTP           = errors.New("a two-factor code is required")
	ErrNoSession          = errors.New("not signed in")
	ErrWeakPassword       = errors.New("password must be at least 12 characters")
	ErrExists             = errors.New("already exists")
	ErrNotFound           = errors.New("not found")
	ErrBad                = errors.New("bad request")
	ErrSetup              = errors.New("setup token is wrong or setup is already done")
	// ErrLocalLoginOff: the server accepts only single sign-on (server setting local_login=false).
	ErrLocalLoginOff = errors.New("password and passkey sign-in are turned off on this server: use single sign-on")
	// ErrNoLocalPassword: the user signs in through an identity provider and has no password here.
	ErrNoLocalPassword = errors.New("this account signs in through single sign-on and has no password on this server")
)

// Timing of sessions.
const (
	IdleTimeout     = 30 * time.Minute
	AbsoluteTimeout = 12 * time.Hour
	MinPassword     = 12
)

// Argon2id cost (a variable so tests can run cheaply; production keeps the defaults).
var (
	Argon2Time    uint32 = 3
	Argon2Memory  uint32 = 64 * 1024
	Argon2Threads uint8  = 2
)

// Sealer protects small secrets (the TOTP seed) with the server's secret-store key.
type Sealer interface {
	Seal(plain []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// Store is the user database.
type Store struct {
	Pool   *pgxpool.Pool
	Sealer Sealer
	Now    func() time.Time
	Issuer string // shown in authenticator apps

	ghosts ghostLocks // lockout state for names that have no password to guess (see Login)
}

// ghostLocks gives names with no password account (unknown names, single sign-on accounts) the same
// lockout a real account has, so the answer to repeated wrong guesses (429 after five) does not tell
// whether a name exists. It is in memory and bounded: a restart forgets it. Entries are keyed by a hash
// of the lower-cased name (a fixed 32 bytes, whatever the name's length; Login also refuses names over
// maxLoginName before they get here), expire ghostTTL after their last failure, and when the map is
// full of live entries the one expiring soonest is evicted, so new names are always tracked. The
// remaining bound: an attacker who sends maxGhostLocks other fresh names can push a ghost's count out
// again, which costs one full password-hash round per name.
type ghostLocks struct {
	mu sync.Mutex
	m  map[[32]byte]ghostLock
}

type ghostLock struct {
	failed int
	until  time.Time // locked until (zero = not locked)
	expiry time.Time // the entry is dropped after this
}

const (
	maxGhostLocks = 10000
	ghostTTL      = 24 * time.Hour // from the last failure; always longer than the longest lock
	maxLoginName  = 256            // a user name is at most 64 bytes (validName); longer is refused before any lookup
)

func ghostKey(name string) [32]byte { return sha256.Sum256([]byte(strings.ToLower(name))) }

func (g *ghostLocks) locked(name string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := ghostKey(name)
	e, ok := g.m[k]
	if ok && !e.expiry.After(now) {
		delete(g.m, k)
		return false
	}
	return e.until.After(now)
}

func (g *ghostLocks) fail(name string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[[32]byte]ghostLock{}
	}
	k := ghostKey(name)
	e, ok := g.m[k]
	if ok && !e.expiry.After(now) {
		e = ghostLock{}
		ok = false
	}
	if !ok && len(g.m) >= maxGhostLocks {
		var oldest [32]byte
		var oldestAt time.Time
		first := true
		for k2, v := range g.m {
			if !v.expiry.After(now) {
				delete(g.m, k2) // drop what has expired
			} else if first || v.expiry.Before(oldestAt) {
				oldest, oldestAt, first = k2, v.expiry, false
			}
		}
		if len(g.m) >= maxGhostLocks {
			delete(g.m, oldest)
		}
	}
	e.failed++
	if d := lockFor(e.failed); d > 0 {
		e.until = now.Add(d)
	}
	e.expiry = now.Add(ghostTTL)
	g.m[k] = e
}

// New returns a Store.
func New(pool *pgxpool.Pool, sealer Sealer) *Store {
	return &Store{Pool: pool, Sealer: sealer, Now: time.Now, Issuer: "incus-backup"}
}

// User is a user row without credential material.
type User struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name"`
	Email       string     `json:"email"`
	Role        Role       `json:"role"`
	TOTP        bool       `json:"totp_enabled"`
	Disabled    bool       `json:"disabled"`
	MustChange  bool       `json:"must_change_password"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLogin   *time.Time `json:"last_login_at,omitempty"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	// HasPassword is false for a user created by single sign-on (no local password).
	HasPassword bool `json:"has_password"`
	// WebAuthnKeys is the number of registered passkeys / security keys.
	WebAuthnKeys int `json:"webauthn_keys"`
	// Identities names the identity providers this user is linked to (docs/authentication.md).
	Identities []string `json:"identities"`
}

// Principal is who a request acts as.
type Principal struct {
	Kind   string // "user", "token", "local"
	User   string // user name, token name, "local-admin"
	UserID string
	Role   Role
	CSRF   string // session CSRF token (sessions only)
	Owner  string // tokens: the name of the user who owns the token
	// Access holds per-repository roles; nil means Role applies to every repository (scopes.go).
	// When set, Role is the GLOBAL role (routes not bound to a repository).
	Access *Access
	// Method is how a session was opened: "password", "passkey" or "oidc" (sessions only).
	Method string
	// Enrol2FA: the server requires a second factor of this user (setting require_2fa) and none is
	// set up; the session may only reach the enrolment routes (mgmt enforces it).
	Enrol2FA bool
}

// Name is the audit-log actor.
func (p Principal) Name() string {
	if p.Kind == "token" {
		if p.Owner != "" { // token names are only unique per user: keep two users' "ci" tokens apart in the log
			return "token:" + p.User + " (by " + p.Owner + ")"
		}
		return "token:" + p.User
	}
	return p.User
}

// Local is the principal of the unix admin socket: whoever can open it is the server administrator.
var Local = Principal{Kind: "local", User: "local-admin", Role: Admin}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) []byte { h := sha256.Sum256([]byte(t)); return h[:] }

// ---- passwords ---------------------------------------------------------------------------

// argonSlots bounds how many Argon2id computations run at once. Each one allocates Argon2Memory
// (64 MiB): without a bound, a few hundred concurrent logins or password checks (any signed-in user
// can send them) exhaust the server's memory. Callers beyond the bound wait their turn.
var argonSlots = make(chan struct{}, max(2, min(runtime.GOMAXPROCS(0), 8)))

// argonInFlight counts running computations (tests check the bound).
var argonInFlight, argonPeak atomic.Int64

func argonKey(pw string, salt []byte, t, m uint32, th uint8, n uint32) []byte {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	cur := argonInFlight.Add(1)
	defer argonInFlight.Add(-1)
	for {
		p := argonPeak.Load()
		if cur <= p || argonPeak.CompareAndSwap(p, cur) {
			break
		}
	}
	return argon2.IDKey([]byte(pw), salt, t, m, th, n)
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	k := argonKey(pw, salt, Argon2Time, Argon2Memory, Argon2Threads, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", Argon2Memory, Argon2Time, Argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k))
}

func checkPassword(pw, enc string) bool {
	p := strings.Split(enc, "$")
	if len(p) != 6 || p[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var th uint8
	if _, err := fmt.Sscanf(p[3], "m=%d,t=%d,p=%d", &m, &t, &th); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[4])
	want, err2 := base64.RawStdEncoding.DecodeString(p[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	if m > 1<<20 || t > 16 || th == 0 || len(want) > 64 { // only this server writes hashes; refuse absurd costs anyway
		return false
	}
	got := argonKey(pw, salt, t, m, th, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var dummyHash = hashPassword("dummy password for constant-time failure")

func validPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPassword {
		return ErrWeakPassword
	}
	return nil
}

func validName(n string) error {
	if n == "" || len(n) > 64 || strings.ContainsAny(n, " \t\r\n/:") {
		return fmt.Errorf("%w: user name must be 1-64 characters without spaces, '/' or ':'", ErrBad)
	}
	return nil
}

// ---- users -------------------------------------------------------------------------------

const userCols = `id::text, name, display_name, email, role, totp_enabled, disabled, must_change, created_at, last_login_at, locked_until,
	password_hash <> '', (SELECT count(*) FROM webauthn_credentials w WHERE w.user_id=users.id),
	ARRAY(SELECT p.name FROM user_identities i JOIN oidc_providers p ON p.id=i.provider_id WHERE i.user_id=users.id AND i.subject IS NOT NULL ORDER BY p.name)`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	var role string
	if err := row.Scan(&u.ID, &u.Name, &u.DisplayName, &u.Email, &role, &u.TOTP, &u.Disabled, &u.MustChange, &u.CreatedAt, &u.LastLogin, &u.LockedUntil,
		&u.HasPassword, &u.WebAuthnKeys, &u.Identities); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Role = Role(role)
	return &u, nil
}

// CreateUser adds a user.
func (s *Store) CreateUser(ctx context.Context, name, password string, role Role, display, email string, mustChange bool) (*User, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if !role.Valid() {
		return nil, fmt.Errorf("%w: role must be admin, operator, restore, readonly or none", ErrBad)
	}
	if err := validPassword(password); err != nil {
		return nil, err
	}
	u, err := scanUser(s.Pool.QueryRow(ctx, `INSERT INTO users (name, display_name, email, role, password_hash, must_change, created_at, password_changed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7) RETURNING `+userCols, name, display, email, string(role), hashPassword(password), mustChange, s.Now()))
	if err != nil {
		if strings.Contains(err.Error(), "users_name_lower") {
			return nil, fmt.Errorf("%w: user %q", ErrExists, name)
		}
		return nil, err
	}
	return u, nil
}

// Users lists everyone.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// User returns one user by name.
func (s *Store) User(ctx context.Context, name string) (*User, error) {
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE lower(name)=lower($1)`, name))
}

// CountUsers returns how many users exist.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	return n, s.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
}

// lastAdmin reports whether changing user id would leave no enabled administrator.
func (s *Store) lastAdmin(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND NOT disabled AND id::text <> $1`, id).Scan(&n)
	return n == 0, err
}

// UpdateUser changes role, display name, email and the disabled flag. The last enabled administrator cannot be
// demoted or disabled: the server must stay manageable.
func (s *Store) UpdateUser(ctx context.Context, name string, role *Role, display, email *string, disabled *bool) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id, cur string
	var dis bool
	if err := tx.QueryRow(ctx, `SELECT id::text, role, disabled FROM users WHERE lower(name)=lower($1) FOR UPDATE`, name).Scan(&id, &cur, &dis); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if cur == string(Admin) && !dis {
		demote := role != nil && *role != Admin
		disable := disabled != nil && *disabled
		if demote || disable {
			if last, err := s.lastAdmin(ctx, tx, id); err != nil {
				return err
			} else if last {
				return fmt.Errorf("%w: this is the last enabled administrator", ErrBad)
			}
		}
	}
	if role != nil {
		if !role.Valid() {
			return fmt.Errorf("%w: unknown role", ErrBad)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET role=$2 WHERE id::text=$1`, id, string(*role)); err != nil {
			return err
		}
	}
	if display != nil {
		if _, err := tx.Exec(ctx, `UPDATE users SET display_name=$2 WHERE id::text=$1`, id, *display); err != nil {
			return err
		}
	}
	if email != nil {
		if _, err := tx.Exec(ctx, `UPDATE users SET email=$2 WHERE id::text=$1`, id, *email); err != nil {
			return err
		}
	}
	if disabled != nil {
		if _, err := tx.Exec(ctx, `UPDATE users SET disabled=$2 WHERE id::text=$1`, id, *disabled); err != nil {
			return err
		}
		if *disabled {
			if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id::text=$1`, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// DeleteUser removes a user (sessions and tokens go with it). The last administrator cannot be deleted.
func (s *Store) DeleteUser(ctx context.Context, name string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id, role string
	var dis bool
	if err := tx.QueryRow(ctx, `SELECT id::text, role, disabled FROM users WHERE lower(name)=lower($1) FOR UPDATE`, name).Scan(&id, &role, &dis); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if role == string(Admin) && !dis {
		if last, err := s.lastAdmin(ctx, tx, id); err != nil {
			return err
		} else if last {
			return fmt.Errorf("%w: this is the last enabled administrator", ErrBad)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id::text=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetPassword replaces a password and ends every session of that user (except keep, if given).
func (s *Store) SetPassword(ctx context.Context, name, password string, mustChange bool, keepSession string) error {
	if err := validPassword(password); err != nil {
		return err
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE users SET password_hash=$2, must_change=$3, password_changed_at=$4, failed_logins=0, locked_until=NULL WHERE lower(name)=lower($1)`,
		name, hashPassword(password), mustChange, s.Now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=(SELECT id FROM users WHERE lower(name)=lower($1)) AND token_hash <> $2`, name, hashToken(keepSession))
	return err
}

// ChangeOwnPassword checks the current password first. Wrong guesses count against the account like
// failed logins, so a stolen session cannot be used to guess the password.
func (s *Store) ChangeOwnPassword(ctx context.Context, name, current, next, keepSession string) error {
	var has bool
	if err := s.Pool.QueryRow(ctx, `SELECT password_hash <> '' FROM users WHERE lower(name)=lower($1)`, name).Scan(&has); err == nil && !has {
		return fmt.Errorf("%w: %v", ErrBad, ErrNoLocalPassword)
	}
	if err := s.VerifyPassword(ctx, name, current); err != nil {
		return err
	}
	return s.SetPassword(ctx, name, next, false, keepSession)
}

// ---- login -------------------------------------------------------------------------------

// Session is what a successful login returns. Token goes in the cookie, CSRF in the JSON body.
type Session struct {
	Token   string
	CSRF    string
	Expires time.Time
	User    User
}

func lockFor(failed int) time.Duration {
	if failed < 5 {
		return 0
	}
	d := time.Minute << uint(min(failed-5, 4)) // 1,2,4,8,16 minutes
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// SecondFactorNeeded is the answer to a correct password when the user has a second factor: which
// methods are set up, and a ticket (single use, five minutes) that starts a WebAuthn second-factor
// ceremony without sending the password again (webauthn.go). errors.Is(err, ErrNeedTOTP) holds, so
// callers that only know TOTP keep working.
type SecondFactorNeeded struct {
	TOTP     bool
	WebAuthn bool
	Ticket   string
}

func (e *SecondFactorNeeded) Error() string { return ErrNeedTOTP.Error() }

// Is makes errors.Is(err, ErrNeedTOTP) true.
func (e *SecondFactorNeeded) Is(target error) bool { return target == ErrNeedTOTP }

// TicketTTL is how long a verified password waits for its second factor, and how long a WebAuthn
// ceremony (begin to finish) may take.
const TicketTTL = 5 * time.Minute

// Login checks name, password and (when enrolled) a TOTP or recovery code, and opens a session.
// Failures count against the user and lock the account for growing intervals; the error never says
// whether the user exists. A user with a security key but no code gets *SecondFactorNeeded: the
// password alone never opens a session when any second factor is set up.
func (s *Store) Login(ctx context.Context, name, password, code, ip, ua string) (*Session, error) {
	pol, err := s.Policy(ctx)
	if err != nil {
		return nil, err
	}
	if !pol.LocalLogin {
		checkPassword(password, dummyHash) // the same work and answer for every name
		return nil, ErrLocalLoginOff
	}
	if len(name) > maxLoginName { // no user has such a name: an ordinary failure, before any lookup or memory use
		checkPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}
	now := s.Now()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id, enc string
	var disabled, totpOn, keys bool
	var failed int
	var locked *time.Time
	var totpSealed []byte
	var lastStep int64
	var recovery []string
	err = tx.QueryRow(ctx, `SELECT id::text, password_hash, disabled, totp_enabled, totp_sealed, totp_last_step, recovery_hashes, failed_logins, locked_until,
			EXISTS (SELECT 1 FROM webauthn_credentials w WHERE w.user_id=users.id)
		FROM users WHERE lower(name)=lower($1) FOR UPDATE`, name).Scan(&id, &enc, &disabled, &totpOn, &totpSealed, &lastStep, &recovery, &failed, &locked, &keys)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && enc == "") {
		// An unknown name or a single sign-on account (no password can match): count and lock in memory
		// exactly like a real account, so the lockout answer does not tell whether the name exists.
		key := strings.ToLower(name)
		if s.ghosts.locked(key, now) {
			return nil, ErrLocked
		}
		checkPassword(password, dummyHash) // same work as a real failure
		s.ghosts.fail(name, now)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if locked != nil && locked.After(now) {
		return nil, ErrLocked
	}
	fail := func() (*Session, error) {
		if err := s.countFailure(ctx, tx, id, failed, now); err != nil {
			return nil, err
		}
		return nil, ErrInvalidCredentials
	}
	if !checkPassword(password, enc) || disabled {
		return fail()
	}
	if totpOn || keys {
		if code == "" {
			ticket, err := s.newCeremony(ctx, tx, "pending_2fa", id, nil, now)
			if err != nil {
				return nil, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return nil, &SecondFactorNeeded{TOTP: totpOn, WebAuthn: keys, Ticket: ticket}
		}
		if !totpOn { // a security key only: a code cannot stand in for it
			return fail()
		}
		secret, err := s.Sealer.Open(totpSealed)
		if err != nil {
			return nil, fmt.Errorf("opening the two-factor secret: %w", err)
		}
		if step, ok := verifyTOTP(secret, code, now, lastStep); ok {
			if _, err := tx.Exec(ctx, `UPDATE users SET totp_last_step=$2 WHERE id::text=$1`, id, step); err != nil {
				return nil, err
			}
		} else if rest, ok := useRecovery(recovery, code); ok {
			if _, err := tx.Exec(ctx, `UPDATE users SET recovery_hashes=$2 WHERE id::text=$1`, id, rest); err != nil {
				return nil, err
			}
		} else {
			return fail()
		}
	}
	sess, err := s.openSession(ctx, tx, id, "password", ip, ua, now)
	if err != nil {
		return nil, err
	}
	return sess, tx.Commit(ctx)
}

// countFailure records one failed sign-in of user id (failed = the count before) inside tx and commits
// it, so the failure persists although the caller returns an error.
func (s *Store) countFailure(ctx context.Context, tx pgx.Tx, id string, failed int, now time.Time) error {
	failed++
	var until *time.Time
	if d := lockFor(failed); d > 0 {
		t := now.Add(d)
		until = &t
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET failed_logins=$2, locked_until=$3 WHERE id::text=$1`, id, failed, until); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// openSession is the ONE place a session starts, whatever proved the user (password with or without a
// second factor, a passkey, single sign-on): the same cookie token, CSRF token, lifetime and counters.
func (s *Store) openSession(ctx context.Context, tx pgx.Tx, userID, method, ip, ua string, now time.Time) (*Session, error) {
	sess := &Session{Token: randToken(32), CSRF: randToken(24), Expires: now.Add(AbsoluteTimeout)}
	if _, err := tx.Exec(ctx, `INSERT INTO user_sessions (token_hash, user_id, csrf, created_at, last_seen, expires_at, ip, user_agent, method)
		VALUES ($1,$2::uuid,$3,$4,$4,$5,$6,$7,$8)`, hashToken(sess.Token), userID, sess.CSRF, now, sess.Expires, ip, truncate(ua, 300), method); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET failed_logins=0, locked_until=NULL, last_login_at=$2 WHERE id::text=$1`, userID, now); err != nil {
		return nil, err
	}
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id::text=$1`, userID))
	if err != nil {
		return nil, err
	}
	sess.User = *u
	return sess, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Authenticate resolves a session cookie value. Activity renews the idle window; the absolute
// lifetime never moves.
func (s *Store) Authenticate(ctx context.Context, token string) (*Principal, error) {
	return s.authenticate(ctx, token, true)
}

// Recheck validates a session for a long-lived stream without counting as activity: it never moves
// last_seen, so an open event stream cannot keep an idle session alive.
func (s *Store) Recheck(ctx context.Context, token string) (*Principal, error) {
	return s.authenticate(ctx, token, false)
}

func (s *Store) authenticate(ctx context.Context, token string, renew bool) (*Principal, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	now := s.Now()
	h := hashToken(token)
	var p Principal
	var role string
	var last, expires time.Time
	var disabled, secondFactor bool
	var require2FA string
	err := s.Pool.QueryRow(ctx, `SELECT u.id::text, u.name, u.role, u.disabled, s.csrf, s.last_seen, s.expires_at, s.method,
			u.totp_enabled OR EXISTS (SELECT 1 FROM webauthn_credentials w WHERE w.user_id=u.id),
			COALESCE((SELECT value FROM server_settings WHERE key='require_2fa'), '')
		FROM user_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1`, h).Scan(&p.UserID, &p.User, &role, &disabled, &p.CSRF, &last, &expires, &p.Method, &secondFactor, &require2FA)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	if disabled || now.After(expires) || now.Sub(last) > IdleTimeout {
		_, _ = s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash=$1`, h)
		return nil, ErrNoSession
	}
	if renew && now.Sub(last) > time.Minute { // do not write on every request
		_, _ = s.Pool.Exec(ctx, `UPDATE user_sessions SET last_seen=$2 WHERE token_hash=$1`, h, now)
	}
	p.Kind, p.Role = "user", Role(role)
	// A password session of a user the policy requires a second factor of, without one, may only enrol
	// it (decided at every request, so a policy change or a removed factor applies at once). Single
	// sign-on delegates the second factor to the identity provider; a passkey is one.
	p.Enrol2FA = p.Method == "password" && !secondFactor && policyCovers(require2FA, p.Role)
	if err := s.loadAccess(ctx, &p, "", "", false); err != nil {
		return nil, err
	}
	return &p, nil
}

// Logout ends one session.
func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash=$1`, hashToken(token))
	return err
}

// SessionInfo is a listed session.
type SessionInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	Expires   time.Time `json:"expires_at"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Current   bool      `json:"current"`
}

// Sessions lists a user's open sessions. IDs are a prefix of the stored hash, enough to revoke one.
func (s *Store) Sessions(ctx context.Context, name, current string) ([]SessionInfo, error) {
	rows, err := s.Pool.Query(ctx, `SELECT encode(s.token_hash,'hex'), s.created_at, s.last_seen, s.expires_at, s.ip, s.user_agent
		FROM user_sessions s JOIN users u ON u.id=s.user_id WHERE lower(u.name)=lower($1) AND s.expires_at > $2 ORDER BY s.created_at DESC`, name, s.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cur := fmt.Sprintf("%x", hashToken(current))
	out := []SessionInfo{}
	for rows.Next() {
		var x SessionInfo
		var full string
		if err := rows.Scan(&full, &x.CreatedAt, &x.LastSeen, &x.Expires, &x.IP, &x.UserAgent); err != nil {
			return nil, err
		}
		x.ID, x.Current = full[:16], full == cur
		out = append(out, x)
	}
	return out, rows.Err()
}

// RevokeSession ends a session by the ID Sessions returned.
func (s *Store) RevokeSession(ctx context.Context, name, id string) error {
	if len(id) != 16 {
		return fmt.Errorf("%w: session id", ErrBad)
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=(SELECT id FROM users WHERE lower(name)=lower($1)) AND encode(token_hash,'hex') LIKE $2`, name, id+"%")
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeSessions deletes expired sessions (called periodically).
func (s *Store) PurgeSessions(ctx context.Context) error {
	now := s.Now()
	if _, err := s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE expires_at < $1 OR last_seen < $2`, now, now.Add(-IdleTimeout)); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM auth_ceremonies WHERE expires_at < $1`, now); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `DELETE FROM oidc_logins WHERE expires_at < $1`, now)
	return err
}

// ---- API tokens --------------------------------------------------------------------------

// TokenInfo is a listed API token (never the secret).
type TokenInfo struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Role     Role       `json:"role"`
	Created  time.Time  `json:"created_at"`
	Expires  *time.Time `json:"expires_at,omitempty"`
	LastUsed *time.Time `json:"last_used_at,omitempty"`
	// Repositories restricts the token to these repositories (null: not restricted; docs/permissions.md).
	Repositories []string `json:"repositories"`
}

const tokenPrefix = "ibt_"

// CreateToken makes an API token for a user. Its role cannot exceed the user's own role. The secret is
// returned once.
func (s *Store) CreateToken(ctx context.Context, userName, name string, role Role, ttl time.Duration) (string, *TokenInfo, error) {
	return s.CreateTokenFor(ctx, userName, name, role, ttl, nil)
}

// Tokens lists a user's tokens.
func (s *Store) Tokens(ctx context.Context, userName string) ([]TokenInfo, error) {
	rows, err := s.Pool.Query(ctx, `SELECT t.id::text, t.name, t.role, t.created_at, t.expires_at, t.last_used_at
		FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE lower(u.name)=lower($1) ORDER BY t.created_at`, userName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		var r string
		if err := rows.Scan(&t.ID, &t.Name, &r, &t.Created, &t.Expires, &t.LastUsed); err != nil {
			return nil, err
		}
		t.Role = Role(r)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.fillTokenRepos(ctx, out)
}

// RevokeToken deletes a token.
func (s *Store) RevokeToken(ctx context.Context, userName, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM api_tokens WHERE id::text=$2 AND user_id=(SELECT id FROM users WHERE lower(name)=lower($1))`, userName, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AuthenticateToken resolves a bearer token. The effective role is the lower of the token's role and the
// owner's current role, so demoting or disabling a user also limits their tokens.
func (s *Store) AuthenticateToken(ctx context.Context, secret string) (*Principal, error) {
	if !strings.HasPrefix(secret, tokenPrefix) {
		return nil, ErrNoSession
	}
	now := s.Now()
	h := hashToken(secret)
	var p Principal
	var trole, urole string
	var exp *time.Time
	var disabled bool
	var tname, tid string
	var restricted bool
	err := s.Pool.QueryRow(ctx, `SELECT u.id::text, u.name, t.name, t.role, u.role, u.disabled, t.expires_at, t.id::text, t.repo_restricted
		FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=$1`, h).Scan(&p.UserID, &p.User, &tname, &trole, &urole, &disabled, &exp, &tid, &restricted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	if disabled || (exp != nil && now.After(*exp)) {
		return nil, ErrNoSession
	}
	role := Role(trole)
	if !Role(urole).Allows(roleLevel[role]) {
		role = Role(urole)
	}
	_, _ = s.Pool.Exec(ctx, `UPDATE api_tokens SET last_used_at=$2 WHERE token_hash=$1 AND (last_used_at IS NULL OR last_used_at < $2 - interval '1 minute')`, h, now)
	p.Kind, p.Role = "user", Role(urole) // the owner's access first, then narrowed to the token
	if err := s.loadAccess(ctx, &p, tid, Role(trole), restricted); err != nil {
		return nil, err
	}
	p.Kind, p.Owner, p.User = "token", p.User, tname
	if p.Access == nil {
		p.Role = role
	}
	return &p, nil
}

// ---- first-run setup ---------------------------------------------------------------------

// SetupToken returns the one-time token the first administrator must present, creating it when needed.
// It returns "" when users already exist.
func (s *Store) SetupToken(ctx context.Context) (string, error) {
	n, err := s.CountUsers(ctx)
	if err != nil || n > 0 {
		return "", err
	}
	tok := randToken(18)
	_, err = s.Pool.Exec(ctx, `INSERT INTO auth_setup (token_hash) VALUES ($1) ON CONFLICT (singleton) DO UPDATE SET token_hash=$1, created_at=now()`, hashToken(tok))
	return tok, err
}

// SetupNeeded reports whether no user exists yet.
func (s *Store) SetupNeeded(ctx context.Context) (bool, error) {
	n, err := s.CountUsers(ctx)
	return n == 0, err
}

// CompleteSetup creates the first administrator when the token matches; it works exactly once.
func (s *Store) CompleteSetup(ctx context.Context, token, name, password string) (*User, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
		return nil, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return nil, err
	}
	var want []byte
	if err := tx.QueryRow(ctx, `SELECT token_hash FROM auth_setup`).Scan(&want); err != nil || n > 0 || subtle.ConstantTimeCompare(want, hashToken(token)) != 1 {
		return nil, ErrSetup
	}
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := validPassword(password); err != nil {
		return nil, err
	}
	u, err := scanUser(tx.QueryRow(ctx, `INSERT INTO users (name, role, password_hash, created_at, password_changed_at) VALUES ($1,'admin',$2,$3,$3) RETURNING `+userCols, name, hashPassword(password), s.Now()))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM auth_setup`); err != nil {
		return nil, err
	}
	return u, tx.Commit(ctx)
}

// ---- audit -------------------------------------------------------------------------------

// AuditEntry is one audit-log row.
type AuditEntry struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	Actor  string          `json:"actor"`
	Kind   string          `json:"actor_kind"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
	IP     string          `json:"ip"`
	OK     bool            `json:"ok"`
}

// Audit appends an entry. Failures to write are returned, but callers log rather than abort an
// operation that already happened.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if e.Detail == nil {
		e.Detail = json.RawMessage(`{}`)
	}
	if e.Kind == "" {
		e.Kind = "user"
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO audit_log (at, actor, actor_kind, action, target, detail, ip, ok) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		s.Now(), e.Actor, e.Kind, e.Action, e.Target, e.Detail, e.IP, e.OK)
	return err
}

// AuditFilter narrows ListAudit.
type AuditFilter struct {
	Actor, Action string // Action matches by prefix
	Before        int64  // id cursor: entries with id < Before
	Limit         int
}

// ListAudit returns entries, newest first.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, at, actor, actor_kind, action, target, detail, ip, ok FROM audit_log
		WHERE ($1='' OR actor=$1) AND ($2='' OR action LIKE $2 || '%') AND ($3=0 OR id < $3) ORDER BY id DESC LIMIT $4`, f.Actor, f.Action, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Kind, &e.Action, &e.Target, &e.Detail, &e.IP, &e.OK); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RequireLocalPassword returns the single-sign-on refusal (ErrBad wrapping ErrNoLocalPassword) for a user
// who has no local password. Routes that need a password check (Reauthenticate) and refuse such users
// anyway call it first, so the user is not asked for a password they do not have.
func (s *Store) RequireLocalPassword(ctx context.Context, name string) error {
	u, err := s.User(ctx, name)
	if err != nil {
		return err
	}
	if !u.HasPassword {
		return fmt.Errorf("%w: %v", ErrBad, ErrNoLocalPassword)
	}
	return nil
}

// VerifyPassword checks a user's current password (for sensitive self-service actions). A wrong password
// counts as a failed login (the same growing lock), and a locked account is refused before any check.
func (s *Store) VerifyPassword(ctx context.Context, name, password string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id, enc string
	var failed int
	var locked *time.Time
	if err := tx.QueryRow(ctx, `SELECT id::text, password_hash, failed_logins, locked_until FROM users WHERE lower(name)=lower($1) FOR UPDATE`, name).
		Scan(&id, &enc, &failed, &locked); err != nil {
		checkPassword(password, dummyHash)
		return ErrInvalidCredentials
	}
	now := s.Now()
	if enc == "" {
		checkPassword(password, dummyHash)
		return ErrInvalidCredentials
	}
	if locked != nil && locked.After(now) {
		return ErrLocked
	}
	if checkPassword(password, enc) {
		return nil
	}
	failed++
	var until *time.Time
	if d := lockFor(failed); d > 0 {
		t := now.Add(d)
		until = &t
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET failed_logins=$2, locked_until=$3 WHERE id::text=$1`, id, failed, until); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return ErrInvalidCredentials
}
