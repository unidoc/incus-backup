package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Rule kinds.
const (
	RuleAlert  = "alert"
	RuleDigest = "digest"
)

// Rule routes alerts (or the daily digest) to channels.
type Rule struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`        // alert (default) or digest
	EventTypes  []string `json:"event_types"` // empty = every type
	MinSeverity string   `json:"min_severity"`
	HostGlob    string   `json:"host_glob"`
	RepoGlob    string   `json:"repo_glob"`
	ObjectGlob  string   `json:"object_glob"`
	ChannelIDs  []string `json:"channel_ids"`
	// RepeatMinutes: reminder interval while an alert keeps firing unacknowledged (0 = never).
	RepeatMinutes int `json:"repeat_minutes"`
	// Quiet hours ("HH:MM" in Timezone; both empty = none). Notifications are held, not dropped,
	// and sent when the window ends if still relevant. CriticalBypass lets critical alerts through.
	QuietStart     string     `json:"quiet_start"`
	QuietEnd       string     `json:"quiet_end"`
	Timezone       string     `json:"timezone"`
	CriticalBypass bool       `json:"critical_bypass"`
	SendResolved   bool       `json:"send_resolved"`
	DigestAt       string     `json:"digest_at"` // digest rules: local send time "HH:MM"
	LastDigestAt   *time.Time `json:"last_digest_at"`
	Enabled        bool       `json:"enabled"`
	Created        time.Time  `json:"created"`

	loc *time.Location
}

// DefaultRule returns a rule with the documented (conservative) defaults.
func DefaultRule(name string, channelIDs ...string) Rule {
	return Rule{Name: name, Kind: RuleAlert, MinSeverity: SeverityWarning, ChannelIDs: channelIDs, RepeatMinutes: 1440, Timezone: "UTC",
		CriticalBypass: true, SendResolved: true, DigestAt: "08:00", Enabled: true}
}

const ruleCols = `id::text, name, kind, event_types, min_severity, host_glob, repo_glob, object_glob, channel_ids::text[], repeat_minutes,
	quiet_start, quiet_end, timezone, critical_bypass, send_resolved, digest_at, last_digest_at, enabled, created`

func scanRule(r pgx.Row) (*Rule, error) {
	var x Rule
	err := r.Scan(&x.ID, &x.Name, &x.Kind, &x.EventTypes, &x.MinSeverity, &x.HostGlob, &x.RepoGlob, &x.ObjectGlob, &x.ChannelIDs, &x.RepeatMinutes,
		&x.QuietStart, &x.QuietEnd, &x.Timezone, &x.CriticalBypass, &x.SendResolved, &x.DigestAt, &x.LastDigestAt, &x.Enabled, &x.Created)
	if err == nil {
		x.loc, _ = time.LoadLocation(x.Timezone)
		if x.loc == nil {
			x.loc = time.UTC
		}
	}
	return &x, err
}

func (e *Engine) validateRule(ctx context.Context, r *Rule) error {
	if !nameOK(r.Name) {
		return bad("invalid rule name")
	}
	if r.Kind == "" {
		r.Kind = RuleAlert
	}
	if r.Kind != RuleAlert && r.Kind != RuleDigest {
		return bad("rule kind must be alert or digest")
	}
	if r.MinSeverity == "" {
		r.MinSeverity = SeverityWarning
	}
	if !validSeverity(r.MinSeverity) {
		return bad("invalid min_severity")
	}
	for _, g := range []string{r.HostGlob, r.RepoGlob, r.ObjectGlob} {
		if !validGlob(g) {
			return bad("invalid filter")
		}
	}
	for _, t := range r.EventTypes {
		if t == "" || !validGlob(t) {
			return bad("invalid event type %q", t)
		}
	}
	if r.EventTypes == nil {
		r.EventTypes = []string{}
	}
	if r.RepeatMinutes < 0 {
		return bad("repeat_minutes must not be negative")
	}
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return bad("unknown time zone %q", r.Timezone)
	}
	if (r.QuietStart == "") != (r.QuietEnd == "") {
		return bad("quiet hours need both a start and an end")
	}
	if r.QuietStart != "" {
		if _, ok := hhmm(r.QuietStart); !ok {
			return bad("quiet_start must be HH:MM")
		}
		if _, ok := hhmm(r.QuietEnd); !ok {
			return bad("quiet_end must be HH:MM")
		}
	}
	if r.DigestAt == "" {
		r.DigestAt = "08:00"
	}
	if _, ok := hhmm(r.DigestAt); !ok {
		return bad("digest_at must be HH:MM")
	}
	if len(r.ChannelIDs) == 0 {
		return bad("a rule needs at least one channel")
	}
	ids := make([]string, 0, len(r.ChannelIDs))
	for _, c := range r.ChannelIDs {
		ch, err := e.channelRow(ctx, c) // accepts names too
		if err != nil {
			return err
		}
		ids = append(ids, ch.ID)
	}
	r.ChannelIDs = ids
	return nil
}

// ListRules returns every rule.
func (e *Engine) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT `+ruleCols+` FROM notify_rules ORDER BY name`)
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

// GetRule returns one rule by id or name.
func (e *Engine) GetRule(ctx context.Context, id string) (*Rule, error) {
	r, err := scanRule(e.Svc.DB.Pool().QueryRow(ctx, `SELECT `+ruleCols+` FROM notify_rules WHERE id::text=$1 OR name=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: rule %q", ErrNotFound, id)
	}
	return r, err
}

// CreateRule adds a rule. Start from DefaultRule for the documented defaults.
func (e *Engine) CreateRule(ctx context.Context, r Rule) (*Rule, error) {
	if err := e.validateRule(ctx, &r); err != nil {
		return nil, err
	}
	var id string
	err := e.Svc.DB.Pool().QueryRow(ctx, `INSERT INTO notify_rules (name, kind, event_types, min_severity, host_glob, repo_glob, object_glob, channel_ids,
		repeat_minutes, quiet_start, quiet_end, timezone, critical_bypass, send_resolved, digest_at, enabled, created)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::uuid[],$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id::text`,
		r.Name, r.Kind, r.EventTypes, r.MinSeverity, r.HostGlob, r.RepoGlob, r.ObjectGlob, r.ChannelIDs, r.RepeatMinutes, r.QuietStart, r.QuietEnd,
		r.Timezone, r.CriticalBypass, r.SendResolved, r.DigestAt, r.Enabled, e.now()).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "notify_rules_name_key") {
			return nil, fmt.Errorf("%w: a rule named %q exists", ErrConflict, r.Name)
		}
		return nil, bad("rule: %s", dbMsg(err))
	}
	return e.GetRule(ctx, id)
}

// UpdateRule replaces a rule's settings (all fields; read it first to change one).
func (e *Engine) UpdateRule(ctx context.Context, id string, r Rule) (*Rule, error) {
	cur, err := e.GetRule(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := e.validateRule(ctx, &r); err != nil {
		return nil, err
	}
	_, err = e.Svc.DB.Pool().Exec(ctx, `UPDATE notify_rules SET name=$2, kind=$3, event_types=$4, min_severity=$5, host_glob=$6, repo_glob=$7, object_glob=$8,
		channel_ids=$9::uuid[], repeat_minutes=$10, quiet_start=$11, quiet_end=$12, timezone=$13, critical_bypass=$14, send_resolved=$15, digest_at=$16, enabled=$17
		WHERE id=$1::uuid`, cur.ID, r.Name, r.Kind, r.EventTypes, r.MinSeverity, r.HostGlob, r.RepoGlob, r.ObjectGlob, r.ChannelIDs, r.RepeatMinutes,
		r.QuietStart, r.QuietEnd, r.Timezone, r.CriticalBypass, r.SendResolved, r.DigestAt, r.Enabled)
	if err != nil {
		if strings.Contains(err.Error(), "notify_rules_name_key") {
			return nil, fmt.Errorf("%w: a rule named %q exists", ErrConflict, r.Name)
		}
		return nil, bad("rule: %s", dbMsg(err))
	}
	return e.GetRule(ctx, cur.ID)
}

// DeleteRule removes a rule (its delivery log entries stay, without the rule reference).
func (e *Engine) DeleteRule(ctx context.Context, id string) error {
	ct, err := e.Svc.DB.Pool().Exec(ctx, `DELETE FROM notify_rules WHERE id::text=$1 OR name=$1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: rule %q", ErrNotFound, id)
	}
	return nil
}

// matches reports whether an alert falls under the rule's filters.
func (r *Rule) matches(a *Alert) bool {
	if sevRank(a.Severity) < sevRank(r.MinSeverity) {
		return false
	}
	return r.matchesIgnoringSeverity(a)
}

func (r *Rule) matchesIgnoringSeverity(a *Alert) bool {
	if len(r.EventTypes) > 0 {
		ok := false
		for _, t := range r.EventTypes {
			if glob(t)(a.Type) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return glob(r.HostGlob)(a.Host) && glob(r.RepoGlob)(a.Repository) && glob(r.ObjectGlob)(a.Object)
}

// quiet reports whether now falls in the rule's quiet hours.
func (r *Rule) quiet(now time.Time) bool {
	if r.QuietStart == "" {
		return false
	}
	s, _ := hhmm(r.QuietStart)
	en, _ := hhmm(r.QuietEnd)
	if s == en {
		return false
	}
	l := now.In(r.loc)
	m := l.Hour()*60 + l.Minute()
	if s < en {
		return m >= s && m < en
	}
	return m >= s || m < en
}
