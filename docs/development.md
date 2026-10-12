# Development: tests, services, demo, end-to-end tests, CI and releases

How to build and test incus-backup, which services the tests need, what CI runs on every push and pull
request, and how a release is cut. For the seeded demo stack see [`dev-demo.md`](dev-demo.md). For the
packaging details see [`packaging.md`](packaging.md). For validation on a real Incus/ZFS host see
[`real-host-validation.md`](real-host-validation.md).

## Tools

| Tool | Version | Used for |
|---|---|---|
| Go | exactly the `go` line of `go.mod` (CI: `GOTOOLCHAIN=local`) | everything; also builds the WebAssembly module of `web-crypto/` |
| Node.js + npm | 22 or 24 (`web/package.json` engines: `>= 22`) | `web/` (Vue 3 + Vite) and `web-crypto/` (TypeScript) |
| `just` | any | the recipes in `Justfile` |
| MinIO server binary | see below | every S3 test, the demo's S3 repository, the e2e storage tests |
| PostgreSQL 16 | embedded (downloaded by the tests) or any server | every control-plane test |
| `shellcheck` | 0.9 or newer | `scripts/`, `packaging/` (CI's packaging job) |

**Platform.** The client and the tests target Linux. `just test` does not compile on macOS: `internal/archive/archive.go` and
`internal/fscat/scan.go` use the Linux `syscall.Stat_t` field types without a build tag. Develop in a Linux container or VM
(the Alpine package and the Incus host are Linux anyway).

**MinIO.** MinIO no longer publishes binaries (`dl.min.io` answers `410 Gone`) and the project is archived.
Build the pinned version CI uses from source (module proxy and checksum database):

```sh
GOBIN=$HOME/.local/bin go install github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c
export INCUS_BACKUP_TEST_MINIO=$HOME/.local/bin/minio
```

The version is pinned in `.github/actions/minio/action.yml`.

## Tests and the services they need

```sh
just test                 # go test ./... -count=1
just test-short           # only the tests that need no PostgreSQL
go test ./internal/server/mgmt -run TestAPIDoc -count=1    # one package, one test
```

Tests that need PostgreSQL or S3 **fail, never skip**, when the service is missing. A database test that
silently skips proves nothing.

| Variable | Read by | Meaning |
|---|---|---|
| `INCUS_BACKUP_TEST_DATABASE_URL` | `internal/testdb` | a PostgreSQL server where the tests may `CREATE DATABASE` (superuser). Each test gets a fresh database, dropped afterwards. Unset: embedded-postgres is started per test package |
| `INCUS_BACKUP_PG_CACHE` | `internal/testdb`, `internal/demo`, `web/e2e/backend.ts` | where embedded-postgres keeps the PostgreSQL 16 binaries (default `~/.cache/incus-backup-embedded-postgres`). The demo and the e2e backend **always** use these binaries; they have no URL mode |
| `INCUS_BACKUP_TEST_S3_ENDPOINT`, `_ACCESS_KEY`, `_SECRET_KEY` | `internal/s3test` | an S3 service (`host:port` is plain HTTP; `https://host:port` is TLS). Each test gets a fresh bucket |
| `INCUS_BACKUP_TEST_MINIO` | `internal/s3test`, `internal/demo`, `cmd/ibdemo`, `web/e2e/minio.ts` | a MinIO server binary. Without `INCUS_BACKUP_TEST_S3_*`, `internal/s3test` starts it once per test package. `internal/demo`'s test and the e2e storage tests need it in any case |

Every package that uses `testdb` or `s3test` has a `TestMain` calling `testdb.Main` (and
`testdb.AtExit(s3test.Start())` when it needs both). A new package must do the same.

For a persistent local PostgreSQL instead of embedded-postgres: `just devenv-up` (Docker, see
`devenv/compose.yml` for the URL to export).

Other test recipes:

* `just fuzz [seconds]`: short fuzz runs over the format parsers.
* `just verify-vectors`: independent re-check of `internal/format/testdata/vectors.json` (Python, PyNaCl).
* `just bench` / `just bench-big`: the benchmarks behind [`performance.md`](performance.md). They need
  MinIO and PostgreSQL like the tests.

## Everything CI checks, locally: `just ci`

`just ci` runs `scripts/ci.sh`, the same commands as the CI jobs `go`, `race`, `web`, `openapi-docs` and
`vendor`. Every step runs even after a failure, and a summary follows:

| Step | What it runs |
|---|---|
| `fmt` | `gofmt -l` prints nothing |
| `vet` | `go vet ./...` |
| `build` | `go build ./...` for linux/amd64 and linux/arm64 with `CGO_ENABLED=0` (as the release), and `cmd/ibwasm` for js/wasm |
| `test` | `go test ./...` (`CGO_ENABLED=0`, 30 min timeout) |
| `race` | `go test -race -p 1` on the concurrency-heavy packages (list in `scripts/ci.sh`); in CI three parallel shards `a`, `b`, `c` (`RACE_SHARD`) |
| `web` | `npm run crypto` (web-crypto: Go -> WebAssembly + TypeScript), web-crypto type check, `npm ci`, `npm run typecheck`, `npm test` (vitest), `npm run build`, then `go build ./cmd/incus-backup-server` with that UI embedded and a check that it is |
| `apidoc` | `IB_WRITE_API_DOC=1 go test ./internal/server/mgmt -run APIDoc`; fails if `docs/api.md` changed (the regenerated file is left in place to commit) |
| `vendor` | `go mod tidy -diff`, `go mod verify`, then `go mod vendor` and a `GOPROXY=off -mod=vendor` build in a scratch copy (the release tarball relies on `vendor/`) |

Run some steps only with `just ci fmt vet build`. Expect about 15 minutes for everything on a 4-core
machine. The race step is the longest.

The `web` and `apidoc` steps rewrite files in the tree: `internal/server/webui/dist`, `web-crypto/wasm`,
and `docs/api.md` when it was stale.

## Web UI

```sh
just web          # web-crypto (Go -> WASM) + npm ci + vite build into internal/server/webui/dist
just web-dev      # Vite dev server on :5173, API proxied to the demo on 127.0.0.1:8099
just web-test     # type check, vitest and the Playwright end-to-end tests
```

`internal/server/webui` embeds `dist/` (`go:embed`). Build the UI before `just build` when the binary
should carry the current UI.

web-crypto's own tests (`cd web-crypto && npm test`) generate a test repository with the real Go engine
(`internal/webfixture`, which needs PostgreSQL like every repository test). They also run the library
in headless Chromium, which needs a Playwright browser: `cd web && npx playwright install chromium`.

## The demo

`cmd/ibdemo` starts the whole server stack in one process and seeds 30 days of realistic history. See
[`dev-demo.md`](dev-demo.md):

```sh
INCUS_BACKUP_TEST_MINIO=$HOME/.local/bin/minio go run ./cmd/ibdemo --reset --web 127.0.0.1:8099
```

## End-to-end tests (Playwright)

```sh
cd web
npx playwright install chromium            # once (CI: --with-deps installs the system libraries too)
go build -o /tmp/ibdemo ../cmd/ibdemo
IB_DEMO_BIN=/tmp/ibdemo INCUS_BACKUP_TEST_MINIO=$HOME/.local/bin/minio npm run e2e
```

The global setup (`web/e2e/backend.ts`) starts two backends:

* **real**: the `incus-backup-server` binary built from this tree with the UI embedded, over HTTPS, on a
  fresh PostgreSQL started from `INCUS_BACKUP_PG_CACHE`. If the binaries are not there yet, run
  `go test ./internal/server/auth` once without `INCUS_BACKUP_TEST_DATABASE_URL` and embedded-postgres
  downloads them.
* **demo**: `IB_DEMO_BIN` (default `/tmp/ibwork/demo-bin/ibdemo`) with the `test` profile. **Without the
  binary the demo project is silently left out**, which is why CI checks that it exists.

Useful variables: `IB_E2E_PORT` and `IB_E2E_DEMO_PORT` (ports), `IB_E2E_SKIP_BUILD=1` (do not run
`vite build` again), `IB_SERVER_BIN` (use this server binary), `IB_CHROMIUM_ROOT` (Chromium libraries
unpacked without root, see `web/playwright.config.ts`). Screenshots go to `web/e2e/screenshots/`.
Traces of failed tests go to `web/test-results/`. Server and demo logs go to `web/e2e/.state/`.

With `CI` set (as on GitHub), Playwright retries a failed test once and also reports in the GitHub
annotation format.

## Continuous integration

`.github/workflows/ci.yml` runs on pushes to `master`/`main`, on every pull request, on demand, and
from the release workflow. Branch protection should require the single check **`ci-ok`**. It fails
unless every job below succeeded; a skipped or cancelled job counts as a failure.

| Job | What it runs | Services |
|---|---|---|
| `minio` | builds the pinned MinIO from source once and caches it (`.github/actions/minio`) | |
| `go` | `scripts/ci.sh fmt vet build test` | PostgreSQL 16 service container (`INCUS_BACKUP_TEST_DATABASE_URL`, default settings), one shared MinIO (`INCUS_BACKUP_TEST_S3_*`), the MinIO binary and the embedded PostgreSQL binaries for `internal/demo` |
| `race` | `scripts/ci.sh race` | PostgreSQL service; `internal/s3test` starts the MinIO binary per package (the other wiring) |
| `web` | `scripts/ci.sh web` on Node 22 and 24 | |
| `e2e` | web-crypto + UI build, `npx playwright install --with-deps chromium`, web-crypto's `npm test` (incl. headless Chromium), then Playwright against the real server and the demo; traces, screenshots and logs are uploaded as an artifact on failure only | embedded PostgreSQL binaries (cached), the MinIO binary |
| `openapi-docs` | regenerates the route table of `docs/api.md` and fails on `git diff --exit-code docs/api.md` | PostgreSQL service (`mgmt`'s `TestMain`) |
| `packaging` | `shellcheck` and `bash -n`/`sh -n` of `scripts/`, `packaging/` and `web-crypto/scripts/`; `scripts/release.sh` (amd64 only); `scripts/release-apk.sh` (real abuild in a proot Alpine root, x86_64 only, including the APKBUILD's `check()`); `scripts/smoke-install.sh` with a real PostgreSQL (`SMOKE_PG_URL`) | PostgreSQL service |
| `vendor` | `scripts/ci.sh vendor` | |
| `ci-ok` | the aggregate check for branch protection | |

The aarch64 Alpine build (about 45 minutes under emulation) is not run in CI. unidoc-aports builds
aarch64 natively.

`codeql.yml` analyses the Go code and the TypeScript/Vue sources on pull requests, on the default branch
and weekly. Its findings go to code scanning; it is not part of `ci-ok`. `dependabot.yml` proposes
grouped weekly updates of Go modules, both npm projects and the GitHub Actions. Each update goes
through the full CI.

The built UI (`internal/server/webui/dist`) is not in git; only `dist/.keep` is, so that `go:embed` compiles.
Jobs that do not build the UI get a placeholder from `.github/actions/webui-dist`. The release refuses a
placeholder. Run `just web` before `just build` to embed the real UI.

## Releasing

1. Make sure `master` is green.
2. Set `version.txt` to the new version. Use apk syntax: `1.2.3`, or `1.2.3_rc1` for a pre-release.
3. Add a `## 1.2.3` section to `CHANGELOG.md`. The release fails without one.
4. Optional local dry run: `just dist` (or `ARCHES=amd64 just dist` for a faster one), then
   `just package smoke-install` for the Alpine packages.
5. Commit, then push a tag `v1.2.3` on that commit.

`.github/workflows/release.yml` then runs:

* **`ci`**: the whole CI workflow on the tagged commit.
* **`build`**: `scripts/release.sh` with the arm64 binaries executed under qemu-user. It also checks
  that the tag equals `version.txt`, that the release notes come from `CHANGELOG.md` and that every
  asset exists.
* **`publish`**: in the `signing` environment. It verifies `SHA256SUMS`, signs every asset with minisign
  and verifies each signature against the repository variable `INCUS_BACKUP_MINISIGN_PUBLIC_KEY`. Then
  it publishes the GitHub release. A `_` in the version (`1.2.3_rc1`) makes it a pre-release.

The release assets are:

* the source tarball with `vendor/` and the built UI;
* the four static binaries;
* `incus-backup-VERSION-aports.tar.gz`, the handoff to unidoc-aports;
* `SHA256SUMS`, `BUILDINFO`, `RELEASE_NOTES.md`;
* a `.minisig` for every asset except `SHA256SUMS`.

Signing follows the alpine-zfsboot convention described in [`packaging.md`](packaging.md):

* The key is generated locally with `minisign -G -W`.
* Its content is the **environment** secret `INCUS_BACKUP_MINISIGN_KEY` of the environment `signing`.
* That environment is limited to `v*` tags, and a tag ruleset limits who may push them.

Once `INCUS_BACKUP_MINISIGN_PUBLIC_KEY` is set, signing is mandatory: a missing secret fails the
release. Only while neither the variable nor the secret is configured is the release published
unsigned, with a warning on the run.

After the release, take `incus-backup-VERSION-aports.tar.gz` **from the published release** into
unidoc-aports (see "Handoff to unidoc-aports" in [`packaging.md`](packaging.md)).
