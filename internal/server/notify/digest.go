package notify

import (
	"bytes"
	"context"
	"fmt"
	htmltemplate "html/template"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"
)

// DigestRepo is one repository's line in the digest.
type DigestRepo struct {
	Name          string `json:"name"`
	PhysicalBytes int64  `json:"physical_bytes"`
	Growth24h     int64  `json:"growth_24h"` // physical bytes added in the window (negative after prune/GC)
	FSFree        int64  `json:"fs_free"`
	HasSamples    bool   `json:"has_samples"`
}

// DigestReport summarises the last 24 hours.
type DigestReport struct {
	From             time.Time    `json:"from"`
	To               time.Time    `json:"to"`
	BackupsOK        int          `json:"backups_ok"`
	BackupsPartial   int          `json:"backups_partial"`
	BackupsFailed    int          `json:"backups_failed"`
	Objects          int          `json:"objects"`           // objects with a backup schedule
	ObjectsProtected int          `json:"objects_protected"` // of those, not overdue
	ProtectedPercent float64      `json:"protected_percent"`
	VerifiedOK       int          `json:"verified_ok"`
	VerifiedFailed   int          `json:"verified_failed"`
	Repositories     []DigestRepo `json:"repositories"`
	OpenAlerts       []AlertView  `json:"open_alerts"`
	NewAlerts        int          `json:"new_alerts"`
	ResolvedAlerts   int          `json:"resolved_alerts"`
}

// Digest builds the summary of the 24 hours before now.
func (e *Engine) Digest(ctx context.Context, now time.Time) (*DigestReport, error) {
	p := e.Svc.DB.Pool()
	from := now.Add(-24 * time.Hour)
	d := &DigestReport{From: from, To: now, Repositories: []DigestRepo{}, OpenAlerts: []AlertView{}}
	rows, err := p.Query(ctx, `SELECT status, count(*) FROM backup_runs WHERE finished_at > $1 AND finished_at <= $2 GROUP BY status`, from, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, err
		}
		switch st {
		case "ok":
			d.BackupsOK = n
		case "partial":
			d.BackupsPartial = n
		case "failed":
			d.BackupsFailed = n
		}
	}
	rows.Close()
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='ok'), count(*) FILTER (WHERE state='failed') FROM snapshot_verifications
		WHERE verified_at > $1 AND verified_at <= $2`, from, now).Scan(&d.VerifiedOK, &d.VerifiedFailed); err != nil {
		return nil, err
	}
	conds, err := e.conditions(ctx)
	if err != nil {
		return nil, err
	}
	missed, eligible, err := e.backupMissed(ctx, conds[TypeBackupMissed], now)
	if err != nil {
		return nil, err
	}
	d.Objects, d.ObjectsProtected = eligible, eligible-len(missed)
	if eligible > 0 {
		d.ProtectedPercent = 100 * float64(d.ObjectsProtected) / float64(eligible)
	} else {
		d.ProtectedPercent = 100
	}
	repos, err := e.Svc.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		dr := DigestRepo{Name: r.Name}
		var then *int64
		err := p.QueryRow(ctx, `SELECT physical_bytes, fs_free_bytes FROM capacity_samples WHERE repository_id=$1 AND taken_at <= $2 ORDER BY taken_at DESC LIMIT 1`, r.ID, now).
			Scan(&dr.PhysicalBytes, &dr.FSFree)
		if err == nil {
			dr.HasSamples = true
			_ = p.QueryRow(ctx, `SELECT physical_bytes FROM capacity_samples WHERE repository_id=$1 AND taken_at <= $2 ORDER BY taken_at DESC LIMIT 1`, r.ID, from).Scan(&then)
			if then == nil {
				_ = p.QueryRow(ctx, `SELECT physical_bytes FROM capacity_samples WHERE repository_id=$1 AND taken_at > $2 ORDER BY taken_at LIMIT 1`, r.ID, from).Scan(&then)
			}
			if then != nil {
				dr.Growth24h = dr.PhysicalBytes - *then
			}
		} else if err != pgx.ErrNoRows {
			return nil, err
		}
		d.Repositories = append(d.Repositories, dr)
	}
	open, err := e.ListAlerts(ctx, AlertFilter{Active: true, Limit: 50})
	if err != nil {
		return nil, err
	}
	for i := range open {
		d.OpenAlerts = append(d.OpenAlerts, open[i].view())
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='fired'), count(*) FILTER (WHERE kind='resolved') FROM notify_alert_events WHERE at > $1 AND at <= $2`,
		from, now).Scan(&d.NewAlerts, &d.ResolvedAlerts); err != nil {
		return nil, err
	}
	return d, nil
}

func bytesHuman(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	s := fmt.Sprintf("%.1f %s", f, units[i])
	if i == 0 {
		s = fmt.Sprintf("%d B", n)
	}
	if neg {
		return "-" + s
	}
	return s
}

var digestFuncs = template.FuncMap{"bytes": bytesHuman, "signed": func(n int64) string {
	if n >= 0 {
		return "+" + bytesHuman(n)
	}
	return bytesHuman(n)
}}

var digestText = template.Must(template.New("d").Funcs(digestFuncs).Parse(`incus-backup daily summary, {{.FromS}} to {{.ToS}}

Backups:    {{.R.BackupsOK}} ok, {{.R.BackupsPartial}} partial, {{.R.BackupsFailed}} failed
Protected:  {{printf "%.1f" .R.ProtectedPercent}}% ({{.R.ObjectsProtected}} of {{.R.Objects}} scheduled objects backed up on time)
Verified:   {{.R.VerifiedOK}} snapshot(s) ok, {{.R.VerifiedFailed}} failed
Alerts:     {{len .R.OpenAlerts}} open, {{.R.NewAlerts}} new, {{.R.ResolvedAlerts}} resolved
{{range .R.Repositories}}Repository {{.Name}}: {{if .HasSamples}}{{bytes .PhysicalBytes}} stored ({{signed .Growth24h}} in 24 h){{else}}no capacity samples yet{{end}}
{{end}}{{if .R.OpenAlerts}}
Open alerts:
{{range .R.OpenAlerts}}  [{{.Severity}}] {{.Title}} ({{.State}})
{{end}}{{end}}{{if .Link}}
Open in the console: {{.Link}}
{{end}}`))

var digestHTML = htmltemplate.Must(htmltemplate.New("d").Funcs(htmltemplate.FuncMap(digestFuncs)).Parse(`<!doctype html><html><body style="font-family:sans-serif;font-size:14px;color:#222">
<h2>incus-backup daily summary</h2><p style="color:#666">{{.FromS}} to {{.ToS}}</p>
<table cellpadding="4">
<tr><td><b>Backups</b></td><td>{{.R.BackupsOK}} ok, {{.R.BackupsPartial}} partial, {{.R.BackupsFailed}} failed</td></tr>
<tr><td><b>Protected</b></td><td>{{printf "%.1f" .R.ProtectedPercent}}% ({{.R.ObjectsProtected}} of {{.R.Objects}})</td></tr>
<tr><td><b>Verified</b></td><td>{{.R.VerifiedOK}} ok, {{.R.VerifiedFailed}} failed</td></tr>
<tr><td><b>Alerts</b></td><td>{{len .R.OpenAlerts}} open, {{.R.NewAlerts}} new, {{.R.ResolvedAlerts}} resolved</td></tr>
{{range .R.Repositories}}<tr><td><b>{{.Name}}</b></td><td>{{if .HasSamples}}{{bytes .PhysicalBytes}} ({{signed .Growth24h}} in 24 h){{else}}no samples yet{{end}}</td></tr>{{end}}
</table>
{{if .R.OpenAlerts}}<h3>Open alerts</h3><ul>{{range .R.OpenAlerts}}<li>[{{.Severity}}] {{.Title}} ({{.State}})</li>{{end}}</ul>{{end}}
{{if .Link}}<p><a href="{{.Link}}">Open in the console</a></p>{{end}}
</body></html>`))

func renderDigest(r *DigestReport, baseURL string, loc *time.Location) Message {
	data := struct {
		R          *DigestReport
		FromS, ToS string
		Link       string
	}{R: r, FromS: fmtTime(r.From, loc), ToS: fmtTime(r.To, loc)}
	if baseURL != "" {
		data.Link = baseURL + "/"
	}
	var tb, hb bytes.Buffer
	_ = digestText.Execute(&tb, data)
	_ = digestHTML.Execute(&hb, data)
	sev := SeverityInfo
	subject := fmt.Sprintf("[DIGEST] incus-backup: %d ok, %d failed, %.0f%% protected", r.BackupsOK, r.BackupsFailed+r.BackupsPartial, r.ProtectedPercent)
	if len(r.OpenAlerts) > 0 {
		subject += fmt.Sprintf(", %d open alert(s)", len(r.OpenAlerts))
	}
	return Message{Kind: "digest", Severity: sev, Subject: subject, Text: tb.String(), HTML: hb.String(), Link: data.Link,
		Payload: map[string]any{"digest": r}}
}

// digestDue reports whether a digest rule's daily send time passed since it last sent (or was created).
func digestDue(r *Rule, now time.Time) bool {
	m, ok := hhmm(r.DigestAt)
	if !ok {
		return false
	}
	l := now.In(r.loc)
	sched := time.Date(l.Year(), l.Month(), l.Day(), m/60, m%60, 0, 0, r.loc)
	if now.Before(sched) {
		return false
	}
	base := r.Created
	if r.LastDigestAt != nil {
		base = *r.LastDigestAt
	}
	return base.Before(sched)
}

func loadDigestRules(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, lock bool) ([]*Rule, error) {
	sql := `SELECT ` + ruleCols + ` FROM notify_rules WHERE enabled AND kind='digest' ORDER BY name`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	var rules []*Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		rules = append(rules, r)
	}
	rows.Close()
	return rules, rows.Err()
}

// prepareDigest builds the report when some digest rule is due (outside the Tick transaction).
func (e *Engine) prepareDigest(ctx context.Context, now time.Time) (*DigestReport, error) {
	rules, err := loadDigestRules(ctx, e.Svc.DB.Pool(), false)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if digestDue(r, now) {
			return e.Digest(ctx, now)
		}
	}
	return nil, nil
}

// digests queues the daily digest of every due digest rule (rep was built by prepareDigest).
func (e *Engine) digests(ctx context.Context, tx pgx.Tx, now time.Time, rep *DigestReport, baseURL string) (int, error) {
	if rep == nil {
		return 0, nil
	}
	rules, err := loadDigestRules(ctx, tx, true)
	if err != nil {
		return 0, err
	}
	var due []*Rule
	for _, r := range rules {
		if digestDue(r, now) {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return 0, nil
	}
	enabledCh, err := enabledChannels(ctx, tx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range due {
		msg := renderDigest(rep, baseURL, r.loc)
		day := now.In(r.loc).Format("2006-01-02")
		for _, c := range r.ChannelIDs {
			if !enabledCh[c] {
				continue
			}
			ok, err := enqueue(ctx, tx, c, r.ID, 0, fmt.Sprintf("digest/r%s/c%s/%s", r.ID, c, day), msg, now)
			if err != nil {
				return 0, err
			}
			if ok {
				n++
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE notify_rules SET last_digest_at=$2 WHERE id=$1::uuid`, r.ID, now); err != nil {
			return 0, err
		}
	}
	return n, nil
}
