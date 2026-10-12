package service

import (
	"context"
	"fmt"
)

// ChecklistItem is one line of the "is everything set up?" check of a new installation.
type ChecklistItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"` // what was found, or what is missing
	// Route is the console page that fixes it (a route name of the web UI).
	Route string `json:"route"`
}

// Checklist is the setup health of the installation.
type Checklist struct {
	Items    []ChecklistItem `json:"items"`
	Done     int             `json:"done"`
	Total    int             `json:"total"`
	Complete bool            `json:"complete"`
}

// SetupChecklist answers "is everything set up?": a notification channel exists and a test message
// reached it, a rule routes alerts to it, a backup host is enrolled, a policy governs objects, a restore
// test passed, every administrator uses two-factor authentication, and the base URL for links is set.
// It only reads.
func (s *Service) SetupChecklist(ctx context.Context) (*Checklist, error) {
	pool := s.DB.Pool()
	var (
		channels, tested, rules, hosts, governed, passed, admins, admins2FA int
		baseURL                                                             string
	)
	queries := []struct {
		q   string
		dst any
	}{
		{`SELECT count(*) FROM notify_channels WHERE enabled`, &channels},
		{`SELECT count(DISTINCT d.channel_id) FROM notify_deliveries d JOIN notify_channels c ON c.id=d.channel_id
			WHERE d.kind='test' AND d.state='sent' AND c.enabled`, &tested},
		{`SELECT count(*) FROM notify_rules WHERE enabled AND kind='alert' AND cardinality(channel_ids) > 0`, &rules},
		{`SELECT count(*) FROM hosts WHERE scope='backup' AND NOT disabled`, &hosts},
		{`SELECT (SELECT count(*) FROM policy_assignments) + (SELECT count(*) FROM storage_objects WHERE retention_policy <> '')`, &governed},
		{`SELECT CASE WHEN to_regclass('restore_tests') IS NULL THEN 0 ELSE (SELECT count(*) FROM restore_tests WHERE state='passed') END`, &passed},
		{`SELECT count(*) FROM users WHERE role='admin' AND NOT disabled`, &admins},
		{`SELECT count(*) FROM users WHERE role='admin' AND NOT disabled AND totp_enabled`, &admins2FA},
		{`SELECT COALESCE((SELECT value FROM server_settings WHERE key='base_url'), '')`, &baseURL},
	}
	for _, x := range queries {
		if err := pool.QueryRow(ctx, x.q).Scan(x.dst); err != nil {
			return nil, fmt.Errorf("setup checklist: %w", err)
		}
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	items := []ChecklistItem{
		{ID: "channel", Title: "A notification channel exists", OK: channels > 0, Route: "notifications",
			Detail: pick(channels > 0, plural(channels, "enabled channel", "enabled channels"), "No enabled channel: nobody hears about failed backups. Add an e-mail or webhook channel.")},
		{ID: "channel_tested", Title: "A test message reached a channel", OK: tested > 0, Route: "notifications",
			Detail: pick(tested > 0, plural(tested, "channel answered a test", "channels answered a test"), "Send a test from the channel list and check that it arrives.")},
		{ID: "rule", Title: "A rule sends alerts to a channel", OK: rules > 0, Route: "notification-rules",
			Detail: pick(rules > 0, plural(rules, "enabled alert rule", "enabled alert rules"), "Without a rule, alerts are only shown in the console. Add a rule.")},
		{ID: "host", Title: "A backup host is enrolled", OK: hosts > 0, Route: "hosts",
			Detail: pick(hosts > 0, plural(hosts, "enabled backup host", "enabled backup hosts"), "Add a host: the onboarding page prints the command to run on it.")},
		{ID: "policy", Title: "A policy governs objects", OK: governed > 0, Route: "policies",
			Detail: pick(governed > 0, plural(governed, "assignment or governed object", "assignments or governed objects"), "Assign a policy to a host, project or object so backups have a schedule and retention.")},
		{ID: "restore_test", Title: "A restore test passed", OK: passed > 0, Route: "restore-tests",
			Detail: pick(passed > 0, plural(passed, "passed restore test", "passed restore tests"), "A backup that was never restored is a hope. Run a restore test.")},
		{ID: "admin_2fa", Title: "Every administrator uses two-factor authentication", OK: admins > 0 && admins2FA == admins, Route: "users",
			Detail: pick(admins > 0 && admins2FA == admins, fmt.Sprintf("%d of %d administrators", admins2FA, admins),
				fmt.Sprintf("%d of %d administrators have two-factor authentication. Each administrator turns it on in their profile.", admins2FA, admins))},
		{ID: "base_url", Title: "The console address for links is set", OK: baseURL != "", Route: "settings",
			Detail: pick(baseURL != "", baseURL, "Notifications carry no link to the alert until the base URL is set.")},
	}
	c := &Checklist{Items: items, Total: len(items)}
	for _, it := range items {
		if it.OK {
			c.Done++
		}
	}
	c.Complete = c.Done == c.Total
	return c, nil
}

func pick(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}
