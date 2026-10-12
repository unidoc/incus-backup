#!/bin/sh
# Gate G2/G3/G4a-lite: the REAL client against a REAL server and REAL Incus, on
# scratch objects only. Proves, on this host: full + incremental VM backup under
# a busy guest, container and volume backup, deep verification, and restore of
# VM disks, volumes and container trees, compared byte for byte.
#
#   sh g-e2e.sh run      IBIN=/path/to/incus-backup SERVER=host:port TOKEN=ibt1....
#   sh g-e2e.sh cleanup
#
# SERVER SIDE (use a scratch repository: the gate uses a throw-away key, and a
# repository is pinned to one key):
#   incus-backup-server repo create ibgate --datastore /srv/incus-backup-gate
#   incus-backup-server -r ibgate token create benfica-gate --scope backup
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
. "$DIR/common.sh"

if [ "${1:-run}" = cleanup ]; then
	need incus
	delete_project
	rm -rf /var/tmp/ibgate.*
	echo "gate objects removed (the scratch repository 'ibgate' on the server is yours to delete)"
	exit 0
fi

need incus sha256sum
IBIN=${IBIN:?set IBIN=/path/to/incus-backup}
SERVER=${SERVER:?set SERVER=host:port of the backup server}
TOKEN=${TOKEN:?set TOKEN=<one-time enrolment token for the scratch repository>}
refuse_if_project_exists

WORK=$(mktemp -d /var/tmp/ibgate.XXXXXX) || exit 1
CFG="$WORK/client.yaml"
ib() { "$IBIN" --config "$CFG" "$@"; }
cleanup() {
	if [ "${KEEP:-0}" = 1 ]; then echo "KEEP=1: leaving $PROJECT and $WORK in place (sh $0 cleanup to remove)"; return; fi
	delete_project
	rm -rf "$WORK"
}
trap cleanup EXIT

say "1. scratch project, client configuration, throw-away key"
create_project
cat > "$CFG" <<YAML
host: benfica-gate
state_dir: $WORK/state
spool_dir: $WORK/spool
key_file: $WORK/key.json
incus:
  projects: [$PROJECT]
policies:
  gate: {schedule: manual, targets: [gate], commit_required: all}
default_policy: gate
YAML
ib init --server "$SERVER" --token "$TOKEN" --name gate --i-stored-the-recovery-key </dev/null >"$WORK/init.log" 2>&1 \
	&& pass "init + enrol at $SERVER (throw-away key)" || { fail "init: $(tail -3 "$WORK/init.log")"; summary; exit 1; }

say "2. guest objects (VM with a busy writer, raw volume, container, filesystem volume)"
incus storage volume create "$POOL" gate-d1 --type=block size=1GiB --project "$PROJECT" >/dev/null || { fail "volume"; exit 1; }
# gate-d2: 256 MiB, only the first 8 MiB written (the rest are holes) and NEVER touched by the guest.
# It checks that a full read of a RUNNING VM skips zero regions correctly: if the allocation
# map of the frozen export were wrong, data would vanish silently.
incus storage volume create "$POOL" gate-d2 --type=block size=256MiB --project "$PROJECT" >/dev/null || { fail "volume d2"; exit 1; }
ib debug write --project "$PROJECT" --volume "$POOL/gate-d2" --mib 8 --seed 7 && H_D2=$(ib debug hash --project "$PROJECT" --volume "$POOL/gate-d2" 2>&1) \
	&& note "gate-d2 before the VM starts: $H_D2" || fail "could not prepare gate-d2"
incus init images:debian/12/cloud gate-vm --vm -s "$POOL" --project "$PROJECT" -d root,size=8GiB \
	-c security.secureboot=false -c raw.qemu.conf="$AUDIO_CONF" -c cloud-init.user-data="$WRITER_USERDATA" >/dev/null \
	&& incus config device add gate-vm d1 disk pool="$POOL" source=gate-d1 --project "$PROJECT" >/dev/null \
	&& incus config device add gate-vm d2 disk pool="$POOL" source=gate-d2 --project "$PROJECT" >/dev/null \
	&& incus config device add gate-vm cloud-init disk source=cloud-init:config --project "$PROJECT" >/dev/null \
	&& incus start gate-vm --project "$PROJECT" && pass "VM gate-vm started" || { fail "VM creation"; summary; exit 1; }
incus storage volume create "$POOL" gate-fs --project "$PROJECT" >/dev/null
incus launch images:alpine/edge gate-ct --project "$PROJECT" -s "$POOL" >/dev/null \
	&& incus config device add gate-ct fs disk pool="$POOL" source=gate-fs path=/mnt/fs --project "$PROJECT" >/dev/null \
	&& pass "container gate-ct started" || fail "container creation"
note "waiting 120 s for the guests to boot and the writer to start..."
sleep 120
incus exec gate-ct --project "$PROJECT" -- sh -c 'echo hello-root > /root/gate-file; mkdir -p /mnt/fs/d; head -c 3000000 /dev/urandom > /mnt/fs/d/big; echo vol > /mnt/fs/note; chmod 640 /root/gate-file; ln -s /root/gate-file /root/link' \
	&& pass "files written in the container and its volume" || fail "exec in container"

say "3. plan and doctor"
ib plan > "$WORK/plan.txt" 2>&1; cat "$WORK/plan.txt"
grep -q 'instance/gate-vm/root' "$WORK/plan.txt" && pass "plan lists the VM root disk" || fail "plan does not list the VM"
ib doctor > "$WORK/doctor.txt" 2>&1; cat "$WORK/doctor.txt"
grep -q '^FAIL' "$WORK/doctor.txt" && fail "doctor reports a failure" || pass "doctor: no failures"

say "4. first run (full read)"
ib run > "$WORK/run1.txt" 2>&1; cat "$WORK/run1.txt"
grep -q FAILED "$WORK/run1.txt" && fail "run 1 has failures" || pass "run 1: everything committed"
grep -q 'full read' "$WORK/run1.txt" && pass "run 1 read the disks in full" || fail "run 1 was not a full read"

say "5. changes while the guest keeps writing, second run (incremental)"
incus exec gate-ct --project "$PROJECT" -- sh -c 'echo changed >> /root/gate-file; head -c 1000000 /dev/urandom > /mnt/fs/d/big2'
sleep 30
ib run > "$WORK/run2.txt" 2>&1; cat "$WORK/run2.txt"
grep -q FAILED "$WORK/run2.txt" && fail "run 2 has failures" || pass "run 2: everything committed"
grep -q 'incremental' "$WORK/run2.txt" && pass "run 2 used dirty bitmaps (incremental)" || fail "run 2 never went incremental"
ib restore volume volume/gate-d2 --project "$PROJECT" --to "$POOL/gate-d2-copy" > "$WORK/rest0.txt" 2>&1
R_D2=$(ib debug hash --project "$PROJECT" --volume "$POOL/gate-d2-copy" 2>&1); note "gate-d2 restored from the RUNNING-VM backup: $R_D2"
[ "$H_D2" = "$R_D2" ] && pass "hole skipping on the frozen export is byte exact (gate-d2)" || fail "gate-d2 restored from a running-VM backup DIFFERS (allocation map on the frozen export is not trustworthy: see docs/design.md 4.4)"
ib verify --last 0 > "$WORK/verify.txt" 2>&1; cat "$WORK/verify.txt"
grep -q PROBLEM "$WORK/verify.txt" && fail "verify found problems" || pass "verify: every chunk decrypts and its id recomputes"

say "6. quiesce the VM and take a final backup (stopped VM: full read through the volume endpoint)"
incus stop gate-vm --project "$PROJECT" && pass "VM stopped cleanly" || fail "VM stop"
ib run --object gate-vm > "$WORK/run3.txt" 2>&1; cat "$WORK/run3.txt"
grep -q FAILED "$WORK/run3.txt" && fail "run 3 has failures" || pass "run 3: stopped VM backed up"
ib run > "$WORK/run3b.txt" 2>&1
H_ROOT=$(ib debug hash --project "$PROJECT" --instance gate-vm 2>&1); note "source root disk: $H_ROOT"
H_D1=$(ib debug hash --project "$PROJECT" --volume "$POOL/gate-d1" 2>&1); note "source d1:        $H_D1"

say "7. restore and compare byte for byte"
ib restore instance gate-vm --project "$PROJECT" --as gate-vm-copy --no-chown > "$WORK/rest1.txt" 2>&1; cat "$WORK/rest1.txt"
R_ROOT=$(ib debug hash --project "$PROJECT" --instance gate-vm-copy 2>&1); note "restored root disk: $R_ROOT"
[ "$H_ROOT" = "$R_ROOT" ] && pass "restored VM root disk is identical to the source" || fail "restored VM root disk DIFFERS"
ib restore volume volume/gate-d1 --project "$PROJECT" --to "$POOL/gate-d1-copy" > "$WORK/rest2.txt" 2>&1; cat "$WORK/rest2.txt"
R_D1=$(ib debug hash --project "$PROJECT" --volume "$POOL/gate-d1-copy" 2>&1); note "restored d1:        $R_D1"
[ "$H_D1" = "$R_D1" ] && pass "restored raw volume is identical to the source" || fail "restored raw volume DIFFERS"

say "8. container: snapshot read path (G3) and restore"
incus snapshot create gate-ct probe --project "$PROJECT" >/dev/null 2>&1
ib debug snapshot-path --project "$PROJECT" --instance gate-ct --snapshot probe > "$WORK/spath.txt" 2>&1; cat "$WORK/spath.txt"
if [ -d "$(head -1 "$WORK/spath.txt")" ]; then pass "an Incus snapshot's contents are readable at the reported path"; else fail "snapshot contents not found (see the message: the engine's SnapshotPath strategies need adjusting)"; fi
incus snapshot delete gate-ct probe --project "$PROJECT" >/dev/null 2>&1
mkdir -p "$WORK/ctree"
ib restore files instance/gate-ct/root --project "$PROJECT" --to "$WORK/ctree" > "$WORK/rest3.txt" 2>&1; cat "$WORK/rest3.txt"
F=$(find "$WORK/ctree" -name gate-file -path '*root*' | head -1)
[ -n "$F" ] && grep -q changed "$F" && pass "container file restored with the later change" || fail "container tree not restored"
mkdir -p "$WORK/fstree"
ib restore files volume/gate-fs --project "$PROJECT" --to "$WORK/fstree" > "$WORK/rest4.txt" 2>&1
[ "$(cat "$WORK/fstree/note" 2>/dev/null)" = vol ] && pass "filesystem volume restored" || fail "filesystem volume not restored"
ib restore instance gate-ct --project "$PROJECT" --as gate-ct-copy > "$WORK/rest5.txt" 2>&1; cat "$WORK/rest5.txt"
if incus exec gate-ct-copy --project "$PROJECT" -- cat /root/gate-file >/dev/null 2>&1 || incus start gate-ct-copy --project "$PROJECT" >/dev/null 2>&1; then
	incus exec gate-ct-copy --project "$PROJECT" -- grep -q changed /root/gate-file 2>/dev/null && pass "container instance restored and running with its data" || fail "restored container lacks its data"
else
	fail "whole-container restore did not produce a startable container (G3 open item: restore target path)"
fi

say "9. server view"
ib list 2>&1 | head -20
summary
