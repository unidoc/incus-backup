# Alerts and notifications

A backup system that fails silently is not a backup system. incus-backup-server watches its own
control plane and tells you when something needs a human: a backup that did not happen, a host that
went quiet, data that failed verification, a replica that falls behind, storage that runs full.

The engine (`internal/server/notify`) works like a monitoring system, not like a mail-per-event
script:

- an **alert** is a state that has a stable identity (its *key*), not a message;
- it **fires** when its condition becomes true, stays open while the condition holds, and
  **resolves by itself** when the condition clears;
- **rules** decide which **channels** hear about which alerts, and when (reminders, quiet hours,
  escalation, the "resolved" message);
- messages go through a durable **delivery queue** in PostgreSQL with retries, backoff, a
  dead-letter state and a per-channel rate limit.

Every threshold is data in PostgreSQL (`notify_settings`, `notify_conditions`, `notify_rules`,
`notify_channels`), changed through the API/CLI/UI. There is no configuration file.

## Alert types

The runner evaluates these conditions every minute (`Tick`) over data the server already has.
Defaults are deliberately conservative: an alert should mean "look at this now", never noise.

| Type | Fires when (defaults) | Severity | Key |
|---|---|---|---|
| `backup_missed` | an object whose policy has a schedule has no committed snapshot for `interval_factor` (1.5) × the schedule interval + `grace_minutes` (60). A daily schedule alerts after 37 h. | warning; critical after `critical_factor` (3) × interval + grace | `backup_missed:<repo>/<group id>` |
| `backup_failed` | the host's latest finished backup run is `failed` or `partial` | failed: `failed_severity` (critical); partial: warning | `backup_failed:<repo>/<host>` |
| `host_offline` | a backup host has not contacted the server for `after_minutes` (120). The daemon contacts its control server every minute. | warning; critical after `critical_after_minutes` (1440) | `host_offline:<repo>/<host>` |
| `maintenance_failed` | the latest run of a maintenance job (gc, verify, scrub, retention, reconcile, heal) failed | warning | `maintenance_failed:<repo>/<kind>` |
| `corruption` | snapshots of an object failed verification, or chunks are quarantined | critical | `corruption:<repo>/<group id>`, `corrupt_chunks:<repo>` |
| `sync_failed` | the latest run of an enabled sync job failed or was partial | warning | `sync_failed:<repo>/<job>` |
| `sync_lag` | a scheduled sync job has not succeeded for `lag_hours` (48) | warning | `sync_lag:<repo>/<job>` |
| `capacity_warning` | the filesystem is `used_percent` (90 %) full, or the 30-day growth trend fills it within `full_within_days` (14) | warning; critical within `critical_within_days` (3) or at 0 bytes free | `capacity:<repo>` |
| `repository_unreachable` | `failures` (2) consecutive probes of an S3 repository failed; probes run every `probe_minutes` (10) | critical | `repository_unreachable:<repo>` |
| `cert_expiry` | a certificate expires within `warn_days` (21), or cannot be read | warning; critical within `critical_days` (7) or expired | `cert_expiry:<name>` |
| `restore_test_failed` | the latest restore test of an object failed | warning | `restore_test_failed:<repo>/<group id>` |
| `login_security`, `job_failed`, others | emitted by other parts of the server (`Emit`) | the event's, else the type's | `<type>:<event key>` |

Details that keep this quiet:

- **New objects** are measured from when they were first seen, so they do not alert before their
  first backup was due.
- **Retired objects** are skipped: an object its host has not reported for `stale_object_days` (7)
  days, e.g. a deleted instance, no longer alerts forever.
- **Objects without an owner host** are skipped by `backup_missed`. These are replicas received
  by sync, or objects rebuilt from the datastore. `sync_lag` covers replicas.
- **Manual schedules** promise nothing and never alert. This applies to policies and to sync jobs.
- **The governing policy** is the server's: an admin assignment (object > project > host) wins over
  the policy pinned when the object was first seen, exactly as retention decides it.
- **Disabled hosts** are ignored.
- **A condition that cannot be evaluated** is reported in the Tick result and logged, and its open
  alerts are left untouched. This happens when a query fails, the certificate source errors, or the
  restore-test source fails. A broken check never sends a false "resolved".
- **Switching a condition off** resolves its open alerts without a message.
- **Object storage** reports no honest free-space figure, so it gets no capacity alerts.
- **A probe that hangs** counts as failed after `ProbeTimeout` (60 s).

Change a threshold with `SetCondition` (params are merged; unknown parameter names are refused):

```json
{"type": "backup_missed", "enabled": true, "severity": "warning", "params": {"grace_minutes": 120}}
```

## The alert state machine

```
            condition true                    condition false
  (none) ───────────────▶ firing ─────────────────────────────▶ resolved
                           │  ▲                                    │
              acknowledge  │  │ unacknowledge          fires again │ (same row, episode+1,
                           ▼  │                                    │  count+1, ack cleared)
                       acknowledged                                ▼
                                                                 firing
     a matching silence turns firing/acknowledged into "silenced" while it lasts
```

- **firing**: open, nobody has taken it.
- **acknowledged**: a user said "I am on it" (`Acknowledge(id, user, comment)`). Reminders stop.
  Escalation and resolution are still announced. The acknowledgement ends with the episode.
- **silenced**: a silence matches. Nothing is sent while it lasts. When the silence ends and the
  alert still fires, it is announced then, if it never was.
- **resolved**: the condition cleared. A later recurrence reopens the same alert as a new episode.

Every alert has `key`, `type`, `severity`, `title`, `detail`, `host`, `repository`, `object`,
`first_seen` (start of the current episode), `last_seen`, `state`, `resolved_at`, `count` and its
timeline (`GetAlert`: fired, escalated, notified, acknowledged, silenced, flapping, stable, resolved,
expired).

**Events** (`Emit`) have no clearing condition. Two things resolve them:
- the same key is emitted with `resolve: true`, which sends a "resolved" message;
- `event_expire_minutes` (1440) pass after the event was last seen, which resolves it silently.

Repeated events of the same key count up on the open alert instead of opening new ones.

## Rules

A rule routes alerts to one or more channels. Its fields:

| Field | Default | Meaning |
|---|---|---|
| `event_types` | all | alert types, globs allowed (`login_*`) |
| `min_severity` | warning | info, warning or critical |
| `host_glob`, `repo_glob`, `object_glob` | any | `*` matches anything (including `/`), `?` one character. Objects are named `project/name`, e.g. `default/bosch`. |
| `repeat_minutes` | 1440 | reminder interval while an alert keeps firing unacknowledged; 0 = never |
| `quiet_start`, `quiet_end`, `timezone` | none, UTC | quiet hours (may wrap midnight), IANA zone |
| `critical_bypass` | true | critical alerts ignore quiet hours |
| `send_resolved` | true | send a "resolved" message |
| `kind` | alert | `digest` = the daily summary instead (sent at `digest_at`, default 08:00, in `timezone`) |

What a rule sends is a pure comparison between the alert and what the rule last told its channels
(`notify_rule_state`). Running it twice changes nothing.

- **firing**: once per episode. This happens when the alert opens, when it reaches the rule's
  minimum severity, when its silence ends, or when its flapping hold ends.
- **escalation**: the severity rose above what was last sent, e.g. a missed backup becoming
  critical. This is sent even when the alert is acknowledged.
- **reminder**: every `repeat_minutes` while it fires unacknowledged and unsilenced.
- **resolved**: only if a firing message went out. An alert nobody was told about resolves quietly.
  If it resolved and fired again before the "resolved" message could go out (quiet hours), the
  channels simply keep believing it fires.

Quiet hours hold notifications. They do not drop them: a held message goes out when the window ends,
if it is still true. An alert that fires and clears entirely inside the quiet window sends nothing.

### Noise control

- **Dedupe**: `dedupe_minutes` (10). A rule never sends a firing or reminder for the same alert
  within this window of its previous message. A quick "resolved, fired again" waits.
- **Flapping**: an alert that fires `flap_threshold` (3) times within `flap_window_minutes` (60) is
  marked flapping, and nothing about it is sent. It is released once it stayed in one state for a
  whole window. If it is still firing then, one firing message goes out. If it resolved and its
  last announced state was "firing", one resolved message goes out.
- **Silences**: matchers on type, key, host, repository and object globs, with an `until`, a
  `reason` and an `author`. At least one matcher is required: no "silence everything" by accident.
  Silences apply at once and end with `Unsilence`.
- **Rate limit**: `rate_per_hour` per channel (30; 0 = unlimited). Excess messages wait in the
  queue. They are never dropped.

## Daily digest

A digest rule sends one summary a day of the last 24 hours:
- backup runs ok/partial/failed;
- the protected percentage: scheduled objects that are not overdue;
- verifications ok/failed;
- stored bytes and their 24 h growth per repository;
- the open alerts;
- counts of new and resolved alerts.

The last send time is recorded in PostgreSQL, so two servers or a restart never send it twice.
`Digest(ctx, now)` returns the same report for the UI.

## Channels

| Kind | Config (non-secret, stored in `notify_channels.config`) | Secret (sealed) |
|---|---|---|
| `smtp` | `host`, `port` (587/465/25), `tls`: `starttls` (default) / `tls` (implicit) / `none`, `auth`: `plain` / `login` / "" (pick from the server's list), `username`, `from`, `to` [..], `helo`, `tls_ca_pem`, `tls_server_name` | `password` |
| `webhook` | `format`: `json` (default) / `slack` / `discord`, `headers` (non-secret), `signature_header` | `url` (always secret: Slack/Discord URLs carry their token), `hmac_secret`, `headers` (e.g. `Authorization`) |
| `ntfy` | `server` (default https://ntfy.sh), `topic`, `username`, `priorities` | `token` (bearer) or `password` (basic, with `username`) |
| `gotify` | `url`, `priorities` | `token` (sent as `X-Gotify-Key`, never in the URL) |

Plus per channel: `enabled`, `rate_per_hour` (30), `timeout_seconds` (15).

The safety rules:
- **Config is decoded strictly.** An unknown field is refused, so a password typed into the config
  by mistake is never stored in clear.
- **SMTP never authenticates without TLS.**
  - `tls: none` with a username is refused when the channel is configured.
  - The client refuses AUTH on an unencrypted session, even to localhost.
  - With `starttls`, a server that does not offer STARTTLS is an error; there is no fallback to clear.
- **HTTP channels never follow redirects.** Secret headers would follow them.
- **Errors never carry URLs.** Errors are rebuilt as `Post https://host/…: <cause>`, and every
  stored secret value is redacted from any error text.
- **Untrusted names cannot inject anything.** Host and object names come from the hosts and are
  untrusted.
  - Control characters are removed from subjects (no header injection).
  - Mail subjects are RFC 2047-encoded.
  - The HTML part is escaped by `html/template`.
  - Slack text is escaped (`<!channel>` becomes text).
  - Discord messages carry `allowed_mentions: {parse: []}`, so they cannot ping `@everyone`.
  - ntfy uses JSON publishing, so non-ASCII titles work.

Priority mapping (override per channel with `priorities`, keys info/warning/critical/resolved):
- ntfy: info 3, warning 4, critical 5, resolved 2.
- Gotify: info 3, warning 6, critical 9, resolved 2.

### Webhook payload and signature

`format: json` POSTs:

```json
{"version": 1, "kind": "firing", "severity": "critical", "subject": "[CRITICAL] bosch: no successful backup for 27 h",
 "text": "...", "link": "https://backup.example.com/alerts/42", "sent_at": "2026-10-08T07:00:00Z",
 "alert": {"id": 42, "key": "backup_missed:custody/…", "type": "backup_missed", "severity": "critical", "state": "firing",
           "title": "...", "detail": "...", "host": "node1", "repository": "custody", "object": "default/bosch",
           "first_seen": "...", "last_seen": "...", "count": 1},
 "previous_severity": ""}
```

`kind` is one of `firing`, `reminder`, `escalation`, `resolved`, `digest` or `test`. A digest carries
`digest` instead of `alert`.

With `hmac_secret` set, the request carries `X-Incus-Backup-Timestamp: <unix seconds>` and
`X-Incus-Backup-Signature: sha256=<hex>`. The `<hex>` is the HMAC-SHA256 of
`timestamp + "." + body`, keyed with the secret. Verify it like this, and reject old timestamps:

```python
expected = hmac.new(secret, f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
ok = hmac.compare_digest("sha256=" + expected, request.headers["X-Incus-Backup-Signature"])
```

### Testing a channel

`TestChannel` sends a real test message right away. It bypasses rules, queue and rate limit, and
works on a disabled channel. It returns the server's own answer as the error, for example:
- `smtp: authentication failed: 535 5.7.8 authentication credentials invalid`
- `smtp: the server does not offer STARTTLS`
- `HTTP 401 Unauthorized: …`
- `Post https://hooks.example.com/…: dial tcp …: connection refused`

The attempt is recorded in the delivery log as kind `test`.

## Messages

Subjects are plain and readable, for example:
- `[CRITICAL] bosch: no successful backup for 27 h`
- `[WARNING] Still firing: node1: offline for 3 h`
- `[RESOLVED] …`
- `[DIGEST] incus-backup: 12 ok, 1 failed, 96% protected`

Bodies say:
- **what** happened: the detail, including the error;
- **when**: since when, for how long, last checked;
- **where**: host, repository and object;
- **what to do**: a hint for each alert type;
- a **link** to the console.

The link is built from the `server_settings` key `base_url` (`SetBaseURL`), for example
`https://backup.example.com` + `/alerts/<id>`. Without it, messages carry no link. Times are shown
in the rule's time zone. Mail is multipart/alternative, plain text plus HTML.

## Delivery queue

Notifications are rendered when queued (`notify_deliveries`). The log therefore shows exactly what
was sent.

- **Order and parallelism.** Channels are served in parallel, in order within a channel.
- **Retries.** A failure is retried after `backoff_base_seconds` × 2^(attempt−1) (60 s, 120 s,
  240 s, …), capped at `backoff_max_seconds` (6 h).
- **Dead letter.** After `max_attempts` (8) a delivery is `dead`: it shows in the delivery log with
  its last error, and `RetryDelivery` puts it back in the queue. A disabled channel dead-letters
  its queued messages. Deleting a channel deletes its queue and log entries.
- **No duplicates.** Each queued message has a unique dedupe key (alert, rule, channel,
  notification number). The evaluation and notification phase runs under a PostgreSQL advisory
  lock: of two concurrent Ticks (two processes, or a slow Tick overlapping the next) exactly one
  evaluates, and the other only helps delivering. Workers claim rows with
  `FOR UPDATE SKIP LOCKED` and a lease.
- **At-least-once.** A crash after the remote accepted a message but before the database recorded
  it resends that message after the 15-minute claim lease.
- **Retention.** Sent and dead deliveries are kept `delivery_retention_days` (30). Resolved alerts
  and ended silences are kept `alert_retention_days` (90).

## Secrets

Channel secrets are sealed in the existing secret store (`credentials`, kinds `smtp`, `token`,
`webhook`; XChaCha20-Poly1305 with `secrets.key` from the state directory, bound to the credential
id). No function of this package returns them:
- listings show `has_secret` and a credential-free `target` (`smtp://host:587 (starttls) -> …`,
  `https://hooks.slack.com/…`);
- updating with `secret: {}` removes the secret;
- updating with a new secret rotates it;
- deleting a channel deletes its secret.

`service.ReadSecret` is for the server's own use only and must never be wired to a route. The S3
credential commands (`credential list/rotate/delete`) never touch these secrets.

## Go API (for the HTTP/CLI layer)

`notify.New(svc)` returns the `*Engine`. Its methods:
- **Channels:** `ListChannels`, `GetChannel`, `CreateChannel`, `UpdateChannel`, `DeleteChannel`,
  `TestChannel`.
- **Rules:** `ListRules`, `GetRule`, `CreateRule` (start from `DefaultRule`), `UpdateRule`,
  `DeleteRule`.
- **Alerts:** `ListAlerts(AlertFilter)`, `GetAlert`, `Acknowledge`, `Unacknowledge`.
- **Silences:** `CreateSilence`, `Unsilence`, `ListSilences`.
- **Deliveries:** `ListDeliveries(DeliveryFilter)`, `RetryDelivery`.
- **Settings:** `GetSettings`, `SetSettings`, `ListConditions`, `SetCondition`, `BaseURL`,
  `SetBaseURL`.
- **Events:** `Emit(Event)`.
- **Running:** `Tick`, `Deliver`, `Digest`.
- **Live feed:** `Subscribe`, `Unsubscribe` and `Run`.

`Subscribe` returns a bounded channel (256) of `Change` values for the UI's live feed. Publishing
never blocks: a slow subscriber loses changes (`DroppedChanges`). The feed is in-process, so a change
made by a separate CLI process shows up in `ListAlerts`, not in the feed.

`Run(ctx, time.Minute)` is the daemon loop. It calls `Tick` every minute and delivers right away
when `Emit` queued something.

Two hooks are wired by the daemon:
- `Engine.CertStatus func(ctx) ([]CertItem, error)`;
- `Engine.RestoreTests RestoreTestSource`.

While a hook is nil, its condition is not evaluated.
