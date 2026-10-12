package service

import (
	"context"
	"fmt"
	"time"

	"github.com/unidoc/incus-backup/internal/datastore"
)

// applyMode copies the persisted maintenance mode of a repository onto its open store, so a change made
// through any process reaches the daemon's stores at the next open or refresh.
func (s *Service) applyMode(ctx context.Context, repoID string) {
	mode := s.persistedMode(ctx, repoID)
	s.mu.Lock()
	mb := s.modes[repoID]
	s.mu.Unlock()
	if mb != nil {
		mb.SetMode(mode)
	}
}

// RefreshModes re-reads every repository's mode (the daemon calls it every few seconds so a change made
// by another process, for example the CLI while the daemon runs, takes effect without a restart).
func (s *Service) RefreshModes(ctx context.Context) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.modes))
	for id := range s.modes {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.applyMode(ctx, id)
	}
}

// SetRepositoryMode switches a repository between online, read-only and offline. The change is
// persisted and applied to the open store immediately; it never interrupts a request in flight, but the
// next backend call of every session sees it, so an upload in progress fails and the host retries later.
func (s *Service) SetRepositoryMode(ctx context.Context, repoName, mode, reason string) error {
	m, ok := datastore.ParseMode(mode)
	if !ok {
		return bad("mode must be online, read-only or offline")
	}
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	if m == datastore.ModeOnline {
		reason = ""
	}
	if _, err := s.DB.Pool().Exec(ctx, `UPDATE repositories SET mode=$2, mode_reason=$3, mode_changed_at=$4 WHERE id=$1`, repo.ID, m.String(), reason, s.Now()); err != nil {
		return fmt.Errorf("setting repository mode: %w", err)
	}
	s.applyMode(ctx, repo.ID)
	return nil
}

// persistedMode reads the repository's mode; when it cannot be read the repository is treated as read-only
// (fail safe: never write on an unknown state).
func (s *Service) persistedMode(ctx context.Context, repoID string) datastore.Mode {
	var m string
	if err := s.DB.Pool().QueryRow(ctx, `SELECT mode FROM repositories WHERE id=$1`, repoID).Scan(&m); err != nil {
		return datastore.ModeReadOnly
	}
	mode, _ := datastore.ParseMode(m)
	return mode
}

// requireWritable refuses a maintenance operation up front (with the real reason) when the repository is
// not online, instead of letting it run to its first write. The backend wrapper remains the enforcement;
// this is the clear error message and the guarantee that a no-op GC or prune does not report success on a
// repository an administrator froze.
func (s *Service) requireWritable(ctx context.Context, repoID string) error {
	switch s.persistedMode(ctx, repoID) {
	case datastore.ModeOffline:
		return datastore.ErrOffline
	case datastore.ModeReadOnly:
		return datastore.ErrReadOnly
	}
	return nil
}

// SampleCapacityIfStale takes a capacity sample only when the last one is older than maxAge. Sampling walks
// the whole datastore (a full listing on S3), so a page that polls the capacity endpoint must not trigger
// it every time.
func (s *Service) SampleCapacityIfStale(ctx context.Context, repoName string, maxAge time.Duration) error {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	var last *time.Time
	_ = s.DB.Pool().QueryRow(ctx, `SELECT max(taken_at) FROM capacity_samples WHERE repository_id=$1`, repo.ID).Scan(&last)
	if last != nil && s.Now().Sub(*last) < maxAge {
		return nil
	}
	return s.SampleCapacity(ctx, repoName)
}
