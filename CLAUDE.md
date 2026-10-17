# incus-backup: working notes

Two Go binaries from one module (`github.com/unidoc/incus-backup`, Go 1.26, `CGO_ENABLED=0`):
`cmd/incus-backup` (client, runs on the Incus host) and `cmd/incus-backup-server` (repository
server). Layout follows `incus-sync`: cobra CLI, `internal/`, Justfile, `version.txt`.

## Ground rules
- `docs/design.md` is the contract; `docs/format.md` is the byte-level spec pinned by
  `internal/format/testdata/vectors.json` (never "update" vectors to make a test pass; bump the
  format version instead). `scripts/verify-vectors.py` re-checks them with an independent implementation.
- PostgreSQL (pgx/v5) is the ONLY control-plane implementation. No SQLite/YAML/in-memory fallbacks
  for the control plane. The DATASTORE is pluggable (`internal/datastore.Backend`: filesystem or native S3): same
  object model everywhere, no POSIX assumptions in repository logic (`MemBackend` enforces that), commit =
  `commit.json` written last. S3 tests run against the in-process fake `internal/s3test/s3fake` (hermetic, strict, fault injection; see
  `docs/testing-s3.md`); `INCUS_BACKUP_TEST_S3_*` points the same tests at a real S3 service. Any new S3
  behaviour the code relies on needs the fake AND a conformance check (`internal/s3test/conformance`), proven
  against versitygw (`just test-s3-real`). No MinIO. The datastore is the source of truth for payload and snapshot/chunk
  relations; PostgreSQL can be rebuilt from it (`reconcile`).
- DB tests use `internal/testdb`: `INCUS_BACKUP_TEST_DATABASE_URL`, else embedded-postgres. They FAIL, never
  skip, when no database is available. Every package using it needs `TestMain(m) { testdb.Main(m) }`.
- Correctness beats incremental optimisation: any doubt about a bitmap/generation/target state means a
  full (deduplicated) read. A bitmap is deleted only when no target still needs it as a base.
- One capture per object per run; several targets and several key domains never cause a second read.
- The server never gets a key. Names of hosts/projects/policies/instances/volumes are control-plane
  data and ARE visible to it; contents, file names inside volumes and VM config are not.
- Backup-scope hosts can never prune/forget/delete; admin = local server CLI only.
- No guest agent is ever installed in guests (crash-consistent backups; gate scripts use cloud-init disks).
- Incus is reached only through `internal/client/incusx` (`Real` for production, `fake` for tests). Do not
  talk QMP, do not use `incus export`.
- Server configuration is data in PostgreSQL (CLI + local admin API over a unix socket, one service layer);
  `server.yaml` is bootstrap only. Secrets (S3 keys) are sealed in `credentials` and never returned by any API.
- Sessions are leases (renewed by activity) with one open session per object group (PostgreSQL partial unique
  index); GC safety comes from session pins, never from touching/copying objects.
- A snapshot that failed verification is never an incremental base; corrupt chunks are quarantined, not left "present".
- Filesystem snapshots default to the catalog format (`internal/fscat`, design 5.1); the tar format stays as the fallback.
  Anything doubtful in change detection means reading the file. Guest filesystems are NEVER mounted on the backup host
  (`internal/recovery`).
- Validation against a real Incus/ZFS host is `scripts/gates/*` and `docs/real-host-validation.md` (scratch project `ibgate` only). Never write
  scripts that touch other projects.

## Management plane (people, UI, API)
- One management API (`internal/server/mgmt`, `/api/v1`, contract in `docs/api.md`) serves the unix admin socket (implicit admin) and the
  HTTPS web listener (session cookie + CSRF, or Bearer API tokens). The CLI, the web UI and automation are clients of it; no business
  logic in handlers (service layer only). Every route has an entry in `mgmt/perms.go` (readonly < restore < operator < admin); a route
  without one panics at start. State-changing requests are audited; secrets are shown once.
- Hosts use mTLS (`httpapi`, :8007); people use `internal/server/auth` (argon2id, TOTP, sessions 30 min idle / 12 h). Never mix the two.
- Zero-knowledge UI: the browser decrypts (`web-crypto/`, Go->WASM `cmd/ibwasm`, key pasted in memory only). The server only serves
  ciphertext. Anything that needs plaintext/Incus (guest files, restore tests) is a HOST JOB (`internal/server/jobs`, `docs/jobs.md`),
  delivered sealed (IBS2) to a key the browser generated.
- Repository maintenance mode (online/read-only/offline) is enforced in `datastore.ModeBackend`, below the object model.
- Server config after bootstrap lives in PostgreSQL/API (`server_settings`, policies, repositories, notifications...), not files.
- Memory: backup, restore, verify, sync, GC stream with bounded memory (see docs/memory.md); never read a whole disk/tree into RAM.

## Commands
`just build` · `just test` (≈30 s, real PostgreSQL) · `just test-s3` (S3 fake + conformance, no services) · `just test-s3-real` (versitygw) · `just fuzz` · `just verify-vectors` · `just fmt`
Go toolchain note for sandboxes without 1.26: fetch the toolchain zip from proxy.golang.org with curl and use a
private GOMODCACHE.
