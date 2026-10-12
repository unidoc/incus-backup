#!/usr/bin/env bash
# scripts/release-apk.sh [DIST] - build the Alpine packages from a release (scripts/release.sh)
# with the real abuild, signed with a THROWAWAY key, into DIST/apk/.
#
# This is the verification build (the published packages are built and
# signed by unidoc-aports' CI from DIST/aports/incus-backup/). It runs the
# same abuild as that CI, but without root and without a container: on an
# Alpine host with abuild it uses the host's abuild; anywhere else it builds
# inside a throwaway Alpine root through proot (scripts/release-lib.sh).
#
# Output: DIST/apk/<arch>/*.apk + APKINDEX.tar.gz, DIST/apk/keys/<key>.rsa.pub
# (the throwaway public key, for `apk add` in smoke tests), DIST/apk/abuild.log.
#
# Environment: ALPINE_VERSION (3.24), ALPINE_CACHE, APK_BUILDROOT (default: a
# cached root under ALPINE_CACHE, reused between runs; KEEP_BUILDROOT=0 rebuilds it).
set -euo pipefail
umask 022
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIST=$(cd "${1:-$ROOT/dist}" && pwd)
# shellcheck source=release-lib.sh
. "$ROOT/scripts/release-lib.sh"

AP=$DIST/aports/incus-backup
[ -f "$AP/APKBUILD" ] || lib_die "$AP/APKBUILD missing: run scripts/release.sh first"
VERSION=$(sed -n 's/^pkgver=//p' "$AP/APKBUILD")
TARBALL=$DIST/incus-backup-$VERSION.tar.gz
[ -f "$TARBALL" ] || lib_die "$TARBALL missing"
OUT=$DIST/apk
rm -rf "$OUT" && mkdir -p "$OUT/keys"

if [ -f /etc/alpine-release ] && command -v abuild >/dev/null && [ "$(id -u)" != 0 ]; then
	lib_log "native Alpine: using the host's abuild (makedepends: abuild -r installs them via abuild-apk)"
	H=$(mktemp -d)
	trap 'rm -rf "$H"' EXIT
	mkdir -p "$H/aports" "$H/distfiles"
	cp -R "$AP" "$H/aports/"
	cp "$TARBALL" "$H/distfiles/"
	HOME=$H PACKAGER="incus-backup-throwaway" abuild-keygen -a -n >/dev/null 2>&1
	printf 'SRCDEST=%s\nREPODEST=%s\n' "$H/distfiles" "$H/packages" >> "$H/.abuild/abuild.conf"
	(cd "$H/aports/incus-backup" && HOME=$H abuild -r) 2>&1 | tee "$OUT/abuild.log"
	cp -R "$H/packages/aports/." "$OUT/"
	cp "$H"/.abuild/*.rsa.pub "$OUT/keys/"
else
	BR=${APK_BUILDROOT:-$ALPINE_CACHE/buildroot-$ALPINE_VERSION-$ALPINE_ARCH}
	uid=$(id -u) gid=$(id -g)
	if [ "${KEEP_BUILDROOT:-1}" = 0 ] || [ ! -x "$BR/usr/bin/abuild" ]; then
		lib_log "creating the build root $BR (alpine-sdk, go, tzdata)"
		# makedepends + checkdepends of the APKBUILD, installed up front: abuild-apk
		# (setuid) cannot work under proot, so abuild runs with -d.
		deps=$(sed -n 's/^\(make\|check\)depends="\(.*\)"$/\2/p' "$AP/APKBUILD" | tr '\n' ' ')
		lib_rootfs "$BR" alpine-sdk $deps
		# The building user = the calling uid (proot reports it as is); fakeroot
		# records ownership inside abuild; pkgusers must exist in the root.
		lib_in0 "$BR" /bin/sh -c "
			set -e
			getent passwd builder >/dev/null || adduser -D -u $uid builder
			addgroup builder abuild 2>/dev/null || :
			getent group incus-backup >/dev/null || addgroup -S incus-backup
			getent passwd incus-backup >/dev/null || adduser -S -D -H -h /var/lib/incus-backup-server -s /sbin/nologin -G incus-backup incus-backup
			chown -R $uid:$gid /home/builder"
	fi
	[ "$(lib_in0 "$BR" /usr/bin/id -u builder)" = "$uid" ] || lib_die "builder in $BR is not uid $uid; KEEP_BUILDROOT=0 to recreate"
	lib_in0 "$BR" /bin/sh -c "rm -rf /home/builder/aports /home/builder/packages /home/builder/distfiles /home/builder/.abuild"
	mkdir -p "$BR/home/builder/aports" "$BR/home/builder/distfiles"
	cp -R "$AP" "$BR/home/builder/aports/"
	cp "$TARBALL" "$BR/home/builder/distfiles/"
	export LIB_HOME=/home/builder LIB_USER=builder
	lib_in "$BR" /bin/sh -c 'PACKAGER="incus-backup-throwaway" abuild-keygen -a -n >/dev/null 2>&1 &&
		printf "SRCDEST=/home/builder/distfiles\nREPODEST=/home/builder/packages\n" >> ~/.abuild/abuild.conf'
	# abuild indexes the repository with apk, which must trust the key (abuild-keygen -i needs sudo).
	lib_in0 "$BR" /bin/sh -c 'rm -f /etc/apk/keys/builder-*.rsa.pub /etc/apk/keys/incus-backup-throwaway-*.rsa.pub; cp /home/builder/.abuild/*.rsa.pub /etc/apk/keys/'
	lib_log "abuild -d -r (log: $OUT/abuild.log)"
	set +e
	lib_in "$BR" /bin/sh -c 'cd ~/aports/incus-backup && abuild -d -r' > "$OUT/abuild.log" 2>&1
	rc=$?
	set -e
	[ $rc = 0 ] || { tail -n 40 "$OUT/abuild.log" >&2; lib_die "abuild failed (rc $rc)"; }
	cp -R "$BR/home/builder/packages/aports/." "$OUT/"
	cp "$BR"/home/builder/.abuild/*.rsa.pub "$OUT/keys/"
fi
ls -l "$OUT"/*/ >&2
lib_log "packages in $OUT (signed with a throwaway key: $(ls "$OUT/keys"))"
