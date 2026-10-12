package notify

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Delivery is one queued or sent message (the delivery log).
type Delivery struct {
	ID            int64      `json:"id"`
	ChannelID     string     `json:"channel_id"`
	ChannelName   string     `json:"channel_name"`
	RuleID        string     `json:"rule_id"`
	AlertID       *int64     `json:"alert_id"`
	Kind          string     `json:"kind"`
	Severity      string     `json:"severity"`
	Subject       string     `json:"subject"`
	BodyText      string     `json:"body_text"`
	Link          string     `json:"link"`
	State         string     `json:"state"` // pending, sending, sent, dead
	Attempts      int        `json:"attempts"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	LastError     string     `json:"last_error"`
	Created       time.Time  `json:"created"`
	SentAt        *time.Time `json:"sent_at"`
}

// DeliveryFilter selects deliveries.
type DeliveryFilter struct {
	ChannelID string `json:"channel_id"`
	AlertID   int64  `json:"alert_id"`
	State     string `json:"state"`
	Limit     int    `json:"limit"` // default 200, max 1000
}

const deliveryCols = `d.id, d.channel_id::text, COALESCE(c.name,''), COALESCE(d.rule_id::text,''), d.alert_id, d.kind, d.severity, d.subject, d.body_text, d.link,
	d.state, d.attempts, d.next_attempt_at, d.last_error, d.created, d.sent_at`

func scanDelivery(r pgx.Row) (*Delivery, error) {
	var d Delivery
	err := r.Scan(&d.ID, &d.ChannelID, &d.ChannelName, &d.RuleID, &d.AlertID, &d.Kind, &d.Severity, &d.Subject, &d.BodyText, &d.Link,
		&d.State, &d.Attempts, &d.NextAttemptAt, &d.LastError, &d.Created, &d.SentAt)
	return &d, err
}

// ListDeliveries returns the delivery log, newest first.
func (e *Engine) ListDeliveries(ctx context.Context, f DeliveryFilter) ([]Delivery, error) {
	q := `SELECT ` + deliveryCols + ` FROM notify_deliveries d LEFT JOIN notify_channels c ON c.id=d.channel_id WHERE true`
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if f.ChannelID != "" {
		q += ` AND (d.channel_id::text=` + arg(f.ChannelID) + ` OR c.name=` + arg(f.ChannelID) + `)`
	}
	if f.AlertID != 0 {
		q += ` AND d.alert_id=` + arg(f.AlertID)
	}
	if f.State != "" {
		q += ` AND d.state=` + arg(f.State)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	q += ` ORDER BY d.id DESC LIMIT ` + arg(limit)
	rows, err := e.Svc.DB.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// RetryDelivery puts a dead-lettered delivery back in the queue (attempts start again).
func (e *Engine) RetryDelivery(ctx context.Context, id int64) error {
	ct, err := e.Svc.DB.Pool().Exec(ctx, `UPDATE notify_deliveries SET state='pending', attempts=0, next_attempt_at=$2 WHERE id=$1 AND state='dead'`, id, e.now())
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: dead delivery %d", ErrNotFound, id)
	}
	e.poke()
	return nil
}

// redact removes every secret value from an error text (defence in depth: senders already avoid them).
func redact(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

func (e *Engine) sendTo(ctx context.Context, ch *channelRow, sec *ChannelSecret, m Message, now time.Time) error {
	timeout := time.Duration(ch.TimeoutSeconds) * time.Second
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var err error
	switch ch.Kind {
	case KindSMTP:
		err = e.sendSMTP(sctx, ch.Config, sec, m, timeout, now)
	case KindWebhook:
		err = e.sendWebhook(sctx, ch.Config, sec, m, timeout, now)
	case KindNtfy:
		err = e.sendNtfy(sctx, ch.Config, sec, m, timeout)
	case KindGotify:
		err = e.sendGotify(sctx, ch.Config, sec, m, timeout)
	default:
		err = fmt.Errorf("unknown channel kind %q", ch.Kind)
	}
	if err != nil {
		return errors.New(redact(clean(err.Error(), 1000), sec.values()))
	}
	return nil
}

type claimed struct {
	id       int64
	channel  string
	attempts int
	msg      Message
}

// backoff is base * 2^(attempts-1), capped.
func backoff(st *Settings, attempts int) time.Duration {
	d := time.Duration(st.BackoffBaseSeconds) * time.Second
	max := time.Duration(st.BackoffMaxSeconds) * time.Second
	for i := 1; i < attempts && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// Deliver sends every due message (queued, or retry due), channel by channel in parallel and in
// order within a channel. Delivery is at-least-once: a crash between sending and recording means
// the message goes out again after its claim lease ends.
func (e *Engine) Deliver(ctx context.Context) (sent, failed int) {
	e.init()
	now := e.now()
	st, err := e.GetSettings(ctx)
	if err != nil {
		e.log.Error("notify: reading settings", "err", err)
		return 0, 0
	}
	rows, err := e.Svc.DB.Pool().Query(ctx, `UPDATE notify_deliveries SET state='sending', claimed_until=$2 WHERE id IN (
		SELECT id FROM notify_deliveries WHERE (state='pending' AND next_attempt_at <= $1) OR (state='sending' AND claimed_until < $1)
		ORDER BY id LIMIT 500 FOR UPDATE SKIP LOCKED)
		RETURNING id, channel_id::text, attempts, kind, severity, subject, body_text, body_html, link, payload`, now, now.Add(15*time.Minute))
	if err != nil {
		e.log.Error("notify: claiming deliveries", "err", err)
		return 0, 0
	}
	byChan := map[string][]claimed{}
	var order []string
	for rows.Next() {
		var c claimed
		var payload []byte
		if err := rows.Scan(&c.id, &c.channel, &c.attempts, &c.msg.Kind, &c.msg.Severity, &c.msg.Subject, &c.msg.Text, &c.msg.HTML, &c.msg.Link, &payload); err != nil {
			rows.Close()
			e.log.Error("notify: reading claimed deliveries", "err", err)
			return 0, 0
		}
		_ = json.Unmarshal(payload, &c.msg.Payload)
		if _, ok := byChan[c.channel]; !ok {
			order = append(order, c.channel)
		}
		byChan[c.channel] = append(byChan[c.channel], c)
	}
	rows.Close()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, chID := range order {
		items := byChan[chID]
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, f := e.deliverChannel(ctx, chID, items, st, now)
			mu.Lock()
			sent += s
			failed += f
			mu.Unlock()
		}()
	}
	wg.Wait()
	return sent, failed
}

func (e *Engine) deliverChannel(ctx context.Context, chID string, items []claimed, st *Settings, now time.Time) (sent, failed int) {
	p := e.Svc.DB.Pool()
	ch, err := e.channelRow(ctx, chID)
	if err == nil && !ch.Enabled {
		err = errors.New("channel disabled")
	}
	var sec *ChannelSecret
	if err == nil {
		sec, err = e.loadSecret(ctx, ch.secretID)
	}
	if err != nil {
		for _, it := range items {
			e.markFailed(ctx, it, errText(err), st, now, true)
			failed++
		}
		return
	}
	for _, it := range items {
		if ch.RatePerHour > 0 {
			var n int
			var oldest *time.Time
			if err := p.QueryRow(ctx, `SELECT count(*), min(sent_at) FROM notify_deliveries WHERE channel_id=$1::uuid AND state='sent' AND sent_at > $2`,
				chID, now.Add(-time.Hour)).Scan(&n, &oldest); err == nil && n >= ch.RatePerHour {
				next := now.Add(time.Minute)
				if oldest != nil {
					next = oldest.Add(time.Hour)
				}
				_, _ = p.Exec(ctx, `UPDATE notify_deliveries SET state='pending', claimed_until=NULL, next_attempt_at=$2, last_error=$3 WHERE id=$1`,
					it.id, next, fmt.Sprintf("held by the channel rate limit (%d per hour)", ch.RatePerHour))
				continue
			}
		}
		if err := e.sendTo(ctx, ch, sec, it.msg, now); err != nil {
			e.markFailed(ctx, it, err.Error(), st, now, false)
			failed++
			e.log.Warn("notify: delivery failed", "channel", ch.Name, "delivery", it.id, "attempt", it.attempts+1, "err", err.Error())
			continue
		}
		if _, err := p.Exec(ctx, `UPDATE notify_deliveries SET state='sent', attempts=attempts+1, sent_at=$2, claimed_until=NULL, last_error='' WHERE id=$1`, it.id, now); err != nil {
			e.log.Error("notify: recording a sent delivery", "err", err)
		}
		sent++
		e.publish(Change{Kind: "delivery", At: now, ID: it.id, Transition: "sent", Severity: it.msg.Severity, Title: it.msg.Subject})
	}
	return
}

func (e *Engine) markFailed(ctx context.Context, it claimed, msg string, st *Settings, now time.Time, permanent bool) {
	attempts := it.attempts + 1
	state, next := "pending", now.Add(backoff(st, attempts))
	transition := "failed"
	if permanent || attempts >= st.MaxAttempts {
		state, next, transition = "dead", now, "dead"
	}
	if _, err := e.Svc.DB.Pool().Exec(ctx, `UPDATE notify_deliveries SET state=$2, attempts=$3, next_attempt_at=$4, last_error=$5, claimed_until=NULL WHERE id=$1`,
		it.id, state, attempts, next, msg); err != nil {
		e.log.Error("notify: recording a failed delivery", "err", err)
	}
	e.publish(Change{Kind: "delivery", At: now, ID: it.id, Transition: transition, Severity: it.msg.Severity, Title: it.msg.Subject})
}

// TestChannel sends a real test message now (bypassing queue, rules and rate limit) and returns the
// delivery record; err carries the server's answer (SMTP reply, HTTP status) for the UI.
func (e *Engine) TestChannel(ctx context.Context, id string) (*Delivery, error) {
	e.init()
	ch, err := e.channelRow(ctx, id)
	if err != nil {
		return nil, err
	}
	sec, err := e.loadSecret(ctx, ch.secretID)
	if err != nil {
		return nil, err
	}
	now := e.now()
	m := renderTest(ch.Name, e.BaseURL(ctx), now)
	serr := e.sendTo(ctx, ch, sec, m, now)
	state, lastErr := "sent", ""
	var sentAt *time.Time
	if serr != nil {
		state, lastErr = "dead", serr.Error()
	} else {
		sentAt = &now
	}
	var did int64
	var key [8]byte
	_, _ = rand.Read(key[:])
	payload, _ := json.Marshal(m.Payload)
	if err := e.Svc.DB.Pool().QueryRow(ctx, `INSERT INTO notify_deliveries (channel_id, kind, dedupe_key, severity, subject, body_text, body_html, link, payload,
		state, attempts, next_attempt_at, last_error, created, sent_at) VALUES ($1::uuid,'test',$2,$3,$4,$5,$6,$7,$8,$9,1,$10,$11,$10,$12) RETURNING id`,
		ch.ID, fmt.Sprintf("test/%s/%x", ch.ID, key), m.Severity, m.Subject, m.Text, m.HTML, m.Link, payload, state, now, lastErr, sentAt).Scan(&did); err != nil {
		return nil, err
	}
	d, err := scanDelivery(e.Svc.DB.Pool().QueryRow(ctx, `SELECT `+deliveryCols+` FROM notify_deliveries d LEFT JOIN notify_channels c ON c.id=d.channel_id WHERE d.id=$1`, did))
	if err != nil {
		return nil, err
	}
	if serr != nil {
		return d, serr
	}
	return d, nil
}
