#!/usr/bin/env bash
# scripts/release.sh VERSION - build a reproducible incus-backup release into dist/.
#
# Produces (dist/):
#   incus-backup-VERSION.tar.gz            source tarball: the tree + vendor/ (all Go modules)
#                                          + the BUILT web UI, so Alpine builds it offline
#   incus-backup-VERSION-linux-{amd64,arm64}         static client binaries
#   incus-backup-server-VERSION-linux-{amd64,arm64}  static server binaries
#   SHA256SUMS                             sha256 of every asset above
#   *.minisig                              per-asset minisign signatures (only when a key is given)
#   RELEASE_NOTES.md                       the CHANGELOG.md section of VERSION (or a stub)
#   BUILDINFO                              Go version, SOURCE_DATE_EPOCH, flags
#   aports/incus-backup/                   APKBUILD + files for unidoc-aports, checksums filled in
#   incus-backup-VERSION-aports.tar.gz     the same directory as a release asset
#
# The binaries are built FROM THE EXTRACTED TARBALL with GOPROXY=off and
# -mod=vendor: if the tarball misses anything, the release fails here instead
# of in the Alpine build. Then every binary is checked: statically linked
# (file + readelf: no PT_INTERP, no dynamic section), right architecture, and
# `--version` / `version` print VERSION (arm64 is executed only when
# qemu-aarch64 is available: set QEMU_AARCH64, or have it in PATH).
#
# Reproducible: same commit + same Go version + same SOURCE_DATE_EPOCH give
# byte-identical artefacts (tar sorted, owner 0, fixed mtime, gzip -n; Go
# -trimpath -buildvcs=false -buildid=). The flags are the ones the APKBUILD
# uses, so the apk's binaries equal these (docs/packaging.md).
#
# Environment:
#   SOURCE_DATE_EPOCH   timestamp for the tarball and man pages (default: last git
#                       commit time; without git it is REQUIRED)
#   ARCHES              default "amd64 arm64"
#   WEBUI_SRC           web UI source dir with package.json (default: web); its `crypto` script
#                       (web-crypto, needs Go) runs first when package.json has one
#   WEBUI_DIST          where the Go code embeds it from (default: internal/server/webui/dist)
#   ALLOW_PLACEHOLDER_WEBUI=1   release even if the web UI dist is only a placeholder
#   MINISIGN_SECRET_KEY_FILE    sign with this minisign secret key file (no password, -W)
#   INCUS_BACKUP_MINISIGN_KEY   ... or with the key CONTENT (CI: the "signing" environment secret)
#   MINISIGN_PUBLIC_KEY         if set, every signature is verified against it
#   DIST                default dist
set -euo pipefail
umask 022

die() { echo "release: ERROR: $*" >&2; exit 1; }
log() { echo "release: $*" >&2; }

[ $# -eq 1 ] || die "usage: scripts/release.sh VERSION   (e.g. 0.1.0)"
VERSION=$1
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

# apk accepts 1.2.3, 1.2.3_rc1, ... (not 1.2.3-rc1): keep releases apk-valid.
[[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+(_(alpha|beta|pre|rc)[0-9]+)?$ ]] ||
	die "VERSION $VERSION is not of the form 1.2.3 or 1.2.3_rc1 (apk version syntax)"
FILEVER=$(tr -d '[:space:]' < version.txt)
[ "$FILEVER" = "$VERSION" ] || die "version.txt says $FILEVER, not $VERSION: bump version.txt first"

command -v go >/dev/null || die "go not found"
for t in tar gzip sha256sum sha512sum file readelf; do
	command -v $t >/dev/null || die "$t not found"
done

GIT=""
if command -v git >/dev/null && git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	GIT=1
fi
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	[ -n "$GIT" ] || die "SOURCE_DATE_EPOCH is required outside a git checkout"
	SOURCE_DATE_EPOCH=$(git -C "$ROOT" log -1 --format=%ct)
fi
[[ $SOURCE_DATE_EPOCH =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be a unix timestamp"
export SOURCE_DATE_EPOCH

ARCHES=${ARCHES:-"amd64 arm64"}
WEBUI_SRC=${WEBUI_SRC:-web}
WEBUI_DIST=${WEBUI_DIST:-internal/server/webui/dist}
DIST=${DIST:-$ROOT/dist}
NAME=incus-backup-$VERSION
# The ONE set of build flags (the APKBUILD's _goflags/ldflags are the same).
GOFLAGS_REL="-trimpath -buildvcs=false -mod=vendor"
LDFLAGS_REL="-s -w -buildid= -X main.version=$VERSION"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/incus-backup-release.XXXXXX")
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT
# dist/ is wiped: never let a mistyped DIST wipe the tree (or anything else).
mkdir -p "$DIST"
DIST=$(cd "$DIST" && pwd)
case "$DIST" in / | "$ROOT" | "$HOME") die "refusing DIST=$DIST" ;; esac
[ ! -e "$DIST/go.mod" ] && [ ! -d "$DIST/.git" ] || die "refusing DIST=$DIST: it looks like a source tree"
rm -rf "$DIST"
mkdir -p "$DIST"

# ── 1. Web UI ────────────────────────────────────────────────────────────
if [ -f "$WEBUI_SRC/package.json" ]; then
	command -v npm >/dev/null || die "npm is needed to build the web UI ($WEBUI_SRC)"
	log "building the web UI ($WEBUI_SRC -> $WEBUI_DIST)"
	# The UI ships the browser decryption library (web-crypto: Go -> WebAssembly + TypeScript). Its
	# build output is not in git and `vite build` fails without it: build it first, as `just web` does.
	if grep -q '"crypto":' "$WEBUI_SRC/package.json"; then
		(cd "$WEBUI_SRC" && npm run crypto)
	fi
	# Vite writes straight into $WEBUI_DIST (outDir in web/vite.config.ts); a build into
	# $WEBUI_SRC/dist (the older layout) is copied there. Both are cleared first, so that a stale
	# build can never pass for this one.
	rm -rf "${WEBUI_SRC:?}/dist" "${WEBUI_DIST:?}"
	(cd "$WEBUI_SRC" && npm ci --no-audit --no-fund && npm run build)
	if [ -d "$WEBUI_SRC/dist" ]; then
		mkdir -p "$WEBUI_DIST"
		cp -R "$WEBUI_SRC/dist/." "$WEBUI_DIST/"
	fi
	[ -f "$WEBUI_DIST/index.html" ] || die "npm run build produced neither $WEBUI_DIST/index.html nor $WEBUI_SRC/dist"
fi
if [ -d "$(dirname "$WEBUI_DIST")" ]; then
	# A placeholder dist (what the tree carries before the web UI is built) has
	# no index.html or carries a file named PLACEHOLDER.
	if [ ! -f "$WEBUI_DIST/index.html" ] || [ -e "$WEBUI_DIST/PLACEHOLDER" ]; then
		[ "${ALLOW_PLACEHOLDER_WEBUI:-}" = 1 ] ||
			die "$WEBUI_DIST holds no built web UI (no index.html, or a PLACEHOLDER file); build it or set ALLOW_PLACEHOLDER_WEBUI=1"
		log "WARNING: releasing with a PLACEHOLDER web UI ($WEBUI_DIST)"
	fi
fi

# ── 2. Source tree ───────────────────────────────────────────────────────
SRC=$WORK/$NAME
mkdir -p "$SRC"
if [ -n "$GIT" ]; then
	[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=no)" ] ||
		log "WARNING: the working tree has uncommitted changes; they ARE in this release"
	# Tracked files only (what a clone has), plus the generated web UI below. A committed web UI
	# dist is left out: step 1 rebuilt it, so its tracked (hashed) file names may no longer exist,
	# and tar would stop on them. The fresh build is copied in below.
	git -C "$ROOT" ls-files -z -- . ":(exclude)$WEBUI_DIST" | (cd "$ROOT" && tar --null -T - -cf -) | tar -xf - -C "$SRC"
else
	log "not a git checkout: packaging the directory, minus build output and caches"
	tar -C "$ROOT" \
		--exclude=./.git --exclude=./bin --exclude=./dist --exclude=./temp \
		--exclude=node_modules --exclude='./.venv*' --exclude='*.test' --exclude=.cache --exclude=.state --exclude=test-results \
		-cf - . | tar -xf - -C "$SRC"
fi
if [ -d "$WEBUI_DIST" ]; then
	rm -rf "${SRC:?}/$WEBUI_DIST"
	mkdir -p "$SRC/$WEBUI_DIST"
	cp -R "$WEBUI_DIST/." "$SRC/$WEBUI_DIST/"
fi
# The web UI's build output is in the tree once, where go:embed reads it.
rm -rf "${SRC:?}/$WEBUI_SRC/node_modules" "${SRC:?}/$WEBUI_SRC/dist"
log "vendoring Go modules"
(cd "$SRC" && GOFLAGS=-mod=mod go mod vendor)
# go.mod/go.sum must be exactly the committed ones (go mod vendor never changes them,
# but a toolchain that did would make the tarball disagree with the tag).
cmp -s "$SRC/go.mod" "$ROOT/go.mod" || die "go mod vendor changed go.mod"
cmp -s "$SRC/go.sum" "$ROOT/go.sum" || die "go mod vendor changed go.sum"

# ── 3. Deterministic tarball ─────────────────────────────────────────────
TARBALL=$DIST/$NAME.tar.gz
tar -C "$WORK" --sort=name --format=gnu --owner=0 --group=0 --numeric-owner \
	--mtime="@$SOURCE_DATE_EPOCH" --mode='u+rwX,go+rX,go-w' \
	-cf - "$NAME" | gzip -n -9 > "$TARBALL"
log "source tarball: $(du -h "$TARBALL" | cut -f1)"

# ── 4. Binaries, built from the extracted tarball, offline ──────────────
BUILD=$WORK/build
mkdir -p "$BUILD"
tar -xzf "$TARBALL" -C "$BUILD"
for arch in $ARCHES; do
	for b in incus-backup incus-backup-server; do
		out=$DIST/$b-$VERSION-linux-$arch
		(cd "$BUILD/$NAME" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOPROXY=off GOFLAGS="$GOFLAGS_REL" \
			go build -ldflags "$LDFLAGS_REL" -o "$out" ./cmd/$b)
	done
done

# ── 5. Check every binary ────────────────────────────────────────────────
QEMU=${QEMU_AARCH64:-$(command -v qemu-aarch64 || command -v qemu-aarch64-static || true)}
for arch in $ARCHES; do
	case $arch in
		amd64) machine="X86-64"; magic="x86-64" ;;
		arm64) machine="AArch64"; magic="aarch64|ARM aarch64" ;;
		*) die "unknown arch $arch" ;;
	esac
	for b in incus-backup incus-backup-server; do
		f=$DIST/$b-$VERSION-linux-$arch
		# Outputs are captured first: with pipefail, `! readelf | grep -q X` would
		# PASS when X is present (grep exits early, readelf gets SIGPIPE).
		desc=$(file -b "$f")
		hdr=$(readelf -h "$f")
		phdr=$(readelf -l "$f")
		dyn=$(readelf -d "$f" 2>&1 || true)
		grep -Eq "$magic" <<<"$desc" || die "$f: wrong architecture: $desc"
		grep -q 'statically linked' <<<"$desc" || die "$f: not statically linked: $desc"
		grep -q "Machine:.*$machine" <<<"$hdr" || die "$f: readelf machine is not $machine"
		! grep -q 'INTERP' <<<"$phdr" || die "$f: has a PT_INTERP (dynamic loader)"
		! grep -q 'NEEDED' <<<"$dyn" || die "$f: needs shared libraries"
		run="" how="natively"
		if [ "$arch" = amd64 ] && [ "$(uname -m)" = x86_64 ]; then
			run="$f"
		elif [ "$arch" = arm64 ] && [ "$(uname -m)" = aarch64 ]; then
			run="$f"
		elif [ "$arch" = arm64 ] && [ -n "$QEMU" ]; then
			run="$QEMU $f" how="under $QEMU"
		fi
		if [ -n "$run" ]; then
			got=$($run version)
			[ "$got" = "$VERSION" ] || die "$f version printed '$got', want $VERSION"
			got=$($run --version)
			[ "$got" = "$b version $VERSION" ] || die "$f --version printed '$got'"
			$run --help >/dev/null || die "$f --help failed"
			log "ok  $b linux/$arch: static, $machine, version $VERSION (executed $how)"
		else
			log "ok  $b linux/$arch: static, $machine (NOT executed: no qemu-aarch64)"
		fi
	done
done

# ── 6. Notes, build info, aports files ───────────────────────────────────
if [ -f CHANGELOG.md ] && grep -q "^## \[\?$VERSION\]\?" CHANGELOG.md; then
	awk -v v="$VERSION" '
		/^## / { if (p) exit; if ($0 ~ "^## \\[?" v "\\]?([^0-9.]|$)") p=1 }
		p' CHANGELOG.md > "$DIST/RELEASE_NOTES.md"
else
	log "WARNING: CHANGELOG.md has no section for $VERSION: writing a stub"
	printf '## %s\n\n(TODO: release notes)\n' "$VERSION" > "$DIST/RELEASE_NOTES.md"
fi
{
	echo "version: $VERSION"
	echo "go: $(go version)"
	echo "source_date_epoch: $SOURCE_DATE_EPOCH"
	echo "goflags: $GOFLAGS_REL"
	echo "ldflags: $LDFLAGS_REL"
	echo "cgo: disabled"
	[ -n "$GIT" ] && echo "commit: $(git -C "$ROOT" rev-parse HEAD)"
} > "$DIST/BUILDINFO"

AP=$DIST/aports/incus-backup
mkdir -p "$AP"
aux="incus-backup.initd incus-backup.confd incus-backup.logrotate incus-backup-server.initd incus-backup-server.confd incus-backup-server.logrotate"
scripts="incus-backup.post-install incus-backup.pre-deinstall incus-backup-server.pre-install incus-backup-server.post-install incus-backup-server.post-upgrade incus-backup-server.pre-deinstall incus-backup-openrc.pre-deinstall incus-backup-server-openrc.pre-deinstall"
for f in $aux $scripts; do
	install -m 0644 "packaging/$f" "$AP/$f"
done
{
	sed -e "s/^pkgver=.*/pkgver=$VERSION/" -e '/^# Filled in by scripts\/release.sh/,$d' packaging/APKBUILD
	echo 'sha512sums="'
	(cd "$DIST" && sha512sum "$NAME.tar.gz")
	(cd "$AP" && for f in $aux; do sha512sum "$f"; done)
	echo '"'
} > "$AP/APKBUILD"
grep -q "^pkgver=$VERSION\$" "$AP/APKBUILD" || die "could not set pkgver in the aports APKBUILD"
# The same files as a release asset, for unidoc-aports to take from the release.
tar -C "$DIST/aports" --sort=name --format=gnu --owner=0 --group=0 --numeric-owner \
	--mtime="@$SOURCE_DATE_EPOCH" --mode='u+rwX,go+rX,go-w' -cf - incus-backup |
	gzip -n -9 > "$DIST/$NAME-aports.tar.gz"

# ── 7. Checksums and signatures ──────────────────────────────────────────
(cd "$DIST" && find . -maxdepth 1 -type f ! -name SHA256SUMS ! -name '*.minisig' -printf '%f\n' | LC_ALL=C sort |
	xargs sha256sum > SHA256SUMS)

KEYFILE=""
if [ -n "${INCUS_BACKUP_MINISIGN_KEY:-}" ]; then
	KEYFILE=$WORK/minisign.key
	(umask 077 && printf '%s\n' "$INCUS_BACKUP_MINISIGN_KEY" > "$KEYFILE")
elif [ -n "${MINISIGN_SECRET_KEY_FILE:-}" ]; then
	KEYFILE=$MINISIGN_SECRET_KEY_FILE
fi
if [ -n "$KEYFILE" ]; then
	command -v minisign >/dev/null || die "minisign not found (needed to sign)"
	# Like alpine-zfsboot: every asset gets its own .minisig, SHA256SUMS is not signed.
	(cd "$DIST" && for f in *; do
		[ -f "$f" ] || continue
		case $f in SHA256SUMS | *.minisig) continue ;; esac
		minisign -S -s "$KEYFILE" -x "$f.minisig" -t "incus-backup $VERSION - $f" -m "$f" </dev/null >/dev/null
	done)
	[ "$KEYFILE" = "$WORK/minisign.key" ] && rm -f "$KEYFILE"
	if [ -n "${MINISIGN_PUBLIC_KEY:-}" ]; then
		(cd "$DIST" && for f in *; do
			[ -f "$f" ] || continue
			case $f in SHA256SUMS | *.minisig) continue ;; esac
			minisign -V -q -P "$MINISIGN_PUBLIC_KEY" -m "$f" -x "$f.minisig"
		done)
		log "signatures verified against MINISIGN_PUBLIC_KEY"
	fi
	log "signed every asset (minisign)"
else
	log "not signed (no MINISIGN_SECRET_KEY_FILE / INCUS_BACKUP_MINISIGN_KEY)"
fi

(cd "$DIST" && sha256sum -c --quiet SHA256SUMS) || die "SHA256SUMS does not verify"
log "done: $DIST"
ls -l "$DIST" >&2
