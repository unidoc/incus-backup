package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Alert is one alert (one row per stable key; a resolved alert that fires again is reopened as a
// new episode of the same row).
type Alert struct {
	ID               int64      `json:"id"`
	Key              string     `json:"key"`
	Type             string     `json:"type"`
	Severity         string     `json:"severity"`
	Title            string     `json:"title"`
	Detail           string     `json:"detail"`
	Host             string     `json:"host"`
	Repository       string     `json:"repository"`
	Object           string     `json:"object"`
	Source           string     `json:"source"` // condition or event
	State            string     `json:"state"`
	Episode          int        `json:"episode"`
	FirstSeen        time.Time  `json:"first_seen"`
	LastSeen         time.Time  `json:"last_seen"`
	ResolvedAt       *time.Time `json:"resolved_at"`
	ResolvedSilently bool       `json:"resolved_silently"`
	Count            int        `json:"count"`
	Flapping         bool       `json:"flapping"`
	LastChange       time.Time  `json:"last_change"`
	AckedBy          string     `json:"acked_by"`
	AckedAt          *time.Time `json:"acked_at"`
	AckComment       string     `json:"ack_comment"`
	SilenceID        *int64     `json:"silence_id"`
}

func (a *Alert) view() AlertView {
	return AlertView{ID: a.ID, Key: a.Key, Type: a.Type, Severity: a.Severity, State: a.State, Title: a.Title, Detail: a.Detail, Host: a.Host,
		Repository: a.Repository, Object: a.Object, FirstSeen: a.FirstSeen, LastSeen: a.LastSeen, ResolvedAt: a.ResolvedAt, Count: a.Count}
}

const alertCols = `id, key, type, severity, title, detail, host, repository, object, source, state, episode, first_seen, last_seen, resolved_at,
	resolved_silently, count, flapping, last_change, acked_by, acked_at, ack_comment, silence_id`

func scanAlert(r pgx.Row) (*Alert, error) {
	var a Alert
	err := r.Scan(&a.ID, &a.Key, &a.Type, &a.Severity, &a.Title, &a.Detail, &a.Host, &a.Repository, &a.Object, &a.Source, &a.State, &a.Episode,
		&a.FirstSeen, &a.LastSeen, &a.ResolvedAt, &a.ResolvedSilently, &a.Count, &a.Flapping, &a.LastChange, &a.AckedBy, &a.AckedAt, &a.AckComment, &a.SilenceID)
	return &a, err
}

func (a *Alert) change(transition string, at time.Time) Change {
	return Change{Kind: "alert", At: at, AlertID: a.ID, Key: a.Key, Type: a.Type, Severity: a.Severity, State: a.State, Title: a.Title, Transition: transition}
}

// AlertFilter selects alerts. Zero fields match everything; Active = every state but resolved.
type AlertFilter struct {
	States   []string  `json:"states"`
	Active   bool      `json:"active"`
	Type     string    `json:"type"`
	Severity string    `json:"severity"` // minimum
	Host     string    `json:"host"`     // glob
	Since    time.Time `json:"since"`    // last_seen >= since
	Limit    int       `json:"limit"`    // default 200, max 1000
}

// ListAlerts returns alerts, most recently seen first.
func (e *Engine) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q := `SELECT ` + alertCols + ` FROM notify_alerts WHERE true`
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if len(f.States) > 0 {
		q += ` AND state = ANY(` + arg(f.States) + `)`
	}
	if f.Active {
		q += ` AND state <> 'resolved'`
	}
	if f.Type != "" {
		q += ` AND type = ` + arg(f.Type)
	}
	if f.Severity != "" {
		if !validSeverity(f.Severity) {
			return nil, bad("invalid severity")
		}
		var sevs []string
		for _, s := range []string{SeverityInfo, SeverityWarning, SeverityCritical} {
			if sevRank(s) >= sevRank(f.Severity) {
				sevs = append(sevs, s)
			}
		}
		q += ` AND severity = ANY(` + arg(sevs) + `)`
	}
	if !f.Since.IsZero() {
		q += ` AND last_seen >= ` + arg(f.Since)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	fetch := limit
	if f.Host != "" {
		fetch = 1000 // the host glob is applied below
	}
	q += ` ORDER BY (state='resolved'), last_seen DESC, id DESC LIMIT ` + arg(fetch)
	rows, err := e.Svc.DB.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hostOK := glob(f.Host)
	out := []Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		if f.Host != "" && !hostOK(a.Host) {
			continue
		}
		if len(out) < limit {
			out = append(out, *a)
		}
	}
	return out, rows.Err()
}

// AlertEvent is one entry of an alert's timeline.
type AlertEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

// AlertDetail is an alert with its timeline and deliveries.
type AlertDetail struct {
	Alert      Alert        `json:"alert"`
	Events     []AlertEvent `json:"events"`
	Deliveries []Delivery   `json:"deliveries"`
}

// GetAlert returns an alert with its history.
func (e *Engine) GetAlert(ctx context.Context, id int64) (*AlertDetail, error) {
	a, err := scanAlert(e.Svc.DB.Pool().QueryRow(ctx, `SELECT `+alertCols+` FROM notify_alerts WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: alert %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	d := &AlertDetail{Alert: *a, Events: []AlertEvent{}}
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT at, kind, detail FROM notify_alert_events WHERE alert_id=$1 ORDER BY at DESC, id DESC LIMIT 200`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ev AlertEvent
		if err := rows.Scan(&ev.At, &ev.Kind, &ev.Detail); err != nil {
			rows.Close()
			return nil, err
		}
		d.Events = append(d.Events, ev)
	}
	rows.Close()
	d.Deliveries, err = e.ListDeliveries(ctx, DeliveryFilter{AlertID: id, Limit: 100})
	return d, err
}

func addEvent(ctx context.Context, q pgx.Tx, alertID int64, at time.Time, kind, detail string) error {
	_, err := q.Exec(ctx, `INSERT INTO notify_alert_events (alert_id, at, kind, detail) VALUES ($1,$2,$3,$4)`, alertID, at, kind, detail)
	return err
}

// Acknowledge marks a firing alert as being handled: no more reminders; escalation and resolution are
// still announced. The acknowledgement ends when the alert resolves.
func (e *Engine) Acknowledge(ctx context.Context, id int64, user, comment string) (*Alert, error) {
	user = clean(user, 100)
	if user == "" {
		return nil, bad("acknowledging needs a user")
	}
	now := e.now()
	var a *Alert
	err := e.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAlert(tx.QueryRow(ctx, `SELECT `+alertCols+` FROM notify_alerts WHERE id=$1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: alert %d", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if a.State == StateResolved {
			return fmt.Errorf("%w: the alert is resolved", ErrConflict)
		}
		if a.State == StateFiring {
			a.State = StateAcknowledged
		}
		a.AckedBy, a.AckedAt, a.AckComment = user, &now, clean(comment, 500)
		if _, err := tx.Exec(ctx, `UPDATE notify_alerts SET state=$2, acked_by=$3, acked_at=$4, ack_comment=$5 WHERE id=$1`, id, a.State, a.AckedBy, now, a.AckComment); err != nil {
			return err
		}
		return addEvent(ctx, tx, id, now, "acknowledged", user+": "+a.AckComment)
	})
	if err != nil {
		return nil, err
	}
	e.publish(a.change("acknowledged", now))
	return a, nil
}

// Unacknowledge removes an acknowledgement (reminders start again).
func (e *Engine) Unacknowledge(ctx context.Context, id int64, user string) (*Alert, error) {
	now := e.now()
	var a *Alert
	err := e.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAlert(tx.QueryRow(ctx, `SELECT `+alertCols+` FROM notify_alerts WHERE id=$1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: alert %d", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if a.AckedAt == nil {
			return fmt.Errorf("%w: the alert is not acknowledged", ErrConflict)
		}
		if a.State == StateAcknowledged {
			a.State = StateFiring
		}
		a.AckedBy, a.AckedAt, a.AckComment = "", nil, ""
		if _, err := tx.Exec(ctx, `UPDATE notify_alerts SET state=$2, acked_by='', acked_at=NULL, ack_comment='' WHERE id=$1`, id, a.State); err != nil {
			return err
		}
		return addEvent(ctx, tx, id, now, "unacknowledged", clean(user, 100))
	})
	if err != nil {
		return nil, err
	}
	e.publish(a.change("unacknowledged", now))
	return a, nil
}

// Silence mutes every alert matching all of its non-empty globs until Until.
type Silence struct {
	ID         int64     `json:"id"`
	TypeGlob   string    `json:"type_glob"`
	KeyGlob    string    `json:"key_glob"`
	HostGlob   string    `json:"host_glob"`
	RepoGlob   string    `json:"repo_glob"`
	ObjectGlob string    `json:"object_glob"`
	StartsAt   time.Time `json:"starts_at"`
	Until      time.Time `json:"until"`
	Reason     string    `json:"reason"`
	Author     string    `json:"author"`
	Created    time.Time `json:"created"`
	Active     bool      `json:"active"`
}

func (s *Silence) matches(a *Alert) bool {
	return glob(s.TypeGlob)(a.Type) && glob(s.KeyGlob)(a.Key) && glob(s.HostGlob)(a.Host) && glob(s.RepoGlob)(a.Repository) && glob(s.ObjectGlob)(a.Object)
}

// CreateSilence adds a silence (starting now unless StartsAt is later) and applies it at once.
func (e *Engine) CreateSilence(ctx context.Context, s Silence) (*Silence, error) {
	now := e.now()
	for _, g := range []string{s.TypeGlob, s.KeyGlob, s.HostGlob, s.RepoGlob, s.ObjectGlob} {
		if !validGlob(g) {
			return nil, bad("invalid matcher")
		}
	}
	if s.TypeGlob == "" && s.KeyGlob == "" && s.HostGlob == "" && s.RepoGlob == "" && s.ObjectGlob == "" {
		return nil, bad("a silence needs at least one matcher (type, key, host, repository or object)")
	}
	s.Reason, s.Author = clean(s.Reason, 500), clean(s.Author, 100)
	if s.Reason == "" || s.Author == "" {
		return nil, bad("a silence needs a reason and an author")
	}
	if s.StartsAt.IsZero() || s.StartsAt.Before(now) {
		s.StartsAt = now
	}
	if !s.Until.After(s.StartsAt) {
		return nil, bad("until must be in the future")
	}
	s.Created = now
	err := e.Svc.DB.Pool().QueryRow(ctx, `INSERT INTO notify_silences (type_glob, key_glob, host_glob, repo_glob, object_glob, starts_at, ends_at, reason, author, created)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, s.TypeGlob, s.KeyGlob, s.HostGlob, s.RepoGlob, s.ObjectGlob, s.StartsAt, s.Until, s.Reason, s.Author, now).Scan(&s.ID)
	if err != nil {
		return nil, err
	}
	s.Active = !s.StartsAt.After(now)
	e.publish(Change{Kind: "silence", At: now, ID: s.ID, Transition: "created"})
	if err := e.applySilencesNow(ctx); err != nil {
		return nil, err
	}
	return &s, nil
}

// Unsilence ends a silence now.
func (e *Engine) Unsilence(ctx context.Context, id int64) error {
	now := e.now()
	ct, err := e.Svc.DB.Pool().Exec(ctx, `UPDATE notify_silences SET ends_at=$2 WHERE id=$1 AND ends_at > $2`, id, now)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: active silence %d", ErrNotFound, id)
	}
	e.publish(Change{Kind: "silence", At: now, ID: id, Transition: "ended"})
	return e.applySilencesNow(ctx)
}

// ListSilences returns silences; active only unless all.
func (e *Engine) ListSilences(ctx context.Context, all bool) ([]Silence, error) {
	now := e.now()
	q := `SELECT id, type_glob, key_glob, host_glob, repo_glob, object_glob, starts_at, ends_at, reason, author, created FROM notify_silences`
	var args []any
	if !all {
		q += ` WHERE ends_at > $1`
		args = append(args, now)
	}
	rows, err := e.Svc.DB.Pool().Query(ctx, q+` ORDER BY ends_at DESC, id DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Silence{}
	for rows.Next() {
		var s Silence
		if err := rows.Scan(&s.ID, &s.TypeGlob, &s.KeyGlob, &s.HostGlob, &s.RepoGlob, &s.ObjectGlob, &s.StartsAt, &s.Until, &s.Reason, &s.Author, &s.Created); err != nil {
			return nil, err
		}
		s.Active = !s.StartsAt.After(now) && s.Until.After(now)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (e *Engine) activeSilences(ctx context.Context, q pgx.Tx, now time.Time) ([]Silence, error) {
	rows, err := q.Query(ctx, `SELECT id, type_glob, key_glob, host_glob, repo_glob, object_glob FROM notify_silences WHERE starts_at <= $1 AND ends_at > $1 ORDER BY id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Silence
	for rows.Next() {
		var s Silence
		if err := rows.Scan(&s.ID, &s.TypeGlob, &s.KeyGlob, &s.HostGlob, &s.RepoGlob, &s.ObjectGlob); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// applySilences recomputes firing/acknowledged/silenced for every open alert.
// only limits it to one alert (0 = all).
func (e *Engine) applySilences(ctx context.Context, tx pgx.Tx, now time.Time, only int64) ([]Change, error) {
	sils, err := e.activeSilences(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+alertCols+` FROM notify_alerts WHERE state <> 'resolved' AND ($1 = 0 OR id = $1) ORDER BY id FOR UPDATE`, only)
	if err != nil {
		return nil, err
	}
	var open []*Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		open = append(open, a)
	}
	rows.Close()
	var changes []Change
	for _, a := range open {
		var sid *int64
		for i := range sils {
			if sils[i].matches(a) {
				id := sils[i].ID
				sid = &id
				break
			}
		}
		want := StateFiring
		switch {
		case sid != nil:
			want = StateSilenced
		case a.AckedAt != nil:
			want = StateAcknowledged
		}
		if want == a.State && ((sid == nil) == (a.SilenceID == nil)) {
			continue
		}
		prev := a.State
		a.State, a.SilenceID = want, sid
		if _, err := tx.Exec(ctx, `UPDATE notify_alerts SET state=$2, silence_id=$3 WHERE id=$1`, a.ID, a.State, sid); err != nil {
			return nil, err
		}
		if prev != want {
			kind := "unsilenced"
			if want == StateSilenced {
				kind = "silenced"
			}
			if err := addEvent(ctx, tx, a.ID, now, kind, ""); err != nil {
				return nil, err
			}
			changes = append(changes, a.change(kind, now))
		}
	}
	return changes, nil
}

func (e *Engine) applySilencesNow(ctx context.Context) error {
	now := e.now()
	var cs []Change
	err := e.inTx(ctx, func(tx pgx.Tx) error {
		// wait for a running Tick: both lock many alerts, and taking turns avoids a deadlock
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, tickLockKey); err != nil {
			return err
		}
		var err error
		cs, err = e.applySilences(ctx, tx, now, 0)
		return err
	})
	e.publish(cs...)
	return err
}

func (e *Engine) inTx(ctx context.Context, f func(pgx.Tx) error) error {
	tx, err := e.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Event is something that happened (a burst of failed logins, a failed job) fed in by other parts of
// the server. Events have no clearing condition: an event alert resolves when the same Key is emitted
// with Resolve, or silently event_expire_minutes after it was last seen.
type Event struct {
	Type       string `json:"type"`     // login_security, job_failed, or another non-condition type
	Key        string `json:"key"`      // identity within the type (e.g. the source address); default ""
	Severity   string `json:"severity"` // default: the type's condition severity, else warning
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Object     string `json:"object"`
	Resolve    bool   `json:"resolve"`
}

// Emit records an event as an alert (opening, refreshing or resolving it) and runs the notification
// rules at once when no Tick is running (otherwise the next Tick picks it up). It never sends over
// the network itself: Run delivers right after (Wake).
func (e *Engine) Emit(ctx context.Context, ev Event) error {
	e.init()
	ev.Type = strings.TrimSpace(ev.Type)
	if ev.Type == "" || !nameOK(ev.Type) {
		return bad("invalid event type")
	}
	if conditionTypes[ev.Type] {
		return bad("%s is evaluated by the server itself and cannot be emitted", ev.Type)
	}
	conds, err := e.conditions(ctx)
	if err != nil {
		return err
	}
	cc, known := conds[ev.Type]
	if known && !cc.enabled {
		return nil
	}
	if ev.Severity == "" {
		ev.Severity = SeverityWarning
		if known {
			ev.Severity = cc.severity
		}
	}
	if !validSeverity(ev.Severity) {
		return bad("invalid severity")
	}
	ev.Title = clean(ev.Title, 200)
	if ev.Title == "" && !ev.Resolve {
		return bad("an event needs a title")
	}
	key := ev.Type + ":" + clean(ev.Key, 200)
	now := e.now()
	var changes []Change
	body := func(tx pgx.Tx) error {
		a, err := scanAlert(tx.QueryRow(ctx, `SELECT `+alertCols+` FROM notify_alerts WHERE key=$1 FOR UPDATE`, key))
		if errors.Is(err, pgx.ErrNoRows) {
			if ev.Resolve {
				return nil
			}
			o := observation{Key: key, Type: ev.Type, Severity: ev.Severity, Title: ev.Title, Detail: cleanBlock(ev.Detail, 4000),
				Host: clean(ev.Host, 200), Repo: clean(ev.Repository, 200), Object: clean(ev.Object, 300)}
			c, err := e.openAlert(ctx, tx, o, "event", now)
			if err != nil {
				return err
			}
			changes = append(changes, c...)
			return nil
		}
		if err != nil {
			return err
		}
		if ev.Resolve {
			if a.State != StateResolved {
				c, err := e.resolveAlert(ctx, tx, a, now, false, "resolved")
				if err != nil {
					return err
				}
				changes = append(changes, c...)
			}
			return nil
		}
		o := observation{Key: key, Type: ev.Type, Severity: ev.Severity, Title: ev.Title, Detail: cleanBlock(ev.Detail, 4000),
			Host: clean(ev.Host, 200), Repo: clean(ev.Repository, 200), Object: clean(ev.Object, 300)}
		c, err := e.observe(ctx, tx, a, o, now, true)
		if err != nil {
			return err
		}
		changes = append(changes, c...)
		return nil
	}
	err = e.inTx(ctx, func(tx pgx.Tx) error {
		if err := body(tx); err != nil {
			return err
		}
		if len(changes) == 0 {
			return nil
		}
		// only this alert: locking every open alert here could deadlock against a running Tick
		cs, err := e.applySilences(ctx, tx, now, changes[0].AlertID)
		changes = append(changes, cs...)
		return err
	})
	if err != nil {
		return err
	}
	e.publish(changes...)
	// notify now if no Tick holds the lock; a running Tick (or the next one) covers it otherwise
	if n, derr := e.dispatchIfFree(ctx); derr != nil {
		e.log.Error("notify: dispatching after an event failed", "err", derr)
	} else if n > 0 {
		e.poke()
	}
	return nil
}
