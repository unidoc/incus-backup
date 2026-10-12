package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/chacha20poly1305"
)

// The secret store holds INFRASTRUCTURE credentials the server needs to do its job (today:
// S3 access keys). It has nothing to do with backup encryption: payload keys exist only on
// clients, and the server can never decrypt a backup whatever is in here.
//
// PostgreSQL stores only ciphertext (credentials.sealed). The sealing key is a random
// 32-byte file (`secrets.key`, mode 0600) in the server state directory, created on first
// use. Stealing the database alone yields no credential; a secret, once stored, is never
// returned by any API: it can be replaced (rotate) or deleted, not read back.

const sealVersion = 1

// sealingKey returns the key that seals credentials. It is CREATED only when asked to (create) AND no
// credential exists yet: if secrets.key was lost, every stored credential is unreadable, and silently
// minting a new key (even from a read-only command) would hide that.
func (s *Service) sealingKey(create bool) ([]byte, error) {
	p := s.stateFile("secrets.key")
	b, err := os.ReadFile(p)
	if err == nil {
		if len(b) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("%s is corrupt", p)
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !create {
		return nil, fmt.Errorf("%s is missing: the stored credentials cannot be opened (restore it from your backup of the state directory, or re-create the credentials)", p)
	}
	var n int
	if qerr := s.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM credentials`).Scan(&n); qerr != nil {
		return nil, qerr
	}
	if n > 0 {
		return nil, fmt.Errorf("%s is missing although %d credential(s) are stored: refusing to create a new key (restore the old one)", p, n)
	}
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return nil, err
	}
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.sealingKey(false) // lost a creation race
		}
		return nil, err
	}
	if _, err := f.Write(k); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	return k, f.Close()
}

func (s *Service) seal(id string, plain []byte) ([]byte, error) {
	k, err := s.sealingKey(true)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{sealVersion}, nonce...)
	return aead.Seal(out, nonce, plain, []byte(id)), nil // bound to the credential id
}

func (s *Service) unseal(id string, sealed []byte) ([]byte, error) {
	k, err := s.sealingKey(false)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 1+aead.NonceSize()+aead.Overhead() || sealed[0] != sealVersion {
		return nil, errors.New("credential: bad sealed data")
	}
	nonce := sealed[1 : 1+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[1+aead.NonceSize():], []byte(id))
	if err != nil {
		return nil, errors.New("credential cannot be unsealed (wrong secrets.key for this database?)")
	}
	return plain, nil
}

// S3Secret is the content of an S3 credential.
type S3Secret struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// Credential is what the API may show about a stored credential: never the secret.
type Credential struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	AccessKey string     `json:"access_key"` // the access key id is not secret and helps operators tell credentials apart
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at"`
	UsedBy    []string   `json:"used_by"` // repositories
}

// CreateS3Credential stores a new S3 credential.
func (s *Service) CreateS3Credential(ctx context.Context, name string, sec S3Secret) (*Credential, error) {
	if !repoNameRe.MatchString(name) {
		return nil, bad("invalid credential name")
	}
	if sec.AccessKey == "" || sec.SecretKey == "" {
		return nil, bad("access key and secret key are required")
	}
	var id string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(sec)
	sealed, err := s.seal(id, raw)
	if err != nil {
		return nil, err
	}
	var c Credential
	err = s.DB.Pool().QueryRow(ctx, `INSERT INTO credentials (id, name, kind, sealed) VALUES ($1::uuid,$2,'s3',$3) RETURNING id::text, name, kind, created_at`,
		id, name, sealed).Scan(&c.ID, &c.Name, &c.Kind, &c.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("storing credential: %w", err)
	}
	c.AccessKey = sec.AccessKey
	return &c, nil
}

// RotateS3Credential replaces the secret of an existing credential (repositories using it pick it up on next open).
func (s *Service) RotateS3Credential(ctx context.Context, name string, sec S3Secret) error {
	// only the process that serves the repositories may swap the credentials of its open stores
	release, gerr := s.guard(ctx)
	if gerr != nil {
		return gerr
	}
	defer release()
	if sec.AccessKey == "" || sec.SecretKey == "" {
		return bad("access key and secret key are required")
	}
	var id string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT id::text FROM credentials WHERE name=$1 AND kind='s3'`, name).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: credential %q", ErrNotFound, name)
		}
		return err
	}
	raw, _ := json.Marshal(sec)
	sealed, err := s.seal(id, raw)
	if err != nil {
		return err
	}
	if _, err := s.DB.Pool().Exec(ctx, `UPDATE credentials SET sealed=$2, rotated_at=now() WHERE id=$1::uuid`, id, sealed); err != nil {
		return err
	}
	s.rebindStores(ctx, id, sec) // running sessions keep their Store (and its pins); only the credentials change
	return nil
}

// Credentials lists credentials without secrets.
func (s *Service) Credentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT c.id::text, c.name, c.kind, c.sealed, c.created_at, c.rotated_at,
		COALESCE((SELECT array_agg(r.name ORDER BY r.name) FROM repositories r WHERE r.credential_id=c.id), '{}')
		FROM credentials c ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		var sealed []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &sealed, &c.CreatedAt, &c.RotatedAt, &c.UsedBy); err != nil {
			return nil, err
		}
		if c.Kind != "s3" { // generic secrets (secret_generic.go) have no public part
			out = append(out, c)
			continue
		}
		if raw, err := s.unseal(c.ID, sealed); err == nil {
			var sec S3Secret
			if json.Unmarshal(raw, &sec) == nil {
				c.AccessKey = sec.AccessKey
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCredential removes an unused credential.
func (s *Service) DeleteCredential(ctx context.Context, name string) error {
	ct, err := s.DB.Pool().Exec(ctx, `DELETE FROM credentials c WHERE c.name=$1 AND c.kind='s3' AND NOT EXISTS (SELECT 1 FROM repositories r WHERE r.credential_id=c.id)`, name)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: credential %q does not exist or is still used by a repository", ErrConflict, name)
	}
	return nil
}

// credentialSecret returns the S3 secret of a credential id (internal use only).
func (s *Service) credentialSecret(ctx context.Context, id string) (*S3Secret, error) {
	var sealed []byte
	if err := s.DB.Pool().QueryRow(ctx, `SELECT sealed FROM credentials WHERE id=$1::uuid AND kind='s3'`, id).Scan(&sealed); err != nil {
		return nil, err
	}
	raw, err := s.unseal(id, sealed)
	if err != nil {
		return nil, err
	}
	var sec S3Secret
	if err := json.Unmarshal(raw, &sec); err != nil {
		return nil, err
	}
	return &sec, nil
}
