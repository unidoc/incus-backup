# Permissions: roles and repository scopes

People and automation use the management API (`/api/v1`) with a **role**:
`readonly` < `restore` < `operator` < `admin`. The role each route needs is the table in
`internal/server/mgmt/perms.go`. `docs/api.md` describes the roles. A role can be narrowed to some
repositories, for users and for API tokens. Hosts (backup agents) are not covered here: a host
certificate belongs to one repository and can only reach that repository through the agent API.

## Model

Every user has:

- a **global role** (`users.role`): `admin`, `operator`, `restore`, `readonly` or `none`. It decides the
  routes that are not about one repository: users, settings, notifications, certificates, credentials,
  creating repositories, the audit log;
- optionally, **per-repository roles** (`user_repo_scopes`): one role per repository. When a user is
  restricted (`users.repo_scoped = true`), each repository-bound route uses the user's role **on that
  repository**. A repository without a row is invisible to that user.

| user | on repository R | on a global route |
|---|---|---|
| not restricted (the default) | the global role | the global role |
| restricted, has a row for R | the role of that row | the global role |
| restricted, no row for R | nothing (403, or 404 for ids) | the global role |
| global role `admin` | admin (administrators are never restricted) | admin |

A per-repository role may be higher than the global role. For example, global `none` with `operator` on
`offsite` lets the user run verify and sync jobs on `offsite` and nothing else.

Administrators cannot be restricted. They manage users, so they could lift their own restriction. To
restrict a person, give them a lower global role first. The API refuses to restrict an administrator.

**Fail closed.** The restriction is an explicit flag, not "has at least one row". If a repository is
deleted, its rows go with it. A restricted user who had only that repository then sees nothing. They do
not fall back to the global role on every repository.

### API tokens

A token has its own role, at most the highest role its owner has anywhere. A token can also be
restricted to a list of repositories (`api_tokens.repo_restricted`, `api_token_repos`).

- **Unrestricted token:** on each repository it has the lower of its own role and the owner's role there.
  On global routes it has the lower of its own role and the owner's global role.
- **Restricted token:** it has no global rights at all, so it cannot reach users, settings or `/about`. On
  each listed repository it has the lower of its own role and the owner's role there.

A token never has more rights than its owner has now. Demoting, restricting or disabling the owner
limits the token at once, because the token's rights are computed on every request. Deleting every
listed repository leaves a restricted token with no access. It does not become unrestricted.

### Users who sign in through an identity provider

A user linked to an OpenID Connect provider (`docs/authentication.md`) has the same global role and
per-repository roles as anyone else, but the provider's mappings **set them at every sign-in**:

- global role = the highest of the provider's default role (never `admin`) and the role of each
  group → role mapping whose group is in the token's groups claim;
- when the provider has group → repository mappings and the role is not `admin`, the user is restricted
  (`repo_scoped = true`) to the repositories of the matching mappings, with the highest mapped role on
  each. A user in no mapped group is restricted to no repository at all (fail closed). Without repository
  mappings the global role applies to every repository.

Changes made by hand in the Repository access panel or the role editor last until that user's next
sign-in through the provider. Passkeys and password sign-in use exactly the same model: the method of
sign-in never changes what a user may do.

## Enforcement: one place

`internal/server/mgmt/scope.go` (`authorize`) runs before every handler. It reads the route's
**binding** from the route metadata (`routes_meta.go`), finds the repository the request touches, and
checks the principal's role on that repository.

| binding | repository | routes (examples) |
|---|---|---|
| `query` | `?repository=`, else the only repository (exactly what the handler resolves) | objects, snapshots, raw index/manifest/chunks, policies, hosts, maintenance, prune, GC, sync |
| `path` | `{name}` of `/repositories/{name}/…` | mode, probe, check, info |
| `body` | the JSON body's `repository`, else as `query` | `POST /jobs`, `POST /restore-tests/run` |
| `job` | the job's repository (looked up by id) | `GET /jobs/{id}`, cancel, delivery |
| `alert` | the alert's repository (an alert without one uses the global role) | `GET /alerts/{id}`, ack |
| `list` | per item | `GET /repositories`, `/overview`, `/jobs`, `/alerts`, `/restore-tests`, `/events` |
| `global` | none: the global role decides | users, settings, notifications, certificates, credentials, traffic |
| `self` | none: any signed-in principal | session, ping, `/me/*`, `/openapi.json` |

Further guarantees:

- **Pinned repository.** On `query` and `body` routes, the checked repository is written back into
  `?repository=`. The handler therefore resolves exactly the repository that was checked. Leaving
  `?repository=` out gives no free default: the default is resolved and checked first.
- **Every repository route is declared.** `withRepo` refuses to run (500) unless `authorize` treated the
  route as repository-bound. A handler that reads `?repository=` but is declared `global` therefore fails
  in tests. `TestNoUndeclaredRepositoryRoutes` calls every route.
- **Every route has metadata.** `TestEveryRouteHasMetadata` fails for a registered route without metadata.
  It also fails for metadata that names a route that does not exist.
- **Other repositories' ids look absent.** A job or alert id from a repository the caller cannot read
  answers 404, not 403. Snapshot groups and chunks are stored per repository: with
  `?repository=<allowed>` they are looked up only in that repository. With any other repository the
  request is refused before it is looked up.
- **The handler sees the effective role.** After the check, the principal's role is the role on the
  request's repository. Checks inside handlers therefore apply the per-repository role. An example is the
  job-type check in `POST /jobs`, where a restore needs `restore` and other jobs need `operator`.
- **Lists are filtered per item.** `GET /repositories` is filtered by `name`. `/jobs`, `/alerts` and
  `/restore-tests` are filtered by `repository`. The answer is filtered after the handler. For a
  restricted principal, an answer that is not a JSON array is refused. The overview is computed over the
  allowed repositories only (`service.OverviewFor`), so totals such as runs and protection counts leak
  nothing about other repositories.
- **Live events are filtered per subscriber.** Each SSE subscriber has a filter. An event's repository
  comes from its `repository` field, a `repository` event's `name`, or an alert's repository. An event
  that cannot be placed is not sent to a restricted subscriber. A global event needs the global role.

## The restore-role exfiltration problem (audit)

Before scopes, `restore` could download the encrypted index, manifest and chunks of **every**
repository. That was ciphertext, but a full copy. These routes are now `query`-bound. A restore user
scoped to `offsite` gets 403 for `?repository=custody`. Chunks are stored per repository
(`chunks/` of that repository's datastore). A chunk id that exists only in another repository is
therefore "not found", even with an allowed `?repository=`.

## Administration

- **Web console:** Users → the database button on a user's row opens **Repository access**. There you set
  the global role (including *No access*), "every repository" or "only these repositories", and a role
  per repository. Profile → API tokens → "Only these repositories" creates a restricted token.
- **API:**
  - `GET /api/v1/users/{name}/access` returns `{global_role, scoped, scopes: [{repository, role}]}`.
  - `PUT /api/v1/users/{name}/access` takes `{scoped, scopes}` and replaces all rows at once. With
    `scoped: false`, the rows are removed and the global role applies everywhere.
  - Change the global role with `PUT /users/{name}` `{role}`.
  - `POST /me/tokens` takes `{name, role, ttl_hours, repositories: [...]}`.
  - `GET /api-tokens` and `GET /me/tokens` show `repositories`: `null` means not restricted.
- **CLI:** `incus-backup-server user create NAME --role none` creates a person who only gets
  per-repository roles.
- **Session:** `GET /session` returns `access: {global, repositories: {name: role}}` for a restricted
  principal. The console then uses the role on the selected repository, or the global role if that is
  higher, to decide what it shows. The server decides again on every request.

## Tests

`internal/server/mgmt/scope_test.go` uses real PostgreSQL and two repositories:

- a matrix of 4 roles × every route × 2 repositories for a user with global `none` and one
  per-repository role;
- lists, the overview and the session for that user;
- IDOR attempts: a job id or alert id of another repository, including cancel and ack;
- jobs and restore tests created through the body for another repository;
- raw chunk, index and manifest requests for another repository;
- restricted tokens: no global rights, operator capped below admin, no widening past the owner;
- fail-closed behaviour after scope rows disappear;
- the access editor's validation;
- the SSE stream filtered per subscriber.

Negative controls were run by hand: declaring a `withRepo` route `global`, removing the list filter, and
removing the SSE filter each make these tests fail.

`internal/server/auth/scopes.go` holds the model. `migrations/20261008000000_initial_schema.sql`
(section "people") holds the schema.

## Known limits

- The console decides what to show from the role on the **selected** repository. On a global page, such
  as Certificates, a restricted user with global `none` may see a link whose page then answers 403 ("your
  role (none) may not do this"). The server is the authority. The sidebar is a convenience.
- `GET /about` reports the total number of repositories (`datastore_count`) to any global `readonly`.
  Restricted users with global `none` cannot call it.
- Sync jobs, policies and hosts belong to one repository and follow its scope. Creating, attaching and
  deleting repositories, credentials and traffic rules are global (administrators).
