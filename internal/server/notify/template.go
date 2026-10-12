package notify

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Message is one rendered notification. It is stored in the delivery queue as rendered, so the
// delivery log shows exactly what was sent.
type Message struct {
	Kind     string         `json:"kind"` // firing, reminder, escalation, resolved, digest, test
	Severity string         `json:"severity"`
	Subject  string         `json:"subject"`
	Text     string         `json:"text"`
	HTML     string         `json:"-"`
	Link     string         `json:"link,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
}

var hints = map[string]string{
	TypeBackupFailed:          "Open the host's last run in the console and read the error. Check that the host can reach the server and that Incus and storage are healthy; then run `incus-backup run` on the host.",
	TypeBackupMissed:          "Check that incus-backup runs on the host (systemctl/rc-service status), that its policy schedule is what you expect and that the last runs did not fail. Run `incus-backup run` to take a backup now.",
	TypeHostOffline:           "The host has not contacted the server. Check that the host is up, that the incus-backup daemon runs and that it can reach the server (network, firewall, certificate).",
	TypeMaintenanceFailed:     "Read the maintenance run's error in the console. Repeated failures stop retention, garbage collection or verification; fix the cause and run the job again.",
	TypeCorruption:            "Verification found damaged data. Affected snapshots are no longer used as incremental bases and corrupt chunks are quarantined. Take a fresh backup of the object (or resync it from a replica) to heal it, and check the storage hardware.",
	TypeSyncFailed:            "Read the sync run's error. Check that the remote server is reachable and its certificate is still valid; then start the job again.",
	TypeSyncLag:               "The replica is falling behind. Check that the sync job runs on schedule, its rate limit and the link to the remote server.",
	TypeCapacityWarning:       "Free space is running out. Review retention (prune), run garbage collection, or add storage before backups start failing.",
	TypeRepositoryUnreachable: "The server cannot reach the repository storage. Check the S3 endpoint, the bucket, network and the stored credential (`credential rotate` after a key change).",
	TypeCertExpiry:            "Renew the certificate before it expires, or clients and replicas will stop connecting.",
	TypeUpdateAvailable:       "A newer incus-backup release is available. Read its release notes, then upgrade the server package (apk upgrade incus-backup-server) and the hosts.",
	TypeRestoreTestFailed:     "A restore test failed: the backup could not be restored as expected. Inspect the test's log and test a manual restore of this object.",
	TypeLoginSecurity:         "Review the login attempts in the security log. If they are not yours, block the source and rotate credentials.",
	TypeJobFailed:             "Read the job's error in the console and run it again once the cause is fixed.",
}

// AlertView is what templates and JSON payloads see of an alert.
type AlertView struct {
	ID         int64      `json:"id"`
	Key        string     `json:"key"`
	Type       string     `json:"type"`
	Severity   string     `json:"severity"`
	State      string     `json:"state"`
	Title      string     `json:"title"`
	Detail     string     `json:"detail"`
	Host       string     `json:"host,omitempty"`
	Repository string     `json:"repository,omitempty"`
	Object     string     `json:"object,omitempty"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   time.Time  `json:"last_seen"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	Count      int        `json:"count"`
}

type alertTplData struct {
	A         AlertView
	Kind      string
	SevUpper  string
	Prev      string
	When      string
	Since     string
	LastSeen  string
	Resolved  string
	Where     string
	Hint      string
	Link      string
	AckedBy   string
	Generated string
}

var alertText = template.Must(template.New("t").Parse(`{{.A.Title}}

What:  {{if .A.Detail}}{{.A.Detail}}{{else}}{{.A.Title}}{{end}}
When:  {{.When}}
Where: {{.Where}}
State: {{.A.State}}{{if .AckedBy}} (acknowledged by {{.AckedBy}}){{end}}{{if gt .A.Count 1}}, seen {{.A.Count}} times{{end}}{{if .Prev}}
Severity raised from {{.Prev}} to {{.A.Severity}}.{{end}}
{{if eq .Kind "resolved"}}
The condition has cleared; nothing to do.
{{else}}
What to do: {{.Hint}}
{{end}}{{if .Link}}
Open in the console: {{.Link}}
{{end}}
--
incus-backup-server, alert {{.A.Key}}, {{.Generated}}
`))

var alertHTML = htmltemplate.Must(htmltemplate.New("h").Parse(`<!doctype html><html><body style="font-family:sans-serif;font-size:14px;color:#222">
<h2 style="margin:0 0 12px 0">{{.SevUpper}}: {{.A.Title}}</h2>
<table cellpadding="4" style="border-collapse:collapse">
<tr><td><b>What</b></td><td>{{if .A.Detail}}{{.A.Detail}}{{else}}{{.A.Title}}{{end}}</td></tr>
<tr><td><b>When</b></td><td>{{.When}}</td></tr>
<tr><td><b>Where</b></td><td>{{.Where}}</td></tr>
<tr><td><b>State</b></td><td>{{.A.State}}{{if .AckedBy}} (acknowledged by {{.AckedBy}}){{end}}{{if gt .A.Count 1}}, seen {{.A.Count}} times{{end}}</td></tr>
{{if .Prev}}<tr><td><b>Severity</b></td><td>raised from {{.Prev}} to {{.A.Severity}}</td></tr>{{end}}
</table>
{{if eq .Kind "resolved"}}<p>The condition has cleared; nothing to do.</p>{{else}}<p><b>What to do:</b> {{.Hint}}</p>{{end}}
{{if .Link}}<p><a href="{{.Link}}">Open in the console</a></p>{{end}}
<p style="color:#888;font-size:12px">incus-backup-server, alert {{.A.Key}}, {{.Generated}}</p>
</body></html>`))

func fmtTime(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func renderAlert(a *Alert, kind, prevSeverity, baseURL string, loc *time.Location, now time.Time) Message {
	if loc == nil {
		loc = time.UTC
	}
	v := a.view()
	sev := strings.ToUpper(a.Severity)
	var subject string
	switch kind {
	case "resolved":
		subject = "[RESOLVED] " + a.Title
	case "reminder":
		subject = fmt.Sprintf("[%s] Still firing: %s", sev, a.Title)
	default:
		subject = fmt.Sprintf("[%s] %s", sev, a.Title)
	}
	d := alertTplData{A: v, Kind: kind, SevUpper: sev, Hint: hints[a.Type], AckedBy: a.AckedBy, Generated: fmtTime(now, loc)}
	if d.Hint == "" {
		d.Hint = "Open the alert in the console for details."
	}
	if kind == "escalation" {
		d.Prev = prevSeverity
	}
	if kind == "resolved" && a.ResolvedAt != nil {
		d.When = fmt.Sprintf("from %s to %s (%s)", fmtTime(a.FirstSeen, loc), fmtTime(*a.ResolvedAt, loc), human(a.ResolvedAt.Sub(a.FirstSeen)))
	} else {
		d.When = fmt.Sprintf("since %s (%s), last checked %s", fmtTime(a.FirstSeen, loc), human(now.Sub(a.FirstSeen)), fmtTime(a.LastSeen, loc))
	}
	var where []string
	if a.Host != "" {
		where = append(where, "host "+a.Host)
	}
	if a.Repository != "" {
		where = append(where, "repository "+a.Repository)
	}
	if a.Object != "" {
		where = append(where, "object "+a.Object)
	}
	if len(where) == 0 {
		where = append(where, "server")
	}
	d.Where = strings.Join(where, ", ")
	if baseURL != "" {
		d.Link = baseURL + "/alerts/" + strconv.FormatInt(a.ID, 10)
	}
	var tb, hb bytes.Buffer
	_ = alertText.Execute(&tb, d)
	_ = alertHTML.Execute(&hb, d)
	sevOut := a.Severity
	return Message{Kind: kind, Severity: sevOut, Subject: clean(subject, 200), Text: tb.String(), HTML: hb.String(), Link: d.Link,
		Payload: map[string]any{"alert": v, "previous_severity": prevSeverity}}
}

func renderTest(channelName, baseURL string, now time.Time) Message {
	text := fmt.Sprintf(`This is a test message from incus-backup-server, sent to the channel %q at %s.

If you can read it, alerts will reach you here.
`, channelName, fmtTime(now, time.UTC))
	link := ""
	if baseURL != "" {
		link = baseURL + "/alerts"
		text += "\nOpen the console: " + link + "\n"
	}
	html := "<!doctype html><html><body style=\"font-family:sans-serif\"><p>" + htmltemplate.HTMLEscapeString(strings.ReplaceAll(text, "\n", " ")) + "</p></body></html>"
	return Message{Kind: "test", Severity: SeverityInfo, Subject: "[TEST] incus-backup notification test", Text: text, HTML: html, Link: link,
		Payload: map[string]any{"channel": channelName}}
}
