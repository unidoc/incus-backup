#!/usr/bin/env bash
# scripts/ci.sh [STEP...] - the checks of .github/workflows/ci.yml, runnable locally (`just ci`).
#
# Steps (default: all of them, in this order; every step runs even after a failure, then a
# summary is printed and the exit status is non-zero if any step failed):
#   fmt      gofmt -l prints nothing
#   vet      go vet ./...
#   build    go build ./... for linux/amd64 and linux/arm64 with CGO_ENABLED=0 (as the release)
#   test     go test ./... (needs PostgreSQL and an S3 service, see below)
#   race     go test -race -p 1 on the concurrency-heavy packages (RACE_PKGS below)
#   web      web-crypto (Go -> WebAssembly + TypeScript), npm ci, type checks, vitest, vite build,
#            then go build ./cmd/incus-backup-server with that UI embedded
#   apidoc   docs/api.md route table is what the code generates (regenerates it if not, and fails)
#   vendor   go mod tidy changes nothing, go mod verify, and a vendored offline build works
#
# Not here (slower, see docs/development.md): the Playwright end-to-end tests (`just web-test`),
# web-crypto's headless-Chromium tests (`cd web-crypto && npm test`) and the Alpine package
# build + smoke install (`just package smoke-install`). CI runs them in the e2e and packaging jobs.
#
# Services: tests FAIL (never skip) without them.
#   PostgreSQL  INCUS_BACKUP_TEST_DATABASE_URL, else embedded-postgres (downloads PostgreSQL once)
#   S3          INCUS_BACKUP_TEST_S3_ENDPOINT/_ACCESS_KEY/_SECRET_KEY, else a MinIO server binary
#               from INCUS_BACKUP_TEST_MINIO (or `minio` in PATH). internal/demo always needs
#               INCUS_BACKUP_TEST_MINIO (its second repository is an S3 one).
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

# The race subset: packages with goroutine pools, leases, queues and shared caches.
# RACE_SHARD=a|b|c runs one third (CI runs the three in parallel; the split is by measured run time,
# about 190 s, 180 s and 130 s). Without it every package runs, as `just ci` does locally.
RACE_PKGS_A=(
	./internal/client/engine/...
	./internal/client/jobs/...
	./internal/server/tasks/...
	./internal/server/maint/...
	./internal/webcore/...
	./internal/format/...
)
RACE_PKGS_B=(
	./internal/datastore/...
	./internal/server/syncer/...
	./internal/server/traffic/...
	./internal/server/notify/...
	./internal/nbd/...
	./internal/fscat/...
)
RACE_PKGS_C=(
	./internal/server/service/...
	./internal/server/mgmt/...
	./internal/server/auth/...
	./internal/server/httpapi/...
	./internal/server/jobs/...
)
RACE_PKGS=("${RACE_PKGS_A[@]}" "${RACE_PKGS_B[@]}" "${RACE_PKGS_C[@]}")

ALL_STEPS=(fmt vet build test race web apidoc vendor)

hr() { printf '\n=== %s\n' "$*"; }

need_s3() {
	if [ -z "${INCUS_BACKUP_TEST_S3_ENDPOINT:-}" ] && [ -z "${INCUS_BACKUP_TEST_MINIO:-}" ] && ! command -v minio >/dev/null; then
		echo "no S3 service for the tests: set INCUS_BACKUP_TEST_MINIO=/path/to/minio (or INCUS_BACKUP_TEST_S3_*)" >&2
		return 1
	fi
}

step_fmt() {
	local out
	out=$(find . \( -name node_modules -o -name vendor -o -name .git -o -path ./temp -o -path ./dist \) -prune -o -name '*.go' -print0 |
		xargs -0 gofmt -l) || return 1
	if [ -n "$out" ]; then
		echo "not gofmt-formatted (run gofmt -w):"
		echo "$out"
		return 1
	fi
}

step_vet() { go vet ./...; }

step_build() {
	local arch
	for arch in amd64 arm64; do
		echo "linux/$arch"
		CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build ./... || return 1
	done
	echo "js/wasm (cmd/ibwasm)"
	CGO_ENABLED=0 GOOS=js GOARCH=wasm go build -o /dev/null ./cmd/ibwasm
}

step_test() {
	need_s3 || return 1
	CGO_ENABLED=0 go test -count=1 -timeout 30m ./...
}

step_race() {
	need_s3 || return 1
	local pkgs=("${RACE_PKGS[@]}")
	case "${RACE_SHARD:-}" in
	"") ;;
	a) pkgs=("${RACE_PKGS_A[@]}") ;;
	b) pkgs=("${RACE_PKGS_B[@]}") ;;
	c) pkgs=("${RACE_PKGS_C[@]}") ;;
	*) echo "RACE_SHARD must be a, b or c" >&2; return 1 ;;
	esac
	CGO_ENABLED=1 go test -race -p 1 -count=1 -timeout 30m "${pkgs[@]}"
}

step_web() {
	(
		set -e
		cd web
		npm run crypto
		(cd ../web-crypto && npm run typecheck)
		npm ci --no-audit --no-fund
		npm run typecheck
		npm test
		npm run build
	) || return 1
	local d=internal/server/webui/dist
	[ -f "$d/index.html" ] && [ ! -e "$d/PLACEHOLDER" ] || { echo "$d holds no built UI"; return 1; }
	ls "$d"/assets/crypto-*/wasm/ibwasm.wasm >/dev/null 2>&1 || { echo "$d lacks the web-crypto module"; return 1; }
	local bin
	bin=$(mktemp -d)/incus-backup-server
	CGO_ENABLED=0 go build -o "$bin" ./cmd/incus-backup-server || return 1
	grep -q 'assets/crypto-' "$bin" || { echo "the server binary does not embed the built UI"; return 1; }
	"$bin" --version
	rm -rf "$(dirname "$bin")"
}

step_apidoc() {
	local before
	before=$(mktemp)
	cp docs/api.md "$before"
	IB_WRITE_API_DOC=1 go test -count=1 -run 'APIDoc' ./internal/server/mgmt || { rm -f "$before"; return 1; }
	if ! cmp -s "$before" docs/api.md; then
		diff -u "$before" docs/api.md | head -n 60
		echo "docs/api.md was stale and has been regenerated: review and commit it"
		rm -f "$before"
		return 1
	fi
	rm -f "$before"
}

step_vendor() {
	go mod tidy -diff || { echo "go mod tidy would change go.mod/go.sum (diff above)"; return 1; }
	go mod verify || return 1
	# In a scratch copy: a vendor/ directory in the tree would switch every later local build to -mod=vendor.
	local tmp rc=0
	tmp=$(mktemp -d)
	tar -cf - go.mod go.sum cmd internal migrations | tar -xf - -C "$tmp"
	(cd "$tmp" && go mod vendor && GOFLAGS=-mod=vendor GOPROXY=off CGO_ENABLED=0 go build ./...) || rc=1
	[ $rc = 0 ] && echo "vendored offline build ok ($(du -sh "$tmp/vendor" | cut -f1) of vendor/)"
	chmod -R u+w "$tmp" 2>/dev/null
	rm -rf "$tmp"
	return $rc
}

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=("${ALL_STEPS[@]}")
for s in "${steps[@]}"; do
	declare -F "step_$s" >/dev/null || { echo "unknown step '$s' (steps: ${ALL_STEPS[*]})" >&2; exit 2; }
done

results=()
failed=0
for s in "${steps[@]}"; do
	hr "$s"
	t0=$SECONDS
	if "step_$s"; then r=PASS; else r=FAIL; failed=$((failed + 1)); fi
	results+=("$(printf '%-7s %s  (%ds)' "$s" "$r" $((SECONDS - t0)))")
done

hr "summary"
printf '%s\n' "${results[@]}"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo '| step | result |'
		echo '|---|---|'
		for r in "${results[@]}"; do
			read -r n v _ <<<"$r"
			echo "| $n | $v |"
		done
	} >>"$GITHUB_STEP_SUMMARY"
fi
[ $failed = 0 ] || { echo "$failed step(s) failed"; exit 1; }
echo "all steps passed"
