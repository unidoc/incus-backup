# Management API: "Protection & recovery" additions

Routes added or changed for the web console's snapshot, restore, verification and job pages.
Everything else these pages use is in `docs/api.md` and `docs/jobs.md`.

## `GET /api/v1/snapshot-stats` (readonly, repository-scoped)

`?repository=NAME` (as every repository route), optional `?group=g1,g2` (comma separated group ids).

What PostgreSQL recorded for each snapshot when it was committed or verified, for the whole
repository in **one query** (the snapshot list needs no per-snapshot call). `GET /snapshots` stays
the authority for existence, protection, notes and verify state (it reads the datastore); this route
adds the run counters, which only the control plane has:

```json
[{
  "group_id": "23e894c5cc988ed7bfdd67a7fb7780a2",
  "snapshot_id": "20261008030000-00000080",
  "logical_bytes": 6017024, "dirty_bytes": 2863104, "read_bytes": 2863104, "uploaded_bytes": 1300480,
  "full_read": false, "duration_ms": 54,
  "previous_snapshot_id": "20261007030000-0000006a",
  "source": "client",
  "verify_state": "ok", "verified_at": "2026-10-08T18:31:02Z"
}]
```

* An empty repository answers `[]` (never `null`); an unknown group is an empty list.
* `source` is `client` (a backup run), `sync` or `reconcile` (rebuilt from the datastore: the
  counters are zero there).
* `verified_at` is UTC, `null` when never verified.
* Implementation: `service.SnapshotStatsList` (`internal/server/service/snapshot_stats.go`), route in
  `internal/server/mgmt/routes_protect.go`, permission in that file's `init()`.
* Tests: `TestSnapshotStatsRoute` (counters, base, ordering, verify state and time after a real
  `POST /verify`, group filter, unknown repository 404).

## Fix: `POST /api/v1/restore-tests/run` now takes `project`

Body `{host, project, object, snapshot?, mode?, repository?}`. Before the fix the route built the
job params from object, snapshot and mode only. `jobspec` requires `project` for `restore_test`, and
the request struct refused an unknown `project` field. So every call answered 400.
`TestRunRestoreTestRouteCarriesTheProject` failed before the change (400 `unknown field "project"`)
and passes after it. It also checks that the mode defaults to `deep` and that a missing project is a 400.

## Serving the browser decryption library

* `web/build/crypto.ts` (Vite plugin) copies `web-crypto/dist/*.js`, `wasm/ibwasm.wasm` and
  `wasm/wasm_exec.js` unchanged into the build as `assets/crypto-<hash>/{dist,wasm}/…`. The hash
  covers every file, so the server's `assets/` rule caches them as immutable. The page imports
  `assets/crypto-<hash>/dist/index.js` at run time. `vite build` fails with a clear message when the
  library is not built (`just web` builds it: `npm run crypto`).
* `webui.Handler` sets `Content-Type: application/wasm` for `.wasm` explicitly, so
  `WebAssembly.compileStreaming` works whatever the system MIME table says. The test is
  `TestWebCryptoFilesTypesAndCaching`.
* CSP: `worker-src 'self'` was added. `script-src 'self' 'wasm-unsafe-eval'` was already there.
  Image previews use `data:` URLs, which `img-src` allows; `blob:` stays forbidden.
