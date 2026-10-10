# shellcheck shell=bash
# scripts/release-lib.sh - shared helpers of release-apk.sh and smoke-install.sh (sourced, bash).
#
# A throwaway Alpine root without root privileges: apk-tools-static
# (signature checked against the keys of the official minirootfs) installs
# packages into a directory with --usermode, and proot (ptrace, no
# namespaces, no setuid) runs programs inside it, as fake root (-0) or as the
# calling user. Nothing here needs sudo, user namespaces or a container
# runtime; it does need ptrace and network access to the Alpine mirror.

: "${ALPINE_VERSION:=3.24}"          # keep in step with unidoc-aports' CI (alpine:3.24)
: "${ALPINE_MIRROR:=https://dl-cdn.alpinelinux.org/alpine}"
ALPINE_ARCH=$(uname -m)
: "${ALPINE_CACHE:=${XDG_CACHE_HOME:-$HOME/.cache}/incus-backup-alpine}"

lib_die() { echo "${0##*/}: ERROR: $*" >&2; exit 1; }
lib_log() { echo "${0##*/}: $*" >&2; }

# lib_fetch_tools: downloads (once) the minirootfs release file and
# apk-tools-static, verifies both, and leaves $ALPINE_CACHE/apk.static and
# $ALPINE_CACHE/keys/ (the official signing keys from the minirootfs).
lib_fetch_tools() {
	local c=$ALPINE_CACHE b=$ALPINE_MIRROR/v$ALPINE_VERSION
	mkdir -p "$c"
	[ "$ALPINE_ARCH" = x86_64 ] || [ "$ALPINE_ARCH" = aarch64 ] || lib_die "unsupported host arch $ALPINE_ARCH"
	if [ ! -x "$c/apk.static" ] || [ ! -d "$c/keys" ]; then
		local rel mr apkst
		rel=$(curl -fsSL "$b/releases/$ALPINE_ARCH/latest-releases.yaml") || lib_die "cannot reach $b"
		mr=$(printf '%s\n' "$rel" | sed -n 's/^ *file: \(alpine-minirootfs-.*\.tar\.gz\)$/\1/p' | head -n1)
		[ -n "$mr" ] || lib_die "no minirootfs in latest-releases.yaml"
		curl -fsSL -o "$c/$mr" "$b/releases/$ALPINE_ARCH/$mr"
		curl -fsSL -o "$c/$mr.sha256" "$b/releases/$ALPINE_ARCH/$mr.sha256"
		(cd "$c" && sha256sum -c "$mr.sha256" >/dev/null) || lib_die "$mr: sha256 mismatch"
		rm -rf "$c/keys" "$c/mr" && mkdir -p "$c/mr"
		tar -xzf "$c/$mr" -C "$c/mr" ./etc/apk/keys
		mv "$c/mr/etc/apk/keys" "$c/keys" && rm -rf "$c/mr"
		apkst=$(curl -fsSL "$b/main/$ALPINE_ARCH/" | grep -o 'apk-tools-static-[0-9][^"]*\.apk' | sort -uV | tail -n1)
		[ -n "$apkst" ] || lib_die "apk-tools-static not found on the mirror"
		curl -fsSL -o "$c/$apkst" "$b/main/$ALPINE_ARCH/$apkst"
		rm -rf "$c/ats" && mkdir -p "$c/ats"
		tar -xzf "$c/$apkst" -C "$c/ats" 2>/dev/null
		# The binary carries its own detached RSA signature (sbin/apk.static.SIGN.RSA.sha256.<key>).
		local sig key ok=""
		for sig in "$c"/ats/sbin/apk.static.SIGN.RSA.sha256.*; do
			key=$c/keys/${sig##*.SIGN.RSA.sha256.}
			[ -f "$key" ] && openssl dgst -sha256 -verify "$key" -signature "$sig" "$c/ats/sbin/apk.static" >/dev/null && ok=1
		done
		[ -n "$ok" ] || lib_die "apk.static signature does not verify against the Alpine keys"
		install -m 0755 "$c/ats/sbin/apk.static" "$c/apk.static"
		rm -rf "$c/ats"
		lib_log "apk.static ($apkst) verified against the Alpine signing keys"
	fi
}

# lib_rootfs DIR PACKAGES... : a fresh Alpine root at DIR with PACKAGES (plus
# alpine-base and proot), maintainer scripts and triggers run inside proot.
lib_rootfs() {
	local r=$1
	shift
	lib_fetch_tools
	rm -rf "$r" && mkdir -p "$r/etc/apk"
	cp -R "$ALPINE_CACHE/keys" "$r/etc/apk/keys"
	printf '%s\n' "$ALPINE_MIRROR/v$ALPINE_VERSION/main" "$ALPINE_MIRROR/v$ALPINE_VERSION/community" > "$r/etc/apk/repositories"
	# Outside the root, apk cannot run scripts (no chroot without privileges):
	# install files only, then let apk run every script and trigger inside proot.
	"$ALPINE_CACHE/apk.static" --usermode --root "$r" --initdb --no-scripts --quiet add alpine-base proot "$@" >&2 ||
		lib_die "bootstrapping $r failed"
	lib_in0 "$r" /bin/busybox --install -s
	lib_in0 "$r" /sbin/apk fix --quiet >&2 || lib_die "running the package scripts in $r failed"
	cp /etc/resolv.conf "$r/etc/resolv.conf" 2>/dev/null || :
}

# lib_in0 ROOT CMD...  : run CMD in ROOT as (fake) root.
# lib_in  ROOT CMD...  : run CMD in ROOT as the calling uid (HOME=$LIB_HOME).
lib_proot() {
	local r=$1
	shift
	env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin TERM=dumb LANG=C.UTF-8 \
		HOME="${LIB_HOME:-/root}" ${LIB_USER:+USER=$LIB_USER LOGNAME=$LIB_USER} \
		"$r/lib/ld-musl-$ALPINE_ARCH.so.1" --library-path "$r/lib:$r/usr/lib" "$r/usr/bin/proot" \
		-r "$r" -w / -b /proc -b /dev -b /sys "$@"
}
lib_in0() {
	local r=$1
	shift
	LIB_HOME=/root lib_proot "$r" -0 "$@"
}
lib_in() { lib_proot "$@"; }
