package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Generic sealed secrets (SMTP passwords, API tokens, webhook URLs and signing secrets) in the same
// store as the S3 credentials: sealed with secrets.key, bound to the credential id, never returned by
// any API. ReadSecret exists for the server's own use (e.g. the notification engine sending a mail);
// no HTTP, admin-socket or CLI route may ever hand its result out.

// Secret kinds accepted by CreateSecret (the credentials.kind CHECK).
const (
	SecretSMTP    = "smtp"
	SecretToken   = "token"
	SecretWebhook = "webhook"
	SecretOIDC    = "oidc" // OpenID Connect client secrets (docs/authentication.md)
)

func genericKind(kind string) bool {
	return kind == SecretSMTP || kind == SecretToken || kind == SecretWebhook || kind == SecretOIDC
}

// CreateSecret seals plain under a new credential and returns its id.
func (s *Service) CreateSecret(ctx context.Context, name, kind string, plain []byte) (string, error) {
	if !repoNameRe.MatchString(name) {
		return "", bad("invalid secret name")
	}
	if !genericKind(kind) {
		return "", bad("invalid secret kind %q", kind)
	}
	if len(plain) == 0 {
		return "", bad("empty secret")
	}
	var id string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return "", err
	}
	sealed, err := s.seal(id, plain)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.Pool().Exec(ctx, `INSERT INTO credentials (id, name, kind, sealed, created_at) VALUES ($1::uuid,$2,$3,$4,$5)`,
		id, name, kind, sealed, s.Now()); err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: a secret named %q exists", ErrConflict, name)
		}
		return "", fmt.Errorf("storing secret: %w", err)
	}
	return id, nil
}

// ReadSecret unseals a generic secret by id. INTERNAL USE ONLY: never expose the result through an API.
func (s *Service) ReadSecret(ctx context.Context, id string) ([]byte, error) {
	var sealed []byte
	var kind string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT kind, sealed FROM credentials WHERE id=$1::uuid`, id).Scan(&kind, &sealed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: secret", ErrNotFound)
		}
		return nil, err
	}
	if !genericKind(kind) {
		return nil, fmt.Errorf("%w: credential is not a generic secret", ErrForbidden)
	}
	return s.unseal(id, sealed)
}

// RotateSecret replaces the sealed value of a generic secret.
func (s *Service) RotateSecret(ctx context.Context, id string, plain []byte) error {
	if len(plain) == 0 {
		return bad("empty secret")
	}
	sealed, err := s.seal(id, plain)
	if err != nil {
		return err
	}
	ct, err := s.DB.Pool().Exec(ctx, `UPDATE credentials SET sealed=$2, rotated_at=$3 WHERE id=$1::uuid AND kind IN ('smtp','token','webhook','oidc')`, id, sealed, s.Now())
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: secret", ErrNotFound)
	}
	return nil
}

// DeleteSecret removes a generic secret (fails while something still references it).
func (s *Service) DeleteSecret(ctx context.Context, id string) error {
	ct, err := s.DB.Pool().Exec(ctx, `DELETE FROM credentials WHERE id=$1::uuid AND kind IN ('smtp','token','webhook','oidc')`, id)
	if err != nil {
		return fmt.Errorf("%w: secret still in use", ErrConflict)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: secret", ErrNotFound)
	}
	return nil
}
