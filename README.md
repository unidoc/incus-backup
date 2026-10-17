# incus-backup

Encrypted, deduplicated, incremental backups for [Incus](https://linuxcontainers.org/incus/)
**VMs, containers and custom volumes**, to one or more independent repository servers, with a web
console that never sees your data.

**Incus provides the primitives. incus-backup provides the backup system.** It uses only what stock
Incus (7.5.1 or newer) offers through its API: NBD exports on a frozen view, persistent dirty bitmaps,
snapshots, storage volumes, the instance API and a disposable recovery VM. The one exception: on ZFS the
client mounts a snapshot dataset read-only itself (mount(2)) when Incus has not mounted it. No Incus fork
and no QEMU patch is needed (`docs/design.md` §0).

Think "Proxmox Backup Server, but Incus-native": the unit of backup is the **storage object**, a volume
shared by many instances is captured once, and what to back up is declared with plain Incus config keys
(or `incus-sync`) and server-side policies.

## Status

All planned core backup functionality is implemented and sandbox-tested: real PostgreSQL, an in-process
S3 test server pinned to a real one by a conformance suite (versitygw), real mutual TLS, a real browser (headless Chromium) and a faithful fake of Incus
(`go test ./...`, Playwright). **It has not yet been validated on a real Incus/ZFS host, a real S3
provider, a real mail server or a public ACME CA.** The plan for that, with pass/fail criteria, is
[`docs/real-host-validation.md`](docs/real-host-validation.md). Do not trust production data to it before
that gate passes. Version 0.1.0 is not released yet ([`CHANGELOG.md`](CHANGELOG.md)).

## Architecture

```
 Incus host (many)                          incus-backup-server (one per site)
┌─────────────────────────────┐   mTLS    ┌────────────────────────────────────────┐
│ incus-backup daemon         │  :8007    │ agent API ─┐                           │
│  NBD frozen view + bitmaps  │ ────────▶ │            ├─ service layer ─ PostgreSQL │
│  ZFS snapshots + catalog    │ encrypted │ /api/v1 ───┘        │      (control plane)│
│  chunk·compress·encrypt     │  chunks   │ web UI :8443        ▼                    │
│  holds the KEY              │ ◀──────── │ admin socket   datastore: directory or │
│  runs host jobs (allow-list)│  jobs     │ /metrics       S3 (opaque chunks, no key)│
└─────────────────────────────┘           └───────────────────────┬────────────────┘
 Browser                                    ciphertext only         │ push / pull sync
┌─────────────────────────────┐  HTTPS            ▲                 ▼ (repository-aware)
│ web console + WASM decrypt  │ ◀─────────────────┘   second independent server
│ key pasted, stays in page   │
└─────────────────────────────┘
```

```mermaid
flowchart LR
  subgraph Host["Incus host"]
    I["Incus API (NBD, bitmaps, snapshots, volumes, instances, recovery VM)"] --> C["incus-backup: capture, chunk, compress, encrypt (holds the key)"]
  end
  subgraph Server["incus-backup-server (no key)"]
    A["agent API, mutual TLS, :8007"] --> S["service layer"]
    M["management API /api/v1 and web UI, HTTPS :8443"] --> S
    S --> P[("PostgreSQL: control plane")]
    S --> D[("datastore: directory or S3")]
  end
  C -- "encrypted chunks, runs, job results" --> A
  A -- "policies, jobs" --> C
  B["Browser: WASM decrypts in the page"] -- "ciphertext, sealed deliveries" --> M
  S -- "push / pull sync" --> R[("second server")]
```

## What it does

* **Zero-knowledge for payloads.** The server stores encrypted chunks, indexes and manifests and never
  holds a key. The key lives on the host and in your password manager (recovery key), optionally also
  wrapped to an offline escrow key.
* **No chains.** Every snapshot is a complete image made of chunk references; deleting any backup never
  breaks another. "Incremental" means unchanged chunks are neither read nor sent again.
* **Consistent VM backups without a guest agent.** One frozen NBD view of all disks of a VM, dirty-bitmap
  generations that work across several targets, fail-closed fallback to a deduplicated full read.
* **Containers and volumes read only what changed.** An encrypted file catalog compares metadata
  (including ctime and inode) and reuses whole chunks of unchanged files. Browse and restore single files
  without downloading the snapshot. VM disks give up single files through a disposable, network-less
  recovery VM, never by mounting the guest filesystem on the host.
* **A web console that decrypts in the browser.** Browse and download files of encrypted snapshots in the
  page (Go compiled to WebAssembly). Files from VM disks come back from the host sealed (IBS2) to a key the
  browser generated, authenticated with the repository key. Whoever serves the console's code can read a
  key pasted into it; the CLI is the alternative (`docs/design.md` §18.3).
* **People and automation:** users with roles (readonly, restore, operator, admin), per-repository scopes,
  TOTP, API tokens, a complete audit log, and an OpenAPI 3.1 contract with an in-console explorer.
* **Operations like a monitoring system:** alerts for missed/failed backups, offline hosts, corruption,
  sync lag, capacity, certificates and failed restore tests; SMTP, webhooks (Slack, Discord), ntfy, Gotify;
  quiet hours, escalation, digests. Prometheus metrics. Tasks with live logs and cancel. Server status and
  logs in the browser. Maintenance modes. Traffic control with time windows.
* **Integrity that heals.** Keyless verification (SHA-256 trailers), quarantine of corrupt chunks, a failed
  snapshot is never an incremental base, scheduled restore tests (deep, extract, full).
* **Datastore on a directory or any S3-compatible bucket** (native S3, conditional writes, no FUSE),
  several targets from one capture, and server-to-server **sync** (push and pull).

## Compared with Proxmox Backup Server

"Tested" means a test in this tree (sandbox: real PostgreSQL, an in-process S3 test server checked against versitygw, mutual TLS, a browser, fake Incus);
nothing is validated on a real Incus host yet. The PBS side comes from the PBS 4.2.8 source and docs
(`docs/pbs-comparison.md`).

| Area | PBS feature | incus-backup | Status |
|---|---|---|---|
| Datastore | directory datastore | filesystem repository | **have** |
| | S3 datastore | native S3 repository with conditional writes and a presence set | **have** (S3 test server + conformance suite run against versitygw; a real provider is unproven) |
| | local chunk cache for S3 restores | none | **missing** |
| | removable datastore (USB rotation) | `repo detach` / `repo attach`, offline mode | **partial** (no mount/unmount workflow) |
| | remove a datastore | `repo detach`, `repo destroy` | **have** |
| | namespaces | projects and hosts structure the objects | n/a (by design) |
| Integrity | client-side encryption | always on; manifests encrypted too | **have** (PBS manifests are plaintext) |
| | verify jobs, verify state, re-verify after N days | `verify` new/stale/all, per-snapshot state, `reverify_days` | **have** |
| | verify right after each backup | daily `verify` of new snapshots | **partial** |
| | corrupt chunk handling | quarantine, never a base, healed by the next backup or sync | **have** |
| | GC with grace | mark and sweep, pins instead of atime, dry run | **have** |
| Retention | prune keep-last/hourly/daily/weekly/monthly/yearly | identical decisions (differential test against a PBS port); time zone per policy; server commit time | **have** |
| | prune simulator (what-if) | exact dry-run plan per snapshot; preview in the policy editor | **partial** (no synthetic-schedule simulator) |
| | protected snapshots, notes | yes | **have** |
| | delete a whole group | `object delete` | **have** |
| Sync | push and pull sync jobs | yes, either side filesystem or S3 | **have** |
| | verified-only, resync-corrupt, transfer-last, rate limit | yes | **have** |
| | remove-vanished | none | **missing** |
| | group filter | group ids | **partial** (no globs by project, host or kind) |
| Access | local users, password policy | argon2id, at least 12 characters | **have** |
| | TOTP and recovery codes | yes | **have** |
| | WebAuthn / U2F / YubiKey OTP | WebAuthn passkeys and security keys (U2F keys work through WebAuthn); no YubiKey OTP | **have** (sandbox-tested with a software authenticator) |
| | PAM / LDAP / AD / OpenID Connect realms | OpenID Connect (any standard IdP: Keycloak, Authentik, Google...) with group-to-role and group-to-repository mapping; no PAM, LDAP or AD realm | **partial** (OIDC sandbox-tested against a local test IdP) |
| | API tokens | role-capped, expiring, repository-restricted | **have** |
| | ACLs | 4 roles plus per-repository roles for users and tokens | **have** (per repository, not per object) |
| | audit | queryable log of every change and sign-in | **have** (PBS has none) |
| Notifications | SMTP, sendmail, Gotify, webhook targets | SMTP, webhook (JSON, Slack, Discord), ntfy, Gotify | **have** (sendmail n/a; real SMTP/ntfy/Gotify servers unproven) |
| | matchers, test, delivery log | rules, simulator, test, delivery log with retry, silences, digest | **have** (plus alert states, missed backups, offline hosts, capacity forecast) |
| Tasks | task list, live log, abort | tasks with logs, progress and cancel; host jobs with logs | **have** |
| Monitoring | datastore usage history and forecast | capacity page, growth trends, full date | **have** |
| | node status (CPU, RAM, load, disk) | server status page with a week of samples | **have** |
| | syslog / journal viewer | daemon log viewer (last 5000 lines) | **have** |
| | external metric server (InfluxDB) | Prometheus `/metrics` (pull) | **partial** (no InfluxDB push) |
| | disk / ZFS pool health, SMART | none | **missing** |
| | system report | support report (redacted `.tar.gz`) | **have** |
| | update notice | opt-in update check | **have** |
| Administration | certificates: upload, ACME | web TLS `self`, `files` (reloaded), `acme` (TLS-ALPN-01, HTTP-01) | **partial** (no DNS-01; ACME unproven) |
| | host certificates | private CA, automatic renewal, CA rotate/promote | **have** (PBS pins fingerprints only) |
| | traffic control | per host or shared, time windows, upload and download | **have** |
| | API viewer | OpenAPI 3.1 and an in-console explorer | **have** |
| | CLI parity | server CLI for repositories, policies, snapshots, tasks, jobs, alerts, audit, certificates, users | **partial** (notifications, settings, traffic rules, API tokens: API/UI only) |
| Client | file-level browse and restore in the UI | yes, **including encrypted snapshots** (the browser decrypts) | **have** (PBS cannot browse encrypted snapshots in its UI) |
| | single-file restore from VM disks | isolated recovery VM | **have** (a real recovery VM is unproven) |
| | FUSE mount / map an image | none | **missing** |
| | change detection (metadata mode) | catalog with ctime and inode | **have** (stability on ZFS unproven) |
| | change the key passphrase | via the recovery key and `key import` | **partial** (no `key passwd`) |
| | master key / paperkey | offline X25519 escrow; printable recovery key | **partial** (no QR or print sheet for the recovery key) |
| | scheduled restore tests | deep, extract, full; history; alerts | **have** (PBS has none) |
| | live restore, tape | none | n/a |

## Quick start

### Try it locally (no Incus needed)

```sh
just web                                   # build the web UI into the server (needs Node and npm)
go run ./cmd/ibdemo --reset                # seeded server stack on http://127.0.0.1:8099
```

Log in as `admin` / `demo-demo-demo-1`. The first run downloads PostgreSQL binaries for embedded-postgres.
The demo's key is published: never put real data in it ([`docs/dev-demo.md`](docs/dev-demo.md)).

### Server

```sh
apk add incus-backup-server postgresql postgresql-client     # docs/packaging.md
incus-backup-server init \
  --database-url 'postgres://incus_backup:SECRET@127.0.0.1/incus_backup?sslmode=disable' \
  --datastore /srv/incus-backup --repository custody --san backup.example.net
rc-update add incus-backup-server && rc-service incus-backup-server start
incus-backup-server user setup-token       # for the first login at https://backup.example.net:8443
```

In the console: create the administrator, then **Hosts -> Onboarding** (prints the host command),
**Policies** and **Notifications**. Or on the command line:

```sh
incus-backup-server -r custody token create benfica --scope backup
incus-backup-server -r custody policy set nightly --schedule 03:00 --keep-last 3 --keep-daily 14 \
    --keep-weekly 8 --keep-monthly 12 --timezone Atlantic/Reykjavik
incus-backup-server -r custody policy assign benfica nightly --project custody
```

### Host

```sh
apk add incus-backup
incus-backup init --server backup.example.net:8007 --token ibt1.…   # shows the RECOVERY KEY once
incus-backup doctor            # Incus, servers, key, what would be backed up
incus config set bosch user.backup.policy nightly
incus-backup run               # back up now (first backup of a big VM: --max-freeze 3h)
rc-update add incus-backup && rc-service incus-backup start          # scheduled
```

### Restore, verify, look

```sh
incus-backup list instance/bosch/root --project custody
incus-backup restore instance bosch --project custody --as bosch-restored --volumes
incus-backup fs extract instance/web/root etc/nginx --to /tmp/x
incus-backup restore guest-files instance/bosch/root /etc/hostname --to /tmp/x
incus-backup restore-test run --object instance/bosch/root --project custody --mode full
incus-backup verify --last 1
```

The full first-run flow, disaster recovery, upgrades and hardening are in
[`docs/operations.md`](docs/operations.md).

## Screenshots

Screenshots are not stored in the repository. The pages worth capturing, light and dark:

- Overview: problems list, protection coverage, setup checklist
- Hosts and Onboarding
- Snapshots with the file browser: key gate, file list, ZIP download
- Restore: sealed delivery progress
- Policies with the retention preview
- Tasks with a live log
- Alerts and an alert timeline
- Notification channels and rules
- Capacity forecast
- Repositories: mode and checks
- Traffic control: live rates
- Certificates, Server status, Users & tokens with repository access, API explorer, Audit log

The Playwright suites in `web/e2e` drive these pages against the demo (`npm run e2e` in `web/`).

## Documentation

Start with [`docs/index.md`](docs/index.md), which says who should read what.

| Document | What |
|---|---|
| [`docs/design.md`](docs/design.md) | the contract: architecture, security model, every subsystem |
| [`docs/operations.md`](docs/operations.md) | install to restore test, DR, upgrades, troubleshooting, hardening |
| [`docs/packaging.md`](docs/packaging.md) | Alpine packages, services, release build |
| [`docs/real-host-validation.md`](docs/real-host-validation.md) | the plan and gate checklist for real hardware |
| [`docs/format.md`](docs/format.md) | byte-level repository format |
| [`docs/sealed-delivery.md`](docs/sealed-delivery.md) | IBS2 deliveries, browser ZIP names, key hygiene |
| [`docs/web-crypto.md`](docs/web-crypto.md) | browser decryption library |
| [`docs/web-protect.md`](docs/web-protect.md) | the console's protection and recovery pages |
| [`docs/api.md`](docs/api.md) | management API (generated route table, OpenAPI) |
| [`docs/api-config.md`](docs/api-config.md), [`docs/api-infra.md`](docs/api-infra.md), [`docs/api-ops.md`](docs/api-ops.md), [`docs/api-protect.md`](docs/api-protect.md), [`docs/api-storage.md`](docs/api-storage.md) | API details per area |
| [`docs/permissions.md`](docs/permissions.md) | roles and repository scopes |
| [`docs/jobs.md`](docs/jobs.md) | host jobs and restore tests |
| [`docs/certificates.md`](docs/certificates.md) | certificate lifecycle and CA rotation |
| [`docs/notifications.md`](docs/notifications.md) | alerts and notification channels |
| [`docs/metrics.md`](docs/metrics.md) | Prometheus metrics and alert rules |
| [`docs/traffic.md`](docs/traffic.md) | traffic control |
| [`docs/retention.md`](docs/retention.md) | retention semantics compared with PBS |
| [`docs/performance.md`](docs/performance.md) | measurements and tunables |
| [`docs/memory.md`](docs/memory.md) | memory model and measurements |
| [`docs/pbs-comparison.md`](docs/pbs-comparison.md) | source-level comparison with PBS |
| [`docs/block-level-research.md`](docs/block-level-research.md) | why filesystems are backed up file by file |
| [`docs/dev-demo.md`](docs/dev-demo.md) | the development demo stack |
| [`docs/development.md`](docs/development.md) | tests and their services, `just ci`, e2e, CI jobs, releasing |

## Build and test

```sh
just web          # web UI (web-crypto WASM + Vite) into internal/server/webui/dist; run before `just build`
just build        # static binaries in bin/ (CGO_ENABLED=0)
just test         # go test ./... against a real PostgreSQL (embedded-postgres, or INCUS_BACKUP_TEST_DATABASE_URL)
just web-test     # type check, unit tests and Playwright end-to-end tests of the console
just fuzz         # format parsers
just verify-vectors   # independent check of the format test vectors
just bench        # the benchmarks behind docs/performance.md (PostgreSQL; S3 is the in-process fake)
just ci           # what CI runs: fmt, vet, cross builds, tests, race subset, web checks, API doc, vendor
```

Tests that need PostgreSQL or an S3 service **fail** rather than skip when none is available (S3 tests
run against an in-process S3 server, no binary or network needed; `INCUS_BACKUP_TEST_S3_*` points the same
tests at a real S3 service, see [`docs/testing-s3.md`](docs/testing-s3.md)). Incus
itself cannot run in CI sandboxes: [`scripts/gates`](scripts/gates) validate the real Incus/ZFS behaviour
on a host, using scratch objects only.

CI (`.github/workflows/ci.yml`; the branch-protection check is `ci-ok`) runs all of that plus the
Playwright end-to-end tests and an x86_64 Alpine package build with a smoke install. Every release tag
must pass it first. See [`docs/development.md`](docs/development.md).

License: Apache-2.0 (see `LICENSE.md`).
