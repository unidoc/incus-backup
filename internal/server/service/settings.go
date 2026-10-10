package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/schedule"
)

// MaintenanceSettings are the housekeeping schedules of a repository. They live in
// PostgreSQL and are changed through the API (CLI today, UI later); there is no
// configuration file to maintain.
type MaintenanceSettings struct {
	Capacity            string        `json:"capacity"`
	Reconcile           string        `json:"reconcile"`
	Prune               string        `json:"prune"`
	GC                  string        `json:"gc"`
	Verify              string        `json:"verify"` // verify new snapshots and re-verify stale ones
	Scrub               string        `json:"scrub"`  // full chunk scrub
	GCGrace             time.Duration `json:"gc_grace"`
	DefaultReverifyDays int           `json:"default_reverify_days"`
}

// DefaultMaintenance returns the defaults a repository starts with.
func DefaultMaintenance() MaintenanceSettings {
	return MaintenanceSettings{Capacity: "every 1h", Reconcile: "daily", Prune: "04:00", GC: "05:00", Verify: "daily", Scrub: "monthly",
		GCGrace: DefaultGCGrace, DefaultReverifyDays: 30}
}

// Maintenance returns a repository's schedules (defaults when never changed).
func (s *Service) Maintenance(ctx context.Context, repoName string) (*MaintenanceSettings, error) {
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return nil, err
	}
	return s.maintenanceByID(ctx, repo.ID)
}

func (s *Service) maintenanceByID(ctx context.Context, repoID string) (*MaintenanceSettings, error) {
	m := DefaultMaintenance()
	var hours int
	err := s.DB.Pool().QueryRow(ctx, `SELECT capacity, reconcile, prune, gc, verify, scrub, gc_grace_hours, default_reverify_days FROM maintenance_settings WHERE repository_id=$1`, repoID).
		Scan(&m.Capacity, &m.Reconcile, &m.Prune, &m.GC, &m.Verify, &m.Scrub, &hours, &m.DefaultReverifyDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return &m, nil
	}
	if err != nil {
		return nil, err
	}
	m.GCGrace = time.Duration(hours) * time.Hour
	return &m, nil
}

// SetMaintenance replaces a repository's schedules after validating them.
func (s *Service) SetMaintenance(ctx context.Context, repoName string, m MaintenanceSettings) error {
	for name, spec := range map[string]string{"capacity": m.Capacity, "reconcile": m.Reconcile, "prune": m.Prune, "gc": m.GC, "verify": m.Verify, "scrub": m.Scrub} {
		if _, err := schedule.Parse(spec); err != nil {
			return bad("%s: %v", name, err)
		}
	}
	if m.GCGrace < MinGCGrace {
		return bad("gc grace must be at least %s", MinGCGrace)
	}
	if m.DefaultReverifyDays < 0 {
		return bad("default reverify days must not be negative")
	}
	repo, err := s.Repository(ctx, repoName)
	if err != nil {
		return err
	}
	_, err = s.DB.Pool().Exec(ctx, `INSERT INTO maintenance_settings (repository_id, capacity, reconcile, prune, gc, verify, scrub, gc_grace_hours, default_reverify_days)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (repository_id) DO UPDATE SET capacity=EXCLUDED.capacity, reconcile=EXCLUDED.reconcile, prune=EXCLUDED.prune, gc=EXCLUDED.gc,
		 verify=EXCLUDED.verify, scrub=EXCLUDED.scrub, gc_grace_hours=EXCLUDED.gc_grace_hours, default_reverify_days=EXCLUDED.default_reverify_days`,
		repo.ID, m.Capacity, m.Reconcile, m.Prune, m.GC, m.Verify, m.Scrub, int(m.GCGrace/time.Hour), m.DefaultReverifyDays)
	if err != nil {
		return fmt.Errorf("saving maintenance settings: %w", err)
	}
	return nil
}
