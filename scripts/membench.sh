#!/bin/sh
# membench.sh: run the memory scenarios at large sizes and print one table.
#
# The same tests run in `go test ./...` at small sizes; this script only passes larger sizes
# through the environment. It needs what the test suite needs (PostgreSQL: embedded or
# INCUS_BACKUP_TEST_DATABASE_URL) and free disk space in TMPDIR of about twice the data written
# (spool + repository): the 8 GiB disk writes 2 GiB of data, the 300k-file tree about 300 MiB.
#
#   scripts/membench.sh                    # defaults below
#   DISK_GIB=1,4,8 SPARSE_GIB=64,1024,4096 FILES=30000,300000 CHUNKS=50000,200000 INDEX=1048576 scripts/membench.sh
#
# The scenarios run one after the other (one heavy process at a time) with GOMEMLIMIT=1GiB, so a
# path that hoards memory shows up as a failed bound or a GC-bound crawl, not as a swapped host.
set -eu
cd "$(dirname "$0")/.."

DISK_GIB=${DISK_GIB:-1,4,8}
SPARSE_GIB=${SPARSE_GIB:-64,1024,4096}
FILES=${FILES:-30000,300000}
CHUNKS=${CHUNKS:-50000,200000}
INDEX=${INDEX:-1048576}
OUT=${OUT:-$(mktemp)}
: >"$OUT"
export GOMEMLIMIT=${GOMEMLIMIT:-1GiB}
export IB_MEMBENCH_OUT="$OUT"

run() { # package test-regex
	echo "== $2" >&2
	go test "$1" -run "$2" -count=1 -timeout 60m -v | grep -E '^(--- |ok|FAIL)|mem_test.go:[0-9]+: [^|]' >&2 || true
}

IB_MEMBENCH_DISK_GIB=$DISK_GIB run ./internal/client/engine '^TestBlockBackupRestoreVerifyMemoryIsFlat$'
IB_MEMBENCH_SPARSE_GIB=$SPARSE_GIB run ./internal/client/engine '^TestSparseDiskMemoryPerEntry$'
IB_MEMBENCH_FILES=$FILES run ./internal/client/engine '^TestFilesystemCatalogMemoryPerEntry$'
IB_MEMBENCH_CHUNKS=$CHUNKS run ./internal/datastore '^TestMaintenanceMemory'
IB_MEMBENCH_INDEX_ENTRIES=$INDEX run ./internal/format '^TestIndexMemoryPerEntry$'

echo "| scenario | size | phase | heap base MiB | heap in use, peak delta MiB | live heap, peak delta MiB | RSS peak delta MiB | allocated MiB | time |"
echo "|---|---|---|---|---|---|---|---|---|"
cat "$OUT"
