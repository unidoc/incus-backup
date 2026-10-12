# Management API (`/api/v1`)

The management API is what the web UI, the server CLI and automation all use. It is one router served on
two listeners:

| listener | who | authentication |
|---|---|---|
| unix admin socket (`<state_dir>/admin.sock`, mode 0660) | the server CLI, local scripts | none: whoever can open the socket is `local-admin` (role admin) |
| HTTPS web listener (`web_listen`, default `:8443`) | browsers and automation | session cookie + CSRF header, or `Authorization: Bearer ibt_…` |

Hosts (backup agents) do **not** use this API; they use the mutual-TLS agent API on `listen` (`:8007`).

## Conventions

* JSON in, JSON out (`Content-Type: application/json`), UTF-8. Request bodies are limited to 1 MiB and unknown
  fields are rejected (400).
* Errors are `{"error": "human readable text"}` with the status: 400 bad request, 401 not signed in, 403 role
  too low / CSRF failure, 404 not found, 409 conflict (exists, protected, busy), 429 throttled,
  503 repository in maintenance (read-only / offline), 500 server fault.
  A 401 from `POST /session` with `"totp_required": true` means the password was right and a second factor is needed;
  `second_factor` says which ones the user has (`{"totp": bool, "webauthn": bool}`) and `ticket` starts the security-key
  step (`POST /session/webauthn/begin`). Passkeys and single sign-on: [authentication.md](authentication.md).
* `?repository=NAME` selects the repository on routes that need one; it may be omitted when exactly one
  repository exists.
* Times are RFC 3339 UTC. Byte counts are integers. Secrets (S3 keys, enrolment tokens, API tokens,
  recovery codes) are accepted or shown **once** and never returned again.
* State-changing requests from a browser must send `X-CSRF-Token` (the `csrf` value returned by
  `POST /session` / `GET /session`) and must not carry a foreign `Origin`. Bearer-token requests need
  neither.
* Every state-changing request is written to the audit log (`GET /audit`); bodies are never logged.
* Live updates: `GET /events` is a Server-Sent-Events stream. Event types: `job`, `alert`, `run`,
  `repository`, `host`. Each event is `{"type": …, "data": …}`; after a reconnect the UI refetches.
  A principal with per-repository roles only receives events of repositories it may read.
* Machine-readable contract: `GET /openapi.json` (any signed-in principal) is an OpenAPI 3.1 document of
  every route, with its role (`x-role`), how its repository is found (`x-repository-binding`), parameters
  and JSON schemas derived from the Go types. `incus-backup-server openapi` prints the same document
  offline. The web console renders it at **Settings → API** (`/settings/api`): routes by area, schemas,
  "try it" with your session and a curl command for scripts. It is generated from the route metadata in
  `internal/server/mgmt/routes_meta.go`; a test fails when a route has no metadata, so it cannot drift.

## Roles

`readonly` < `restore` < `operator` < `admin`. Each route's required role is the table in
`internal/server/mgmt/perms.go` (reviewed and tested as a table; a route without an entry cannot start).
Roles can be narrowed per repository for users and API tokens; see [permissions.md](permissions.md).
A user's global role may then be `none`.

* readonly: look at everything except credentials, users, audit, settings.
* restore: + browse snapshots, fetch the encrypted index/manifest/chunks, start restores.
* operator: + run verify/sync/backup-now jobs, edit policies/assignments/schedules, notes, protection,
  enable/disable hosts, probe repositories.
* admin: + repositories, credentials, enrolment/onboarding, retention apply (prune), GC, reconcile,
  forget snapshots, maintenance settings and modes, users, tokens of others, audit, settings, notifications,
  certificates.

## Session and people

| route | role | notes |
|---|---|---|
| `GET /ping` | readonly | `{ok, version}` |
| `GET /setup` | public | `{needed: bool}`: true while no user exists |
| `POST /setup` | public | `{token, name, password}` creates the first admin (token printed at start / `user setup-token`) |
| `POST /session` | public | `{name, password, code?}` → cookie `ib_session` + `{user, csrf, expires_at, method}`; `code` is a TOTP or recovery code; 403 `local_login_disabled` when only single sign-on is allowed |
| `POST /session/webauthn/begin`, `/finish` | public | passkey sign-in (`{}`) or the security-key second factor (`{ticket}`) → the same session answer ([authentication.md](authentication.md)) |
| `GET /auth/oidc/providers`, `GET /auth/oidc/{provider}/login`, `/callback` | public | single sign-on: sign-in options, redirect to the identity provider, its redirect back |
| `GET /session` | any | `{user, csrf, access?, method, enrol_2fa_required?}`; `access` = `{global, repositories: {name: role}}` when per-repository roles apply; `enrol_2fa_required`: only the enrolment routes answer until a second factor is set up |
| `DELETE /session` | any | sign out |
| `GET/POST /users`, `PUT/DELETE /users/{name}` | admin | `{name, password, role, display, email, must_change}`; `PUT` takes `{role?, display_name?, email?, disabled?}`; the last admin cannot be removed |
| `POST /users/{name}/password`, `DELETE /users/{name}/totp` | admin | reset password (ends sessions) / remove 2FA |
| `POST /me/password` | any user | `{current, new}`; other sessions end |
| `POST /me/totp` | any user | start 2FA enrolment → `{secret, uri}` (render `uri` as QR); `{password}` when the sign-in is older than 10 minutes (else 400 `password_required`); refused for single sign-on accounts |
| `POST /me/totp/confirm` | any user | `{code, password?}` → `{recovery_codes[10]}` (shown once); `password` as for `POST /me/totp` |
| `DELETE /me/totp` | any user | `{password}` |
| `GET /me/sessions`, `DELETE /me/sessions/{id}` | any user | list / revoke own sessions |
| `GET/POST /me/tokens`, `DELETE /me/tokens/{id}` | any user | API tokens: `{name, role, ttl_hours, repositories?, password?}`; role ≤ own role; a token always expires: `ttl_hours` 0 or missing = 90 days, at most 365 days (400 above); `password` is needed when the sign-in is older than 10 minutes (400 `password_required`, 401 wrong password), as for passkeys; accounts that sign in only through single sign-on get no tokens (400); `repositories` restricts the token (no global rights then); secret shown once |
| `GET/PUT /users/{name}/access` | admin | `{global_role, scoped, scopes: [{repository, role}]}`; `PUT` takes `{scoped, scopes}` (see permissions.md) |
| `GET /api-tokens`, `DELETE /api-tokens/{id}` | admin | every user's tokens (owner, effective role, repositories, expiry); revoke any |
| `GET /audit` | admin | `?actor=&action=prefix&before=<id>&limit=`; newest first |
| `GET /me/webauthn`, `POST /me/webauthn/register/begin`, `/finish`, `PATCH/DELETE /me/webauthn/{id}` | any user | own passkeys and security keys (register/remove: a sign-in within 10 minutes or `{password}`) |
| `DELETE /users/{name}/webauthn`, `GET/POST /users/{name}/identities`, `DELETE /users/{name}/identities/{provider}` | admin | reset a user's keys; link a user to an identity provider by e-mail |
| `GET/POST /oidc/providers`, `PUT/DELETE /oidc/providers/{name}`, `POST /oidc/providers/{name}/test` | admin | identity providers (client secret write-only) and a discovery test |
| `GET/PUT /auth/settings` | admin | `require_2fa` (none, admin, all), `local_login`, `webauthn_rp_id`, `webauthn_origins`, and what is in effect |

Sessions expire after 30 min idle / 12 h absolute. Idle means no request from a person: the open event stream's periodic session checks do not renew it, and the console pauses its event-driven refetches while the tab is hidden (`docs/authentication.md`, "What counts as activity"). Five failed logins lock an account for growing intervals
(1–15 min); login attempts are also rate limited per address.

## Dashboard and repositories

| route | role | notes |
|---|---|---|
| `GET /overview` | readonly | only the repositories the caller may read (totals too); everything for the dashboard: per repository health (mode, snapshots, objects, logical/physical bytes, dedup ratio, free space, corrupt chunks, failed verifications, last backup/verify/GC/retention, sync failing), per host (online, counts of instances/disks/filesystems, stale, never backed up, last backup/run status), `protection` {objects, protected, stale, never_backed_up, unscheduled}, `runs_24h`, `problems[]` {severity, kind, repository, subject, detail} |
| `GET /status` | readonly | per repository status incl. per-host unprotected objects |
| `GET /capacity` | readonly | forecast: growth/day (7/30/90), projected 80 % / full, dedup |
| `GET /capacity/series?days=30` | readonly | chart points |
| `GET /repositories` | readonly | `{name, backend, location, domain_id, s3, credential, mode, mode_reason}`; only the repositories the caller may read |
| `POST /repositories/check` | admin | check connection settings before creating (`service.RepoCheckRequest`) |
| `POST /repositories/{name}/check`, `GET /repositories/{name}/info` | operator / readonly | storage check of an existing repository; identity and owner marker |
| `POST /repositories`, `POST /repositories/attach` | admin | `{name, kind: filesystem\|s3, path, s3{endpoint,region,bucket,prefix,…}, credential \| access_key+secret_key, adopt}` |
| `POST /repositories/{name}/probe` | operator | S3 reachability/permission/versioning/lifecycle checks |
| `PUT /repositories/{name}/mode` | admin | `{mode: online\|read-only\|offline, reason}` |
| `GET /credentials`, `POST /credentials`, `PUT /credentials/{name}`, `DELETE /credentials/{name}` | admin | names + access key ids only; secrets never returned; `PUT` rotates `{access_key, secret_key}` |
| `GET /snapshot-stats?group=a,b`, `GET /objects/stats?host=`, `GET /runs/backups` | readonly | read models of the console pages (docs/api-protect.md, docs/api-infra.md) |

## Hosts, onboarding, policies

| route | role | notes |
|---|---|---|
| `GET /hosts` | readonly | enrolled identities |
| `POST /hosts/{name}/{scope}/{enable\|disable}` | operator | revoke / restore a host |
| `POST /onboarding` | admin | `{host, scope?, ttl_hours?}` → `{token, server, command, install, expires_in_hours}`: the command to run on the new host |
| `POST /tokens` | admin | raw enrolment token (CLI parity) |
| `GET /onboarding/pending`, `DELETE /onboarding/pending/{id}` | admin | enrolment tokens not used yet (metadata only); revoke one |
| `GET /policies`, `PUT /policies/{name}`, `DELETE /policies/{name}`, `GET /policies/effective` | readonly / operator / admin / readonly | `api.Policy` (schedule, targets, commit_required, keep_*, timezone, reverify_days, verify_schedule, restore_test_schedule) |
| `GET/PUT/DELETE /assignments` | readonly / operator | `{host, project, object, policy}`; scope: host default → project → object |

## Objects and snapshots

| route | role | notes |
|---|---|---|
| `GET /objects` | readonly | storage objects with host, kind, policy, snapshot counts, latest snapshot |
| `PUT /objects/{group}/retention`, `PUT /objects/{group}/notes` | operator | |
| `POST /objects/{group}/transfer` | admin | `{host}` re-pins an object to another host |
| `GET /snapshots?group=` | readonly | `api.SnapshotRef` list (protected, verify_state, notes, logical bytes, policy, host, object, kind) |
| `PUT /snapshots/{group}/{snap}/protected`, `PUT /snapshots/{group}/{snap}/notes` | operator | `{protected}` / `{notes}` |
| `DELETE /snapshots/{group}/{snap}` | admin | 409 when protected |
| `GET /snapshots/{group}/{snap}/verifications` | readonly | verification history |
| `GET /snapshots/{group}/{snap}/index`, `…/manifest`, `GET /chunks/{id}` | restore | raw **encrypted** bytes for browser-side decryption (`application/octet-stream`; chunk ids are 64 hex) |

### Browsing without giving the server a key

The server stores only ciphertext. The UI decrypts in the browser: the user pastes the host's key into the
page (kept in memory only, never sent anywhere), the WASM module in `web/` opens the manifest/index, walks
the file catalog and decrypts exactly the chunks it needs, fetched through the three raw routes above.
Single-file download is assembled in the browser. Guest files of VM disks are recovered by a host job (see
below) and delivered as a stream sealed to a key generated in the browser.

## Maintenance and verification

| route | role | notes |
|---|---|---|
| `GET/PUT /maintenance` | readonly / admin | schedules (capacity, reconcile, prune, gc, verify, scrub), gc grace, default reverify days |
| `GET /runs?limit=` | readonly | maintenance run history |
| `POST /prune[?dry_run=1]`, `POST /gc[?dry_run=1&grace_hours=]` | admin | the retention preview equals what is applied; retention decides exactly as PBS (docs/retention.md) |
| `POST /verify` | operator | `{mode: all\|new\|stale, stale_days, no_quarantine}`; a finding is a result (`ok:false`), not an error |
| `POST /reconcile` | admin | rebuild PostgreSQL rows from the datastore |
| `GET /corrupt[?healed=1]` | readonly | quarantined chunks and the snapshots they affect |
| `DELETE /escrow` | admin | |

## Sync

`GET/POST /sync`, `GET/PUT/DELETE /sync/{name}`, `POST /sync/{name}/run`: push/pull jobs to another server
(roles: read / admin / operator). `GET /sync/{name}/runs` is the run history, `PUT /sync/{name}/schedule`
sets the schedule, `POST /sync/check-remote` `{remote, token}` checks a peer without spending its token
(docs/api-storage.md). See `docs/design.md` §sync.

## Settings

`GET/PUT /settings` (admin): `base_url` (links in notifications), `advertise_address` (host:port hosts use),
`instance_name`. `GET /about` (readonly): version, uptime, database state. `GET /setup-checklist` (admin).
`GET /schedule/preview?spec=&count=&after=` (readonly): the next firings of a schedule by the server's clock.

## Jobs and restore tests

Host jobs are work only a host can do (it has the key and Incus); see `docs/jobs.md`.

| route | role | notes |
|---|---|---|
| `GET /jobs?repository=&host=&type=&state=&limit=` | readonly | newest first; only repositories the caller may read |
| `POST /jobs` | restore (restores, file listings) / operator (others) | `{type, host, repository?, params}` → 201 job; the role is checked on the job's repository |
| `GET /jobs/{id}`, `POST /jobs/{id}/cancel` | readonly / operator | another repository's job answers 404 |
| `GET /jobs/{id}/delivery` | restore | the sealed (IBS2, docs/sealed-delivery.md) result, verified and decrypted in the browser |
| `GET /restore-tests[?latest=1]` | readonly | restore test history, or the latest per object of one repository |
| `POST /restore-tests/run` | operator | `{host, repository?, project, object, snapshot?, mode?}` → 201 job |

## Notifications and alerts

See `docs/notifications.md`. Channel secrets are masked on output.

| route | role | notes |
|---|---|---|
| `GET /notifications/channels`, `POST /notifications/channels` | operator / admin | `notify.ChannelInput` |
| `PUT /notifications/channels/{id}`, `DELETE /notifications/channels/{id}` | admin | replace / delete a channel |
| `POST /notifications/channels/{id}/test` | admin | `{ok, error?, delivery}`: the receiver's answer is the result |
| `GET /notifications/rules`, `POST /notifications/rules` | operator / admin | routing rules (`notify.Rule`) |
| `PUT /notifications/rules/{id}`, `DELETE /notifications/rules/{id}` | admin | |
| `GET /notifications/deliveries?channel=&state=&alert=&limit=` | operator | delivery log |
| `POST /notifications/deliveries/{id}/retry` | admin | retry a failed delivery |
| `GET /notifications/settings`, `PUT /notifications/settings` | operator / admin | `{settings, conditions}` / `notify.Settings` |
| `PUT /notifications/conditions/{type}` | admin | `{enabled, severity, params}` for one alert condition |
| `POST /notifications/tick`, `POST /notifications/simulate` | admin / operator | one evaluation round now; which rules and channels an event would reach |
| `GET /alerts?state=&severity=&type=&host=&active=1&limit=` | readonly | only alerts of repositories the caller may read (alerts without a repository need a global role) |
| `GET /alerts/{id}`, `POST /alerts/{id}/ack` | readonly / operator | `{comment?, undo?}`; another repository's alert answers 404 |
| `GET /silences[?all=1]`, `POST /silences`, `DELETE /silences/{id}` | readonly / operator | |

## Certificates

`GET /certificates` (readonly): CA, server and sync peer certificates with expiry, and the web certificate.
`PUT /certificates/sans` `{sans}` (admin) sets the server certificate's names; `POST /certificates/ca/rotate`
(admin) starts a CA rotation and returns `{new_ca_fingerprint}`; `POST /certificates/ca/promote[?force=1]`
(admin) makes the next CA the signing CA. See `docs/certificates.md`.

## Traffic control

Rate limits of the agent API (backups and restores of hosts); see [traffic.md](traffic.md).

| route | role | notes |
|---|---|---|
| `GET /traffic/rules` | readonly | `traffic.Rule` list |
| `POST /traffic/rules`, `PUT /traffic/rules/{name}`, `DELETE /traffic/rules/{name}` | admin | `{name, enabled?, scope: host\|global, hosts, direction: upload\|download, rate_bps, burst_bytes?, schedule?, timezone?, comment?}` |
| `GET /traffic/current` | readonly | `{at, hosts: [{repository, host, upload_bps, download_bps, upload_bytes_total, download_bytes_total, last_seen}], rules: [{name, active, …}]}` |

## Repository scoping

Per-repository roles for users and API tokens are enforced in one place, before every handler: see
[permissions.md](permissions.md). The `restore` role on one repository no longer reaches the encrypted
index, manifest and chunks of the others.

## All routes

Generated from the route metadata (`internal/server/mgmt/routes_meta.go`); a test fails when this table
and the server disagree. "repository" says how a route finds the repository its role is checked on:
`query` (`?repository=`), `path` (`{name}`), `body` (`repository` field), `job`/`alert` (the id's
repository), `list` (filtered per item), `global` (the global role), `self` (any signed-in principal).

<!-- routes:begin (generated: IB_WRITE_API_DOC=1 go test ./internal/server/mgmt -run APIDoc) -->
| area | route | role | repository | what it does |
|---|---|---|---|---|
| Alerts | `GET /alerts` | readonly | list | Alerts the caller may see |
| Alerts | `GET /alerts/{id}` | readonly | alert | One alert with its history |
| Alerts | `POST /alerts/{id}/ack` | operator | alert | Acknowledge (or undo=true: unacknowledge) an alert |
| Alerts | `GET /silences` | readonly | global | Silences (all=1 includes ended ones) |
| Alerts | `POST /silences` | operator | global | Create a silence |
| Alerts | `DELETE /silences/{id}` | operator | global | End a silence |
| Auth | `GET /auth/oidc/providers` | public | self | Sign-in options for the sign-in page: identity providers (name, label), whether password and passkey sign-in are on (no login) |
| Auth | `GET /auth/oidc/{provider}/callback` | public | self | Identity provider's redirect back: sets the session cookie and redirects to the console |
| Auth | `GET /auth/oidc/{provider}/login` | public | self | Redirect the browser to the identity provider (?next=/path after sign-in) |
| Auth | `GET /session` | readonly | self | Who am I: user, CSRF token and repository access |
| Auth | `POST /session` | public | self | Sign in (sets the session cookie; returns the CSRF token) |
| Auth | `DELETE /session` | readonly | self | Sign out |
| Auth | `POST /session/webauthn/begin` | public | self | Start a passkey sign-in (no body) or the security-key second factor ({ticket} from POST /session); returns navigator.credentials.get() options |
| Auth | `POST /session/webauthn/finish` | public | self | Finish a passkey or second-factor sign-in (sets the session cookie like POST /session) |
| Auth | `GET /setup` | public | self | Whether first-run setup is needed (no login) |
| Auth | `POST /setup` | public | self | Create the first administrator with the setup token (no login) |
| Authentication | `GET /auth/settings` | admin | global | Sign-in policy: require_2fa, local_login, passkey relying party (and what is in effect) |
| Authentication | `PUT /auth/settings` | admin | global | Change the sign-in policy (local_login=false needs an administrator who signs in through an identity provider) |
| Authentication | `GET /oidc/providers` | admin | global | Identity providers (never the client secret) |
| Authentication | `POST /oidc/providers` | admin | global | Add an identity provider (the client secret is sealed and never returned) |
| Authentication | `PUT /oidc/providers/{name}` | admin | global | Replace an identity provider's settings (an empty client_secret keeps the stored one) |
| Authentication | `DELETE /oidc/providers/{name}` | admin | global | Remove an identity provider and its user links |
| Authentication | `POST /oidc/providers/{name}/test` | admin | global | Run discovery against the issuer and fetch its key set |
| Capacity | `GET /capacity` | readonly | query | Capacity, growth and fill forecast of a repository |
| Capacity | `GET /capacity/series` | readonly | query | Capacity samples over time |
| Certificates | `GET /certificates` | readonly | global | CA, server and sync peer certificates with expiry |
| Certificates | `POST /certificates/ca/promote` | admin | global | Promote the next CA (force=1 even if hosts lag) |
| Certificates | `POST /certificates/ca/rotate` | admin | global | Start a CA rotation (new CA trusted alongside the old) |
| Certificates | `PUT /certificates/sans` | admin | global | Set the server certificate's subject alternative names |
| Credentials | `GET /credentials` | admin | global | S3 credentials (names and key ids, never secrets) |
| Credentials | `POST /credentials` | admin | global | Store an S3 credential (sealed) |
| Credentials | `PUT /credentials/{name}` | admin | global | Rotate an S3 credential |
| Credentials | `DELETE /credentials/{name}` | admin | global | Delete an unused S3 credential |
| Dashboard | `GET /overview` | readonly | list | Dashboard model across the repositories the caller may see |
| Dashboard | `GET /status` | readonly | query | Repository status counters |
| Hosts | `GET /hosts` | readonly | query | Enrolled hosts of a repository |
| Hosts | `POST /hosts/{name}/{scope}/{action}` | operator | query | Enable or disable a host certificate ({action} = enable\|disable, {scope} = backup\|restore) |
| Hosts | `POST /onboarding` | admin | query | Enrolment token plus the exact command to run on the new host |
| Hosts | `GET /onboarding/pending` | admin | query | Enrolment tokens issued and not used yet (metadata only) |
| Hosts | `DELETE /onboarding/pending/{id}` | admin | query | Revoke an unused enrolment token |
| Hosts | `GET /runs/backups` | readonly | query | Backup runs reported by hosts, newest first |
| Hosts | `POST /tokens` | admin | query | Create a one-time enrolment token for a host |
| Jobs | `GET /jobs` | readonly | list | Host jobs, newest first |
| Jobs | `POST /jobs` | restore | body | Start a host job (restore, file listing...) |
| Jobs | `GET /jobs/{id}` | readonly | job | One job |
| Jobs | `POST /jobs/{id}/cancel` | operator | job | Cancel a job |
| Jobs | `GET /jobs/{id}/delivery` | restore | job | A job's sealed result (IBS2, decrypted by the browser) |
| Jobs | `GET /restore-tests` | readonly | list | Restore tests (latest=1: the latest per object of one repository) |
| Jobs | `POST /restore-tests/run` | operator | body | Run a restore test now |
| Maintenance | `GET /corrupt` | readonly | query | Quarantined chunks and the snapshots they affect |
| Maintenance | `DELETE /escrow` | admin | query | Delete the escrowed key material of a repository |
| Maintenance | `POST /gc` | admin | query | Garbage-collect unreferenced chunks |
| Maintenance | `GET /maintenance` | readonly | query | Maintenance schedules and GC grace |
| Maintenance | `PUT /maintenance` | admin | query | Change maintenance schedules (fields not sent are unchanged) |
| Maintenance | `POST /prune` | admin | query | Apply retention (dry_run=1 returns the plan with a reason per snapshot) |
| Maintenance | `POST /reconcile` | admin | query | Rebuild PostgreSQL rows from the datastore |
| Maintenance | `GET /runs` | readonly | query | Maintenance runs (prune, GC, verify, reconcile...), newest first |
| Maintenance | `POST /verify` | operator | query | Verify snapshots (mode all, new or stale) |
| Me | `POST /me/password` | readonly | self | Change my password |
| Me | `GET /me/sessions` | readonly | self | My sessions |
| Me | `DELETE /me/sessions/{id}` | readonly | self | End one of my sessions |
| Me | `GET /me/tokens` | readonly | self | My API tokens |
| Me | `POST /me/tokens` | readonly | self | Create an API token (optionally restricted to repositories; 90 days unless ttl_hours, at most 365 days; password needed when the sign-in is older than 10 minutes; not for single sign-on accounts); the secret is shown once |
| Me | `DELETE /me/tokens/{id}` | readonly | self | Revoke one of my API tokens |
| Me | `POST /me/totp` | readonly | self | Begin two-factor enrolment (password needed when the sign-in is older than 10 minutes) |
| Me | `DELETE /me/totp` | readonly | self | Remove my two-factor enrolment (needs the password) |
| Me | `POST /me/totp/confirm` | readonly | self | Confirm two-factor enrolment; returns recovery codes once (password needed when the sign-in is older than 10 minutes) |
| Me | `GET /me/webauthn` | readonly | self | My passkeys and security keys |
| Me | `POST /me/webauthn/register/begin` | readonly | self | Start registering a passkey or security key (password needed when the sign-in is older than 10 minutes); returns navigator.credentials.create() options |
| Me | `POST /me/webauthn/register/finish` | readonly | self | Finish registering: the browser's credential and a name |
| Me | `PATCH /me/webauthn/{id}` | readonly | self | Rename one of my keys |
| Me | `DELETE /me/webauthn/{id}` | readonly | self | Remove one of my keys (password needed when the sign-in is older than 10 minutes; the last second factor the policy requires is refused) |
| Notifications | `GET /notifications/channels` | operator | global | Notification channels (secrets masked) |
| Notifications | `POST /notifications/channels` | admin | global | Create a channel |
| Notifications | `PUT /notifications/channels/{id}` | admin | global | Replace a channel |
| Notifications | `DELETE /notifications/channels/{id}` | admin | global | Delete a channel |
| Notifications | `POST /notifications/channels/{id}/test` | admin | global | Send a test message (the receiver's answer is the result) |
| Notifications | `PUT /notifications/conditions/{type}` | admin | global | Enable, disable or tune one alert condition |
| Notifications | `GET /notifications/deliveries` | operator | global | Delivery attempts |
| Notifications | `POST /notifications/deliveries/{id}/retry` | admin | global | Retry a failed delivery |
| Notifications | `GET /notifications/rules` | operator | global | Routing rules |
| Notifications | `POST /notifications/rules` | admin | global | Create a rule |
| Notifications | `PUT /notifications/rules/{id}` | admin | global | Replace a rule |
| Notifications | `DELETE /notifications/rules/{id}` | admin | global | Delete a rule |
| Notifications | `GET /notifications/settings` | operator | global | Engine settings and alert conditions |
| Notifications | `PUT /notifications/settings` | admin | global | Set engine settings |
| Notifications | `POST /notifications/simulate` | operator | global | Which rules and channels an event would reach |
| Notifications | `POST /notifications/tick` | admin | global | Run one evaluation and delivery round now |
| Objects | `GET /objects` | readonly | query | Storage objects (backup groups) of a repository |
| Objects | `GET /objects/stats` | readonly | query | Every storage object with freshness, latest snapshot and size history |
| Objects | `DELETE /objects/{group}` | admin | query | Delete a whole backup group (all snapshots) as a task |
| Objects | `PUT /objects/{group}/notes` | operator | query | Set an object's notes |
| Objects | `PUT /objects/{group}/retention` | operator | query | Pin a retention policy to an object |
| Objects | `POST /objects/{group}/transfer` | admin | query | Transfer an object to another backup host |
| Policies | `GET /assignments` | readonly | query | Policy assignments (host, project, object) |
| Policies | `PUT /assignments` | operator | query | Assign a policy |
| Policies | `DELETE /assignments` | operator | query | Remove an assignment |
| Policies | `GET /policies` | readonly | query | Policies of a repository |
| Policies | `GET /policies/effective` | readonly | query | The retention policy that applies to every object, and why |
| Policies | `PUT /policies/{name}` | operator | query | Create or replace a policy |
| Policies | `DELETE /policies/{name}` | admin | query | Delete a policy |
| Repositories | `GET /repositories` | readonly | list | Repositories the caller may see (never credentials) |
| Repositories | `POST /repositories` | admin | global | Create a repository (filesystem or S3) |
| Repositories | `POST /repositories/attach` | admin | global | Attach an existing datastore (adopt=true takes over ownership) |
| Repositories | `POST /repositories/check` | admin | global | Check connection settings before creating a repository |
| Repositories | `DELETE /repositories/{name}` | admin | path | Detach (forget, keep the data) or destroy (delete every backup in) an offline repository |
| Repositories | `POST /repositories/{name}/check` | operator | path | Check an existing repository's storage |
| Repositories | `GET /repositories/{name}/info` | readonly | path | Repository identity and owner marker |
| Repositories | `PUT /repositories/{name}/mode` | admin | path | Set maintenance mode (online, read-only, offline) |
| Repositories | `POST /repositories/{name}/probe` | operator | path | Run the storage probe (put-if-absent, listing, read-after-write...) |
| Settings | `GET /schedule/preview` | readonly | global | Parse a schedule and list its next firings by the server's clock |
| Settings | `GET /settings` | admin | global | Server settings (key/value) |
| Settings | `PUT /settings` | admin | global | Set server settings (keys not sent are unchanged) |
| Snapshots | `GET /chunks/{id}` | restore | query | One encrypted chunk of the repository |
| Snapshots | `GET /snapshot-stats` | readonly | query | Recorded statistics of every snapshot (optionally ?group=a,b) |
| Snapshots | `GET /snapshots` | readonly | query | Snapshots of a repository (optionally one ?group=) |
| Snapshots | `DELETE /snapshots/{group}/{snap}` | admin | query | Forget a snapshot (refused while protected) |
| Snapshots | `GET /snapshots/{group}/{snap}/index` | restore | query | Encrypted index of a snapshot (the browser decrypts it) |
| Snapshots | `GET /snapshots/{group}/{snap}/manifest` | restore | query | Encrypted manifest of a snapshot (the browser decrypts it) |
| Snapshots | `PUT /snapshots/{group}/{snap}/notes` | operator | query | Set a snapshot's notes |
| Snapshots | `PUT /snapshots/{group}/{snap}/protected` | operator | query | Protect or unprotect a snapshot |
| Snapshots | `GET /snapshots/{group}/{snap}/verifications` | readonly | query | Verification history of a snapshot |
| Sync | `GET /sync` | readonly | query | Sync jobs of a repository |
| Sync | `POST /sync` | admin | query | Add a sync job (the peer token is used once) |
| Sync | `POST /sync/check-remote` | admin | global | Check a sync peer without spending its token |
| Sync | `GET /sync/{name}` | readonly | query | Sync job status |
| Sync | `PUT /sync/{name}` | operator | query | Change a sync job's options |
| Sync | `DELETE /sync/{name}` | admin | query | Remove a sync job |
| Sync | `POST /sync/{name}/run` | operator | query | Run a sync job now |
| Sync | `GET /sync/{name}/runs` | readonly | query | Run history of a sync job |
| Sync | `PUT /sync/{name}/schedule` | operator | query | Set a sync job's schedule |
| System | `GET /about` | readonly | global | Version, uptime and whether PostgreSQL answers |
| System | `GET /events` | readonly | list | Live updates as Server-Sent Events (job, alert, repository); filtered by the caller's repository access |
| System | `GET /metrics` | readonly | global | Prometheus text exposition (use a bearer token) |
| System | `GET /openapi.json` | readonly | self | This OpenAPI 3.1 document |
| System | `GET /ping` | public | self | Liveness and version (no login) |
| System | `GET /setup-checklist` | admin | global | First-run checklist (what is configured and what is missing) |
| System | `GET /system/logs` | admin | global | Recent server log lines (secrets redacted) |
| System | `GET /system/samples` | readonly | global | Per-minute resource samples (7 days) |
| System | `GET /system/status` | readonly | global | Host, resources, database, listeners and version of this server |
| System | `POST /system/support-report` | admin | global | Download a redacted support report (.tar.gz) |
| System | `GET /system/update` | readonly | global | Update notice state (the check is off unless enabled) |
| System | `POST /system/update/check` | admin | global | Check for a newer release now |
| Tasks | `GET /tasks` | readonly | list | Tasks (long operations), newest first |
| Tasks | `GET /tasks/{id}` | readonly | global | A task with its log (?after=<seq> tails it) |
| Tasks | `POST /tasks/{id}/cancel` | operator | global | Cancel a task |
| Traffic | `GET /traffic/current` | readonly | global | Live throughput per host (5-second average) and which rules are active now |
| Traffic | `GET /traffic/rules` | readonly | global | Rate-limit rules of the agent API |
| Traffic | `POST /traffic/rules` | admin | global | Add a rate-limit rule |
| Traffic | `PUT /traffic/rules/{name}` | admin | global | Replace a rate-limit rule (the body may rename it) |
| Traffic | `DELETE /traffic/rules/{name}` | admin | global | Delete a rate-limit rule |
| Users | `GET /api-tokens` | admin | global | Every user's API tokens (never the secret) |
| Users | `DELETE /api-tokens/{id}` | admin | global | Revoke any API token |
| Users | `GET /audit` | admin | global | Audit log, newest first |
| Users | `GET /users` | admin | global | Users |
| Users | `POST /users` | admin | global | Create a user |
| Users | `PUT /users/{name}` | admin | global | Change a user's global role, name, e-mail or disabled flag |
| Users | `DELETE /users/{name}` | admin | global | Delete a user |
| Users | `GET /users/{name}/access` | admin | global | A user's global role and per-repository roles |
| Users | `PUT /users/{name}/access` | admin | global | Replace a user's per-repository roles (scoped=false: the global role applies everywhere) |
| Users | `GET /users/{name}/identities` | admin | global | A user's links to identity providers (pending: linked by e-mail, not signed in yet) |
| Users | `POST /users/{name}/identities` | admin | global | Link a user to an identity provider by e-mail: the first sign-in with that verified address binds the account |
| Users | `DELETE /users/{name}/identities/{provider}` | admin | global | Remove a user's link to an identity provider |
| Users | `POST /users/{name}/password` | admin | global | Set a user's password |
| Users | `DELETE /users/{name}/totp` | admin | global | Remove a user's two-factor enrolment |
| Users | `DELETE /users/{name}/webauthn` | admin | global | Remove every passkey and security key of a user (lost key) |
<!-- routes:end -->
