package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
)

// PutEscrow stores the host's wrapped master key (opaque to the server). Only the first upload
// for a key domain is accepted: replacing it is an administrator's decision (ClearEscrow).
func (s *Service) PutEscrow(ctx context.Context, h *Host, domain string, blob []byte) error {
	if err := requireScope(h, api.ScopeBackup); err != nil {
		return err
	}
	repo, err := s.RepositoryByID(ctx, h.RepoID)
	if err != nil {
		return err
	}
	if err := s.pinDomain(ctx, repo.ID, domain); err != nil {
		return err
	}
	if wd, err := format.WrappedDomain(blob); err != nil || wd != domain {
		return bad("the blob is not an escrow blob for key domain %s", domain)
	}
	st, err := s.openStore(ctx, repo)
	if err != nil {
		return err
	}
	switch err := st.PutEscrow(domain, blob); {
	case errors.Is(err, datastore.ErrExists):
		return fmt.Errorf("%w: an escrow blob for this key domain already exists (an administrator must clear it first)", ErrConflict)
	case err != nil:
		return bad("%v", err)
	}
	return nil
}

// GetEscrow returns the wrapped master key of the repository's key domain.
func (s *Service) GetEscrow(ctx context.Context, h *Host) ([]byte, error) {
	if err := requireScope(h, api.ScopeBackup); err != nil {
		return nil, err
	}
	repo, err := s.RepositoryByID(ctx, h.RepoID)
	if err != nil {
		return nil, err
	}
	if repo.DomainID == "" {
		return nil, fmt.Errorf("%w: the repository has no key domain yet", ErrNotFound)
	}
	st, err := s.openStore(ctx, repo)
	if err != nil {
		return nil, err
	}
	b, err := st.GetEscrow(repo.DomainID)
	if errors.Is(err, datastore.ErrNotFound) {
		return nil, fmt.Errorf("%w: no escrow blob stored", ErrNotFound)
	}
	return b, err
}

// ClearEscrow removes the stored blob (admin, local).
func (s *Service) ClearEscrow(ctx context.Context, repoName string) error {
	repo, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return err
	}
	if repo.DomainID == "" {
		return nil
	}
	return st.DeleteEscrow(repo.DomainID)
}
