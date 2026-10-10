# Management API: operations (tasks, system, repository lifecycle)

Routes for running the server from the console: asynchronous server tasks, the server status page, the
daemon log, support reports, the update notice, Prometheus metrics, retiring a repository and deleting a
whole object. Code: `internal/server/tasks`, `internal/server/mgmt/routes_tasks.go`, `routes_system.go`,
`routes_lifecycle.go`, `metrics_collect.go`, `internal/server/service/repo_lifecycle.go`,
`internal/server/sysinfo`, `internal/server/logring`, `internal/server/updatecheck`. Conventions are those of
`docs/api.md`; roles are registered in each file's `init()` (the role matrix test covers them).

| route | role | notes |
|---|---|---|
| `GET /tasks?state=&kind=&repository=&started_by=&limit=&before=` | readonly | tasks, newest first. `state` is `queued`, `running`, `succeeded`, `failed`, `cancelled` or `active` (queued or running). `before` is a `seq` cursor |
| `GET /tasks/{id}?after=&limit=` | readonly | the task and its log lines with `seq > after` (default 1000 per call); `next_seq` continues the tail |
| `POST /tasks/{id}/cancel` | operator (admin for gc, retention, reconcile, repository_destroy, group_delete) | asks the task to stop; it ends `cancelled` at its next check. An ended task: 409 |
| `POST /prune` | admin | without `dry_run=1`: **202** `{task}` (kind `retention`, destructive). `dry_run=1` answers the plan directly, as before |
| `POST /gc[?dry_run=1&grace_hours=]` | admin | **202** `{task}` (kind `gc`; destructive unless dry run) |
| `POST /verify` | operator | **202** `{task}` (kind `verify` for `new`/`stale`, `scrub` for `all`; destructive unless `no_quarantine`). The task result is `{ok, error?, stats}` |
| `POST /reconcile` | admin | **202** `{task}` (kind `reconcile`) |
| `POST /sync/{name}/run` | operator | **202** `{task}` (kind `sync`) |
| `POST /repositories/{name}/probe` | operator | runs as a task (kind `probe`, history and log) but answers synchronously as before |
| `DELETE /repositories/{name}` | admin | body `{mode: detach\|destroy, confirm?, force_hosts?, clear_owner?}`; detach answers 200 `DetachResult`, destroy **202** `{task}` (kind `repository_destroy`) |
| `DELETE /objects/{group}` | admin | body `{force_protected?}`; **202** `{task}` (kind `group_delete`) |
| `GET /metrics` | readonly | Prometheus text exposition (see `docs/metrics.md`). Served as `/api/v1/metrics` |
| `GET /system/status` | readonly | `SystemStatus` (below) |
| `GET /system/samples?hours=&points=` | readonly | one sample a minute from `system_samples` (kept 7 days; `hours` ≤ 168, thinned to `points`, default 500) |
| `GET /system/logs?level=&q=&after=&limit=` | admin | the daemon's recent log (in memory, last 5000 lines at info and above): `{lines[], last_seq, capacity, dropped}` |
| `POST /system/support-report` | admin | body `{include_host_names?}`; answers a `.tar.gz` download |
| `GET /system/update` | readonly | the opt-in update check's state |
| `POST /system/update/check` | admin | asks the release feed now (refused, with `error`, while `update_check` is off) |

## Tasks

Every long server-side operation runs as a task: manual GC, verify/scrub, retention, reconcile, sync runs,
repository probes, repository destroys and group deletes, and the scheduler's runs (capacity samples,
reconcile, retention, GC, verify, scrub, scheduled syncs; `started_by: "system"`).

`Task`: `{id, kind, repository, started_by, actor_kind: user|token|local|system, state, progress (0-100),
message, params, result, error, exclusive, cancel_requested, log_lines, log_dropped, created, started,
finished, duration_ms}`. `TaskDetail` adds `log: [{seq, at, level: debug|info|warn|error, line}]` and
`next_seq`.

* **Asynchronous by default.** The task routes check what they can before accepting (the repository exists,
  is online for an operation that writes, the GC grace is at least 12 h, the verify mode is known, the sync
  job exists, the delete is allowed) and answer 400/404/409/503 at once; otherwise **202** with the task.
  Follow it with `GET /tasks/{id}?after=` or the SSE event `task` (its data is the `Task`; progress events
  are throttled to about one a second per task).
* **`?wait=1`** keeps the old synchronous behaviour and body (the CLI's forwarding to the daemon uses it).
  The work is still a task and continues if the caller goes away.
* **One destructive task per repository.** `retention`, `gc` (not dry run), `verify`/`scrub` that may
  quarantine, `repository_destroy` and `group_delete` hold the repository's maintenance slot (a partial
  unique index in PostgreSQL). A second one gets **409** naming the task that holds it. A scheduled job that
  meets a busy repository is skipped (logged, not failed) and runs at the next minute it is still due.
* **Cancel.** The task's context is cancelled; GC, verify, retention, reconcile, sync and destroy check it
  between steps. A cancelled GC stops before its sweep or between sweep batches: nothing half-deleted, every
  remaining snapshot verifies (tested). The maintenance run of a cancelled task is `cancelled`, which does
  not raise `maintenance_failed`, and the scheduler does not restart it at once.
* **Restart.** Tasks a previous daemon left `queued` or `running` are marked `failed` ("server restarted
  while the task was running") when the daemon starts, as are its unfinished `maintenance_runs` and
  `sync_runs`. Only the daemon does this, never a CLI command.
* **Log.** At most 5000 lines per task (further lines are counted in `log_dropped`); lines are at most 4000
  bytes. History: 30 days and at most 2000 finished tasks (purged hourly).
* **Scheduled runs.** `maintenance_runs` stays the history the scheduler reads (`GET /runs`); every run now
  has `task_id`, the task that executed it.

## Maintenance mode and backups (F2)

A repository that is `read-only` or `offline` refuses a backup when the session opens
(`POST /v1/sessions` → **503** "repository is in read-only maintenance mode ... (the host retries at its next
run)"), before the host has read anything. A commit refused by a mode change during the upload is also 503
(it was a 400 before). `GET /v1/whoami` reports `repository_mode`; the host asks it at the start of a run
and does not read objects whose every target is in maintenance. Such objects are **deferred**, not failed:
the run is reported with status `deferred` and `objects_deferred` (`backup_runs.status` accepts
`deferred`), `incus-backup run` exits 0 and prints how many were deferred, and the alert condition
`backup_failed` ignores deferred runs (they neither raise nor resolve it). A run with real failures stays
`failed`/`partial`. A newer host talking to an older server falls back to `partial`/`failed`.

## Repository detach and destroy

Both need the repository **offline** (`PUT /repositories/{name}/mode`), no open upload session, and, when
identities (hosts or sync peers) are enrolled, `force_hosts` listing every one of them (their enrolment
goes with the repository).

* `detach` forgets the repository in PostgreSQL (repository, hosts, policies, objects, snapshot records,
  runs) and leaves the datastore untouched. `clear_owner: true` removes `owner.json` when this server owns
  it, so it can be attached again here or on another server without `--adopt`. `DetachResult`:
  `{repository, location, owner_cleared}`.
* `destroy` needs `confirm` equal to the repository name. In a task it checks that the datastore is the one
  PostgreSQL knows (its id), deletes every object (S3: every key under the repository's prefix, nothing
  outside it; filesystem: the directory's contents, the directory itself stays), the repository's marker
  files last, then forgets it. Progress is reported per 500 objects. If it fails or is cancelled the
  repository stays registered and offline with `mode_reason` saying so. `DestroyResult`:
  `{repository, location, objects_deleted, bytes_deleted}`.
* Both write an audit entry when they end (`repository detach`, `repository destroy`) with the result.

## Deleting a whole object (group)

`DELETE /objects/{group}` deletes every snapshot of one object and forgets the object (its chunks are freed
by the next GC). Refused with 409 while a backup of it runs or a running backup builds on it, and while any
snapshot is protected unless `force_protected: true` (the protection is then removed first and logged).
The repository must be online. Result: `{group, object, snapshots_deleted, protected_overruled}`; audit
action `object delete`.

## System status

`SystemStatus`: `{version, go_version, server_time, started, uptime_seconds, instance_name, host, process,
state_dir, datastores[], database, listeners[], update?}`.

* `host`: `{hostname, kernel, os, uptime_seconds, load1, load5, load15, cpus, cpu_percent (busy share since
  the previous reading; -1 unknown), mem_total_bytes, mem_available_bytes, mem_used_bytes, swap_total_bytes,
  swap_free_bytes}` (from `/proc`).
* `process`: `{pid, rss_bytes, open_fds, goroutines, heap_bytes, heap_sys_bytes, gc_cycles,
  gc_pause_total_seconds, go_version}`.
* `state_dir` and `datastores[]`: `{path, label, total_bytes, used_bytes, free_bytes, error?}` (statfs; an S3
  repository has `error: "object storage: no filesystem to measure"`).
* `database`: `{ok, error?, version, size_bytes, connections, max_connections, pool_total, pool_idle,
  pool_in_use, pool_max, migrations[], latency_ms}`.
* `listeners`: `{name: agent|web|admin|metrics, addr, detail}`.
* Clock: compare `server_time` with the browser's clock; the console warns about skew (no NTP query).

`GET /system/samples` rows: `{at, cpu_percent, load1, mem_used_bytes, mem_total_bytes, disk_used_bytes,
disk_total_bytes (state directory), db_bytes, process_rss_bytes, goroutines}`.

## Daemon log

The daemon tees its log into an in-memory ring (5000 lines, info and above). Attributes whose key looks
secret (`token`, `password`, `secret`, `key`, `credential`, `authorization`, `cookie`, `private`) are
replaced by `[redacted]` in the ring; the service log file is unchanged. `level` = `debug|info|warn|error`
(minimum), `q` = case-insensitive substring of message or attributes, `after` = seq to follow. A process
that is not the daemon answers an empty list with a `note`.

## Support report

A `.tar.gz` with `support/README.txt`, `version.json`, `config.json` (server.yaml with the database
password and every secret-looking value redacted), `status.json`, `overview.json`, `repositories.json` (no
credential names or secrets), `tasks.json` (last 200), `alerts.json` (open and the last 7 days),
`certificates.json` (public data), `settings.json`, `logs.txt` (the ring). It never contains database
passwords, S3 keys, API, enrolment or setup tokens (tested by searching the archive for each). Host names
(and the machine's hostname) are replaced by `host-1`, `host-2`, ... unless `include_host_names: true`.

## Update notice

Settings `update_check` (`on`/`off`, default off: the server never contacts anything) and
`update_check_url` (default `https://github.com/unidoc/incus-backup/releases/latest`). When on, the daemon
asks once a day: a `.../releases/latest` URL with HEAD, reading the version from the redirect's `/tag/vX.Y.Z`
without following it; any other URL with GET, expecting `{"version": "X.Y.Z"}` (or `tag_name`). Nothing
about the server is sent. `GET /system/update`: `{enabled, url, current, latest?, update_available,
checked_at?, error?, release_url?}`. While an update is available the alert condition `update_available`
(severity info) fires; it resolves after the upgrade or when checks are turned off.
