package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
)

// Administrative snapshot operations (local admin: CLI today, UI later). They are all
// written to the datastore first (authoritative) and mirrored to PostgreSQL.

// FindGroup resolves "project" + object name to a group id (names are control-plane data).
func (s *Service) FindGroup(ctx context.Context, repoName, project, name string) (string, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return "", err
	}
	var g string
	err = s.DB.Pool().QueryRow(ctx, `SELECT group_id FROM storage_objects WHERE repository_id=$1 AND project=$2 AND name=$3`, repo.ID, project, name).Scan(&g)
	if err != nil {
		return "", fmt.Errorf("%w: object %s/%s", ErrNotFound, project, name)
	}
	return g, nil
}

func (s *Service) repoStore(ctx context.Context, repoName string) (*Repo, *datastore.Store, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, nil, err
	}
	st, err := s.openStore(ctx, repo)
	return repo, st, err
}

func mapStoreErr(err error, what string) error {
	switch {
	case errors.Is(err, datastore.ErrNotFound):
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	case errors.Is(err, datastore.ErrProtected):
		return fmt.Errorf("%w: %s is protected (unprotect it first)", ErrConflict, what)
	}
	return err
}

// SetSnapshotProtected protects or unprotects a snapshot. Protection lives in the repository,
// so rebuilding PostgreSQL cannot lose it.
func (s *Service) SetSnapshotProtected(ctx context.Context, repoName, group, snap string, protected bool) error {
	repo, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return err
	}
	if err := st.SetProtected(group, snap, protected); err != nil {
		return mapStoreErr(err, "snapshot")
	}
	_, err = s.DB.Pool().Exec(ctx, `UPDATE snapshots SET protected=$4 WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, group, snap, protected)
	return err
}

// SetSnapshotNotes sets the notes of a snapshot ("" clears them).
func (s *Service) SetSnapshotNotes(ctx context.Context, repoName, group, snap, notes string) error {
	repo, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return err
	}
	if err := st.SetNotes(group, snap, notes); err != nil {
		return mapStoreErr(err, "snapshot")
	}
	_, err = s.DB.Pool().Exec(ctx, `UPDATE snapshots SET notes=$4 WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, group, snap, notes)
	return err
}

// SetGroupNotes sets the notes of a whole object ("" clears them).
func (s *Service) SetGroupNotes(ctx context.Context, repoName, group, notes string) error {
	repo, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return err
	}
	if err := st.SetGroupNotes(group, notes); err != nil {
		return err
	}
	_, err = s.DB.Pool().Exec(ctx, `UPDATE storage_objects SET notes=$3 WHERE repository_id=$1 AND group_id=$2`, repo.ID, group, notes)
	return err
}

// ForgetSnapshot removes one snapshot by hand (its chunks are left for GC). Protected snapshots
// and snapshots a running backup builds on are refused.
func (s *Service) ForgetSnapshot(ctx context.Context, repoName, group, snap string) error {
	release, gerr := s.guard(ctx)
	if gerr != nil {
		return gerr
	}
	defer release()
	repo, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return err
	}
	pins, err := s.pinnedBases(ctx, repo.ID)
	if err != nil {
		return err
	}
	if pins[group][snap] {
		return fmt.Errorf("%w: a running backup builds on this snapshot", ErrConflict)
	}
	if err := st.DeleteSnapshot(group, snap); err != nil {
		return mapStoreErr(err, "snapshot")
	}
	_, err = s.DB.Pool().Exec(ctx, `DELETE FROM snapshots WHERE repository_id=$1 AND group_id=$2 AND snapshot_id=$3`, repo.ID, group, snap)
	return err
}

// SnapshotVerification is one entry of a snapshot's verification history.
type SnapshotVerification struct {
	At       time.Time `json:"at"`
	State    string    `json:"state"`
	Chunks   int       `json:"chunks"`
	Problems []string  `json:"problems"`
}

// VerificationHistory returns the verification history of a snapshot, newest first (from the
// repository's own record, so it survives a database rebuild).
func (s *Service) VerificationHistory(ctx context.Context, repoName, group, snap string) ([]SnapshotVerification, error) {
	_, st, err := s.repoStore(ctx, repoName)
	if err != nil {
		return nil, err
	}
	rec, err := st.ReadVerify(group, snap)
	if err != nil {
		return nil, err
	}
	var out []SnapshotVerification
	if rec != nil {
		for i := len(rec.History) - 1; i >= 0; i-- {
			e := rec.History[i]
			out = append(out, SnapshotVerification{At: e.At, State: e.State, Chunks: e.Chunks, Problems: e.Problems})
		}
	}
	return out, nil
}

// Snapshots lists snapshots of a repository for administration, with all metadata.
func (s *Service) AdminSnapshots(ctx context.Context, repoName string, groups []string) ([]api.SnapshotRef, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	return s.ListAllSnapshots(ctx, repo, groups)
}
