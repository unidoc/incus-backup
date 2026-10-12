#!/bin/sh
# Generates the test repository with the real engine (internal/webfixture) into $1 (default:
# .cache/fixture). Needs PostgreSQL the way every repository test does (embedded by default).
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$here/.cache/fixture}
case $out in /*) ;; *) out=$(pwd)/$out ;; esac
cd "$here/.."
IB_WEBFIXTURE_OUT="$out" go test -count=1 -run TestGenerateWebFixture ./internal/webfixture
