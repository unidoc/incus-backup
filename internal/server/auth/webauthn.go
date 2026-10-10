package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
)

// Passkeys and security keys (WebAuthn, docs/authentication.md), on github.com/go-webauthn/webauthn.
//
//   - Registration: a signed-in user (who signed in within ReauthWindow, or confirms the password)
//     registers an authenticator. Discoverable credentials are preferred, so the same key can later
//     sign in without a user name.
//   - Second factor: after a correct password Login returns a ticket; the ticket starts an assertion
//     limited to that user's credentials. A security key satisfies the second factor like TOTP.
//   - Passkey sign-in: a discoverable assertion with user verification REQUIRED; the begin answer names
//     no user (no enumeration), the user is found by the credential's user handle.
//
// Every ceremony's state (go-webauthn SessionData) is kept server-side in auth_ceremonies for
// TicketTTL and is deleted when finish starts, whatever the outcome (single use). Signature counters
// that do not increase mark the credential as possibly cloned: it is refused from then on.

// ReauthWindow: sensitive changes to one's own sign-in methods need a sign-in this recent, or the password.
const ReauthWindow = 10 * time.Minute

// More errors.
var (
	// ErrReauth: the change needs the current password (the session is older than ReauthWindow).
	ErrReauth = errors.New("confirm your password to change how you sign in")
	// ErrCloned: an authenticator's signature counter went backwards; it is blocked.
	ErrCloned = errors.New("this security key reported a signature counter that is not higher than before, so it may have been copied: it is blocked; sign in another way and remove it, or ask an administrator")
	// ErrLastFactor: the sign-in policy requires a second factor and this is the last one.
	ErrLastFactor = errors.New("the sign-in policy requires a second factor for your account and this is your last one: add another (a security key or an authenticator app) before removing it")
)

// CeremonyStart is a begin answer: the ceremony id the finish call must send back, and the options for
// navigator.credentials.create() / get() ({"publicKey": {...}} with base64url binary fields).
type CeremonyStart struct {
	Ceremony string `json:"ceremony"`
	Options  any    `json:"options"`
}

// WebAuthnKey is a registered authenticator as listed (never the public key material).
type WebAuthnKey struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsed       *time.Time `json:"last_used_at,omitempty"`
	SignCount      int64      `json:"sign_count"`
	Transports     []string   `json:"transports"`
	Attachment     string     `json:"attachment"`
	UserVerified   bool       `json:"user_verified"`
	BackupEligible bool       `json:"backup_eligible"`
	BackupState    bool       `json:"backup_state"`
	Discoverable   *bool      `json:"discoverable,omitempty"`
	CloneWarning   bool       `json:"clone_warning"`
}

// waUser is a user as go-webauthn sees it.
type waUser struct {
	id, name, display string
	handle            []byte
	creds             []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.handle }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.display }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// relyingParty builds the go-webauthn configuration from the settings (per request: settings are data).
func (s *Store) relyingParty(ctx context.Context, origin string) (*webauthn.WebAuthn, error) {
	rpID, origins, err := s.WebAuthnRP(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBad, err)
	}
	if origin != "" {
		ok := false
		for _, o := range origins {
			ok = ok || strings.EqualFold(o, origin)
		}
		if !ok {
			return nil, fmt.Errorf("%w: this page is open at %s, but passkeys are set up for %s: open the console at that address, or add this one to the passkey origins (Settings → Authentication)", ErrBad, origin, strings.Join(origins, ", "))
		}
	}
	name := settingIn(ctx, s.Pool, "instance_name")
	if name == "" {
		name = "incus-backup"
	}
	return webauthn.New(&webauthn.Config{
		RPID:                  rpID,
		RPDisplayName:         name,
		RPOrigins:             origins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
	})
}

func (s *Store) newCeremony(ctx context.Context, tx pgx.Tx, kind, userID string, data any, now time.Time) (string, error) {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	id := randToken(32)
	_, err = tx.Exec(ctx, `INSERT INTO auth_ceremonies (id_hash, kind, user_id, data, created_at, expires_at) VALUES ($1,$2,NULLIF($3,'')::uuid,$4,$5,$6)`,
		hashToken(id), kind, userID, b, now, now.Add(TicketTTL))
	return id, err
}

// takeCeremony deletes and returns an unexpired ceremony of kind (single use: the delete commits
// before the response is checked, so a failed attempt cannot be retried with the same challenge).
func (s *Store) takeCeremony(ctx context.Context, id string, kinds ...string) (kind, userID string, data []byte, err error) {
	err = s.Pool.QueryRow(ctx, `DELETE FROM auth_ceremonies WHERE id_hash=$1 AND kind = ANY($2) AND expires_at > $3 RETURNING kind, COALESCE(user_id::text,''), data`,
		hashToken(id), kinds, s.Now()).Scan(&kind, &userID, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, fmt.Errorf("%w: this request expired or was already used: start again", ErrBad)
	}
	return kind, userID, data, err
}

func loadCredentials(ctx context.Context, q querier, userID string) ([]webauthn.Credential, error) {
	rows, err := q.Query(ctx, `SELECT credential_id, public_key, attestation_type, attestation_format, aaguid, sign_count, clone_warning, attachment, transports,
		user_present, user_verified, backup_eligible, backup_state FROM webauthn_credentials WHERE user_id::text=$1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var c webauthn.Credential
		var count int64
		var attach string
		var tr []string
		if err := rows.Scan(&c.ID, &c.PublicKey, &c.AttestationType, &c.AttestationFormat, &c.Authenticator.AAGUID, &count, &c.Authenticator.CloneWarning, &attach, &tr,
			&c.Flags.UserPresent, &c.Flags.UserVerified, &c.Flags.BackupEligible, &c.Flags.BackupState); err != nil {
			return nil, err
		}
		c.Authenticator.SignCount = uint32(count)
		c.Authenticator.Attachment = protocol.AuthenticatorAttachment(attach)
		for _, t := range tr {
			c.Transport = append(c.Transport, protocol.AuthenticatorTransport(t))
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// loadWAUser reads a user for a ceremony inside tx (row locked). The handle is created when missing.
func (s *Store) loadWAUser(ctx context.Context, tx pgx.Tx, where string, arg any) (*waUser, string, bool, *time.Time, int, error) {
	u := &waUser{}
	var enc string
	var disabled bool
	var locked *time.Time
	var failed int
	err := tx.QueryRow(ctx, `SELECT id::text, name, display_name, password_hash, webauthn_handle, disabled, locked_until, failed_logins FROM users WHERE `+where+` FOR UPDATE`, arg).
		Scan(&u.id, &u.name, &u.display, &enc, &u.handle, &disabled, &locked, &failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, "", false, nil, 0, err
	}
	if u.display == "" {
		u.display = u.name
	}
	if u.creds, err = loadCredentials(ctx, tx, u.id); err != nil {
		return nil, "", false, nil, 0, err
	}
	return u, enc, disabled, locked, failed, nil
}

func validKeyName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" {
		n = "Security key"
	}
	if utf8.RuneCountInString(n) > 64 || strings.IndexFunc(n, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: a key name is 1-64 characters on one line", ErrBad)
	}
	return n, nil
}

// RecentlyAuthenticated reports whether the session (cookie value) was opened within ReauthWindow.
func (s *Store) RecentlyAuthenticated(ctx context.Context, token string) bool {
	var created time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT created_at FROM user_sessions WHERE token_hash=$1`, hashToken(token)).Scan(&created); err != nil {
		return false
	}
	return s.Now().Sub(created) <= ReauthWindow
}

// Reauthenticate is the gate in front of changes to one's own sign-in methods: a recent sign-in, or
// the current password (VerifyPassword: wrong guesses count against the account).
func (s *Store) Reauthenticate(ctx context.Context, name, token, password string) error {
	if s.RecentlyAuthenticated(ctx, token) {
		return nil
	}
	if password == "" {
		return ErrReauth
	}
	return s.VerifyPassword(ctx, name, password)
}

// BeginWebAuthnRegistration starts registering a new authenticator for user name. origin is the
// request's Origin header ("" = not checked here; the browser's clientData is checked at finish).
func (s *Store) BeginWebAuthnRegistration(ctx context.Context, name, origin string) (*CeremonyStart, error) {
	wa, err := s.relyingParty(ctx, origin)
	if err != nil {
		return nil, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	u, enc, disabled, _, _, err := s.loadWAUser(ctx, tx, `lower(name)=lower($1)`, name)
	if err != nil {
		return nil, err
	}
	if disabled {
		return nil, ErrNoSession
	}
	if enc == "" {
		return nil, fmt.Errorf("%w: this account signs in through single sign-on; its second factor is the identity provider's", ErrBad)
	}
	if len(u.handle) == 0 {
		u.handle = make([]byte, 32)
		if _, err := rand.Read(u.handle); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET webauthn_handle=$2 WHERE id::text=$1`, u.id, u.handle); err != nil {
			return nil, err
		}
	}
	creation, sd, err := wa.BeginRegistration(u,
		webauthn.WithExclusions(webauthn.Credentials(u.creds).CredentialDescriptors()),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
		webauthn.WithExtensions(webauthn.WithExtensionCredProps()),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBad, err)
	}
	id, err := s.newCeremony(ctx, tx, "register", u.id, sd, s.Now())
	if err != nil {
		return nil, err
	}
	return &CeremonyStart{Ceremony: id, Options: creation}, tx.Commit(ctx)
}

// FinishWebAuthnRegistration verifies the browser's attestation and stores the credential.
func (s *Store) FinishWebAuthnRegistration(ctx context.Context, name, ceremony, keyName string, credential json.RawMessage) (*WebAuthnKey, error) {
	keyName, err := validKeyName(keyName)
	if err != nil {
		return nil, err
	}
	_, owner, data, err := s.takeCeremony(ctx, ceremony, "register")
	if err != nil {
		return nil, err
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(data, &sd); err != nil {
		return nil, err
	}
	wa, err := s.relyingParty(ctx, "")
	if err != nil {
		return nil, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	u, _, _, _, _, err := s.loadWAUser(ctx, tx, `lower(name)=lower($1)`, name)
	if err != nil {
		return nil, err
	}
	if u.id != owner {
		return nil, fmt.Errorf("%w: this registration was started by another user", ErrBad)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credential)
	if err != nil {
		return nil, fmt.Errorf("%w: the browser's answer could not be read: %v", ErrBad, protoErr(err))
	}
	cred, err := wa.CreateCredential(u, sd, parsed)
	if err != nil {
		return nil, fmt.Errorf("%w: the security key's answer was not accepted: %v", ErrBad, protoErr(err))
	}
	tr := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		tr = append(tr, string(t))
	}
	var k WebAuthnKey
	err = tx.QueryRow(ctx, `INSERT INTO webauthn_credentials (user_id, credential_id, public_key, attestation_type, attestation_format, aaguid, sign_count, attachment, transports,
			user_present, user_verified, backup_eligible, backup_state, discoverable, name, created_at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id::text`,
		u.id, cred.ID, cred.PublicKey, cred.AttestationType, cred.AttestationFormat, nonNil(cred.Authenticator.AAGUID), int64(cred.Authenticator.SignCount),
		string(cred.Authenticator.Attachment), tr, cred.Flags.UserPresent, cred.Flags.UserVerified, cred.Flags.BackupEligible, cred.Flags.BackupState,
		cred.Extensions.RK, keyName, s.Now()).Scan(&k.ID)
	if err != nil {
		if strings.Contains(err.Error(), "webauthn_credentials_credential_id_key") {
			return nil, fmt.Errorf("%w: this security key is already registered", ErrExists)
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	keys, err := s.WebAuthnKeys(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, x := range keys {
		if x.ID == k.ID {
			return &x, nil
		}
	}
	return nil, ErrNotFound
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// protoErr returns go-webauthn's detailed reason (its Error() is only the generic type).
func protoErr(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		if pe.Details != "" {
			return pe.Details
		}
		return pe.Type
	}
	return err.Error()
}

// BeginWebAuthnSecondFactor starts the second factor for the user whose password Login just verified
// (ticket from SecondFactorNeeded). The ticket stays valid until a second factor succeeds or it expires.
func (s *Store) BeginWebAuthnSecondFactor(ctx context.Context, ticket, origin string) (*CeremonyStart, error) {
	if pol, err := s.Policy(ctx); err != nil {
		return nil, err
	} else if !pol.LocalLogin {
		return nil, ErrLocalLoginOff
	}
	wa, err := s.relyingParty(ctx, origin)
	if err != nil {
		return nil, err
	}
	var userID string
	if err := s.Pool.QueryRow(ctx, `SELECT user_id::text FROM auth_ceremonies WHERE id_hash=$1 AND kind='pending_2fa' AND expires_at > $2`, hashToken(ticket), s.Now()).Scan(&userID); err != nil {
		return nil, fmt.Errorf("%w: sign in with your password again", ErrInvalidCredentials)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	u, _, _, _, _, err := s.loadWAUser(ctx, tx, `id::text=$1`, userID)
	if err != nil {
		return nil, err
	}
	if len(u.creds) == 0 {
		return nil, fmt.Errorf("%w: no security key is registered for this account: use your authenticator code", ErrBad)
	}
	assertion, sd, err := wa.BeginLogin(u)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBad, err)
	}
	id, err := s.newCeremony(ctx, tx, "second_factor", u.id, map[string]any{"session": sd, "ticket": hex.EncodeToString(hashToken(ticket))}, s.Now())
	if err != nil {
		return nil, err
	}
	return &CeremonyStart{Ceremony: id, Options: assertion}, tx.Commit(ctx)
}

// FinishWebAuthnLogin ends a passkey sign-in or a security-key second factor (the ceremony says which:
// method "passkey" or "webauthn") and opens the session. user names the account for the audit log
// when it is known, also on failure. A wrong assertion counts as a failed sign-in of that account (the
// same growing lock as a wrong password or code).
func (s *Store) FinishWebAuthnLogin(ctx context.Context, ceremony string, credential json.RawMessage, ip, ua string) (sess *Session, user, method string, err error) {
	kind, owner, data, err := s.takeCeremony(ctx, ceremony, "second_factor", "passkey")
	if err != nil {
		return nil, "", "", err
	}
	if kind == "passkey" {
		sess, user, err = s.finishPasskey(ctx, data, credential, ip, ua)
		return sess, user, "passkey", err
	}
	_ = s.Pool.QueryRow(ctx, `SELECT name FROM users WHERE id::text=$1`, owner).Scan(&user)
	sess, err = s.finishSecondFactor(ctx, owner, data, credential, ip, ua)
	return sess, user, "webauthn", err
}

func (s *Store) finishSecondFactor(ctx context.Context, owner string, data []byte, credential json.RawMessage, ip, ua string) (*Session, error) {
	var st struct {
		Session webauthn.SessionData `json:"session"`
		Ticket  string               `json:"ticket"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	ticketHash, err := hex.DecodeString(st.Ticket)
	if err != nil {
		return nil, err
	}
	return s.finishAssertion(ctx, `id::text=$1`, owner, credential, ip, ua, "password", func(wa *webauthn.WebAuthn, u *waUser, parsed *protocol.ParsedCredentialAssertionData) (*webauthn.Credential, error) {
		return wa.ValidateLogin(u, st.Session, parsed)
	}, func(tx pgx.Tx, userID string) error {
		tag, err := tx.Exec(ctx, `DELETE FROM auth_ceremonies WHERE id_hash=$1 AND kind='pending_2fa' AND user_id::text=$2 AND expires_at > $3`, ticketHash, userID, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: sign in with your password again", ErrInvalidCredentials)
		}
		return nil
	})
}

// BeginPasskeyLogin starts a sign-in without a user name (discoverable credential, user verification
// required). The answer is the same for everybody: no user is named, so nothing can be enumerated.
func (s *Store) BeginPasskeyLogin(ctx context.Context, origin string) (*CeremonyStart, error) {
	if pol, err := s.Policy(ctx); err != nil {
		return nil, err
	} else if !pol.LocalLogin {
		return nil, ErrLocalLoginOff
	}
	wa, err := s.relyingParty(ctx, origin)
	if err != nil {
		return nil, err
	}
	var open int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM auth_ceremonies WHERE kind='passkey' AND expires_at > $1`, s.Now()).Scan(&open); err != nil {
		return nil, err
	}
	if open > 10000 { // anonymous callers cannot grow the table without bound (the IP throttle comes first)
		return nil, fmt.Errorf("%w: too many sign-ins in progress: try again in a few minutes", ErrLocked)
	}
	assertion, sd, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBad, err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	id, err := s.newCeremony(ctx, tx, "passkey", "", sd, s.Now())
	if err != nil {
		return nil, err
	}
	return &CeremonyStart{Ceremony: id, Options: assertion}, tx.Commit(ctx)
}

// finishPasskey checks a discoverable assertion: the user is the owner of the credential's user handle
// ("" when unknown: nothing to count against).
func (s *Store) finishPasskey(ctx context.Context, data []byte, credential json.RawMessage, ip, ua string) (*Session, string, error) {
	if pol, err := s.Policy(ctx); err != nil {
		return nil, "", err
	} else if !pol.LocalLogin {
		return nil, "", ErrLocalLoginOff
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(data, &sd); err != nil {
		return nil, "", err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		return nil, "", fmt.Errorf("%w: the browser's answer could not be read: %v", ErrBad, protoErr(err))
	}
	var userID, name string
	if len(parsed.Response.UserHandle) == 0 || s.Pool.QueryRow(ctx, `SELECT id::text, name FROM users WHERE webauthn_handle=$1`, []byte(parsed.Response.UserHandle)).Scan(&userID, &name) != nil {
		return nil, "", ErrInvalidCredentials
	}
	sess, err := s.finishAssertion(ctx, `id::text=$1`, userID, credential, ip, ua, "passkey", func(wa *webauthn.WebAuthn, u *waUser, p *protocol.ParsedCredentialAssertionData) (*webauthn.Credential, error) {
		_, c, err := wa.ValidatePasskeyLogin(func(rawID, handle []byte) (webauthn.User, error) {
			if !bytes.Equal(handle, u.handle) {
				return nil, errors.New("user handle changed")
			}
			return u, nil
		}, sd, p)
		return c, err
	}, nil)
	return sess, name, err
}

// finishAssertion is the common end of both assertion ceremonies: lock the user, refuse disabled,
// locked and cloned, validate, record the counter (or the failure), open the session.
func (s *Store) finishAssertion(ctx context.Context, where, userID string, credential json.RawMessage, ip, ua, method string,
	validate func(*webauthn.WebAuthn, *waUser, *protocol.ParsedCredentialAssertionData) (*webauthn.Credential, error),
	before func(pgx.Tx, string) error) (*Session, error) {
	wa, err := s.relyingParty(ctx, "")
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		return nil, fmt.Errorf("%w: the browser's answer could not be read: %v", ErrBad, protoErr(err))
	}
	now := s.Now()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	u, _, disabled, locked, failed, err := s.loadWAUser(ctx, tx, where, userID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if locked != nil && locked.After(now) {
		return nil, ErrLocked
	}
	if disabled {
		return nil, s.countFailureErr(ctx, tx, u.id, failed, now, ErrInvalidCredentials)
	}
	for _, c := range u.creds {
		if bytes.Equal(c.ID, parsed.RawID) && c.Authenticator.CloneWarning {
			return nil, s.countFailureErr(ctx, tx, u.id, failed, now, ErrCloned)
		}
	}
	cred, err := validate(wa, u, parsed)
	if err != nil {
		return nil, s.countFailureErr(ctx, tx, u.id, failed, now, fmt.Errorf("%w (%s)", ErrInvalidCredentials, protoErr(err)))
	}
	if cred.Authenticator.CloneWarning { // go-webauthn flags a non-increasing counter but does not fail
		if _, err := tx.Exec(ctx, `UPDATE webauthn_credentials SET clone_warning=true WHERE credential_id=$1`, cred.ID); err != nil {
			return nil, err
		}
		return nil, s.countFailureErr(ctx, tx, u.id, failed, now, ErrCloned)
	}
	if before != nil {
		if err := before(tx, u.id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE webauthn_credentials SET sign_count=$2, last_used_at=$3, user_verified=$4, backup_state=$5 WHERE credential_id=$1`,
		cred.ID, int64(cred.Authenticator.SignCount), now, cred.Flags.UserVerified, cred.Flags.BackupState); err != nil {
		return nil, err
	}
	sess, err := s.openSession(ctx, tx, u.id, method, ip, ua, now)
	if err != nil {
		return nil, err
	}
	return sess, tx.Commit(ctx)
}

func (s *Store) countFailureErr(ctx context.Context, tx pgx.Tx, id string, failed int, now time.Time, reason error) error {
	if err := s.countFailure(ctx, tx, id, failed, now); err != nil {
		return err
	}
	return reason
}

// WebAuthnKeys lists a user's authenticators.
func (s *Store) WebAuthnKeys(ctx context.Context, name string) ([]WebAuthnKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT w.id::text, w.name, w.created_at, w.last_used_at, w.sign_count, w.transports, w.attachment, w.user_verified,
			w.backup_eligible, w.backup_state, w.discoverable, w.clone_warning
		FROM webauthn_credentials w JOIN users u ON u.id=w.user_id WHERE lower(u.name)=lower($1) ORDER BY w.created_at`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebAuthnKey{}
	for rows.Next() {
		var k WebAuthnKey
		if err := rows.Scan(&k.ID, &k.Name, &k.CreatedAt, &k.LastUsed, &k.SignCount, &k.Transports, &k.Attachment, &k.UserVerified,
			&k.BackupEligible, &k.BackupState, &k.Discoverable, &k.CloneWarning); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RenameWebAuthnKey renames one of a user's authenticators.
func (s *Store) RenameWebAuthnKey(ctx context.Context, name, id, keyName string) error {
	keyName, err := validKeyName(keyName)
	if err != nil {
		return err
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE webauthn_credentials SET name=$3 WHERE id::text=$2 AND user_id=(SELECT id FROM users WHERE lower(name)=lower($1))`, name, id, keyName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// lockFactors locks the user row and reports what the last-factor guard needs.
func lockFactors(ctx context.Context, tx pgx.Tx, name string) (id string, covered, totp bool, keys int, err error) {
	var role, pol string
	err = tx.QueryRow(ctx, `SELECT id::text, role, totp_enabled, (SELECT count(*) FROM webauthn_credentials w WHERE w.user_id=users.id),
			COALESCE((SELECT value FROM server_settings WHERE key='require_2fa'), '')
		FROM users WHERE lower(name)=lower($1) FOR UPDATE`, name).Scan(&id, &role, &totp, &keys, &pol)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, 0, ErrNotFound
	}
	if err != nil {
		return
	}
	return id, policyCovers(pol, Role(role)), totp, keys, nil
}

// DeleteWebAuthnKey removes one of the user's own authenticators. The last second factor of a user the
// policy covers cannot be removed by the user (an administrator can reset it: ResetWebAuthn).
func (s *Store) DeleteWebAuthnKey(ctx context.Context, name, id string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	uid, covered, totp, keys, err := lockFactors(ctx, tx, name)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM webauthn_credentials WHERE id::text=$2 AND user_id::text=$1`, uid, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if covered && !totp && keys <= 1 {
		return fmt.Errorf("%w: %v", ErrBad, ErrLastFactor)
	}
	return tx.Commit(ctx)
}

// DisableOwnTOTP removes the user's own TOTP enrolment, unless it is the last second factor the policy
// requires (DisableTOTP is the administrator's reset and has no such guard).
func (s *Store) DisableOwnTOTP(ctx context.Context, name string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	uid, covered, totp, keys, err := lockFactors(ctx, tx, name)
	if err != nil {
		return err
	}
	if covered && totp && keys == 0 {
		return fmt.Errorf("%w: %v", ErrBad, ErrLastFactor)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET totp_enabled=false, totp_sealed=NULL, totp_last_step=0, recovery_hashes='{}' WHERE id::text=$1`, uid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResetWebAuthn removes every authenticator of a user (an administrator's reset of a lost key, or the
// CLI `user webauthn-reset`). It returns how many were removed.
func (s *Store) ResetWebAuthn(ctx context.Context, name string) (int, error) {
	var id string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE lower(name)=lower($1)`, name).Scan(&id); err != nil {
		return 0, ErrNotFound
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM webauthn_credentials WHERE user_id::text=$1`, id)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
