# Host jobs and restore tests

Status: implemented (2026-10-08). Tested against real PostgreSQL, a real mutual-TLS server
(`testsupport.Node`), the filesystem and S3 (MinIO) datastores, and the fake Incus. Not yet run
against a real Incus host (see "Unproven" at the end).

## Why

The server never holds a payload key. Work that needs the key (proving that a backup restores,
restoring, handing a few files to an operator, deep verification) can therefore only run on a
host. Jobs let the management plane (the CLI, the admin API, later the browser GUI) **ask** a host
to do that work:

```
management plane --CreateJob--> PostgreSQL (host_jobs) <--GET /v1/jobs/next (long poll, mTLS)-- host daemon
                                                       <--progress / log / finish / delivery----
```

The server queues a job for one host. The host pulls it over its existing client certificate,
checks it again, runs it with its own key and reports progress, log lines and a result. A host only
ever sees, renews and finishes **its own** jobs: the repository and host name of its certificate,
backup scope. Any other job id gets 404, so a host cannot even learn that other hosts' jobs exist.

## Threat model change (read this)

Before jobs, design 3.1 said "the server cannot make it restore or delete anything". Jobs relax that on
purpose, so the **host decides what it accepts** (design 3.1 now states this rule):

- `client.yaml`:

  ```yaml
  jobs:
    # disabled: true                # take no jobs from any server
    allow: [backup_now, restore_test, deep_verify, restore_instance, restore_volume, guest_files, fs_extract]
  ```

  Without `allow`, a host accepts only **`backup_now`, `restore_test` and `deep_verify`**. These
  cannot change Incus objects (a full restore test only creates and always deletes its own scratch
  volume `ibk-rt-*`) and hand out no data.
- `restore_instance` and `restore_volume` create objects in Incus. They never overwrite: an
  existing instance or volume is refused.
- `guest_files` and `fs_extract` hand **file contents** to whoever holds the recipient key in the
  job. A compromised server could queue one with its own key. Allow them only on hosts where that
  risk is accepted.
- A refused type is not run. It is claimed and failed at once with "job type X is not enabled on
  this host (jobs.allow in client.yaml)", so the operator sees why.
- The executor validates parameters again (strict JSON, enums, cleaned paths, sizes, key
  length). The server's validation is not trusted.
- No backup-scope credential can delete anything. Jobs add no delete either.

**What the server learns.** It already knows operational names (hosts, projects, instance and
object names, snapshot ids). Jobs add:

- the paths an operator explicitly typed into a `guest_files` or `fs_extract` request (plus, for
  `guest_files`, the per-path status `ok` or `missing` the recovery VM reported **for those paths
  only**);
- counts, sizes, durations and chunk ids.

Nothing a host sends back names a file inside a volume. Messages and log lines are written by the
executor itself. Engine errors reach the server only when they are `engine.PublicError` (made only
of chunk ids, snapshot ids, object names and counts). Anything else is summarised as "failed; the
full error is in the host's log `<state>/jobs/<job>.log`". A test proves this: an extract test over
a corrupted chunk names the chunk, and no file name of the tree appears in the message, the log,
the result or the restore-test details.

## Job types

| type | params | permission | heavy | attempts | timeout | default on host |
|---|---|---|---|---|---|---|
| `backup_now` | `project`, `object` (owner name like `vm1` or logical name like `volume/biggi`), `policy` (AND, at least one) | `jobs.backup` | yes | 2 | 24 h | allowed |
| `restore_test` | `project`, `object` (logical name), `snapshot`?, `mode` deep\|extract\|full (default deep) | `jobs.restore_test` | yes | 2 | 12 h | allowed |
| `deep_verify` | `last` (1..1000, default 1), `project`?, `object`? | `jobs.verify` | yes | 2 | 24 h | allowed |
| `restore_instance` | `project`, `instance`, `snapshot`?, `as_project`?, `as_name`?, `volumes`, `start`, `keep_network` | `jobs.restore` | yes | 1 | 24 h | **off** |
| `restore_volume` | `project`, `object`, `snapshot`?, `pool`, `as_project`?, `as_name` | `jobs.restore` | yes | 1 | 24 h | **off** |
| `guest_files` | `project`, `object` (VM disk), `snapshot`?, `paths[]` (≤256), `partition`, `max_entries`? (default 1,000,000, at most 10,000,000), `recipient_public_key` | `jobs.files` | yes | 2 | 2 h | **off** |
| `fs_extract` | `project`, `object` (filesystem), `snapshot`?, `paths[]` (≤256), `recipient_public_key` | `jobs.files` | no | 2 | 2 h | **off** |

`internal/jobspec` is the one definition: types, `Validate` (strict decode, canonical JSON
returned), `RequiredPermission`, limits and wire types. The server and the executor both call it.

**Execution on the host.**

- One heavy job runs at a time. It never runs next to a scheduled backup run: it shares a lock with
  `App.Tick`.
- Up to two light jobs (`fs_extract`) run beside it.
- The host asks only for the types it can run right now (`?types=`), so a claimed job never waits
  for capacity.
- Cancellation goes through the job context: it reaches the engine, the NBD streams and the chunk
  downloads.
- `backup_now` records per-object state. Unlike a scheduled run it does **not** move the policy's
  schedule, so tonight's run still happens. A test proves this.

**restore_test methods.** None of them touches the original object.

- **deep**: downloads, decrypts and recomputes the keyed id of every distinct chunk of the
  snapshot, in parallel. It lists **every** bad chunk (up to 50 by id), not just the first.
- **extract** (filesystem catalog snapshot):
  - restores into a scratch directory `ibk-rt-*` under `<state>/scratch` (0700), after a
    free-space check against `fs.payload_size`;
  - every file is checked against its SHA-256 while it is written, then re-read from disk and
    compared again;
  - the files, directories and symlinks on disk are counted against the catalog;
  - the directory is always deleted, even when restored directories are read-only.
  - Archive-format (tar) snapshots have no per-file checksums. The result says so (`details.note`)
    instead of claiming more.
- **full**:
  - **Block objects** are restored through the normal NBD write path into a scratch volume
    `ibk-rt-<random>`, in `recovery.pool` or else the object's pool. The SHA-256 of the decrypted
    stream (exactly `Index.Size` bytes, zero chunks included) is computed on the way. The volume is
    then read back over a **new** read-only NBD connection and hashed.
    `details.expected_sha256` and `details.volume_sha256` must match.
  - **Filesystem objects** are restored into a scratch filesystem volume and checked like extract.
  - The scratch volume is deleted on every path (success, failure, cancellation) with a fresh
    context. The "created" flag is set before `CreateVolume`. If a delete fails, the volume name is
    logged on the host.

## Host protocol (mutual TLS, backup scope)

| route | purpose |
|---|---|
| `GET /v1/jobs/next?wait=30&types=a,b` | claim the oldest queued job of this host (FIFO), long poll up to 60 s; 200 with the job, or 204 |
| `POST /v1/jobs/{id}/progress` | `{attempt, progress, bytes_done, bytes_total, message, log[]}`; renews the lease; the answer carries `cancel_requested` |
| `POST /v1/jobs/{id}/finish` | `{attempt, state: succeeded\|failed\|cancelled, message, result, log[]}` |
| `GET /v1/jobs/{id}/cancel-requested` | `{cancel_requested, state}` |
| `PUT /v1/jobs/{id}/delivery` | header `X-Incus-Backup-Attempt`; streaming body of an already sealed file, at most 4 GiB; answer `{size, sha256}` |
| `POST /v1/restore-tests` | a restore test the host ran on its own schedule |

**Claiming and fencing.**

- A claim is one `UPDATE … WHERE id=(SELECT … FOR UPDATE SKIP LOCKED LIMIT 1)`. A test with 8
  concurrent pollers hands each of 20 jobs out exactly once.
- Every claim increments `attempt`.
- Every host write (progress, finish, delivery) carries its attempt. A host that was declared dead
  and comes back after its job was re-queued gets **409** and cannot overwrite the new attempt.

**Leases.**

- A running job is a lease of 5 minutes, renewed by every progress report. The executor reports
  every 30 s from a ticker that is independent of engine progress.
- When the lease runs out, `Maintain` (called from the server's maintenance tick) acts:
  - it re-queues the job while it has attempts left;
  - otherwise it fails the job;
  - a job whose cancellation was requested is cancelled instead.
- Partial deliveries of the dead attempt are deleted.
- A queued job nobody picks up within 24 h expires.

**Cancellation.**

- A queued job is cancelled at once.
- A running job gets `cancel_requested`. The host sees it in its next progress answer, cancels
  the context and finishes the job as `cancelled`.
- A cancelled restore test leaves no `restore_tests` row: it is no evidence either way.

**Daemon shutdown.** When the host daemon stops, its running jobs are not reported as failed: that
would burn an attempt. The lease runs out and the server re-queues each job its type allows to be
retried.

**Logs and messages.**

- Logs are capped at 1000 lines per job, plus one "truncated" marker. A line is at most 1000
  bytes.
- Control characters become spaces.
- At most 200 lines per call.

## Sealed deliveries

Limits of the file-recovery jobs: a delivery is at most 4 GiB (`jobspec.MaxDeliveryBytes`; `incus-backup restore
guest-files --max-bytes` defaults to the same 4 GiB), and a `guest_files` recovery is at most `max_entries`
files and directories (default 1,000,000; raise it with the job parameter or `--max-entries` up to the hard
ceiling of 10,000,000, e.g. for a large maildir).

`fs_extract` runs as root and restores the catalog's ownership, so the delivered tar is faithful. What the host
kernel refuses (a uid it cannot chown to, an xattr it rejects with EPERM/EINVAL/ENOTSUP) is skipped, not fatal:
the job log carries a `warn` line with the number of skipped operations and the first few names, and a file whose
chown was skipped loses its set-id bits. `security.*` and `trusted.*` attributes of the guest (SELinux labels,
file capabilities) are never applied on the host; `user.*` and POSIX ACLs are. The CLI (`fs extract`,
`restore instance`) is stricter by design: as recorded, any refusal is an error.

`guest_files` and `fs_extract` produce files for someone who holds a private X25519 key (the
browser, typically) AND the repository key. The format is IBS2 (`docs/sealed-delivery.md`): the
server that relays a delivery can neither read it nor forge, alter, truncate, reorder or substitute
it, and a delivery made for another job or other parameters does not open. The flow:

1. The host extracts into a temporary directory under `<state>/scratch` (0700).
2. It tars that directory (`archive.Write`), seals the stream with
   `format.NewSealWriter(w, keys, recipientPub, SealBinding{JobID, RequestDigest(type, params)})`
   (`IBS2`: X25519 to the recipient, authenticated with the target's repository key for this job
   id and the digest of the parameters the host received) and streams it to `PUT …/delivery`
   through a pipe. The plaintext never leaves the host unencrypted, and the server stores opaque
   bytes.
3. If tar or seal fails, the pipe is closed with an error. The server never sees a clean end, so
   no delivery is recorded.
4. The temporary directory is deleted.

**Storage.** The server stores the stream in the repository's datastore, through the `Backend`
(filesystem or S3 alike), as parts: `deliveries/<job>/<attempt>/p-NNNNNN`, 32 MiB each. The
`Backend` only writes whole objects, and a 4 GiB delivery must not sit in memory.

- Size and SHA-256 are computed while streaming.
- The `job_deliveries` row is written only after a clean end of the stream, and only if the job is
  still running on that attempt.
- `finish` compares the host's size and SHA-256 with the server's. A mismatch, or "succeeded"
  without a delivery, turns the job into `failed`.

**Lifetime.**

- `OpenDelivery` hands out the delivery only once the job has **succeeded**. A delivery of a job
  that failed or was cancelled is deleted at its finish.
- `OpenDelivery` streams the parts back and fails, instead of ending cleanly, if the size or
  SHA-256 does not match.
- After 1 h the reaper deletes the parts, then the row.
- Parts no row names whose job is not running on that attempt are orphans and are deleted. Both
  lookups happen in one SQL statement, so an upload in flight is never reaped.

**GC is not confused.** The `deliveries/` key space is outside `chunks/`, `groups/` and
`quarantine/`. Those are the only prefixes GC, verify, scrub, reconcile, the orphan cleaner and
capacity list. A test runs GC, verify-all, reconcile, `CleanOrphans(0)` and `CleanTmp(0)` with a
delivery present, on the filesystem and on S3. The delivery stays byte-identical.

**Tests on the delivery.**

- Generate an X25519 key and run a job.
- Assert that no stored object contains the plaintext.
- Unseal with the private key, the repository key and the binding (job id, digest of the
  parameters as sent) and check the tar.
- A different private key, another job id, other parameters or another repository's key cannot
  open it (`internal/client/jobs/executor_test.go`); what a malicious server can produce
  (a stream sealed to the right public key without the repository key, frames dropped, swapped,
  duplicated or spliced from another delivery, an IBS1 stream) is refused
  (`internal/format/sealedbox_test.go`, `internal/webcore/seal_test.go`, `web-crypto/test/seal.test.ts`).

## Restore tests

**Table.** Results go to `restore_tests`:

- object `"<project>/<logical name>"`, snapshot, method, state passed|failed|running;
- times, duration, bytes, files, `details` (JSON without file names);
- `job_id` when a job ran the test. `ON DELETE SET NULL`, so records outlive job retention.

**Scheduling.** The client daemon schedules tests itself, from the policy's
`restore_test_schedule` (server-side policies; default `monthly`).

- **Coverage:**
  - The schedule defines a period within which every (object, target) pair must be tested.
  - Once a day (or once per period, if shorter) a slot tests a rotating share: oldest test first,
    `ceil(N / slots-per-period)` pairs, plus any pair already older than one period and one slot
    (the daemon was down).
  - Simulated with a fake clock (60 objects, monthly, 4 months, hourly ticks): 123 slots, at most
    4 tests per slot, longest gap between two tests of an object 30 days.
  - After 10 days of downtime, every overdue object is tested at the first slot back.
- **Method:**
  - filesystem objects: `extract`;
  - VM disks and block volumes: `deep`, and `full` every 4th test;
  - instance objects (stored configuration): `deep`.
- **Recording:**
  - Locally in `<state>/restore-tests.json` (the planner's history, plus job-driven tests).
  - On the server through `POST /v1/restore-tests`. The server accepts a report only for an object
    that host owns (`storage_objects`), so a host cannot forge evidence for another host's objects.
- Policies defined only in `client.yaml` have no restore-test schedule and are never tested
  automatically.
- Manual runs:
  - `incus-backup restore-test run [--object X --project P --mode deep|extract|full --server S]`
  - `restore-test run --all`
  - `restore-test run` with neither flag runs what the schedule has due.
  - `restore-test list` shows the local history.

## Server-side Go API (`internal/server/jobs`)

`jobs.For(svc)` returns THE job service of a `*service.Service`. Use it everywhere (host routes,
management routes, maintenance), so the change hub is shared.

- `RequiredPermission(type) string`: the authorisation hint to check before `CreateJob`.
- `CreateJob(ctx, principal, repo, host, type, params) (*Job, error)`:
  - validates the type and params and stores the canonical form;
  - refuses unknown, disabled and non-backup-scope hosts (404 or 409);
  - rings the long-poll bell.
- `ListJobs(ctx, JobFilter{Repository, Host, Type, States, Before, Limit})`, newest first.
- `GetJob(ctx, id)`: includes the log and the delivery metadata.
- `Cancel(ctx, principal, id)`.
- `ListRestoreTests(ctx, RestoreTestFilter{...})` and `LatestRestoreTestPerObject(ctx, repo)`. The
  latter returns the newest finished test per (host, object); a running one does not hide the last
  result.
- `OpenDelivery(ctx, jobID) (io.ReadCloser, size, sha256, error)`. The bytes are opaque: relay them
  to the browser, which holds the private key.
- `Subscribe() (<-chan Event, cancel)`:
  - non-blocking and bounded: 64 events per subscriber, 256 subscribers;
  - a slow subscriber loses events, and its next event has `Lagged=true` (re-list);
  - events carry no params or results.
- `Maintain(ctx)` = `RequeueStale` + `ExpireQueued` + `ReapDeliveries` + `Retention`. It is
  already called from `maint.Runner.Tick`.
- `Retention` keeps finished jobs for 30 days and at most 1000 per repository (`KeepFor`,
  `KeepPerRepo`).

Errors wrap `service.ErrBadRequest`, `ErrNotFound`, `ErrConflict` and `ErrForbidden`, so
`httpapi.fail` maps them as usual.

## Tables (`migrations/20261008000000_initial_schema.sql`, section "host jobs")

- `host_jobs`: id, seq, repository_id, host_name, type, params, state, progress, message, result,
  created_by, created_at, started_at, finished_at, cancel_requested, attempt, max_attempts,
  lease_until, queue_until, bytes_done, bytes_total.
- `host_job_log`: job_id, seq, at, level, line.
- `restore_tests`: see above.
- `job_deliveries`: job_id, repository_id, size, sha256, parts, storage_key, created_at,
  expires_at.

## Unproven / not done

- Not run against a real Incus host. Untested in real life:
  - full restore tests: a real scratch volume on ZFS, the NBD write and read-back speed;
  - `guest_files` through a real recovery VM (gate G3c);
  - a real host daemon under load.
- Browser-facing routes exist (`/api/v1/jobs`, `/jobs/{id}/delivery`, `/restore-tests`; `docs/api.md`) and the
  console uses them (Protection -> Restore).
- `incus-backup restore-test run|list` builds and prints `--help`, but it has no automated test.
  The functions it calls (`App.PlanRestoreTests`, `RunRestoreTests`, `History`) are tested.
- The server CLI has `jobs list|create|cancel`; restore tests are run from the console or the host CLI.
- A job created by another process (the CLI next to the daemon) wakes a long poll within the
  database re-check interval (1 s), not instantly.
- No resume of a half-done job: a re-queued attempt starts over. That is idempotent for every
  retried type: `restore_*` are never retried, and partial deliveries are deleted.
- Delivery parts are written in the order received. A server crash mid-upload leaves parts the
  orphan reaper removes; there is no resumable upload.
