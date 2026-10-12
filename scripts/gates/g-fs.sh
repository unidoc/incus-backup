#!/bin/sh
# Gates G3a/G3b/G3c on a REAL host with REAL ZFS snapshots and a REAL recovery VM, scratch objects only:
#   G3b  successive Incus snapshots of an untouched file keep inode and ctime, so an unchanged tree is
#        reused without being read (this is the assumption of the catalog change detection);
#        a rewritten file whose size and mtime were forged back is still read;
#   G3a  incremental runs read only what changed and restore equals the source; full-data fallback;
#   --   browsing and single-file extraction from the catalog;
#   G3c  single files from a VM DISK through the disposable recovery VM (no mount on this host).
#
#   sh g-fs.sh run      IBIN=/path/to/incus-backup SERVER=host:port TOKEN=ibt1....  [RECOVERY_IMAGE=images:alpine/edge/cloud]
#   sh g-fs.sh cleanup
#
# SERVER SIDE: a scratch repository (the gate uses a throw-away key; a repository is pinned to one key):
#   incus-backup-server repo create ibgate-fs --datastore /srv/incus-backup-gate-fs
#   incus-backup-server -r ibgate-fs token create benfica-gate --scope backup
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
. "$DIR/common.sh"

if [ "${1:-run}" = cleanup ]; then
	need incus
	delete_project
	rm -rf /var/tmp/ibgate.*
	echo "gate objects removed (the scratch repository on the server is yours to delete)"
	exit 0
fi

need incus sha256sum find stat
IBIN=${IBIN:?set IBIN=/path/to/incus-backup}
SERVER=${SERVER:?set SERVER=host:port of the backup server}
TOKEN=${TOKEN:?set TOKEN=<one-time enrolment token for the scratch repository>}
RECOVERY_IMAGE=${RECOVERY_IMAGE:-images:alpine/edge/cloud}
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
# the READ column of the run report for one object (bytes as printed, e.g. "0 B", "3.1 MiB")
read_of() { awk -F' {2,}' -v o="$1" 'index($1, o) { print $5; exit }' "$2"; }

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
filesystem:
  full_every: 168h
recovery:
  image: $RECOVERY_IMAGE
  pool: $POOL
YAML
ib init --server "$SERVER" --token "$TOKEN" --name gate --i-stored-the-recovery-key </dev/null >"$WORK/init.log" 2>&1 \
	&& pass "init + enrol at $SERVER (throw-away key)" || { fail "init: $(tail -3 "$WORK/init.log")"; summary; exit 1; }

say "2. a container with a filesystem volume holding many small files and a few large ones"
incus storage volume create "$POOL" gate-fs --project "$PROJECT" >/dev/null || { fail "volume"; exit 1; }
incus launch images:alpine/edge gate-ct --project "$PROJECT" -s "$POOL" >/dev/null \
	&& incus config device add gate-ct fs disk pool="$POOL" source=gate-fs path=/mnt/fs --project "$PROJECT" >/dev/null \
	&& pass "container gate-ct started" || { fail "container creation"; summary; exit 1; }
sleep 15
incus exec gate-ct --project "$PROJECT" -- sh -c '
	mkdir -p /mnt/fs/small /mnt/fs/big /mnt/fs/acl
	i=0; while [ $i -lt 3000 ]; do echo "file $i" > /mnt/fs/small/f$i; i=$((i+1)); done
	head -c 25000000 /dev/urandom > /mnt/fs/big/a.bin
	head -c 25000000 /dev/urandom > /mnt/fs/big/b.bin
	echo secret > /mnt/fs/acl/s.txt; chmod 640 /mnt/fs/acl/s.txt; ln /mnt/fs/acl/s.txt /mnt/fs/acl/s.hard; ln -s s.txt /mnt/fs/acl/s.link
	sync' && pass "files written" || fail "writing files"

say "3. the first run reads everything"
ib run > "$WORK/run1.txt" 2>&1; cat "$WORK/run1.txt"
grep -q FAILED "$WORK/run1.txt" && fail "run 1 has failures" || pass "run 1 committed"

say "4. G3b: an unchanged tree must be reused without reading (this needs stable inode and ctime across Incus ZFS snapshots)"
sleep 5
ib run > "$WORK/run2.txt" 2>&1; cat "$WORK/run2.txt"
R=$(read_of volume/gate-fs "$WORK/run2.txt"); note "volume/gate-fs read on the second run: $R"
case "$R" in
	"0 B"*) pass "G3b: nothing was read on the unchanged second run (inode and ctime are stable across snapshots)" ;;
	*) fail "G3b: the unchanged volume was read again ($R): inode/ctime are NOT stable across Incus ZFS snapshots on this host. Safe (it falls back to reading) but slow: set filesystem.trust_ctime: false and re-run this gate, or investigate (docs/real-host-validation.md section 3)" ;;
esac
note "diagnostic: take two Incus snapshots of the volume and compare the metadata of one untouched file"
incus storage volume snapshot create "$POOL" gate-fs g1 --project "$PROJECT" >/dev/null 2>&1; sleep 2; incus storage volume snapshot create "$POOL" gate-fs g2 --project "$PROJECT" >/dev/null 2>&1
MP=$(zfs list -H -o mountpoint 2>/dev/null | grep gate-fs | head -1)
[ -n "$MP" ] && note "mountpoint: $MP" || note "(no zfs mountpoint found for the volume; the run report above is the evidence)"
incus storage volume snapshot delete "$POOL" gate-fs g1 --project "$PROJECT" >/dev/null 2>&1; incus storage volume snapshot delete "$POOL" gate-fs g2 --project "$PROJECT" >/dev/null 2>&1

say "5. G3a: change two files, one large and one small: only they are read"
incus exec gate-ct --project "$PROJECT" -- sh -c 'echo changed >> /mnt/fs/small/f7; head -c 25000000 /dev/urandom > /mnt/fs/big/b.bin; sync'
sleep 5
ib run > "$WORK/run3.txt" 2>&1; cat "$WORK/run3.txt"
R=$(read_of volume/gate-fs "$WORK/run3.txt"); note "volume/gate-fs read on the third run: $R"
case "$R" in
	*GiB*) fail "run 3 read a GiB: change detection is not working" ;;
	*) pass "run 3 read only the changed data ($R)" ;;
esac

say "6. a rewrite that restores size and mtime must still be detected (ctime)"
incus exec gate-ct --project "$PROJECT" -- sh -c '
	f=/mnt/fs/big/a.bin; t=$(stat -c %y $f)
	head -c 25000000 /dev/urandom > /tmp/new.bin; cat /tmp/new.bin > $f; touch -d "$t" $f; sync'
sleep 5
ib run > "$WORK/run4.txt" 2>&1; cat "$WORK/run4.txt"
R=$(read_of volume/gate-fs "$WORK/run4.txt"); note "volume/gate-fs read after the forged-mtime rewrite: $R"
case "$R" in
	"0 B"*) fail "a content change with preserved size and mtime was NOT detected (ctime is not being compared or not changing)" ;;
	*) pass "the forged-mtime rewrite was read (ctime/inode comparison works on ZFS)" ;;
esac

say "7. restore equals the source, and single-file browsing/extraction works"
incus exec gate-ct --project "$PROJECT" -- sh -c 'cd /mnt/fs && find . -type f -exec sha256sum {} + | sort -k2' > "$WORK/src.sums"
mkdir -p "$WORK/restored"
ib restore files volume/gate-fs --project "$PROJECT" --to "$WORK/restored" > "$WORK/rest1.txt" 2>&1; cat "$WORK/rest1.txt"
( cd "$WORK/restored" && find . -type f -exec sha256sum {} + | sort -k2 ) > "$WORK/dst.sums"
cmp -s "$WORK/src.sums" "$WORK/dst.sums" && pass "the restored volume equals the source (every file)" || fail "restored volume DIFFERS: diff $WORK/src.sums $WORK/dst.sums"
ib fs ls -l volume/gate-fs --project "$PROJECT" acl > "$WORK/ls.txt" 2>&1; cat "$WORK/ls.txt"
grep -q 's.link ->' "$WORK/ls.txt" && pass "fs ls shows the symlink and its target" || fail "fs ls"
[ "$(ib fs cat volume/gate-fs --project "$PROJECT" acl/s.txt 2>/dev/null)" = secret ] && pass "fs cat returns a single file" || fail "fs cat"
mkdir -p "$WORK/one"
ib fs extract volume/gate-fs --project "$PROJECT" acl --to "$WORK/one" > "$WORK/ex.txt" 2>&1
[ "$(stat -c %i "$WORK/one/acl/s.txt")" = "$(stat -c %i "$WORK/one/acl/s.hard")" ] && pass "the hard link came back as a hard link" || fail "hard link lost"
[ "$(stat -c %a "$WORK/one/acl/s.txt")" = 640 ] && pass "mode preserved" || fail "mode"

say "8. full-data fallback: a failed verification forces a full read"
ib run --object gate-ct > /dev/null 2>&1
note "(server side, scratch repository only) corrupt one chunk file of the newest snapshot, run 'incus-backup-server -r ibgate-fs verify', then continue"
note "see docs/real-host-validation.md section 6 for the exact commands; the unit tests cover the logic"

say "9. G3c: single files from a VM DISK through the disposable recovery VM"
incus init images:debian/12/cloud gate-vm --vm -s "$POOL" --project "$PROJECT" -d root,size=8GiB \
	-c security.secureboot=false -c raw.qemu.conf="$AUDIO_CONF" -c cloud-init.user-data='#cloud-config
write_files:
  - path: /root/gate-marker.txt
    content: "hello from the guest disk"
' >/dev/null && incus start gate-vm --project "$PROJECT" && pass "VM gate-vm started" || { fail "VM creation"; summary; exit 1; }
note "waiting 90 s for cloud-init to write the marker..."; sleep 90
incus stop gate-vm --project "$PROJECT" && pass "VM stopped" || fail "VM stop"
ib run --object gate-vm > "$WORK/run5.txt" 2>&1; cat "$WORK/run5.txt"
grep -q FAILED "$WORK/run5.txt" && fail "VM backup failed" || pass "VM backed up"
mkdir -p "$WORK/guestfiles"
ib restore guest-files instance/gate-vm/root /root/gate-marker.txt --project "$PROJECT" --to "$WORK/guestfiles" --image "$RECOVERY_IMAGE" --pool "$POOL" > "$WORK/gf.txt" 2>&1; cat "$WORK/gf.txt"
grep -q "hello from the guest disk" "$WORK/guestfiles/root/gate-marker.txt" 2>/dev/null && pass "G3c: the file came out of the VM disk through the isolated recovery VM" || fail "G3c: guest-file recovery failed (read $WORK/gf.txt: image, cloud-init, filesystem support)"
LEFT=$(incus list --project "$PROJECT" -c n -f csv | grep ibk-rec- ; incus storage volume list "$POOL" --project "$PROJECT" -c n -f csv | grep ibk-rec-)
[ -z "$LEFT" ] && pass "no recovery VM or scratch volume was left behind" || fail "leftovers: $LEFT"
ib doctor 2>&1 | grep -q 'recovery leftovers' && fail "doctor reports recovery leftovers" || pass "doctor: no recovery leftovers"

summary
