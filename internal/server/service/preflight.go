package service

import "context"

// Preflight checks, before an asynchronous maintenance task is accepted, what the operation would
// otherwise only find out once it runs: the repository exists and, for an operation that writes, it is
// online (read-only or offline give datastore.ErrReadOnly / ErrOffline, which the API maps to 503).
// The operation checks again when it runs; this only makes the answer immediate.
func (s *Service) Preflight(ctx context.Context, repoName string, write bool) (*Repo, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if write {
		if err := s.requireWritable(ctx, repo.ID); err != nil {
			return nil, err
		}
	}
	return repo, nil
}
