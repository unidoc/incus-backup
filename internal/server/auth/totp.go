package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; authenticator apps use HMAC-SHA1
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	totpPeriod = 30
	totpDigits = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpCode(secret []byte, step int64) string {
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(c[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%0*d", totpDigits, v)
}

// verifyTOTP accepts the current step and one either side (clock skew), never a step at or before the
// last one accepted, so an observed code cannot be replayed.
func verifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	found := int64(0)
	for d := int64(-1); d <= 1; d++ {
		st := cur + d
		if subtle.ConstantTimeCompare([]byte(code), []byte(totpCode(secret, st))) == 1 && st > lastStep {
			found = st
		}
	}
	return found, found != 0
}

func newRecoveryCodes() (plain, hashed []string) {
	for i := 0; i < 10; i++ {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		s := strings.ToLower(b32.EncodeToString(b)) // 10 characters
		c := s[:5] + "-" + s[5:]
		plain = append(plain, c)
		hashed = append(hashed, hashPassword(c))
	}
	return
}

func useRecovery(hashes []string, code string) ([]string, bool) {
	code = strings.ToLower(strings.TrimSpace(code))
	if len(code) != 11 || code[5] != '-' {
		return nil, false
	}
	for i, h := range hashes {
		if checkPassword(code, h) {
			rest := append(append([]string{}, hashes[:i]...), hashes[i+1:]...)
			return rest, true
		}
	}
	return nil, false
}

// TOTPEnrolment is what the user scans or types into an authenticator app.
type TOTPEnrolment struct {
	Secret string `json:"secret"` // base32
	URI    string `json:"uri"`    // otpauth://
}

// BeginTOTP generates a new seed for a user. It is stored sealed but not active until ConfirmTOTP proves the
// user's app produces matching codes.
func (s *Store) BeginTOTP(ctx context.Context, name string) (*TOTPEnrolment, error) {
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	sealed, err := s.Sealer.Seal(secret)
	if err != nil {
		return nil, err
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE users SET totp_sealed=$2, totp_enabled=false, totp_last_step=0 WHERE lower(name)=lower($1) AND NOT totp_enabled`, name, sealed)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("%w: two-factor is already enabled (disable it first) or the user does not exist", ErrBad)
	}
	enc := b32.EncodeToString(secret)
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + s.Issuer + ":" + name}
	q := url.Values{"secret": {enc}, "issuer": {s.Issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	u.RawQuery = q.Encode()
	return &TOTPEnrolment{Secret: enc, URI: u.String()}, nil
}

// ConfirmTOTP activates two-factor when code is valid and returns the recovery codes (shown once).
func (s *Store) ConfirmTOTP(ctx context.Context, name, code string) ([]string, error) {
	var sealed []byte
	var on bool
	if err := s.Pool.QueryRow(ctx, `SELECT totp_sealed, totp_enabled FROM users WHERE lower(name)=lower($1)`, name).Scan(&sealed, &on); err != nil {
		return nil, ErrNotFound
	}
	if on || sealed == nil {
		return nil, fmt.Errorf("%w: no two-factor enrolment in progress", ErrBad)
	}
	secret, err := s.Sealer.Open(sealed)
	if err != nil {
		return nil, err
	}
	step, ok := verifyTOTP(secret, code, s.Now(), 0)
	if !ok {
		return nil, errors.New("the code does not match: check the time on your device and try again")
	}
	plain, hashed := newRecoveryCodes()
	if _, err := s.Pool.Exec(ctx, `UPDATE users SET totp_enabled=true, totp_last_step=$2, recovery_hashes=$3 WHERE lower(name)=lower($1)`, name, step, hashed); err != nil {
		return nil, err
	}
	return plain, nil
}

// DisableTOTP removes two-factor from a user (an administrator resetting a lost device, or the user
// themselves after confirming their password in the caller).
func (s *Store) DisableTOTP(ctx context.Context, name string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE users SET totp_enabled=false, totp_sealed=NULL, totp_last_step=0, recovery_hashes='{}' WHERE lower(name)=lower($1)`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
