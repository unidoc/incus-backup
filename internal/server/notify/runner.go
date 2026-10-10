package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// tickLockKey serialises the evaluation and notification phase across processes ("ibk_notf").
const tickLockKey int64 = 0x69626b5f6e6f7466

// observation is one currently true condition.
type observation struct {
	Key, Type, Severity, Title, Detail, Host, Repo, Object string
}

// TickResult reports what one Tick did.
type TickResult struct {
	Skipped   bool     `json:"skipped"` // another Tick held the lock
	Observed  int      `json:"observed"`
	Fired     int      `json:"fired"`
	Resolved  int      `json:"resolved"`
	Enqueued  int      `json:"enqueued"`
	Delivered int      `json:"delivered"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"` // conditions that could not be evaluated (their alerts are left as they were)
}

func (e *Engine) tryLock(ctx context.Context) (func(), bool, error) {
	conn, err := e.Svc.DB.Pool().Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, tickLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, tickLockKey)
		conn.Release()
	}, true, nil
}

// Tick evaluates every condition at the service clock's now, updates the alerts, queues the
// notifications the rules ask for (and the digests that are due) and then delivers what is due.
// Two concurrent Ticks (two processes, or a slow one overlapping the next) never both evaluate:
// the loser returns Skipped and only helps with delivery.
func (e *Engine) Tick(ctx context.Context) (*TickResult, error) {
	e.init()
	res := &TickResult{}
	release, ok, err := e.tryLock(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		res.Skipped = true
		res.Delivered, res.Failed = e.Deliver(ctx)
		return res, nil
	}
	func() {
		defer release()
		now := e.now()
		conds, cerr := e.conditions(ctx)
		if cerr != nil {
			err = cerr
			return
		}
		st, serr := e.GetSettings(ctx)
		if serr != nil {
			err = serr
			return
		}
		paused, perr := e.pausedRepositories(ctx)
		if perr != nil {
			err = perr
			return
		}
		e.probeRepositories(ctx, conds[TypeRepositoryUnreachable], now, paused)
		obs, evaluated, errs := e.evaluate(ctx, conds, now)
		obs = withoutPaused(obs, paused)
		res.Observed, res.Errors = len(obs), errs
		for _, m := range errs {
			e.log.Error("notify: condition evaluation failed (its alerts stay as they are)", "err", m)
		}
		// Everything the transaction needs from outside is read first: a query on another pool
		// connection while this transaction holds row locks could wait for a connection held by
		// someone who waits for those locks (an application-level deadlock PostgreSQL cannot see).
		baseURL := e.BaseURL(ctx)
		digest, derr := e.prepareDigest(ctx, now)
		if derr != nil {
			res.Errors = append(res.Errors, "digest: "+derr.Error())
		}
		var changes []Change
		err = e.inTx(ctx, func(tx pgx.Tx) error {
			cs, err := e.sync(ctx, tx, obs, evaluated, conds, st, now, paused)
			if err != nil {
				return err
			}
			changes = append(changes, cs...)
			n, cs2, err := e.dispatch(ctx, tx, st, now, baseURL)
			if err != nil {
				return err
			}
			res.Enqueued += n
			changes = append(changes, cs2...)
			n, err = e.digests(ctx, tx, now, digest, baseURL)
			if err != nil {
				return err
			}
			res.Enqueued += n
			return e.housekeeping(ctx, tx, st, now)
		})
		for _, c := range changes {
			switch c.Transition {
			case "fired":
				res.Fired++
			case "resolved", "expired":
				res.Resolved++
			}
		}
		if err == nil {
			e.publish(changes...)
		}
	}()
	if err != nil {
		return res, err
	}
	res.Delivered, res.Failed = e.Deliver(ctx)
	return res, nil
}

// dispatchIfFree runs the notification phase when no Tick is running (used by Emit).
func (e *Engine) dispatchIfFree(ctx context.Context) (int, error) {
	release, ok, err := e.tryLock(ctx)
	if err != nil || !ok {
		return 0, err
	}
	defer release()
	st, err := e.GetSettings(ctx)
	if err != nil {
		return 0, err
	}
	now := e.now()
	baseURL := e.BaseURL(ctx)
	var n int
	var changes []Change
	err = e.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		n, changes, err = e.dispatch(ctx, tx, st, now, baseURL)
		return err
	})
	if err == nil {
		e.publish(changes...)
	}
	return n, err
}

func (e *Engine) openAlert(ctx context.Context, tx pgx.Tx, o observation, source string, now time.Time) ([]Change, error) {
	a, err := scanAlert(tx.QueryRow(ctx, `INSERT INTO notify_alerts (key, type, severity, title, detail, host, repository, object, source, state, first_seen, last_seen, last_change)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'firing',$10,$10,$10) RETURNING `+alertCols,
		o.Key, o.Type, o.Severity, o.Title, o.Detail, o.Host, o.Repo, o.Object, source, now))
	if err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, a.ID, now, "fired", a.Severity); err != nil {
		return nil, err
	}
	return []Change{a.change("fired", now)}, nil
}

// observe applies a fresh observation to an existing alert (reopening it when resolved).
func (e *Engine) observe(ctx context.Context, tx pgx.Tx, a *Alert, o observation, now time.Time, countIt bool) ([]Change, error) {
	if a.State == StateResolved {
		a.Episode++
		a.Count++
		a.State, a.Severity, a.Title, a.Detail, a.Host, a.Repository, a.Object = StateFiring, o.Severity, o.Title, o.Detail, o.Host, o.Repo, o.Object
		a.FirstSeen, a.LastSeen, a.LastChange, a.ResolvedAt, a.ResolvedSilently = now, now, now, nil, false
		a.AckedBy, a.AckedAt, a.AckComment, a.SilenceID = "", nil, "", nil
		if _, err := tx.Exec(ctx, `UPDATE notify_alerts SET state='firing', episode=$2, count=$3, severity=$4, title=$5, detail=$6, host=$7, repository=$8, object=$9,
			first_seen=$10, last_seen=$10, last_change=$10, resolved_at=NULL, resolved_silently=false, acked_by='', acked_at=NULL, ack_comment='', silence_id=NULL
			WHERE id=$1`, a.ID, a.Episode, a.Count, a.Severity, a.Title, a.Detail, a.Host, a.Repository, a.Object, now); err != nil {
			return nil, err
		}
		if err := addEvent(ctx, tx, a.ID, now, "fired", a.Severity); err != nil {
			return nil, err
		}
		return []Change{a.change("fired", now)}, nil
	}
	var cs []Change
	transition := ""
	if sevRank(o.Severity) > sevRank(a.Severity) {
		transition = "escalated"
		if err := addEvent(ctx, tx, a.ID, now, "escalated", a.Severity+" -> "+o.Severity); err != nil {
			return nil, err
		}
	} else if o.Severity != a.Severity || o.Title != a.Title || o.Detail != a.Detail || countIt {
		transition = "updated"
	}
	if countIt {
		a.Count++
	}
	a.Severity, a.Title, a.Detail, a.Host, a.Repository, a.Object, a.LastSeen = o.Severity, o.Title, o.Detail, o.Host, o.Repo, o.Object, now
	if _, err := tx.Exec(ctx, `UPDATE notify_alerts SET severity=$2, title=$3, detail=$4, host=$5, repository=$6, object=$7, last_seen=$8, count=$9 WHERE id=$1`,
		a.ID, a.Severity, a.Title, a.Detail, a.Host, a.Repository, a.Object, now, a.Count); err != nil {
		return nil, err
	}
	if transition != "" {
		cs = append(cs, a.change(transition, now))
	}
	return cs, nil
}

func (e *Engine) resolveAlert(ctx context.Context, tx pgx.Tx, a *Alert, now time.Time, silent bool, kind string) ([]Change, error) {
	a.State, a.ResolvedAt, a.ResolvedSilently, a.LastChange, a.SilenceID = StateResolved, &now, silent, now, nil
	if _, err := tx.Exec(ctx, `UPDATE notify_alerts SET state='resolved', resolved_at=$2, resolved_silently=$3, last_change=$2, silence_id=NULL WHERE id=$1`,
		a.ID, now, silent); err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, a.ID, now, kind, ""); err != nil {
		return nil, err
	}
	return []Change{a.change(kind, now)}, nil
}

// sync turns the set of current observations into alert transitions.
func (e *Engine) sync(ctx context.Context, tx pgx.Tx, obs []observation, evaluated map[string]bool, conds map[string]condCfg, st *Settings, now time.Time, paused map[string]bool) ([]Change, error) {
	keys := make([]string, 0, len(obs))
	for _, o := range obs {
		keys = append(keys, o.Key)
	}
	rows, err := tx.Query(ctx, `SELECT `+alertCols+` FROM notify_alerts WHERE (source='condition' AND state<>'resolved') OR key = ANY($1) ORDER BY id FOR UPDATE`, keys)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		byKey[a.Key] = a
	}
	rows.Close()
	var changes []Change
	seen := map[string]bool{}
	var fired []*Alert
	for _, o := range obs {
		if seen[o.Key] {
			continue
		}
		seen[o.Key] = true
		a := byKey[o.Key]
		if a == nil {
			cs, err := e.openAlert(ctx, tx, o, "condition", now)
			if err != nil {
				return nil, err
			}
			changes = append(changes, cs...)
			continue
		}
		if a.Source != "condition" {
			continue // an event key never collides with a condition key (types are disjoint); be safe anyway
		}
		wasResolved := a.State == StateResolved
		cs, err := e.observe(ctx, tx, a, o, now, false)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
		if wasResolved {
			fired = append(fired, a)
		}
	}
	for _, a := range byKey {
		if a.Source != "condition" || a.State == StateResolved || seen[a.Key] || !evaluated[a.Type] {
			continue
		}
		if a.Repository != "" && paused[a.Repository] {
			continue // the repository is in maintenance: its alerts are neither refreshed nor resolved
		}
		cc, ok := conds[a.Type]
		silent := !ok || !cc.enabled // switching a condition off ends its alerts without a message
		cs, err := e.resolveAlert(ctx, tx, a, now, silent, "resolved")
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}
	// flapping: an alert that fired again flap_threshold times within the window is held
	for _, a := range fired {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM notify_alert_events WHERE alert_id=$1 AND kind='fired' AND at > $2`,
			a.ID, now.Add(-time.Duration(st.FlapWindowMinutes)*time.Minute)).Scan(&n); err != nil {
			return nil, err
		}
		if n >= st.FlapThreshold && !a.Flapping {
			a.Flapping = true
			if _, err := tx.Exec(ctx, `UPDATE notify_alerts SET flapping=true WHERE id=$1`, a.ID); err != nil {
				return nil, err
			}
			if err := addEvent(ctx, tx, a.ID, now, "flapping", fmt.Sprintf("%d transitions in %d min", n, st.FlapWindowMinutes)); err != nil {
				return nil, err
			}
			changes = append(changes, a.change("flapping", now))
		}
	}
	// a flapping alert is released once it stayed in one state for a whole window
	srows, err := tx.Query(ctx, `UPDATE notify_alerts SET flapping=false WHERE flapping AND last_change <= $1 RETURNING `+alertCols,
		now.Add(-time.Duration(st.FlapWindowMinutes)*time.Minute))
	if err != nil {
		return nil, err
	}
	var stable []*Alert
	for srows.Next() {
		a, err := scanAlert(srows)
		if err != nil {
			srows.Close()
			return nil, err
		}
		stable = append(stable, a)
	}
	srows.Close()
	for _, a := range stable {
		if err := addEvent(ctx, tx, a.ID, now, "stable", a.State); err != nil {
			return nil, err
		}
		changes = append(changes, a.change("stable", now))
	}
	// emitted events expire silently
	erows, err := tx.Query(ctx, `SELECT `+alertCols+` FROM notify_alerts WHERE source='event' AND state<>'resolved' AND last_seen < $1 FOR UPDATE`,
		now.Add(-time.Duration(st.EventExpireMinutes)*time.Minute))
	if err != nil {
		return nil, err
	}
	var expired []*Alert
	for erows.Next() {
		a, err := scanAlert(erows)
		if err != nil {
			erows.Close()
			return nil, err
		}
		expired = append(expired, a)
	}
	erows.Close()
	for _, a := range expired {
		cs, err := e.resolveAlert(ctx, tx, a, now, true, "expired")
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}
	cs, err := e.applySilences(ctx, tx, now, 0)
	if err != nil {
		return nil, err
	}
	return append(changes, cs...), nil
}

type ruleState struct {
	exists          bool
	firedEpisode    int
	resolvedEpisode int
	lastSent        *time.Time
	sentSeverity    string
	seq             int
}

// dispatch decides, per rule and alert, what to tell the channels, and queues it. It is a pure state
// comparison (what the alert is vs. what the rule last said), so running it twice changes nothing.
func (e *Engine) dispatch(ctx context.Context, tx pgx.Tx, st *Settings, now time.Time, baseURL string) (int, []Change, error) {
	rrows, err := tx.Query(ctx, `SELECT `+ruleCols+` FROM notify_rules WHERE enabled AND kind='alert' ORDER BY name`)
	if err != nil {
		return 0, nil, err
	}
	var rules []*Rule
	for rrows.Next() {
		r, err := scanRule(rrows)
		if err != nil {
			rrows.Close()
			return 0, nil, err
		}
		rules = append(rules, r)
	}
	rrows.Close()
	if len(rules) == 0 {
		return 0, nil, nil
	}
	enabledCh, err := enabledChannels(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	arows, err := tx.Query(ctx, `SELECT `+alertCols+` FROM notify_alerts a WHERE state<>'resolved'
		OR EXISTS (SELECT 1 FROM notify_rule_state rs WHERE rs.alert_id=a.id AND rs.fired_episode > rs.resolved_episode) ORDER BY id`)
	if err != nil {
		return 0, nil, err
	}
	var alerts []*Alert
	for arows.Next() {
		a, err := scanAlert(arows)
		if err != nil {
			arows.Close()
			return 0, nil, err
		}
		alerts = append(alerts, a)
	}
	arows.Close()
	if len(alerts) == 0 {
		return 0, nil, nil
	}
	states := map[string]*ruleState{}
	srows, err := tx.Query(ctx, `SELECT alert_id, rule_id::text, fired_episode, resolved_episode, last_sent_at, sent_severity, seq FROM notify_rule_state`)
	if err != nil {
		return 0, nil, err
	}
	for srows.Next() {
		var aid int64
		var rid string
		s := &ruleState{exists: true}
		if err := srows.Scan(&aid, &rid, &s.firedEpisode, &s.resolvedEpisode, &s.lastSent, &s.sentSeverity, &s.seq); err != nil {
			srows.Close()
			return 0, nil, err
		}
		states[fmt.Sprintf("%d/%s", aid, rid)] = s
	}
	srows.Close()
	dedupe := time.Duration(st.DedupeMinutes) * time.Minute
	enq := 0
	var changes []Change
	for _, r := range rules {
		var chans []string
		for _, c := range r.ChannelIDs {
			if enabledCh[c] {
				chans = append(chans, c)
			}
		}
		if len(chans) == 0 {
			continue
		}
		for _, a := range alerts {
			sk := fmt.Sprintf("%d/%s", a.ID, r.ID)
			s := states[sk]
			if s == nil {
				s = &ruleState{}
			}
			announced := s.firedEpisode > s.resolvedEpisode // the channels believe this alert is firing
			if !(r.matches(a) || (announced && r.matchesIgnoringSeverity(a))) {
				continue
			}
			if a.Flapping {
				continue // held until stable
			}
			quiet := r.quiet(now) && !(r.CriticalBypass && a.Severity == SeverityCritical)
			kind, prev := "", ""
			changed := false
			switch {
			case a.State == StateResolved:
				if !announced {
					continue
				}
				if a.ResolvedSilently || !r.SendResolved {
					s.resolvedEpisode, changed = s.firedEpisode, true
					break
				}
				if quiet {
					continue
				}
				kind = "resolved"
				s.resolvedEpisode = s.firedEpisode
			case a.State == StateSilenced:
				continue
			case s.firedEpisode < a.Episode:
				if announced {
					// it resolved and fired again before the channels heard about the resolution:
					// for them it simply kept firing
					s.firedEpisode, changed = a.Episode, true
					break
				}
				if !r.matches(a) || quiet || (s.lastSent != nil && now.Sub(*s.lastSent) < dedupe) {
					continue
				}
				kind = "firing"
				s.firedEpisode = a.Episode
			default: // announced and still open
				switch {
				case sevRank(a.Severity) > sevRank(s.sentSeverity):
					if quiet {
						continue
					}
					kind, prev = "escalation", s.sentSeverity
				case a.State == StateFiring && r.matches(a) && r.RepeatMinutes > 0 && s.lastSent != nil &&
					now.Sub(*s.lastSent) >= time.Duration(r.RepeatMinutes)*time.Minute && now.Sub(*s.lastSent) >= dedupe:
					if quiet {
						continue
					}
					kind = "reminder"
				case sevRank(a.Severity) < sevRank(s.sentSeverity):
					s.sentSeverity, changed = a.Severity, true // de-escalated: a later rise is news again
				}
			}
			if kind != "" {
				msg := renderAlert(a, kind, prev, baseURL, r.loc, now)
				for _, c := range chans {
					ok, err := enqueue(ctx, tx, c, r.ID, a.ID, fmt.Sprintf("a%d/r%s/c%s/%d", a.ID, r.ID, c, s.seq+1), msg, now)
					if err != nil {
						return 0, nil, err
					}
					if ok {
						enq++
					}
				}
				s.seq++
				t := now
				s.lastSent = &t
				if kind != "resolved" {
					s.sentSeverity = a.Severity
				}
				if err := addEvent(ctx, tx, a.ID, now, "notified", kind+" via rule "+r.Name); err != nil {
					return 0, nil, err
				}
				changed = true
			}
			if changed {
				if _, err := tx.Exec(ctx, `INSERT INTO notify_rule_state (alert_id, rule_id, fired_episode, resolved_episode, last_sent_at, sent_severity, seq)
					VALUES ($1,$2::uuid,$3,$4,$5,$6,$7) ON CONFLICT (alert_id, rule_id) DO UPDATE SET fired_episode=EXCLUDED.fired_episode,
					resolved_episode=EXCLUDED.resolved_episode, last_sent_at=EXCLUDED.last_sent_at, sent_severity=EXCLUDED.sent_severity, seq=EXCLUDED.seq`,
					a.ID, r.ID, s.firedEpisode, s.resolvedEpisode, s.lastSent, s.sentSeverity, s.seq); err != nil {
					return 0, nil, err
				}
				states[sk] = s
			}
		}
	}
	return enq, changes, nil
}

func enabledChannels(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM notify_channels WHERE enabled`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// enqueue queues one message for one channel; a duplicate dedupe key is ignored (returns false).
func enqueue(ctx context.Context, tx pgx.Tx, channelID, ruleID string, alertID int64, dedupeKey string, m Message, now time.Time) (bool, error) {
	payload, _ := json.Marshal(m.Payload)
	var rid *string
	if ruleID != "" {
		rid = &ruleID
	}
	var aid *int64
	if alertID != 0 {
		aid = &alertID
	}
	ct, err := tx.Exec(ctx, `INSERT INTO notify_deliveries (channel_id, rule_id, alert_id, kind, dedupe_key, severity, subject, body_text, body_html, link, payload,
		state, next_attempt_at, created) VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,'pending',$12,$12) ON CONFLICT (dedupe_key) DO NOTHING`,
		channelID, rid, aid, m.Kind, dedupeKey, m.Severity, m.Subject, m.Text, m.HTML, m.Link, payload, now)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}

func (e *Engine) housekeeping(ctx context.Context, tx pgx.Tx, st *Settings, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM notify_deliveries WHERE created < $1 AND state IN ('sent','dead')`,
		now.AddDate(0, 0, -st.DeliveryRetentionDays)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM notify_alerts WHERE state='resolved' AND resolved_at < $1`, now.AddDate(0, 0, -st.AlertRetentionDays)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM notify_silences WHERE ends_at < $1`, now.AddDate(0, 0, -st.AlertRetentionDays))
	return err
}

func errText(err error) string {
	s := err.Error()
	return strings.TrimSpace(s)
}

// pausedRepositories returns the repositories in maintenance (mode read-only or offline). The mode
// column comes with the repository-mode migration; without it every repository is online.
func (e *Engine) pausedRepositories(ctx context.Context) (map[string]bool, error) {
	p := e.Svc.DB.Pool()
	var has bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name='repositories' AND column_name='mode')`).Scan(&has); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if !has {
		return out, nil
	}
	rows, err := p.Query(ctx, `SELECT name FROM repositories WHERE mode <> 'online'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// withoutPaused drops observations about repositories in maintenance: an operator who put a
// repository read-only or offline knows; its missed backups, failed probes and so on are expected.
func withoutPaused(obs []observation, paused map[string]bool) []observation {
	if len(paused) == 0 {
		return obs
	}
	out := obs[:0]
	for _, o := range obs {
		if o.Repo == "" || !paused[o.Repo] {
			out = append(out, o)
		}
	}
	return out
}
