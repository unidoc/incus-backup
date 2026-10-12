// Package traffic limits the bandwidth of the agent API (docs/traffic.md): token buckets per host or
// shared by several hosts, per direction, optionally only inside weekly time windows. Rules live in
// PostgreSQL (traffic_rules) and are re-read at most every 30 seconds (immediately after a change made
// through this process).
package traffic

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Directions, seen from the server: upload = host to server (backup), download = server to host (restore).
const (
	Upload   = "upload"
	Download = "download"
)

// Scopes.
const (
	ScopeHost   = "host"   // every matching host gets its own bucket of Rate
	ScopeGlobal = "global" // all matching hosts share one bucket of Rate
)

// MinRate is the lowest allowed rate: an 80 MiB blob (the largest) then takes under 22 minutes, inside
// the hosts' 30-minute request timeout.
const MinRate = 64 << 10

// Errors.
var (
	ErrNotFound = errors.New("not found")
	ErrBad      = errors.New("bad request")
	ErrExists   = errors.New("already exists")
)

// Rule is one limit.
type Rule struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	Scope     string    `json:"scope" doc:"host (a bucket per host) or global (one shared bucket)"`
	Hosts     string    `json:"hosts" doc:"comma-separated globs on the host name, or on repository/host when a glob contains '/'"`
	Direction string    `json:"direction" doc:"upload (backups) or download (restores)"`
	RateBps   int64     `json:"rate_bps" doc:"bytes per second (at least 65536)"`
	Burst     int64     `json:"burst_bytes" doc:"bucket size in bytes; 0 = one second of rate"`
	Schedule  string    `json:"schedule" doc:"empty = always, else e.g. 'mon-fri 08:00-18:00; sat,sun'"`
	Timezone  string    `json:"timezone" doc:"IANA time zone of the schedule"`
	Comment   string    `json:"comment"`
	Created   time.Time `json:"created_at"`
	Updated   time.Time `json:"updated_at"`
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Validate checks a rule and fills defaults.
func (r *Rule) Validate() error {
	if !nameRE.MatchString(r.Name) {
		return fmt.Errorf("%w: name must be 1-64 letters, digits, '.', '_' or '-'", ErrBad)
	}
	if r.Scope != ScopeHost && r.Scope != ScopeGlobal {
		return fmt.Errorf("%w: scope must be host or global", ErrBad)
	}
	if r.Direction != Upload && r.Direction != Download {
		return fmt.Errorf("%w: direction must be upload or download", ErrBad)
	}
	if r.RateBps < MinRate {
		return fmt.Errorf("%w: rate_bps must be at least %d (64 KiB/s): slower, a large chunk would outlast the hosts' 30-minute request timeout", ErrBad, MinRate)
	}
	if r.Burst < 0 {
		return fmt.Errorf("%w: burst_bytes must not be negative", ErrBad)
	}
	r.Hosts = strings.TrimSpace(r.Hosts)
	if r.Hosts == "" {
		r.Hosts = "*"
	}
	for _, g := range splitGlobs(r.Hosts) {
		if _, err := path.Match(g, "x"); err != nil {
			return fmt.Errorf("%w: host glob %q: %v", ErrBad, g, err)
		}
	}
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return fmt.Errorf("%w: unknown time zone %q", ErrBad, r.Timezone)
	}
	if _, err := ParseSchedule(r.Schedule); err != nil {
		return fmt.Errorf("%w: schedule: %v", ErrBad, err)
	}
	return nil
}

func splitGlobs(s string) []string {
	var out []string
	for _, g := range strings.Split(s, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// Matches reports whether the rule's host globs match host (in repository repo).
func (r *Rule) Matches(repo, host string) bool {
	for _, g := range splitGlobs(r.Hosts) {
		subject := host
		if strings.Contains(g, "/") {
			subject = repo + "/" + host
		}
		if ok, _ := path.Match(g, subject); ok {
			return true
		}
	}
	return false
}

// Store is the rule table.
type Store struct{ Pool *pgxpool.Pool }

const ruleCols = `id::text, name, enabled, scope, hosts, direction, rate_bps, burst_bytes, schedule, timezone, comment, created_at, updated_at`

func scanRule(row pgx.Row) (*Rule, error) {
	var r Rule
	if err := row.Scan(&r.ID, &r.Name, &r.Enabled, &r.Scope, &r.Hosts, &r.Direction, &r.RateBps, &r.Burst, &r.Schedule, &r.Timezone, &r.Comment, &r.Created, &r.Updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// List returns every rule by name.
func (s Store) List(ctx context.Context) ([]Rule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+ruleCols+` FROM traffic_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// Create adds a rule.
func (s Store) Create(ctx context.Context, r Rule) (*Rule, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	x, err := scanRule(s.Pool.QueryRow(ctx, `INSERT INTO traffic_rules (name, enabled, scope, hosts, direction, rate_bps, burst_bytes, schedule, timezone, comment)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+ruleCols, r.Name, r.Enabled, r.Scope, r.Hosts, r.Direction, r.RateBps, r.Burst, r.Schedule, r.Timezone, r.Comment))
	if err != nil && strings.Contains(err.Error(), "traffic_rules_name_key") {
		return nil, fmt.Errorf("%w: traffic rule %q", ErrExists, r.Name)
	}
	return x, err
}

// Update replaces the rule called name (the body may rename it).
func (s Store) Update(ctx context.Context, name string, r Rule) (*Rule, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	x, err := scanRule(s.Pool.QueryRow(ctx, `UPDATE traffic_rules SET name=$2, enabled=$3, scope=$4, hosts=$5, direction=$6, rate_bps=$7, burst_bytes=$8,
		schedule=$9, timezone=$10, comment=$11, updated_at=now() WHERE name=$1 RETURNING `+ruleCols,
		name, r.Name, r.Enabled, r.Scope, r.Hosts, r.Direction, r.RateBps, r.Burst, r.Schedule, r.Timezone, r.Comment))
	if err != nil && strings.Contains(err.Error(), "traffic_rules_name_key") {
		return nil, fmt.Errorf("%w: traffic rule %q", ErrExists, r.Name)
	}
	return x, err
}

// Delete removes a rule.
func (s Store) Delete(ctx context.Context, name string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM traffic_rules WHERE name=$1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
