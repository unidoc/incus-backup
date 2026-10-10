# Management API: server information, setup checklist, notification tick and simulator, token overview

These routes live in `internal/server/mgmt/routes_config.go`. Their roles are set in that file's `init()` and
added to the `perms` table. They follow the conventions of `docs/api.md`.

| route | role | answer |
|---|---|---|
| `GET /about` | readonly | `{version, started, uptime_seconds, go_version, database_ok, database_error?, datastore_count, instance_name}`. `started` is when the server process started. `database_ok` comes from a pool ping (5 s timeout). |
| `GET /setup-checklist` | admin | `{items[], done, total, complete}`, where each item is `{id, title, ok, detail, route}`. `route` names the console page that fixes the item. |
| `POST /notifications/tick` | admin | One evaluation and delivery round right now, as the daemon runs every minute. Answers `notify.TickResult` `{skipped, observed, fired, resolved, enqueued, delivered, failed, errors?}`. `skipped: true` means another tick held the advisory lock, so nothing was evaluated twice. |
| `POST /notifications/simulate` | operator | Body `{type, severity, host?, repository?, object?, key?}`. Answers `{at, silenced_by[], rules[], deliver}`. Each rule is `{rule_id, rule_name, matched, reason, quiet, channels[]}`. It uses the runner's own matching and changes nothing. |
| `GET /api-tokens` | admin | Every API token of every user, never the secret: `{id, name, role, created_at, expires_at?, last_used_at?, owner, owner_role, owner_disabled, effective_role, expired}`. `effective_role` is what the token can do right now: the lower of its own role and its owner's role, or `""` when the token is unusable. |
| `DELETE /api-tokens/{id}` | admin | Revoke any user's token. Answers 204, or 404 when the token does not exist. |

## Setup checklist items

The checklist has eight items:

- `channel`: an enabled notification channel exists.
- `channel_tested`: a `test` delivery with state `sent` exists for an enabled channel.
- `rule`: an enabled `alert` rule with at least one channel exists.
- `host`: an enabled host with backup scope exists.
- `policy`: a policy assignment exists, or an object has a retention policy.
- `restore_test`: a restore test passed.
- `admin_2fa`: every enabled administrator has TOTP turned on.
- `base_url`: the base URL is set.

The checklist only reads (`service.SetupChecklist`).

## `GET /certificates` and the web listener

`GET /certificates` answers `{report, web}`. `web` is `webtls.Info` for the HTTPS listener:
`{mode, subject, dns_names, not_after, fingerprint, issuer, days_left, auto_renew}`.
- `serve` wires it through `mgmt.Server.WebCert` (`cmd/incus-backup-server/web.go`, an atomic pointer that `serveWeb` sets).
- It is `null` when the server has no web listener.
