package service

import (
	"context"
	"net/url"
	"strings"
)

// Editable server settings. Each key has a validator; unknown keys are refused so the table cannot become a
// dumping ground, and nothing secret is ever stored here (secrets go in the sealed credential store).
var settingValidators = map[string]func(string) error{
	// base_url: how people reach the web UI, used in notification links ("https://backup.example.com").
	"base_url": func(v string) error {
		if v == "" {
			return nil
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" && u.Path != "/" {
			return bad("base_url must look like https://backup.example.com")
		}
		return nil
	},
	// advertise_address: host:port that backup hosts use to reach the agent API (shown in the onboarding command).
	"advertise_address": func(v string) error {
		if v != "" && (strings.ContainsAny(v, " /") || !strings.Contains(v, ":")) {
			return bad("advertise_address must look like backup.example.com:8007")
		}
		return nil
	},
	// instance_name: shown in the UI header and notification subjects.
	"instance_name": func(v string) error {
		if len(v) > 64 || strings.ContainsAny(v, "\r\n") {
			return bad("instance_name: at most 64 characters on one line")
		}
		return nil
	},
}

// SettingKeys lists the editable keys.
func SettingKeys() []string {
	return []string{"base_url", "advertise_address", "instance_name", "update_check", "update_check_url"}
}

// Settings returns every editable setting (unset ones as empty strings).
func (s *Service) Settings(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k := range settingValidators {
		out[k] = ""
	}
	rows, err := s.DB.Pool().Query(ctx, `SELECT key, value FROM server_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if _, ok := settingValidators[k]; ok {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// Setting returns one value ("" when unset).
func (s *Service) Setting(ctx context.Context, key string) string {
	var v string
	_ = s.DB.Pool().QueryRow(ctx, `SELECT value FROM server_settings WHERE key=$1`, key).Scan(&v)
	return v
}

// SetSettings validates and stores the given keys atomically.
func (s *Service) SetSettings(ctx context.Context, kv map[string]string) error {
	for k, v := range kv {
		check, ok := settingValidators[k]
		if !ok {
			return bad("unknown setting %q", k)
		}
		if err := check(strings.TrimSpace(v)); err != nil {
			return err
		}
	}
	tx, err := s.DB.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for k, v := range kv {
		if _, err := tx.Exec(ctx, `INSERT INTO server_settings (key, value, updated_at) VALUES ($1,$2,$3)
			ON CONFLICT (key) DO UPDATE SET value=$2, updated_at=$3`, k, strings.TrimSpace(v), s.Now()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
