#!/bin/sh
# Gate G2a: what does a LONG frozen NBD view cost the guest?
#
# Incus implements the frozen view as a copy-before-write overlay (a qcow2 in the
# VM's config volume, sized like the disk, unlinked). While a session is open every
# overwrite of a not-yet-copied block first copies the old block there, so the
# overlay grows with guest write volume for as long as the session is held.
# Open questions this gate answers with numbers:
#   1. how fast does the config dataset grow under a busy writer?
#   2. (QUOTA_TEST=1) what happens to the GUEST when the overlay cannot grow?
#
#   sh g-freeze.sh run [HOLD_SECONDS=300]   (QUOTA_TEST=1 for the exhaustion test)
#   sh g-freeze.sh cleanup
# Needs: incus, zfs, nbdpoc (from temp/nbdpoc), a ZFS pool. Scratch VM only.
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
. "$DIR/common.sh"
if [ "${1:-run}" = cleanup ]; then need incus; delete_project; rm -rf /var/tmp/ibgate.*; echo removed; exit 0; fi
need incus zfs
NBDPOC=${NBDPOC:?set NBDPOC=/path/to/nbdpoc-linux-amd64}
HOLD=${2:-${HOLD:-300}}
refuse_if_project_exists
WORK=$(mktemp -d /var/tmp/ibgate.XXXXXX) || exit 1
NBDPID=; HOLDPID=
cleanup() {
	[ -n "$HOLDPID" ] && kill "$HOLDPID" 2>/dev/null
	[ -n "$NBDPID" ] && kill "$NBDPID" 2>/dev/null
	[ -n "${DS:-}" ] && case "$DS" in *ibgate_gate-vm*) zfs set quota=none "$DS" 2>/dev/null;; esac
	[ "${KEEP:-0}" = 1 ] || { delete_project; rm -rf "$WORK"; }
}
trap cleanup EXIT

say "scratch VM with a busy writer"
create_project
incus storage volume create "$POOL" gate-d1 --type=block size=1GiB --project "$PROJECT" >/dev/null
incus init images:debian/12/cloud gate-vm --vm -s "$POOL" --project "$PROJECT" -d root,size=8GiB \
	-c security.secureboot=false -c raw.qemu.conf="$AUDIO_CONF" -c cloud-init.user-data="$WRITER_USERDATA" >/dev/null &&
incus config device add gate-vm d1 disk pool="$POOL" source=gate-d1 --project "$PROJECT" >/dev/null &&
incus config device add gate-vm cloud-init disk source=cloud-init:config --project "$PROJECT" >/dev/null &&
incus start gate-vm --project "$PROJECT" || { echo "VM creation failed"; exit 1; }
note "waiting 120 s for boot..."; sleep 120

# the VM's config dataset: <zfs.pool_name>/virtual-machines/<project>_<name>
ZP=$(incus storage get "$POOL" zfs.pool_name 2>/dev/null); [ -n "$ZP" ] || ZP=$(incus storage get "$POOL" source)
DS="$ZP/virtual-machines/${PROJECT}_gate-vm"
zfs list -H -o name "$DS" >/dev/null 2>&1 || { echo "cannot find the VM config dataset $DS (is this a ZFS pool? see: zfs list | grep gate-vm)"; exit 1; }
case "$DS" in *ibgate_gate-vm*) ;; *) echo "refusing: $DS is not the gate VM's dataset"; exit 1;; esac
note "config dataset: $DS"
used() { zfs get -Hp -o value used "$DS"; }

incus low-level nbd gate-vm --project "$PROJECT" --address 127.0.0.1:10899 >"$WORK/proxy.log" 2>&1 &
NBDPID=$!; sleep 2
CONSOLE_BEFORE=$(incus console gate-vm --project "$PROJECT" --show-log 2>/dev/null | grep -ciE 'I/O error|blk_update_request|EXT4-fs error|task .* blocked') || CONSOLE_BEFORE=0

say "open a frozen view and HOLD it for $HOLD s while the guest writes"
rm -f "$WORK/ready" "$WORK/go"
"$NBDPOC" -addr 127.0.0.1:10899 -exports root,d1 -ready-file "$WORK/ready" -go-file "$WORK/go" >"$WORK/hold.log" 2>&1 &
HOLDPID=$!
i=0; while [ ! -f "$WORK/ready" ] && [ $i -lt 100 ]; do sleep 0.2; i=$((i+1)); done
[ -f "$WORK/ready" ] || { echo "could not open the NBD session"; cat "$WORK/hold.log"; exit 1; }
U0=$(used); T0=$(date +%s)
if [ "${QUOTA_TEST:-0}" = 1 ]; then
	zfs set quota=$((U0 + 64*1024*1024)) "$DS" && note "QUOTA_TEST: capped the config dataset at used+64 MiB: the overlay will hit it"
fi
printf '%-8s %-14s %-14s\n' seconds used_bytes growth_bytes
while [ $(( $(date +%s) - T0 )) -lt "$HOLD" ]; do
	sleep 10
	U=$(used)
	printf '%-8s %-14s %-14s\n' $(( $(date +%s) - T0 )) "$U" $((U - U0))
done
UEND=$(used)
RATE=$(( (UEND - U0) / HOLD ))
note "overlay growth: $((UEND - U0)) bytes in $HOLD s = $RATE bytes/s ($((RATE * 3600 / 1048576)) MiB/hour) with a deliberately heavy writer"
touch "$WORK/go"; wait "$HOLDPID" 2>/dev/null; HOLDPID=
note "frozen-view client exit log: $(tail -3 "$WORK/hold.log" | tr '\n' ' ')"
[ "${QUOTA_TEST:-0}" = 1 ] && zfs set quota=none "$DS"

say "what did the guest see?"
sleep 5
CONSOLE_AFTER=$(incus console gate-vm --project "$PROJECT" --show-log 2>/dev/null | grep -ciE 'I/O error|blk_update_request|EXT4-fs error|task .* blocked') || CONSOLE_AFTER=0
note "guest kernel I/O error lines: before=$CONSOLE_BEFORE after=$CONSOLE_AFTER"
if [ "$CONSOLE_AFTER" -gt "$CONSOLE_BEFORE" ]; then
	fail "the guest logged I/O errors while the view was held (record this in docs/design.md 4.6: the freeze limit must be enforced harder)"
else
	pass "no guest I/O errors during the hold"
fi
incus info gate-vm --project "$PROJECT" | grep -i '^status' | grep -qi running && pass "VM still running after the hold" || fail "VM is not running any more"
echo
echo "Paste this whole output back so docs/design.md section 4.6 can record the real numbers."
summary
