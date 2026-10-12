package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Settings are the engine-wide knobs (table notify_settings).
type Settings struct {
	DedupeMinutes         int `json:"dedupe_minutes"`
	FlapWindowMinutes     int `json:"flap_window_minutes"`
	FlapThreshold         int `json:"flap_threshold"`
	MaxAttempts           int `json:"max_attempts"`
	BackoffBaseSeconds    int `json:"backoff_base_seconds"`
	BackoffMaxSeconds     int `json:"backoff_max_seconds"`
	EventExpireMinutes    int `json:"event_expire_minutes"`
	AlertRetentionDays    int `json:"alert_retention_days"`
	DeliveryRetentionDays int `json:"delivery_retention_days"`
}

const settingsCols = `dedupe_minutes, flap_window_minutes, flap_threshold, max_attempts, backoff_base_seconds, backoff_max_seconds,
	event_expire_minutes, alert_retention_days, delivery_retention_days`

// GetSettings returns the engine settings.
func (e *Engine) GetSettings(ctx context.Context) (*Settings, error) {
	var s Settings
	err := e.Svc.DB.Pool().QueryRow(ctx, `SELECT `+settingsCols+` FROM notify_settings`).Scan(&s.DedupeMinutes, &s.FlapWindowMinutes, &s.FlapThreshold,
		&s.MaxAttempts, &s.BackoffBaseSeconds, &s.BackoffMaxSeconds, &s.EventExpireMinutes, &s.AlertRetentionDays, &s.DeliveryRetentionDays)
	return &s, err
}

// SetSettings replaces the engine settings (the database CHECKs validate the ranges).
func (e *Engine) SetSettings(ctx context.Context, s Settings) error {
	_, err := e.Svc.DB.Pool().Exec(ctx, `UPDATE notify_settings SET dedupe_minutes=$1, flap_window_minutes=$2, flap_threshold=$3, max_attempts=$4,
		backoff_base_seconds=$5, backoff_max_seconds=$6, event_expire_minutes=$7, alert_retention_days=$8, delivery_retention_days=$9`,
		s.DedupeMinutes, s.FlapWindowMinutes, s.FlapThreshold, s.MaxAttempts, s.BackoffBaseSeconds, s.BackoffMaxSeconds,
		s.EventExpireMinutes, s.AlertRetentionDays, s.DeliveryRetentionDays)
	if err != nil {
		return bad("settings: %v", err)
	}
	return nil
}

// Condition is one alert type's switch, base severity and thresholds (table notify_conditions).
type Condition struct {
	Type     string          `json:"type"`
	Enabled  bool            `json:"enabled"`
	Severity string          `json:"severity"`
	Params   json.RawMessage `json:"params"`
}

// ListConditions returns every condition with its thresholds.
func (e *Engine) ListConditions(ctx context.Context) ([]Condition, error) {
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT type, enabled, severity, params FROM notify_conditions ORDER BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Condition
	for rows.Next() {
		var c Condition
		if err := rows.Scan(&c.Type, &c.Enabled, &c.Severity, &c.Params); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCondition updates one condition. Params are merged over the stored ones (unknown keys are refused).
func (e *Engine) SetCondition(ctx context.Context, c Condition) error {
	if !validSeverity(c.Severity) {
		return bad("invalid severity %q", c.Severity)
	}
	var cur json.RawMessage
	if err := e.Svc.DB.Pool().QueryRow(ctx, `SELECT params FROM notify_conditions WHERE type=$1`, c.Type).Scan(&cur); err != nil {
		return fmt.Errorf("%w: condition %q", ErrNotFound, c.Type)
	}
	merged := map[string]any{}
	_ = json.Unmarshal(cur, &merged)
	if len(c.Params) > 0 {
		var upd map[string]any
		if err := json.Unmarshal(c.Params, &upd); err != nil {
			return bad("params must be a JSON object")
		}
		for k, v := range upd {
			if _, ok := merged[k]; !ok {
				return bad("condition %s has no parameter %q", c.Type, k)
			}
			if _, isNum := v.(float64); !isNum {
				if _, wasStr := merged[k].(string); !wasStr {
					return bad("parameter %q must be a number", k)
				}
			} else if v.(float64) < 0 {
				return bad("parameter %q must not be negative", k)
			}
			merged[k] = v
		}
	}
	raw, _ := json.Marshal(merged)
	_, err := e.Svc.DB.Pool().Exec(ctx, `UPDATE notify_conditions SET enabled=$2, severity=$3, params=$4 WHERE type=$1`, c.Type, c.Enabled, c.Severity, raw)
	return err
}

type condCfg struct {
	enabled  bool
	severity string
	params   map[string]any
}

func (c condCfg) num(k string, def float64) float64 {
	if v, ok := c.params[k].(float64); ok {
		return v
	}
	return def
}

func (c condCfg) str(k, def string) string {
	if v, ok := c.params[k].(string); ok && v != "" {
		return v
	}
	return def
}

func (e *Engine) conditions(ctx context.Context) (map[string]condCfg, error) {
	cs, err := e.ListConditions(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]condCfg{}
	for _, c := range cs {
		cc := condCfg{enabled: c.Enabled, severity: c.Severity, params: map[string]any{}}
		_ = json.Unmarshal(c.Params, &cc.params)
		out[c.Type] = cc
	}
	return out, nil
}

// BaseURL is the console URL used in links (server_settings key 'base_url'); "" when unset.
func (e *Engine) BaseURL(ctx context.Context) string {
	var v string
	_ = e.Svc.DB.Pool().QueryRow(ctx, `SELECT value FROM server_settings WHERE key='base_url'`).Scan(&v)
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// SetBaseURL stores the console URL used in links.
func (e *Engine) SetBaseURL(ctx context.Context, u string) error {
	u = strings.TrimSpace(u)
	if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return bad("base url must start with http:// or https://")
	}
	_, err := e.Svc.DB.Pool().Exec(ctx, `INSERT INTO server_settings (key, value, updated_at) VALUES ('base_url', $1, $2)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=EXCLUDED.updated_at`, u, e.now())
	return err
}

// ---- small helpers -----------------------------------------------------------------------

// glob compiles a shell-like pattern: '*' matches anything (including '/'), '?' one character.
// The empty pattern matches everything.
func glob(p string) func(string) bool {
	if p == "" || p == "*" {
		return func(string) bool { return true }
	}
	var b strings.Builder
	b.WriteString("^")
	for _, r := range p {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re := regexp.MustCompile(b.String())
	return re.MatchString
}

func validGlob(p string) bool { return len(p) <= 256 && !strings.ContainsAny(p, "\r\n") }

// clean makes host-supplied text safe for subjects and one-line fields: control characters become
// spaces and the length is bounded.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// cleanBlock keeps newlines (for detail text) but removes other control characters.
func cleanBlock(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return strings.TrimSpace(s)
}

// human formats a duration for people: "45 min", "27 h", "3 d".
func human(d time.Duration) string {
	switch {
	case d < 0:
		return "0 min"
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 72*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d d", int(d/(24*time.Hour)))
	}
}

func hhmm(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil || h > 23 || m > 59 || h < 0 || m < 0 {
		return 0, false
	}
	return h*60 + m, true
}
