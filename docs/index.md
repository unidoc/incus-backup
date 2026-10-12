# Documentation index

Every document in `docs/`, what it is for and who should read it. Roles: **operator** (installs and runs
servers and hosts), **security reviewer** (decides whether to trust it), **developer** (changes the code),
**integrator** (scripts, monitoring, automation against the API).

## Start here

| Document | What it is | Read it if you are |
|---|---|---|
| [`../README.md`](../README.md) | product overview, PBS feature matrix, quick start, architecture diagram | everyone |
| [`design.md`](design.md) | the contract: principle and Incus primitives, capture, repository and cryptography, protocol, failure semantics, management plane, zero-knowledge UI, jobs, certificates, tasks, notifications, traffic, performance and memory architecture, validation gates | developer, security reviewer; operator for §0, §3.1, §9, §18.3 |
| [`operations.md`](operations.md) | install to first restore test, keys, restore, disaster recovery, server backups, maintenance windows, certificates, upgrades, troubleshooting, capacity, hardening | operator |
| [`real-host-validation.md`](real-host-validation.md) | the plan and gate checklist for real Incus/ZFS, S3, mail, network and ACME | operator running the gates; anyone deciding on production use |

## Installing and running

| Document | What it is | Read it if you are |
|---|---|---|
| [`packaging.md`](packaging.md) | Alpine packages, users and permissions, ports, PostgreSQL, OpenRC services, upgrade and rollback, release build, how the packaging was validated | operator, packager |
| [`certificates.md`](certificates.md) | private CA, renewal protocol, hot reload, CA rotation, files | operator, security reviewer |
| [`notifications.md`](notifications.md) | alert types and thresholds, alert state machine, rules, channels, delivery queue, secrets | operator, integrator |
| [`metrics.md`](metrics.md) | Prometheus endpoints, scrape config, every metric, example alert rules | operator, integrator |
| [`traffic.md`](traffic.md) | traffic-control rules, schedule grammar, how limits apply, measurements | operator |
| [`retention.md`](retention.md) | retention semantics, equality with PBS, deliberate input differences, tests | operator, developer |
| [`jobs.md`](jobs.md) | host jobs (types, gating with `jobs.allow`, leases, sealed deliveries) and restore tests | operator, security reviewer, developer |
| [`authentication.md`](authentication.md) | sign-in methods (password, TOTP, passkeys, OIDC), policies, setup guides for identity providers, lost-device and break-glass procedures | operator, security reviewer |
| [`permissions.md`](permissions.md) | roles, per-repository scopes for users and tokens, enforcement | operator, security reviewer |
| [`dev-demo.md`](dev-demo.md) | the seeded development stack (`ibdemo`): running it, users, what is seeded, E2E | developer, anyone evaluating the UI |
| [`development.md`](development.md) | tools, test services and their variables, `just ci`, the web UI and end-to-end tests, what CI runs, how a release is cut and signed | developer, release manager |

## Security and formats

| Document | What it is | Read it if you are |
|---|---|---|
| [`format.md`](format.md) | byte-level repository format (keys, chunks, index, manifest, layout, catalog, escrow), pinned by test vectors | security reviewer, developer |
| [`sealed-delivery.md`](sealed-delivery.md) | IBS2 authenticated deliveries (threat model, byte format), browser ZIP name mapping, snapshot identity, key hygiene, security audit entries | security reviewer, developer |
| [`web-crypto.md`](web-crypto.md) | the browser decryption library (WebAssembly + TypeScript API), tests and throughput | developer, security reviewer |

## API

| Document | What it is | Read it if you are |
|---|---|---|
| [`api.md`](api.md) | management API conventions, roles, every area, and the generated table of all routes (OpenAPI at `/api/v1/openapi.json`) | integrator, developer |
| [`api-config.md`](api-config.md) | server information, setup checklist, notification tick and simulator, token overview | integrator |
| [`api-infra.md`](api-infra.md) | read models for hosts, objects, backup runs, pending onboarding | integrator, UI developer |
| [`api-ops.md`](api-ops.md) | tasks, maintenance mode and backups, repository detach/destroy, object delete, system status, logs, support report, update notice | integrator, operator |
| [`api-protect.md`](api-protect.md) | snapshot statistics, restore-test runs, serving the browser decryption library | integrator, UI developer |
| [`api-storage.md`](api-storage.md) | repository checks, effective policies, sync peer check, schedule preview | integrator, UI developer |
| [`web-protect.md`](web-protect.md) | the console's protection and recovery pages, the key gate, sealed deliveries, roles in the UI | UI developer |

## Performance and research

| Document | What it is | Read it if you are |
|---|---|---|
| [`performance.md`](performance.md) | what was measured, what changed because of it, tunables, what could not be measured | developer, operator sizing a deployment |
| [`memory.md`](memory.md) | memory model: what is bounded by what, measured bytes per entry, known linear costs | developer, operator sizing a deployment |
| [`pbs-comparison.md`](pbs-comparison.md) | source-level comparison with Proxmox Backup Server 4.2.8 and what was adopted | developer, anyone comparing products |
| [`block-level-research.md`](block-level-research.md) | why filesystem volumes are backed up file by file, and what block level would need | developer |

The Changelog is [`../CHANGELOG.md`](../CHANGELOG.md); the gate scripts are documented in
[`../scripts/gates/README.md`](../scripts/gates/README.md).
