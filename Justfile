# Justfile for incus-backup (client + server). Same shape as incus-sync.

default: build

# ── Build ────────────────────────────────────────────────────────────────

# Native build of both binaries, version-stamped from version.txt.
build:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p bin
    VERSION=$(cat version.txt | tr -d '[:space:]')
    for b in incus-backup incus-backup-server; do
      CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o bin/$b ./cmd/$b
    done

# Cross-build for Alpine arm64 hosts.
build-arm64:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p bin
    VERSION=$(cat version.txt | tr -d '[:space:]')
    for b in incus-backup incus-backup-server; do
      CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w -X main.version=${VERSION}" -o bin/$b-linux-arm64 ./cmd/$b
    done

# Static linux/amd64 build for shipping to a host.
build-linux-amd64:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p bin
    VERSION=$(cat version.txt | tr -d '[:space:]')
    for b in incus-backup incus-backup-server; do
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" -o bin/$b-linux-amd64 ./cmd/$b
    done

build-all: build build-arm64 build-linux-amd64
    @ls -la bin/

# ── Web UI (web/, Vue 3 + Vite; output embedded from internal/server/webui/dist) ──

# Install the locked dependencies and build the UI into internal/server/webui/dist (run before `build`).
# First builds the browser decryption library (web-crypto: Go -> WebAssembly), which the build ships.
web:
    cd web && npm run crypto && npm ci && npm run build

# Vite dev server on :5173, API proxied to 127.0.0.1:8099 (the demo server; IB_API=... overrides).
web-dev:
    cd web && npm run dev

# Unit tests, type check and the end-to-end tests (real server + demo server when available).
web-test:
    cd web && npm run typecheck && npm test && npm run e2e

fmt:
    go fmt ./...
    go vet ./...

# ── Tests ────────────────────────────────────────────────────────────────
# PostgreSQL is required for the server tests. They use
# INCUS_BACKUP_TEST_DATABASE_URL when set, otherwise start embedded-postgres.
# They FAIL (never skip) when neither works.

test:
    go test ./... -count=1

# The S3 test server (in-process fake), its fault injection, and its conformance suite incl. the mutation tests.
test-s3:
    go test ./internal/s3test/... -count=1

# The same conformance suite and the datastore S3 tests against a REAL S3 server (versitygw, built and run
# locally; see docs/testing-s3.md). Or point INCUS_BACKUP_TEST_S3_* at any other service yourself.
test-s3-real:
    bash scripts/test-s3-real.sh

# Everything except the tests that start a real PostgreSQL.
test-short:
    go test ./... -short -count=1

# Short fuzz run over the format parsers.
fuzz seconds="20":
    go test ./internal/format/ -run xxx -fuzz FuzzParseIndex -fuzztime {{seconds}}s
    go test ./internal/format/ -run xxx -fuzz FuzzDecodeChunk -fuzztime {{seconds}}s

# They use the in-process S3 fake and PostgreSQL like the tests (INCUS_BACKUP_TEST_S3_* measures a real service instead). Other jobs on the
# machine distort wall-clock numbers: compare request counts (HEAD/op, GET/op, LIST/op, conns) first.
# Benchmarks behind docs/performance.md at moderate sizes (about 30 minutes).
bench:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "cores: $(nproc)  $(uptime)"
    go test ./internal/format/ -run '^$' -bench 'EncodeChunk|DecodeChunk|ChunkParts' -benchmem -benchtime 20x
    INCUS_BACKUP_BENCH_N=11000 INCUS_BACKUP_BENCH_RTT=0,20 INCUS_BACKUP_BENCH_PAR=64 INCUS_BACKUP_BENCH_PRESENCE=0,1 \
      go test ./internal/datastore/ -run '^$' -bench 'S3' -benchtime 1x -timeout 2h
    go test ./internal/datastore/ -run '^$' -bench 'FSPutChunk' -benchtime 2s
    INCUS_BACKUP_BENCH_UPLOAD_MB=256 INCUS_BACKUP_BENCH_WORKERS=1,4,8,16 INCUS_BACKUP_BENCH_RTT=0,20 \
      go test ./internal/client/engine/ -run '^$' -bench 'Upload' -benchtime 1x -timeout 1h
    go test ./internal/nbd/ -run '^$' -bench 'ReadAt|ReadBatch|DelayLine' -benchtime 3x
    go test ./internal/fscat/ -run '^$' -bench . -benchmem
    go test ./internal/chunker/ -run '^$' -bench 'Splitter' -benchmem
    go test ./internal/server/service/ -run '^$' -bench 'BenchmarkControlPlaneQueries|BenchmarkReconcileAndSyncList' -benchtime 1x -timeout 30m

# Several hours. INCUS_BACKUP_BENCH_DIR caches the synthetic file trees (point it outside the repository).
# The large runs of docs/performance.md (100k chunks, 1 GiB uploads, 200k/1M-file trees).
bench-big:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "cores: $(nproc)  $(uptime)"
    INCUS_BACKUP_BENCH_N=100000 INCUS_BACKUP_BENCH_RTT=0,20,80 INCUS_BACKUP_BENCH_PAR=64 INCUS_BACKUP_BENCH_PRESENCE=0,1 \
      go test ./internal/datastore/ -run '^$' -bench 'S3Missing|S3Commit' -benchtime 1x -timeout 4h
    INCUS_BACKUP_BENCH_N=100000 INCUS_BACKUP_BENCH_RTT=0,20 INCUS_BACKUP_BENCH_PAR=64 \
      go test ./internal/datastore/ -run '^$' -bench 'S3Maintenance' -benchtime 1x -timeout 4h
    INCUS_BACKUP_BENCH_UPLOAD_MB=1024 INCUS_BACKUP_BENCH_WORKERS=1,4,8,16 INCUS_BACKUP_BENCH_RTT=0,20,80 INCUS_BACKUP_BENCH_STORE=fs,s3 \
      go test ./internal/client/engine/ -run '^$' -bench 'Upload' -benchtime 1x -timeout 2h
    INCUS_BACKUP_BENCH_FILES=200000 go test ./internal/fscat/ -run '^$' -bench 'ScanFull|Incremental|ScanWorkers' -benchmem -count 3 -timeout 60m
    INCUS_BACKUP_BENCH_FILES=1000000 go test ./internal/fscat/ -run '^$' -bench 'ScanFull|Incremental' -benchmem -benchtime 1x -timeout 60m

# Regenerate format vectors. ONLY when the format version is bumped on purpose.
update-vectors:
    go test ./internal/format/ -run TestVectors -update

# Independent (non-Go) re-verification of the vectors with PyNaCl/cryptography.
verify-vectors:
    #!/usr/bin/env bash
    set -euo pipefail
    python3 -m venv .venv-vectors >/dev/null
    .venv-vectors/bin/pip install -q pynacl cryptography
    .venv-vectors/bin/python scripts/verify-vectors.py

check: fmt test

# What CI runs (.github/workflows/ci.yml via scripts/ci.sh): gofmt, vet, linux/amd64+arm64 builds, tests,
# the race subset, the web checks, the API doc table and go.mod/vendor; prints a summary at the end.
# Needs PostgreSQL (embedded, or INCUS_BACKUP_TEST_DATABASE_URL); S3 is the in-process fake.
# Some steps only: `just ci fmt vet build`. docs/development.md lists what CI runs beyond this.
ci *STEPS:
    bash scripts/ci.sh {{STEPS}}

# ── Database ─────────────────────────────────────────────────────────────

# Apply migrations standalone (DATABASE_URL required).
migrate:
    ./bin/incus-backup-server migrate

# Local PostgreSQL for development (see devenv/).
devenv-up:
    cd devenv && docker compose up -d

devenv-down:
    cd devenv && docker compose down

# ── Release and packaging (docs/packaging.md) ────────────────────────────

# Reproducible release into dist/ (VERSION must match version.txt).
release VERSION:
    bash scripts/release.sh {{VERSION}}

# Release of the version in version.txt.
dist:
    bash scripts/release.sh "$(tr -d '[:space:]' < version.txt)"

# Alpine packages from dist/ with the real abuild (throwaway key) into dist/apk/.
package: dist
    bash scripts/release-apk.sh dist

# Install dist/apk/ into a fresh Alpine root and check it (SMOKE_PG_URL=... for the database part).
smoke-install:
    bash scripts/smoke-install.sh dist

clean:
    rm -rf bin dist .venv-vectors
