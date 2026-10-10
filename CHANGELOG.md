# Changelog

Each release has a section `## VERSION`; `scripts/release.sh` copies it into
`dist/RELEASE_NOTES.md` (and writes a stub when it is missing). Subsections use `###`.

## 0.1.0

First release. All planned core backup functionality is implemented and sandbox-tested (real
PostgreSQL, real MinIO, real mutual TLS, headless Chromium, a fake of Incus). It has **not** yet been
validated on a real Incus/ZFS host: see `docs/real-host-validation.md` before trusting production data
to it.

### Backup and restore

- Client (`incus-backup`) and repository server (`incus-backup-server`) as static Linux binaries for
  amd64 and arm64, built only on stock Incus 7.5.1+ primitives (no fork, no QEMU patch).
- VM disks: one frozen NBD view of all disks, persistent dirty-bitmap generations across several
  targets, fail-closed full reads, `max_freeze` with a disk spool.
- Containers and filesystem volumes: catalog format with metadata change detection (ctime and inode),
  browsing and single-file restore (`fs ls|find|stat|cat|extract`); tar format as fallback.
- Single files from VM disks through a disposable, network-less recovery VM (`restore guest-files`).
- Restore of instances (with their custom volumes), volumes and directories; deep verification with
  decryption (`verify`).
- Scheduled restore tests (`restore-test run|list`; methods deep, extract, full) per policy
  (`restore_test_schedule`, default monthly), recorded on the server and alerted on.
- Repositories on a directory or any S3-compatible bucket (conditional writes, presence set, no object
  rewrites); several targets from one capture; push and pull server-to-server sync with
  `verified-only`, `resync-corrupt`, `transfer-last` and rate limits.
- Keyless verification and scrubs, quarantine and healing of corrupt chunks; GC with session pins.
- Retention with exactly the decisions of Proxmox Backup Server (differential test against a port of
  its code), per-policy time zones, protected snapshots, notes, an exact dry run.
- Offline key escrow (X25519); recovery key; optional passphrase-protected key file.

### Management plane and web console

- Management API `/api/v1` on an HTTPS web listener and the unix admin socket; OpenAPI 3.1
  (`GET /api/v1/openapi.json`, `incus-backup-server openapi`) and a generated route table in
  `docs/api.md`.
- Users with roles (readonly, restore, operator, admin), per-repository scopes for users and API
  tokens, TOTP with recovery codes, passkeys/security keys (WebAuthn), OIDC single sign-on, account lockout, audit log of every change and sign-in.
- Web console (Vue 3, embedded in the server binary): dashboard with problems and setup checklist,
  hosts and onboarding, objects, snapshots, verification, tasks, jobs, sync, alerts, traffic control,
  repositories, capacity, policies with retention preview, notifications, users and tokens,
  certificates, settings, server status, logs, API explorer, audit log.
- Zero-knowledge file browsing: the browser decrypts with WebAssembly; the key stays in the page.
- Host jobs (`backup_now`, `restore_test`, `deep_verify`, `restore_instance`, `restore_volume`,
  `guest_files`, `fs_extract`), gated on each host by `jobs.allow` in `client.yaml`.
- Sealed deliveries in the authenticated **IBS2** format (see Breaking changes).
- Notifications: alert engine (missed and failed backups, offline hosts, corruption, sync lag,
  capacity, unreachable S3, certificate expiry, failed restore tests, login attacks), channels SMTP,
  webhook (JSON, Slack, Discord, HMAC signature), ntfy and Gotify, rules, quiet hours, silences,
  digests, durable delivery queue.
- Certificates: automatic renewal of host, sync-peer and server certificates with overlap; CA rotation
  (`ca rotate`, `ca promote`); `ca sans`; `incus-backup cert status|renew`; web listener TLS modes
  `self`, `files` and `acme`.
- Repository maintenance modes `online`, `read-only`, `offline` (`repo mode`); hosts defer instead of
  failing; repository detach and destroy; whole-object delete.
- Server tasks with logs, progress and cancel (`task list|show|cancel`); one destructive task per
  repository.
- Prometheus metrics (`/api/v1/metrics` with an API token, optional `metrics_listen`); server status
  with samples, daemon log viewer, redacted support report, opt-in update notice.
- Traffic control of host uploads and downloads with time windows.
- Performance: bounded-parallel datastore requests, sharded listings, S3 presence set (never used by
  commits), pipelined NBD reads, pooled encoders, larger HTTP/2 windows, parallel filesystem scan,
  control-plane indexes (`docs/performance.md`). Bounded memory on every streaming path
  (`docs/memory.md`, `scripts/membench.sh`).
- Alpine packages: `incus-backup`, `incus-backup-server` and their `-openrc`, `-doc` and
  `-bash-completion` subpackages (`docs/packaging.md`); a seeded development demo (`cmd/ibdemo`).

### Passkeys, security keys and single sign-on

- Passkeys and security keys (WebAuthn, on go-webauthn): register them in the profile (a sign-in within
  10 minutes or the password), sign in without a user name with a passkey (user verification required),
  or use a key as the second factor after the password, like a TOTP code. Signature counters are
  checked: a key that may have been copied is blocked. Administrators reset lost keys
  (`DELETE /users/{name}/webauthn`, `incus-backup-server user webauthn-reset NAME`).
- Single sign-on with OpenID Connect (go-oidc + x/oauth2): providers are data in PostgreSQL (Settings →
  Authentication), Authorization Code flow with PKCE, state bound to the browser, nonce, ID tokens
  verified (signature, issuer, audience, expiry), `email_verified` required. Accounts are linked by the
  provider's subject, never by e-mail alone; an administrator links existing users explicitly or the
  provider creates them. Roles and repository scopes come from group mappings at every sign-in; the
  default role can never be admin. Client secrets are sealed and never returned; a discovery test
  checks a provider.
- Sign-in policy: `require_2fa` (none, admin, all) gives users without a second factor an
  enrolment-only session; password and passkey sign-in can be turned off when single sign-on is the
  only way in (refused unless an administrator signs in through a provider). Break-glass on the server:
  `incus-backup-server user local-login on`.
- Every method opens the same session (cookie, CSRF, lifetime) with the same roles, repository scopes,
  lock-out and audit entries (`action=login`, `detail.method`). See docs/authentication.md.

### Build, CI and release

- GitHub Actions CI (`.github/workflows/ci.yml`, required check `ci-ok`) runs on every push and pull
  request:
  - gofmt, vet, and linux/amd64 + arm64 builds;
  - the full test suite against a real PostgreSQL 16 and MinIO;
  - the race detector on the concurrency-heavy packages;
  - the web checks on Node 22 and 24;
  - web-crypto in headless Chromium, and Playwright against the real server and the demo;
  - the generated API route table and `go mod tidy`/vendor;
  - an x86_64 Alpine package build with a smoke install.
- Releases (`release.yml`) run that whole workflow on the tag first. They then publish the binaries,
  the source tarball, the aports handoff tarball, `SHA256SUMS` and minisign signatures.
- `just ci` runs the same checks locally. Dependabot and CodeQL are configured. See
  docs/development.md.
- `scripts/release.sh` builds web-crypto (`npm run crypto`) before the web UI, so a release from a
  fresh clone works.

### Breaking changes (relative to pre-release builds)

Nobody should run pre-release builds in production; these notes are for test and development
installations.

- **Schema consolidated.** The pre-release migration history is folded into one file,
  `migrations/20261008000000_initial_schema.sql` (one migration per released version). A database that
  applied the earlier pre-release migration files is refused at start by the schema guard ("the
  database was migrated by a newer release (unknown migration 20261009100000_auth.sql)"); the message is
  misleading in this case. **Recreate such databases** (development databases, `devenv`, scratch
  servers of real-host gates); restart the demo with `ibdemo --reset`.
- **Schema guard.** A server binary now refuses a database that names a migration it does not know,
  instead of running against a newer schema. Roll back by restoring the dump taken before the upgrade.
- **Sealed deliveries are IBS2.** The host-to-browser delivery format of `guest_files` and `fs_extract`
  changed from IBS1 to the authenticated IBS2 (bound to the repository key, the job id and the request
  parameters). The new console refuses IBS1 deliveries; an older console cannot read IBS2. **Upgrade the
  server, every host and reload every open console together.** Deliveries live for one hour, so nothing
  persistent needs migrating.
- **Long operations answer 202 with a task.** `POST /api/v1/prune` (not a dry run), `/gc`, `/verify`,
  `/reconcile` and `/sync/{name}/run` return `202 {"task": …}`; follow it with `GET /api/v1/tasks/{id}`
  or the `task` SSE event. Add `?wait=1` for the previous synchronous answer. Repository destroy and
  object delete are tasks too. The CLI waits as before.
- **API token actors.** The audit actor of an API token now names its owner: `token:NAME (by OWNER)`
  instead of `token:NAME`. Adjust audit filters (`audit list --actor`).
- **Maintenance mode answers.** A backup session refused by a `read-only` or `offline` repository now
  gets 503 when it opens (previously 400 at commit); hosts report such runs as `deferred`.
- **New `server.yaml` keys** (all optional): `web_listen` (default `:8443`; `off` disables), `web`
  (`mode: self|files|acme`, `cert`, `key`, `domains`, `email`, `acme_directory`, `acme_http_listen`),
  `metrics_listen`, `metrics_unauthenticated`. The web listener is **on by default**: open or firewall
  port 8443 accordingly.
- **New `client.yaml` keys** (all optional): `jobs` (`disabled`, `allow`), `performance`
  (`encode_workers`, `upload_workers`), `recovery.memory`/`cpu`.
- **Retention decisions** follow PBS exactly: at ISO weeks that straddle a month or year end, a
  `keep-monthly`/`keep-yearly` rule may now keep an older snapshot of that month than before (the
  previous code skipped walked-past snapshots only inside one rule).
