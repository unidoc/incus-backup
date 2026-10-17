# Development demo (`cmd/ibdemo`)

`ibdemo` starts a complete, real incus-backup server stack in one process and fills it with weeks of
history, so the web UI can be developed against realistic data and Playwright tests have a known world.
It is a development tool only. It is never packaged, and it must never hold real data: its key is fixed
and published, and fsync is off.

Everything in it is the production code:

- **PostgreSQL**: embedded-postgres, with its data under `--dir`. It uses the same binaries cache as
  `internal/testdb`. It runs with `fsync=off`.
- **Migrations, PKI, repositories, policies, assignments and maintenance settings**: created through
  `internal/server/service`.
- **Agent API**: `httpapi.Server`, over mutual TLS, on a local port.
- **Management API and UI**: `mgmt.Server.WebHandler` on a **plain-HTTP** listener (`Secure=false`,
  so there is no HSTS and cookies are not Secure). The real auth store, sessions, CSRF, roles and audit
  log are all in place.
- **Unix admin socket**: `mgmt.Server.LocalHandler`.
- **Built-in S3 server** (default, `--s3=false` to turn it off): the in-process S3 test server
  (`internal/s3test/s3fake`, data in `<dir>/s3`) backs the second repository, `offsite`, on S3.

The data is produced only through real code paths:

- Three simulated hosts run the real client:
  - `app.Init` with an enrolment token, mTLS and an escrow upload;
  - `app.Load`;
  - `app.Tick`, which runs scheduled policies, retries and inventory.
- Each host's Incus is the fake from `internal/client/incusx/fake`.
- The server's own `maint.Runner.Tick` provides capacity samples, verification, retention, GC,
  reconcile and the scheduled sync jobs.
- One shared clock is advanced hour by hour over 30 simulated days. It drives the service, the auth
  store and every host's engine, and switches to the real clock when seeding ends.
- No row is inserted with SQL.

## Running it

```sh
go run ./cmd/ibdemo --dir /tmp/ibdemo --reset --web 127.0.0.1:8099 --ui-dir web/dist
```

Or use the static build at `/tmp/ibwork/demo-bin/ibdemo`. When everything is ready, a banner appears
on stdout with:

- the URLs (web, agent API, admin socket, S3);
- the four users;
- the recovery key, the master key in hex and the escrow private key;
- where each seeded situation is.

Ctrl-C stops the listeners, the S3 server and PostgreSQL. Measured: 1.3 s right after a seed, 0.2 s otherwise; no process survives. A demo killed with SIGKILL leaves PostgreSQL running; the next start, or `--reset`, stops it. It only signals a pid whose command line names `postgres` and this directory.

| flag | default | meaning |
|---|---|---|
| `--dir` | `/tmp/ibdemo` | All state: `pg/`, `datastore/custody`, `s3/`, `hosts/` (client configs and trees of the simulated hosts), `server-state/` (CA), `demo.json` |
| `--reset` | false | Wipe `--dir` first, stopping any PostgreSQL a hard-killed demo left behind, then seed again |
| `--web` | `127.0.0.1:8099` | Plain-HTTP management API and UI (`127.0.0.1:0` = a free port, printed in the banner) |
| `--ui-dir` | `web/dist`, `../web/dist` or `../../web/dist` if one exists | Serve a Vite build from disk, re-read on every request, so `npm run build` needs no restart. Without it, the UI embedded in the binary is served |
| `--profile` | `full` | `full`: 30 days (measured 62–106 s on a loaded 4-core machine). `test`: 5 days (11–19 s) |
| `--seed` | true | `--seed=false`: only the users and an empty repository `custody`. A later start without `--seed=false` seeds into it |
| `--admin-password` | `demo-demo-demo-1` | Password of all four users (at least 12 characters) |
| `--s3` | true | Serve the S3 repository `offsite` from the built-in S3 server. `--s3=false`: `offsite` is a filesystem repository |
| `--maintenance` | false | Run the real maintenance scheduler on the real clock after seeding. Retention may then thin the seeded history |
| `--no-jobs` | false | Do not run the job executors of bosch and benfica |
| `--agent`, `--socket`, `--log` | | Agent API listener, admin socket path, server log level |

**Restarting without `--reset`** reuses the seeded directory and is ready in about 0.5 s. The ports of
PostgreSQL, the agent API and the S3 server are kept in `demo.json`, because the S3 endpoint and the sync
peer's address are stored in PostgreSQL. If a seed never finished, the next start refuses and asks
for `--reset`. A demo directory seeded by a build from before the schema consolidation
(`CHANGELOG.md`) is refused by the schema guard; start it once with `--reset`.

Memory use:

- The binary sets `GOMEMLIMIT=1GiB` and `GOGC=50` unless you set them yourself.
- Peak RSS of the full seed was 0.87 GB. After seeding, RSS is about 120 MB.

### Users and login

| user | role |
|---|---|
| `admin` | admin |
| `operator` | operator |
| `restorer` | restore |
| `viewer` | readonly |

To log in:

1. `POST /api/v1/session` with `{"name","password"}`.
2. The response sets the `ib_session` cookie and returns `{"csrf": ...}`.
3. Send `X-CSRF-Token` on every non-GET request.

Login is limited to 20 attempts per minute per IP (inside mgmt). In E2E, log in once per role and
reuse the Playwright `storageState`.

**Two repositories exist.** Every repository-scoped route therefore needs `?repository=custody` or
`?repository=offsite`. Without it, the server answers 400.

### Vite dev server

Proxy `/api` to the demo, and keep the Host header:

```js
// vite.config.js
server: { proxy: { '/api': { target: 'http://127.0.0.1:8099', changeOrigin: false } } }
```

The CSRF check compares `Origin` with `Host`. With `changeOrigin: true`, every POST, PUT or DELETE
gets a 403. Cookies are not `Secure` on the demo listener, so `http://localhost:5173` works.

## What is seeded

The `full` profile simulates the 30 days that end at the moment seeding starts.

**Users:** the four above.

**Repositories:**
- `custody`: filesystem.
- `offsite`: S3 (the built-in test server), bucket `offsite`, prefix `ib`. Its credentials are stored sealed.
- Maintenance schedules:
  - custody: capacity sampled every 4 h;
  - offsite: capacity sampled every 12 h, with verify, reconcile, prune and GC weekly.

**Policies:**

| policy | schedule | keep |
|---|---|---|
| `nightly` | daily 03:00 | 14 daily, 8 weekly, 12 monthly |
| `hourly` | hourly | 48 hourly, 14 daily, 8 weekly |
| `weekly-offsite` | Sunday 03:00 | 12 weekly, 12 monthly |
| `ci` | 01:30 | 7 daily, 4 weekly |
| `none` | manual | keep last 1 |

**Assignments:**
- Host-wide `nightly` for bosch, benfica and uni-02.
- Project `benfica/customer-acme` → `weekly-offsite`.
- Object `bosch/staging/scratch` → `none`.
- `bosch/prod/redis` → `hourly` for the last 48 h. Its retention policy was changed to `hourly` with
  `SetObjectRetentionByGroup`.
- `ci-runner` gets policy `ci` from its own Incus config.
- The `nextcloud-data` volume gets `nightly` from its own volume config.

**Hosts:** all three are enrolled with real tokens and share one key file (one repository = one key
domain).

| host | projects | objects |
|---|---|---|
| `bosch` | `prod`, `staging` | VMs `prod/api-01`, `prod/db-01`, `prod/web-01`, `staging/ci-runner`; containers `prod/redis`, `prod/nginx`, `staging/scratch` |
| `benfica` | `default`, `customer-acme` | VMs `default/gitlab`, `customer-acme/erp`; custom filesystem volume `default/nextcloud-data` |
| `uni-02` | `default` | VM `monitoring`, container `pihole` |

bosch also stored an **escrow blob** (`format.WrapMasterKey`). The escrow private key is in the banner.

**Data:**
- Each VM disk holds incompressible "binaries", then log-like text. Every evening:
  - a 256 KiB rewrite lands in the text, every other day;
  - 192 KiB is appended to a growing log area.
- Container roots: about 400 files each.
- `nextcloud-data`: about 3000 files in up to 6 levels of directories. It includes:
  - text and binary files of many sizes, empty files and directories, hidden files and modes;
  - large "recordings";
  - relative, absolute and dangling symlinks, and a symlink to a directory;
  - unicode and awkward names (`Þórsmörk-ferð.txt`, `日本語のメモ.md`, `emoji 🎉 party.txt`, quotes,
    `;&`, very long names).
- Every evening a few files change, new ones appear, and now and then one is deleted.
- Redis rewrites its dump every hour.

**Situations:**

| situation | where | how it was produced |
|---|---|---|
| offline host | `uni-02` | It stops ticking 72 h before "now". Its last_seen is 3 days old |
| failing backups | `bosch/staging/instance/ci-runner/root` | Its NBD export fails (I/O error) from the last 01:30 run on. The client retries every hour, so runs are `partial` and bosch's last run is `partial` |
| a transient failure | `bosch/prod/api-01`, about 9 days ago | One night's NBD lock error, retried successfully an hour later |
| stale (overdue) | `bosch/prod/instance/web-01/root` | Excluded with `user.backup.exclude=true` 3 days ago. All of uni-02 is stale too |
| never backed up | `bosch/staging/instance/scratch/root` | Policy `none` |
| hourly snapshots | `bosch/prod/instance/redis/root` | Hourly for the last 48 h, daily before that |
| protected snapshot with a note | db-01 root, about 20 days ago | Protected and annotated ("Before the PostgreSQL 16 → 17 upgrade (CHG-2291)…"). The object also has group notes |
| failed verification | db-01 root, the snapshot of 2 nights ago | See below |
| sync | push `offsite`, verified-only: db-01, api-01, nextcloud | Ran daily at 06:00. Its peer identity was disabled for 3 h mid-month, so two hourly retries failed. Its last run is `ok` |
| sync | push `offsite-archive`: erp, transfer-last 2 | Its peer identity was revoked 3 h before "now", and an operator-style `Run` then failed. Its last run is `failed`, which makes the dashboard show `sync_failing` |
| maintenance history | both repositories | The real scheduler ran it: retention 04:00, GC 05:00, verify and reconcile daily, scrub monthly |
| capacity | custody (181 samples over 30 days); offsite | Samples every 4 h (custody) or 12 h (offsite) |
| filesystem snapshots | `benfica/default/volume/nextcloud-data` and the container roots | Catalog format, browsable |

How the failed verification was produced:

1. A scratch area of db-01 was filled with random data one evening and overwritten the next.
2. The chunk holding the first version is therefore referenced by exactly one snapshot.
3. At the end of the seed, its ciphertext was flipped on disk.
4. A full real verification (`VerifyJob`, mode `all`) found it. It quarantined the chunk, marked the
   snapshot `failed` and recorded the failed verify run.
5. The dashboard shows the `corrupt_chunks` and `verify_failed` problems.

Measured on one full seed:

| repository | snapshots | backup runs | physical | logical | dedup ratio |
|---|---|---|---|---|---|
| custody | 425 | 166 (523 snapshots in total) | 569 MiB | 9.6 GiB | 16.9 |
| offsite | 98 | | 266 MiB | | |

On disk this took 590 MB for the datastore and 285 MB for the S3 data (measured with MinIO, before the built-in server replaced it).

The banner and `demo.json` (`facts`) name the exact group and snapshot ids.

### The key

The master key is fixed: SHA-256 of a public string. The recovery key therefore stays the same across
`--reset`, and E2E tests may hard-code it:

```
IBR1-6AY4T-SWIJX-K5C43-V7OSW-4BZPH-W4PX2-M5EJW-NSVNN-IJDTC-7FVEB-TCGWN-CPQ
```

- This is the string `incus-backup key recovery` prints, and what the wasm `loadRecoveryKey` takes.
- The key domain is `6d25f56a1b429a7c`.
- The browser fetches ciphertext from the restore-role routes and decrypts it itself:
  - `GET /api/v1/snapshots/{group}/{snap}/index`
  - `GET /api/v1/snapshots/{group}/{snap}/manifest`
  - `GET /api/v1/chunks/{id}`
  - each with `?repository=…`.
- The package test does the same in Go, with that string alone.

## Host jobs, notifications and restore tests

After seeding, and again on every restart, the demo runs what the real daemon runs next to the APIs:

- the notification engine (`mgmt.NewNotifyEngine`, evaluated every minute) and its bridge to the
  event stream;
- the job bridges (`StartBridges`): live `job` events on `/api/v1/events`, and a failed job raises a
  `job_failed` notification event;
- the job-lease housekeeping (`jobs.Maintain`, every minute). With `--maintenance`, the maintenance
  runner does it instead.

The **job executors** of the simulated hosts `bosch` and `benfica` run `App.StartJobs`, so jobs
created through `POST /api/v1/jobs` (or `POST /api/v1/restore-tests/run`) are actually executed.
uni-02 has no executor, because its polling would bring it back online.

- On a fresh seed, the seeded host objects are reused.
- On a restart, each host is rebuilt from its own `hosts/<host>/client.yaml`, key file and state
  directory. Its fake Incus world is rebuilt too: the container roots and the volume tree on disk are
  reused, and the VM disks are replayed.
- Both hosts allow **every job type** (`jobs.allow` lists all of `jobspec.Types` in their
  `client.yaml`). A real host must opt into restores and file deliveries.
- What works: `backup_now`, `restore_test` (deep, extract, full: the scratch volume is created and
  deleted in the fake), `deep_verify`, `restore_instance`, `restore_volume`, and `fs_extract` with a
  sealed delivery (`GET /api/v1/jobs/{id}/delivery`, opened in the browser with its X25519 key).
- What does not: `guest_files` fails (no recovery VM).
- `--no-jobs` turns the executors off; queued jobs then stay queued.

**Seeded restore-test history.** The hosts' own restore-test planner (inside `App.Tick`, monthly
policy schedule) ran during the 30 simulated days. One full seed recorded 180 tests:

| method | passed | failed |
|---|---|---|
| deep | 117 | 15 |
| extract | 23 | 15 |
| full | 10 | 0 |

All 30 failures belong to the never-backed-up `scratch` object (see the limits).

Memory with executors running (the bosch and benfica worlds stay in memory):

- after a full seed: RSS about 370 MB (seed peak 0.79 GB);
- after a restart: RSS 268 MB (start-up peak 0.43 GB).

## E2E with Playwright

1. In `globalSetup`, spawn `ibdemo --dir <tmp> --reset --profile test --web 127.0.0.1:<port>
   --ui-dir web/dist`. Wait for the line `Ready in` on stdout, and kill it with SIGINT in teardown.
   - Use `--profile full` for visual work.
   - Always use `--reset` for tests. The situations decay with real time:
     - the hourly object goes stale about 3 h after the seed;
     - bosch and benfica count as offline 6 h after their last tick;
     - daily objects go stale after about 30 h.
2. Log in once per role through the API (or the login form), save `storageState`, and reuse it. This
   is because of the login throttle.
3. Assert against the documented situations. Read names and ids from the banner or `<dir>/demo.json`
   (`facts`): offline `uni-02`, stale `web-01`, failing `ci-runner`, `offsite-archive` failing,
   protected and failed db-01 snapshots, and the browsable `nextcloud-data` with the file named in
   `facts.fs_file`.
4. Never run two demos on one `--dir`. Several demos with different `--dir` values and web ports can
   run side by side; each gets its own PostgreSQL and S3 ports.

The package test `internal/demo` (`go test ./internal/demo`)
does this in-process, with random ports and the reduced seed. It:

- logs in with the real cookie and CSRF flow;
- checks every situation in `/api/v1/overview`;
- checks that a PUT without the CSRF token is refused (403);
- checks that `viewer` may not fetch raw index material but `restorer` may;
- decrypts the catalog and one unicode-named file of the filesystem snapshot from the raw routes with
  the printed recovery key, and compares it with the file's SHA-256;
- uses the admin socket;
- runs an `fs_extract` job on benfica as the restorer. It downloads the sealed delivery, checks that it
  carries no plaintext and that the viewer gets 403, unseals it with its own X25519 key, and compares
  the file's SHA-256;
- runs a `restore_test` job (extract) as the operator and finds the passed test, with its job id, in
  `/api/v1/restore-tests`;
- restarts the stack on the same directory and runs a deep `restore_test` on bosch, which proves the
  executors were rebuilt from the hosts' state.

Measured: 13 s, of which the seed takes 7 s (other runs up to 20 s on a loaded machine). Under
`-race` it took 50 s. The < 30 s seed limit is only enforced without `-race`.

## Limits and deviations (honest list)

- **VM disks are 32–128 MiB, not 2–8 GiB.** The fake Incus keeps every disk as a byte slice. Multi-GiB
  "sparse" slices were tried: the seed paged gigabytes in and pushed the shared machine into swap.
  Logical sizes in the UI are smaller accordingly. The deduplication ratio is still realistic, because
  disks are mostly zero, which is reported as logical.
- **VM disks after a restart are a replay.** They only ever lived in memory. A restart rebuilds them
  from the same deterministic writes, so they are close to, not byte-identical with, the seeded state.
  Their dirty bitmaps are gone, so the first `backup_now` of a VM reads it in full (the correct
  reaction to a missing bitmap).
- **`guest_files` jobs fail.** The fake Incus has no recovery VM wired in the demo.
- **Seeded restore tests include failures for `scratch`.** The client's restore-test planner also
  picks the never-backed-up object (policy `none`), and every test of it fails with "holds no
  snapshot". That is the real planner's behaviour, not a seeded situation.
- **GC removes no chunks during the seed.** A chunk's age is its file mtime, which is real time, while
  the GC cutoff is simulated time. All GC runs are recorded, but nothing is collected.
- **`fsync` is off** for PostgreSQL (`fsync`, `synchronous_commit`, `full_page_writes`) and for the
  filesystem datastore (`datastore.SkipFsync`, set only by the demo). Without that, the seed took
  more than 4 minutes on this machine.
- The client's own restore verification (`verify_schedule`) is set to manual on the simulated hosts.
  Server-side verification runs normally.
- Seed time depends heavily on machine load. The full seed took 62–106 s with a load average of 5–20
  on 4 cores.
