# Management API: infrastructure read models

Routes added for the hosts, objects and capacity pages of the web console. They live in
`internal/server/mgmt/routes_infra.go` (handlers) and `internal/server/service/infra_views.go`
(service layer). All of them are plain PostgreSQL aggregates; none reads the datastore.
Conventions (errors, `?repository=`, times) are those of `docs/api.md`.

| route | role | notes |
|---|---|---|
| `GET /runs/backups` | readonly | backup runs the hosts reported (`backup_runs`), newest first |
| `GET /objects/stats` | readonly | every storage object with freshness, newest snapshot and size history |
| `GET /onboarding/pending` | admin | enrolment tokens that were issued and not used yet |
| `DELETE /onboarding/pending/{id}` | admin | revoke one unused token |

## `GET /runs/backups?host=&status=&before=&limit=`

Repository-scoped. `host` filters by host name, `status` by `running|ok|partial|failed`,
`before` (RFC 3339) pages backwards by start time, `limit` is 1..1000 (default 100). A bad
`limit` or `before` is a 400. The answer is a list (`[]` when empty, never `null`):

```json
[{"id": "…", "host": "bosch", "started": "2026-10-08T03:00:00Z", "finished": "2026-10-08T03:04:10Z",
  "status": "partial", "objects_total": 10, "objects_ok": 9, "objects_failed": 1,
  "logical_bytes": 102760448, "uploaded_bytes": 3313500, "error": "instance/ci-runner/root: …"}]
```

`host` is `""` when the host identity was deleted. `logical_bytes` and `uploaded_bytes` are what
the host reported for the run.

## `GET /objects/stats?host=`

Repository-scoped; `host` limits the list to one host's objects. One entry per storage object:

```json
{"group_id": "3d46…", "host": "bosch", "project": "prod", "name": "instance/db-01/root", "kind": "block",
 "declared_policy": "nightly", "retention_policy": "nightly", "policy": "nightly", "schedule": "03:00",
 "notes": "", "first_seen": "…", "snapshots": 7, "protected": 1, "failed_verify": 1,
 "latest": {"id": "20261008030000-000000f5", "created": "…", "logical_bytes": 67108864, "duration_ms": 0, "verify_state": "ok"},
 "freshness": "ok", "due": "2026-10-09T03:00:00Z", "overdue_at": "2026-10-09T09:00:00Z",
 "uploaded_bytes": 9262080, "history": [{"at": "…", "logical_bytes": 67108864}]}
```

* `freshness` is `ok | stale | never | unscheduled | reference`. It is computed by
  `service.Freshness`, the same function the overview's protection counts use (the overview was
  changed to call it), so the object badges and the dashboard cannot disagree; a test checks the
  counts against `GET /overview` now and three days later. `reference` marks instance records:
  their disks carry the data and are judged instead (the overview excludes them the same way).
* `policy`/`schedule`: the policy whose schedule judged freshness (the retention policy if known,
  else the declared one, like the overview); empty when neither is a known policy.
* `due` is when the next snapshot is due by the schedule, `overdue_at` when the object counts as
  stale (`due` plus a grace of 2 hours or a quarter of the interval). Both `null` when not scheduled.
* `latest` is `null` without a snapshot. `history` holds the logical sizes of the newest 30
  snapshots, oldest first.
* `uploaded_bytes` sums what the host reported as newly uploaded for the snapshots that still
  exist. It is a guide to which objects bring new data, not the object's stored size: under
  deduplication an object's stored size is not defined.

## `GET /onboarding/pending`, `DELETE /onboarding/pending/{id}`

Admin, repository-scoped. Lists unused, unexpired enrolment tokens, newest first:

```json
[{"id": "…uuid…", "host": "incus-01", "scope": "backup", "created": "…", "expires": "…", "replaces": false}]
```

The token itself is never returned (only its hash is stored). `replaces` is true when an identity
with that name and scope is already enrolled: using the token re-enrols it and revokes its earlier
certificates. `created` is the database clock, `expires` the service clock (they are the same in
production). `DELETE` removes an unused token of the repository (204), 404 when it does not exist,
was already used or has another repository.

## Notes for the UI

* `GET /capacity` takes a capacity sample on every call (`SampleCapacity` walks the datastore's
  chunks), so the capacity page calls it once per repository when that repository is opened, and
  the overview cards use the last sample from `GET /overview`.
* `POST /prune?dry_run=1` (admin) is the only retention preview. The object page calls it on
  request, never on load (it plans the whole repository and writes an audit entry), and shows the
  object's part of the plan.
* Host jobs started from these pages use `POST /jobs` with `backup_now` (one job per project for
  "everything on a host", since a job selects by project, policy or object) and `restore_test`
  (`{project, object, mode}`).
