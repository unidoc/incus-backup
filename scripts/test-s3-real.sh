#!/usr/bin/env bash
# scripts/test-s3-real.sh - run the S3 conformance suite and the datastore S3 tests against a REAL
# S3 server: versitygw (Apache-2.0, posix backend), built from source at the version CI pins and run
# on a loopback port with a temporary data directory. See docs/testing-s3.md.
#
#   VERSITYGW_VERSION  module version to build (default: the pin below, same as .github/workflows/ci.yml)
#   VERSITYGW_BIN      use this binary instead of building one
#   VGW_PORT           listen port (default 7070)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${VERSITYGW_VERSION:-v1.9.0}
PORT=${VGW_PORT:-7070}
WORK=$(mktemp -d)
PID=
cleanup() {
	if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; fi
	rm -rf "$WORK"
}
trap cleanup EXIT

BIN=${VERSITYGW_BIN:-}
if [ -z "$BIN" ]; then
	# outside the module so go.mod/go.sum of this repository are never touched
	(cd "$WORK" && CGO_ENABLED=0 GOFLAGS='' GOBIN="$WORK/bin" go install "github.com/versity/versitygw/cmd/versitygw@$VERSION")
	BIN=$WORK/bin/versitygw
fi
mkdir -p "$WORK/data" "$WORK/versions"
"$BIN" --port "127.0.0.1:$PORT" --access localaccess --secret local-secret-12345 \
	posix --versioning-dir "$WORK/versions" "$WORK/data" >"$WORK/vgw.log" 2>&1 &
PID=$!
code=
for _ in $(seq 100); do
	code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/" || true)
	[ "$code" = 403 ] && break # an unsigned request is refused: the server is up
	sleep 0.3
done
if [ "$code" != 403 ]; then
	echo "versitygw did not start:" >&2
	tail -n 30 "$WORK/vgw.log" >&2
	exit 1
fi
export INCUS_BACKUP_TEST_S3_ENDPOINT=127.0.0.1:$PORT
export INCUS_BACKUP_TEST_S3_ACCESS_KEY=localaccess
export INCUS_BACKUP_TEST_S3_SECRET_KEY=local-secret-12345
export INCUS_BACKUP_TEST_S3_UNSUPPORTED=lifecycle # versitygw answers 501 to bucket lifecycle
go test -count=1 ./internal/s3test/conformance ./internal/datastore
