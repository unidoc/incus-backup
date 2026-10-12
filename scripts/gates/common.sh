#!/bin/sh
# Shared helpers for the validation gates. SAFETY RULES (enforced here):
#   * everything the gates create lives in the Incus project "ibgate" (or in a
#     scratch directory under /var/tmp/ibgate.*), never in any other project;
#   * a gate REFUSES to start if the project "ibgate" already exists;
#   * cleanup deletes only the project "ibgate" and its scratch directory;
#   * no existing instance, volume, bitmap, snapshot or ZFS dataset is touched.
# (POSIX sh: runs on Alpine's busybox ash.)

PROJECT=ibgate
POOL=${POOL:-default}
FAILS=0
PASSES=0

say()  { printf '\n=== %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
pass() { PASSES=$((PASSES + 1)); printf 'PASS  %s\n' "$*"; }
fail() { FAILS=$((FAILS + 1)); printf 'FAIL  %s\n' "$*"; }

need() {
	for b in "$@"; do
		command -v "$b" >/dev/null 2>&1 || { echo "missing required command: $b"; exit 2; }
	done
}

refuse_if_project_exists() {
	if incus project show "$PROJECT" >/dev/null 2>&1; then
		echo "REFUSING: the Incus project '$PROJECT' already exists (a previous gate run?)."
		echo "Inspect it with: incus project show $PROJECT ; incus list --project $PROJECT"
		echo "If it is yours to remove: sh $0 cleanup"
		exit 1
	fi
}

create_project() {
	incus project create "$PROJECT" \
		-c features.images=false -c features.profiles=false \
		-c features.storage.volumes=true -c features.storage.buckets=false \
		-c user.ibgate=1 || { echo "cannot create project"; exit 1; }
}

# delete_project removes ONLY what lives in the gate project.
delete_project() {
	incus project show "$PROJECT" >/dev/null 2>&1 || return 0
	[ "$(incus project get "$PROJECT" user.ibgate 2>/dev/null)" = "1" ] || { echo "project $PROJECT is not marked as a gate project: not deleting"; return 1; }
	for i in $(incus list --project "$PROJECT" -c n -f csv 2>/dev/null); do
		incus delete --project "$PROJECT" --force "$i" >/dev/null 2>&1
	done
	for v in $(incus storage volume list "$POOL" --project "$PROJECT" -c n -f csv 2>/dev/null | grep -v '^$'); do
		incus storage volume delete "$POOL" "$v" --project "$PROJECT" >/dev/null 2>&1
	done
	incus project delete "$PROJECT" >/dev/null 2>&1 || echo "could not delete the project; remove leftovers by hand: incus list --project $PROJECT"
}

# Cloud-init user data of the guest writer (no incus-agent in guests): writes
# random 64 KiB blocks and a counter to the two extra disks until stopped.
# shellcheck disable=SC2034 # used by the g-*.sh scripts that source this file
WRITER_USERDATA='#cloud-config
write_files:
  - path: /usr/local/bin/gate-writer.sh
    permissions: "0755"
    content: |
      #!/bin/bash
      D1=
      for d in /sys/block/sd*; do [ "$(cat $d/size)" = 2097152 ] && D1=/dev/$(basename $d); done
      [ -n "$D1" ] || exit 1
      i=0
      while true; do
        i=$((i+1))
        printf "%-512s" $i | dd of=$D1 bs=512 count=1 oflag=direct conv=notrunc status=none
        head -c 65536 /dev/urandom | dd of=$D1 bs=64k seek=$((RANDOM%16000+1)) oflag=direct conv=notrunc status=none
        if [ $((i%20)) = 0 ]; then head -c 1048576 /dev/urandom > /var/tmp/gate-root; sync; fi
      done
  - path: /etc/systemd/system/gate-writer.service
    content: |
      [Unit]
      Description=gate guest writer
      [Service]
      ExecStart=/usr/local/bin/gate-writer.sh
      [Install]
      WantedBy=multi-user.target
runcmd:
  - [ systemctl, daemon-reload ]
  - [ systemctl, enable, --now, gate-writer.service ]
'

# QEMU 11 on Benfica needs the audio device disabled (see the project memory).
# shellcheck disable=SC2034 # used by the g-*.sh scripts that source this file
AUDIO_CONF='[audiodev "qemu_sound-audiodev"]
driver = "none"
'

summary() {
	say "SUMMARY: $PASSES passed, $FAILS failed"
	[ "$FAILS" -eq 0 ]
}
