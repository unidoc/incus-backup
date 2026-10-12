# Management API: storage and protection-configuration routes

Routes added for the web console's repository, policy and sync pages
(`internal/server/mgmt/routes_storage.go`, service code in `internal/server/service/repocheck.go`,
`policy_effective.go`, `internal/server/syncer/history.go`, `internal/datastore/bucketcheck_s3.go`).
Conventions are those of `docs/api.md`. Roles are registered in the file's `init()` (the role matrix test
covers them).

| route | role | notes |
|---|---|---|
| `POST /repositories/check` | admin | test settings of a repository that does not exist yet (body = the create/attach body plus `attach`) → `RepoCheckReport` |
| `POST /repositories/{name}/check` | operator | the same deep check of an existing repository (stored credential, no secret leaves the server) |
| `GET /repositories/{name}/info` | readonly | identity: `datastore_id`, `domain_id`, this `server_id`, `owner` (owner.json read now), `datastore_created`, mode |
| `GET /policies/effective` | readonly | R. Every storage object with the policy that governs its retention and why |
| `GET /schedule/preview?spec=&count=&after=` | readonly | the server's parser and clock: `{valid, error, manual, next[], zone, due_now}` |
| `GET /sync/{name}/runs?limit=` | readonly | R. Run history of a sync job, newest first (`status: running` while one runs) |
| `PUT /sync/{name}/schedule` | operator | R. `{schedule?, enabled?}`: change the schedule, pause or resume |
| `POST /sync/check-remote` | admin | `{remote, token?}` → reachability and identity of a sync peer WITHOUT redeeming the token |

## Repository checks

`RepoCheckReport`: `{kind, purpose: create|attach|existing, location, checks[], datastore, server_id, ready, needs_adopt, free_bytes?, total_bytes?}`.
Each check is `{id, label, level: pass|warn|fail|skip|unknown, detail}`; `id` is stable, so a client keys
advice on it, never on the prose. `ready` means no check failed; for create/attach it predicts that the
real request passes (the check is stricter: relative paths and taken names fail here already).

* filesystem: `name`, `path` (absolute; missing → can the server's user create it in the nearest existing
  parent; existing → readable/writable, empty for create, `datastore.json` for attach), `attached` (another
  repository of this server uses the directory or datastore id), `datastore`, `owner`, `space` (statfs; warn
  below 1 GiB or 10 % free), `write` (a temporary file written and removed; skipped for an existing repository
  that is not online). Nothing is created: no directory, no `owner.json`.
* S3: `name`, `credential` (inline keys or a stored one by name; nothing is stored), `tls` (warn for plain
  HTTP or skipped verification), the datastore probe steps `connect`, `put`, `conditional`, `get`, `stat`,
  `list`, `delete` (exactly `repo probe`; later steps are `skip` after a failure; one scratch object under
  `_probe/` is written and deleted), `datastore` (create: none may exist; attach: one must), `attached`,
  `owner`, and the bucket settings `versioning` (warn when enabled or suspended: GC frees no space),
  `lifecycle` (warn for rules that expire or transition objects under the prefix; tag-filtered rules are
  ignored because the repository writes no tags), `object-lock` (warn with a default retention). A
  permission error on the bucket settings is `unknown`, not a failure.
* `owner`: pass when none or this server; for attach of a repository owned by another server `fail`
  without `adopt` and `warn` with it; `needs_adopt` is set in both cases and `datastore.owner` names the
  owner (`server_id`, `host`, `since`, `previous`).
* existing: additionally `open` (the server opens the store exactly as it does for backups) and
  `identity` (datastore id at the location equals the one pinned at creation).

## Effective policies

`[{group_id, host, project, name, kind, owner, declared_policy, pinned_policy, policy, source, assignment?, policy_known, snapshots}]`.
`source` is `object`, `project`, `host` (an assignment of the object's host; `owner` is the name
assignments match, `instance/web/root` → `web`), `pinned` (the retention policy pinned at first sight) or
`none`. It is the decision `Prune` makes (tested against `PrunePreview`); `policy_known: false` means the
policy does not exist and the object keeps everything.

## Sync peer check

`{ok, address, ca_fingerprint?, checks[]}` with check ids `address`, `token` (format), `dns`, `tcp`, `tls`
(TLS 1.3 handshake), `identity` (the peer's certificate chains to the CA the token pins: it is the server
that issued the token) and `api` (an agent API that asks for a client certificate). The token is never
sent; whether it is unused and has the right scope is only known when `POST /sync` redeems it.

## Schedule preview

Times are in the server's local zone (maintenance and sync jobs fire on the server's clock; backup
schedules fire on each host's clock, so for policies the preview is indicative). With `after=` (RFC 3339,
the last run) `due_now` says that the job is overdue and runs at the scheduler's next tick.
