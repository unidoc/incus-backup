#!/bin/sh
# Runs INSIDE the disposable recovery VM (no network, no incus-agent needed).
# Input : /etc/ibk-request  (partition=N, path=/abs/path ...)
# Disks : the restored guest disk (read-only use) and the output disk
# Output: header at offset 0 of the output disk, a tar stream from offset 4096
# Nothing here ever writes to the guest disk and nothing from the guest filesystem is executed.
export PATH="$PATH:/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/sbin:/usr/local/bin"
# Locations are variables only so the script can be exercised on a build machine (see the tests);
# inside the recovery VM they are always the defaults.
BYID=${IBK_BYID:-/dev/disk/by-id}
REQ=${IBK_REQ:-/etc/ibk-request}
MNT=${IBK_MNT:-/mnt/ibk}
TMP=${IBK_TMP:-/tmp}
PROCMOUNTS=${IBK_PROCMOUNTS:-/proc/mounts}
# Whatever happens, the VM powers itself off: the host waits for exactly that.
trap 'sync; poweroff -f' EXIT
is_blk() { if [ -n "${IBK_TESTBLK:-}" ]; then [ -e "$1" ]; else [ -b "$1" ]; fi; }
LOG=$TMP/ibk.log
: >"$LOG"
log() { printf '%s\n' "$*" >>"$LOG"; }

find_dev() { # find_dev NAME -> device path of the Incus disk device called NAME
	for d in "$BYID"/*incus_"$1" "$BYID"/*incus_"$1"-* ; do
		[ -e "$d" ] && { readlink -f "$d"; return 0; }
	done
	return 1
}

json_escape() { printf '%s' "$1" | tr '\n\t\r' '   ' | tr -d '\000-\037' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

write_header() { # write_header JSON (one 4 KiB block; a longer one is refused, never truncated)
	if [ "$(printf 'IBREC1\n%s\n' "$1" | wc -c)" -gt 4096 ]; then
		printf 'IBREC1\n{"status":"error","error":"result header too large"}\n' >"$TMP/ibk.header"
	else
		printf 'IBREC1\n%s\n' "$1" >"$TMP/ibk.header"
	fi
	dd if="$TMP/ibk.header" of="$OUT" bs=4096 count=1 conv=sync,notrunc 2>/dev/null
}

fail() {
	write_header "{\"status\":\"error\",\"error\":\"$(json_escape "$1")\",\"log\":\"$(json_escape "$(tail -c 600 "$LOG")")\"}"
	exit 1
}

# wait for the devices
i=0
while [ $i -lt 60 ]; do
	DISK=$(find_dev guestdisk) && OUT=$(find_dev out) && break
	i=$((i + 1)); sleep 1
done
[ -n "${DISK:-}" ] || { OUT=$(find_dev out || true); fail "the guest disk did not appear"; }
[ -n "${OUT:-}" ] || exit 1   # (the EXIT trap still powers the VM off)
: >"$TMP/ibk.paths"
PART=0
while IFS== read -r k v; do
	case "$k" in
		partition) PART=$v ;;
		path) echo "$v" >>"$TMP/ibk.paths" ;;
	esac
done <"$REQ"
[ -s "$TMP/ibk.paths" ] || fail "no paths requested"

mkdir -p "$MNT"
try_mount() { # try_mount DEV -> 0 on success; sets FSTYPE
	for opts in "ro,nosuid,nodev,noexec,noload" "ro,nosuid,nodev,noexec,norecovery" "ro,nosuid,nodev,noexec"; do
		if mount -o "$opts" "$1" "$MNT" 2>>"$LOG"; then
			FSTYPE=$(awk -v d="$1" '$1==d{print $3}' "$PROCMOUNTS" | tail -n1)
			return 0
		fi
	done
	return 1
}

# candidate devices: the given partition, or every partition IN NUMERIC ORDER, or the whole disk
CANDS=""
nth() { for p in "${DISK}$1" "${DISK}p$1"; do if is_blk "$p"; then printf '%s' "$p"; return 0; fi; done; return 1; }
if [ "$PART" -gt 0 ] 2>/dev/null; then
	CANDS=$(nth "$PART") || fail "partition $PART does not exist on the guest disk"
else
	n=1
	while [ $n -le 128 ]; do
		p=$(nth $n) && CANDS="$CANDS $p"
		n=$((n + 1))
	done
	CANDS="$CANDS $DISK"
fi

# A path whose ANCESTORS are symlinks of the guest would be resolved by this VM's own root (an absolute
# link points at the recovery VM, not at the guest): such requests are refused, not guessed.
sym_ancestor() { # sym_ancestor /a/b/c  (components below $MNT; the last one may itself be a link)
	_sa_rel=${1#/}; _sa_cur="$MNT"
	while :; do
		case "$_sa_rel" in
			*/*) _sa_comp=${_sa_rel%%/*}; _sa_rel=${_sa_rel#*/}
			     _sa_cur="$_sa_cur/$_sa_comp"
			     [ -L "$_sa_cur" ] && return 0 ;;
			*) return 1 ;;
		esac
	done
}

FIRST=$(head -n1 "$TMP/ibk.paths")
CHOSEN=""
for c in $CANDS; do
	if try_mount "$c"; then
		if ! sym_ancestor "$FIRST" && { [ -e "$MNT$FIRST" ] || [ -L "$MNT$FIRST" ]; }; then CHOSEN=$c; break; fi
		umount "$MNT" 2>>"$LOG"
	fi
done
[ -n "$CHOSEN" ] || fail "no filesystem on the guest disk holds $FIRST (or it could not be mounted read-only: unsupported filesystem?)"

# the requested paths that exist on this filesystem
: >"$TMP/ibk.list"
RES=""
while read -r p; do
	rel=${p#/}
	if [ -z "$rel" ]; then
		RES="$RES\"$(json_escape "$p")\":\"not allowed\","
	elif sym_ancestor "$p"; then
		RES="$RES\"$(json_escape "$p")\":\"symlink in path\","
	elif [ -e "$MNT/$rel" ] || [ -L "$MNT/$rel" ]; then
		printf './%s\n' "$rel" >>"$TMP/ibk.list"
		RES="$RES\"$(json_escape "$p")\":\"ok\","
	else
		RES="$RES\"$(json_escape "$p")\":\"missing\","
	fi
done <"$TMP/ibk.paths"
RES=${RES%,}
[ -s "$TMP/ibk.list" ] || fail "none of the requested paths can be recovered (missing, not allowed or behind a symlink)"

# ONE tar run (so ONE archive the host can read to its end), names from a list file: paths with spaces,
# quotes or a leading dash are safe, and its exit status is checked
TAROPT=""
tar --help 2>&1 | grep -q -- --xattrs && TAROPT="--xattrs --acls --numeric-owner"
rm -f "$TMP/ibk.rc"
{ tar $TAROPT -C "$MNT" -cf - -T "$TMP/ibk.list" 2>>"$LOG"; echo $? >"$TMP/ibk.rc"; } | dd of="$OUT" bs=4096 seek=1 conv=notrunc 2>>"$LOG" \
	|| fail "writing the archive failed"
[ "$(cat "$TMP/ibk.rc" 2>/dev/null)" = 0 ] || fail "tar reported problems (an unreadable or damaged file?): $(tail -c 300 "$LOG")"
umount "$MNT" 2>>"$LOG"
HDR="{\"status\":\"ok\",\"partition\":\"$(json_escape "$CHOSEN")\",\"fs\":\"$(json_escape "$FSTYPE")\",\"paths\":{$RES}}"
# a very long request would not fit the 4 KiB header: report the summary only (the archive is complete)
[ "${#HDR}" -le 3800 ] || HDR="{\"status\":\"ok\",\"partition\":\"$(json_escape "$CHOSEN")\",\"fs\":\"$(json_escape "$FSTYPE")\",\"paths_omitted\":true}"
write_header "$HDR"
