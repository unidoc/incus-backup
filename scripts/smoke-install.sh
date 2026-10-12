#!/usr/bin/env bash
# scripts/smoke-install.sh [DIST] - install the built Alpine packages (scripts/release-apk.sh)
# into a FRESH throwaway Alpine root and check them. Fails on the first problem.
#
# Checks:
#   1. every .apk verifies (apk verify) against the throwaway key, and apk add
#      runs with signature checking (no --allow-untrusted)
#   2. package metadata: arch, depends (no postgresql anywhere, logrotate only
#      for -openrc), install_if of -openrc/-doc/-bash-completion, the install
#      scripts are in the right package (none of the server's in the client)
#   3. ownership and modes AS RECORDED IN THE PACKAGES (tar headers):
#      /var/lib/incus-backup-server incus-backup:incus-backup 0750,
#      /var/lib/incus-backup root 0700, /etc/incus-backup root 0755, binaries 0755
#   4. installed root: `apk add` of client + server; -openrc and -doc come in
#      through install_if; the incus-backup account (system uid, nologin, home);
#      file modes on disk; man pages gzipped and readable
#   5. the binaries run from the installed root: version, --version, --help,
#      `incus-backup-server init --help`, `incus-backup init --help`
#   6. OpenRC: `sh -n` of both init scripts, openrc-run parses them
#      (`describe`, `depend` output contains need net / use postgresql),
#      `rc-update add` works
#   7. install scripts: post-upgrade is a clean no-op without server.yaml and
#      with an empty database_url; pre-deinstall + `apk del` keep the state
#   8. optional (SMOKE_PG_URL=postgres://... of an EMPTY database): a REAL
#      `incus-backup-server init` against it from the installed root, key files
#      0600 and never world readable, the real post-upgrade script migrates it,
#      then the real service: openrc-run start (supervise-daemon with the
#      intended options, port SMOKE_PORT answers, log file) and stop
#   9. optional (SMOKE_UPGRADE_TO=DIST of a newer build): a real `apk upgrade`,
#      so apk runs .post-upgrade (migrations with SMOKE_PG_URL), config and state kept
#
# Under proot, chown is faked: owners on disk are NOT meaningful, which is why
# ownership is checked in the package metadata (3) instead. Modes are real.
# No pipefail on purpose: checks are `producer | grep -q`, and with pipefail an
# early-exiting grep -q (SIGPIPE in the producer) turns a match into a failure,
# and `! producer | grep -q` into a false PASS.
set -eu
umask 022
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIST=$(cd "${1:-$ROOT/dist}" && pwd)
# shellcheck source=release-lib.sh
. "$ROOT/scripts/release-lib.sh"

APKDIR=$DIST/apk
REPO=$APKDIR/$ALPINE_ARCH
[ -f "$REPO/APKINDEX.tar.gz" ] || lib_die "$REPO/APKINDEX.tar.gz missing: run scripts/release-apk.sh first"
KEY=$(ls "$APKDIR"/keys/*.rsa.pub | head -n1)
VER=$(ls "$REPO"/incus-backup-server-[0-9]*.apk | sed 's/.*incus-backup-server-\(.*\)\.apk/\1/')
R=${SMOKE_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/incus-backup-smoke.XXXXXX")}
[ -n "${SMOKE_KEEP:-}" ] || trap 'chmod -R u+w "$R" 2>/dev/null; rm -rf "$R"' EXIT

fails=0
ok() { echo "ok    $*"; }
bad() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { # check DESCRIPTION CMD...
	local d=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi
}
in0() { lib_in0 "$R" "$@"; }
sh0() { lib_in0 "$R" /bin/sh -c "$1"; }

echo "== smoke test of $VER packages from $REPO"

# ── 2 + 3: metadata, straight from the .apk files ────────────────────────
pkginfo() { tar -xzOf "$REPO/$1-$VER.apk" .PKGINFO 2>/dev/null; }
controls() { tar -tzf "$REPO/$1-$VER.apk" 2>/dev/null | grep '^\.' | grep -v '^\.SIGN' | sort | tr '\n' ' '; }
# "MODE OWNER/GROUP PATH" for every data entry
entries() { tar -tvzf "$REPO/$1-$VER.apk" 2>/dev/null | grep -v ' \.[A-Z]' | awk '{print $1, $2, $6}'; }

for p in incus-backup incus-backup-server; do
	pkginfo $p | grep -q "^arch = $ALPINE_ARCH\$" && ok "$p: arch $ALPINE_ARCH" || bad "$p: arch"
done
for p in incus-backup-doc incus-backup-openrc incus-backup-bash-completion incus-backup-server-doc incus-backup-server-openrc incus-backup-server-bash-completion; do
	pkginfo $p | grep -q '^arch = noarch$' && ok "$p: noarch" || bad "$p: not noarch"
done
pg=""
for f in "$REPO"/*.apk; do
	n=${f##*/}
	n=${n%-"$VER".apk}
	pkginfo "$n" | grep -q '^depend = .*postgres' && pg="$pg $n"
done
[ -z "$pg" ] && ok "no package depends on PostgreSQL" || bad "depends on PostgreSQL:$pg"
for p in incus-backup incus-backup-server; do
	deps=$(pkginfo $p | sed -n 's/^depend = //p' | tr '\n' ' ')
	[ "$deps" = "/bin/sh " ] && ok "$p: depends only on /bin/sh (scripts)" || bad "$p: depends: $deps"
done
for p in incus-backup-openrc incus-backup-server-openrc; do
	deps=$(pkginfo $p | sed -n 's/^depend = //p' | tr '\n' ' ')
	# /bin/sh: abuild adds it for the pre-deinstall script.
	[ "$deps" = "logrotate /bin/sh " ] && ok "$p: depends on logrotate (+ /bin/sh for its script)" || bad "$p: depends: $deps"
done
want_if() { pkginfo "$1" | grep -qx "install_if = $2" && ok "$1: install_if $2" || bad "$1: install_if (want $2)"; }
want_if incus-backup-openrc "openrc incus-backup=$VER"
want_if incus-backup-server-openrc "openrc incus-backup-server=$VER"
want_if incus-backup-doc "docs incus-backup=$VER"
want_if incus-backup-server-doc "docs incus-backup-server=$VER"
want_if incus-backup-bash-completion "incus-backup=$VER bash-completion"
want_if incus-backup-server-bash-completion "incus-backup-server=$VER bash-completion"

c=$(controls incus-backup-server)
[ "$c" = ".PKGINFO .post-install .post-upgrade .pre-deinstall .pre-install " ] && ok "server scripts: $c" || bad "server scripts: $c"
c=$(controls incus-backup)
[ "$c" = ".PKGINFO .post-install .pre-deinstall " ] && ok "client scripts: $c" || bad "client scripts: $c"
for p in incus-backup-openrc incus-backup-server-openrc; do
	c=$(controls $p)
	[ "$c" = ".PKGINFO .pre-deinstall " ] && ok "$p scripts: $c" || bad "$p scripts: $c"
done

want_entry() { # want_entry PKG MODE OWNER PATH
	if entries "$1" | grep -qx -e "$2 $3 $4"; then ok "$1: $2 $3 $4"; else bad "$1: $4 is '$(entries "$1" | grep -e " $4\$" || echo missing)', want $2 $3"; fi
}
want_entry incus-backup-server drwxr-x--- incus-backup/incus-backup var/lib/incus-backup-server/
want_entry incus-backup-server -rwxr-xr-x root/root usr/bin/incus-backup-server
want_entry incus-backup-server drwxr-xr-x root/root etc/incus-backup/
want_entry incus-backup drwx------ root/root var/lib/incus-backup/
want_entry incus-backup drwxr-xr-x root/root etc/incus-backup/
want_entry incus-backup -rwxr-xr-x root/root usr/bin/incus-backup
want_entry incus-backup-server-openrc -rwxr-xr-x root/root etc/init.d/incus-backup-server
want_entry incus-backup-server-openrc -rw-r--r-- root/root etc/conf.d/incus-backup-server
want_entry incus-backup-server-openrc -rw-r--r-- root/root etc/logrotate.d/incus-backup-server
want_entry incus-backup-openrc -rwxr-xr-x root/root etc/init.d/incus-backup
if entries incus-backup | grep -q 'incus-backup-server'; then bad "client package contains server files"; else ok "client package contains no server files"; fi
if entries incus-backup-server-doc | grep -q 'man1/incus-backup-server-init\.1\.gz$' &&
	! entries incus-backup-server-doc | grep 'man1/.' | grep -vq 'man1/incus-backup-server[.-]'; then
	ok "server man pages (gzipped) in incus-backup-server-doc only"
else bad "server man pages misplaced"; fi
if entries incus-backup-doc | grep -q 'man1/incus-backup-init\.1\.gz$' && ! entries incus-backup-doc | grep -q 'incus-backup-server'; then
	ok "client man pages (gzipped) in incus-backup-doc only"
else bad "client man pages misplaced"; fi
# Nothing in any package is world writable (symlinks aside).
ww=$(for f in "$REPO"/*.apk; do tar -tvzf "$f" 2>/dev/null; done | awk 'substr($1,1,1) != "l" && substr($1,9,1) == "w" {print $6}')
[ -z "$ww" ] && ok "no world-writable entries" || bad "world-writable: $ww"

# ── 1 + 4: fresh root, verify, install ───────────────────────────────────
lib_log "creating the throwaway root $R"
lib_rootfs "$R" openrc logrotate bash-completion mandoc
cp "$KEY" "$R/etc/apk/keys/"
mkdir -p "$R/repo" && cp -R "$APKDIR/$ALPINE_ARCH" "$R/repo/"
for f in "$REPO"/*.apk; do
	check "apk verify ${f##*/}" in0 /sbin/apk verify "/repo/$ALPINE_ARCH/${f##*/}"
done
sh0 "apk add --quiet --repository /repo incus-backup incus-backup-server" > "$R/../smoke-apk-add.log" 2>&1 ||
	{ cat "$R/../smoke-apk-add.log"; lib_die "apk add failed"; }
grep -q 'incus-backup-server is installed but not configured' "$R/../smoke-apk-add.log" && ok "server post-install message shown" || bad "server post-install message missing"
grep -q 'not enrolled yet' "$R/../smoke-apk-add.log" && ok "client post-install message shown" || bad "client post-install message missing"
rm -f "$R/../smoke-apk-add.log"
inst=$(sh0 "apk info -q | sort" | tr '\n' ' ')
for p in incus-backup incus-backup-server incus-backup-openrc incus-backup-server-openrc incus-backup-bash-completion incus-backup-server-bash-completion; do
	case " $inst " in *" $p "*) ok "installed: $p" ;; *) bad "not installed: $p" ;; esac
done
case " $inst " in *" incus-backup-doc "*) bad "-doc installed without docs" ;; *) ok "-doc not pulled in without the docs package" ;; esac
sh0 "apk add --quiet --repository /repo docs" >/dev/null 2>&1 || bad "apk add docs"
inst=$(sh0 "apk info -q" | tr '\n' ' ')
case " $inst " in *" incus-backup-doc "*" incus-backup-server-doc "* | *" incus-backup-server-doc "*" incus-backup-doc "*) ok "docs pulls in both -doc packages (install_if)" ;; *) bad "docs did not pull in the -doc packages" ;; esac
check "apk audit clean for incus-backup*" sh0 '! apk audit --system 2>/dev/null | grep -q incus-backup'
check "man incus-backup-server-init renders" sh0 'mandoc /usr/share/man/man1/incus-backup-server-init.1.gz | grep -q "token create <host>"'

pw=$(sh0 "getent passwd incus-backup")
IFS=: read -r _ _ uid gid _ home shell <<<"$pw"
[ "$home" = /var/lib/incus-backup-server ] && ok "incus-backup home $home" || bad "incus-backup home $home"
[ "$shell" = /sbin/nologin ] && ok "incus-backup shell $shell" || bad "incus-backup shell $shell"
[ "$uid" -lt 1000 ] && ok "incus-backup is a system uid ($uid)" || bad "incus-backup uid $uid"
[ "$(sh0 "getent group incus-backup | cut -d: -f3")" = "$gid" ] && ok "primary group incus-backup" || bad "primary group"
sh0 "grep '^incus-backup:' /etc/shadow | cut -d: -f2" | grep -qx '!\|\*\|!\*' && ok "incus-backup has no password" || bad "incus-backup password field"

mode() { sh0 "stat -c %a '$1'"; }
[ "$(mode /var/lib/incus-backup-server)" = 750 ] && ok "state dir 0750 on disk" || bad "state dir mode $(mode /var/lib/incus-backup-server)"
[ "$(mode /var/lib/incus-backup)" = 700 ] && ok "client state dir 0700 on disk" || bad "client state dir mode"
[ "$(mode /etc/incus-backup)" = 755 ] && ok "/etc/incus-backup 0755" || bad "/etc/incus-backup mode"
[ "$(mode /var/log/incus-backup)" = 755 ] && ok "/var/log/incus-backup 0755" || bad "/var/log/incus-backup mode"

# ── 5: binaries ──────────────────────────────────────────────────────────
v=${VER%-r*}
[ "$(in0 /usr/bin/incus-backup version)" = "$v" ] && ok "incus-backup version = $v" || bad "incus-backup version"
[ "$(in0 /usr/bin/incus-backup-server version)" = "$v" ] && ok "incus-backup-server version = $v" || bad "incus-backup-server version"
[ "$(in0 /usr/bin/incus-backup-server --version)" = "incus-backup-server version $v" ] && ok "incus-backup-server --version" || bad "incus-backup-server --version"
check "incus-backup-server init --help" sh0 'incus-backup-server init --help | grep -q -- --database-url'
check "incus-backup init --help" sh0 'incus-backup init --help | grep -q -- --token'
check "incus-backup-server --help" in0 /usr/bin/incus-backup-server --help
check "incus-backup --help" in0 /usr/bin/incus-backup --help

# ── 6: OpenRC ────────────────────────────────────────────────────────────
for s in incus-backup incus-backup-server; do
	check "sh -n /etc/init.d/$s" in0 /bin/sh -n /etc/init.d/$s
	check "sh -n /etc/conf.d/$s" in0 /bin/sh -n /etc/conf.d/$s
done
sh0 'mkdir -p /run/openrc && touch /run/openrc/softlevel'
d=$(sh0 '/sbin/openrc-run /etc/init.d/incus-backup-server describe' 2>&1 || true)
echo "$d" | grep -q 'incus-backup repository server' && ok "openrc-run parses incus-backup-server (describe)" || bad "openrc-run describe incus-backup-server: $d"
d=$(sh0 '/sbin/openrc-run /etc/init.d/incus-backup describe' 2>&1 || true)
echo "$d" | grep -q 'incus-backup client' && ok "openrc-run parses incus-backup (describe)" || bad "openrc-run describe incus-backup: $d"
dep=$(sh0 'RC_SVCNAME=incus-backup-server; . /usr/libexec/rc/sh/functions.sh 2>/dev/null; need() { echo need $*; }; use() { echo use $*; }; after() { echo after $*; }; before() { :; }; provide() { :; }; keyword() { :; }; . /etc/init.d/incus-backup-server; depend')
echo "$dep" | grep -q '^need net localmount$' && echo "$dep" | grep -q '^use postgresql' && echo "$dep" | grep -q '^after postgresql' &&
	ok "incus-backup-server depend(): need net, use+after postgresql" || bad "depend(): $dep"
check "rc-update add incus-backup-server default" sh0 'rc-update add incus-backup-server default && [ -L /etc/runlevels/default/incus-backup-server ]'
check "rc-update add incus-backup default" sh0 'rc-update add incus-backup default && [ -L /etc/runlevels/default/incus-backup ]'
check "logrotate accepts both configs" sh0 'logrotate -d /etc/logrotate.d/incus-backup /etc/logrotate.d/incus-backup-server'

# ── 7: install scripts ───────────────────────────────────────────────────
pu=$(tar -xzOf "$REPO/incus-backup-server-$VER.apk" .post-upgrade 2>/dev/null)
printf '%s\n' "$pu" > "$R/tmp/post-upgrade" && chmod 755 "$R/tmp/post-upgrade"
rc=0; out=$(sh0 "/tmp/post-upgrade $VER 0.0.9-r0" 2>&1) || rc=$?
[ $rc = 0 ] && echo "$out" | grep -q 'not initialised' && ok "post-upgrade without server.yaml: no-op" || bad "post-upgrade without server.yaml: $out"
sh0 "printf 'listen: \":8007\"\nstate_dir: /var/lib/incus-backup-server\ndatabase_url: \"\"\n' > /etc/incus-backup/server.yaml"
rc=0; out=$(sh0 "/tmp/post-upgrade $VER 0.0.9-r0" 2>&1) || rc=$?
[ $rc = 0 ] && echo "$out" | grep -q 'no database_url' && ok "post-upgrade with empty database_url: no-op" || bad "post-upgrade with empty database_url: $out"
sh0 "rm -f /etc/incus-backup/server.yaml"
# --nodeps: only this service (never start localmount/networking/... of the throwaway root).
out=$(sh0 '/sbin/openrc-run /etc/init.d/incus-backup-server --nodeps start' 2>&1 || true)
echo "$out" | grep -q 'does not exist yet' && ok "service refuses to start without server.yaml, says why" || bad "start without config: $out"
out=$(sh0 '/sbin/openrc-run /etc/init.d/incus-backup --nodeps start' 2>&1 || true)
echo "$out" | grep -q 'does not exist yet: enrol this host first' && ok "client service refuses to start without client.yaml, says why" || bad "client start without config: $out"

# Services under proot: the starting session stays alive as long as
# supervise-daemon runs, so it goes to the background; other sessions stop it.
svc_args() { ps -eo args | grep "^supervise-daemon $1 " || true; }
want_args() { # want_args SERVICE ARG...
	local svc=$1 a want
	shift
	a=$(svc_args "$svc")
	for want in "$@"; do
		case "$a" in *"$want"*) ok "$svc: supervise-daemon $want" ;; *) bad "$svc: supervise-daemon lacks $want: $a" ;; esac
	done
}
wait_gone() { # wait_gone PID DESCRIPTION
	for _ in $(seq 1 40); do kill -0 "$1" 2>/dev/null || break; sleep 1; done
	if kill -0 "$1" 2>/dev/null; then bad "$2"; kill "$1"; else ok "supervise-daemon gone ($2)"; fi
}

# The client service: no Incus here, so the daemon itself exits and is
# respawned; what is checked is how supervise-daemon runs it.
sh0 'printf "host: smoke\n" > /etc/incus-backup/client.yaml && chmod 600 /etc/incus-backup/client.yaml'
sh0 '/sbin/openrc-run /etc/init.d/incus-backup --nodeps start' > "$R/tmp/client-start.log" 2>&1 &
cpid=$!
for _ in $(seq 1 20); do [ -n "$(svc_args incus-backup)" ] && break; sleep 1; done
want_args incus-backup "--no-new-privs" "--respawn-delay 30" "--respawn-max 0" "--retry TERM/120/KILL/5" \
	"--stdout /var/log/incus-backup/client.log" "--umask 077" "-- --config /etc/incus-backup/client.yaml daemon"
case "$(svc_args incus-backup)" in *"--user"*) bad "incus-backup: client runs with --user (must be root)" ;; *) ok "incus-backup: client runs as root (no --user)" ;; esac
[ "$(mode /var/log/incus-backup/client.log)" = 640 ] && ok "client.log 0640" || bad "client.log mode"
out=$(sh0 '/sbin/openrc-run /etc/init.d/incus-backup --nodeps stop' 2>&1 || true)
echo "$out" | grep -q 'Stopping incus-backup' && ok "client: openrc-run stop" || bad "client stop: $out"
wait_gone $cpid "client stopped"
sh0 'rm -f /etc/incus-backup/client.yaml'

server_running=""
if [ -n "${SMOKE_PG_URL:-}" ]; then
	port=${SMOKE_PORT:-18007}
	sh0 "incus-backup-server init --database-url '$SMOKE_PG_URL' --datastore /srv/incus-backup --san localhost --listen 127.0.0.1:$port" ||
		bad "incus-backup-server init against SMOKE_PG_URL"
	for f in ca.key server.key secrets.key; do
		[ -e "$R/var/lib/incus-backup-server/$f" ] || continue
		m=$(mode "/var/lib/incus-backup-server/$f")
		[ "$m" = 600 ] && ok "$f 0600" || bad "$f mode $m"
	done
	[ "$(mode /etc/incus-backup/server.yaml)" = 600 ] && ok "server.yaml 0600" || bad "server.yaml mode"
	# Certificates are public; everything else must not be world readable.
	wr=$(sh0 'find /var/lib/incus-backup-server /etc/incus-backup -type f -perm -004 ! -name "*.crt"') ||
		bad "find for world-readable files failed"
	[ -z "$wr" ] && ok "no world-readable key/secret/config file (certificates excepted)" || bad "world readable: $wr"
	n=$(sh0 'find /var/lib/incus-backup-server -type f -name "*.key" | wc -l')
	[ "$n" -ge 2 ] && ok "found $n key files to check" || bad "only $n key files after init"
	rc=0; out=$(sh0 "/tmp/post-upgrade $VER 0.0.9-r0" 2>&1) || rc=$?
	[ $rc = 0 ] && echo "$out" | grep -q 'applying database migrations' && ! echo "$out" | grep -q WARNING &&
		ok "post-upgrade migrates a real database" || bad "post-upgrade against a real database: $out"
	check "incus-backup-server status (real database)" sh0 'incus-backup-server status'

	# The real server service; it stays running for the upgrade and removal below.
	sh0 '/sbin/openrc-run /etc/init.d/incus-backup-server --nodeps start' > "$R/tmp/start.log" 2>&1 &
	spid=$!
	up=""
	for _ in $(seq 1 30); do
		if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then up=1; break; fi
		sleep 1
	done
	[ -n "$up" ] && ok "server started by openrc-run answers on 127.0.0.1:$port" || bad "server did not come up: $(cat "$R/tmp/start.log")"
	server_running=1
	want_args incus-backup-server "--no-new-privs" "--user incus-backup" "--respawn-delay 10" "--respawn-max 0" \
		"--retry TERM/60/KILL/5" "--stdout /var/log/incus-backup/server.log" "--umask 027"
	grep -q 'incus-backup-server listening' "$R/var/log/incus-backup/server.log" && ok "daemon output in /var/log/incus-backup/server.log" || bad "no daemon output in server.log"
	[ "$(mode /var/log/incus-backup/server.log)" = 640 ] && ok "server.log 0640" || bad "server.log mode"
	# healthcheck() itself (su to the service user, timeout, --config), and a negative control.
	check "rc-service healthcheck passes against the running server" sh0 '/sbin/openrc-run /etc/init.d/incus-backup-server healthcheck'
	sh0 'cp /etc/conf.d/incus-backup-server /tmp/confd.bak && echo INCUS_BACKUP_SERVER_HEALTH_REPOSITORY=nope >> /etc/conf.d/incus-backup-server'
	check "healthcheck FAILS for a repository that does not exist (negative control)" sh0 '! /sbin/openrc-run /etc/init.d/incus-backup-server healthcheck'
	sh0 'cp /tmp/confd.bak /etc/conf.d/incus-backup-server'
fi

# ── optional: a real apk upgrade to the packages in SMOKE_UPGRADE_TO (a DIST
# of a newer build), so apk itself runs .post-upgrade ──────────────────────
if [ -n "${SMOKE_UPGRADE_TO:-}" ]; then
	NEW=$(cd "$SMOKE_UPGRADE_TO" && pwd)/apk
	cp "$NEW"/keys/*.rsa.pub "$R/etc/apk/keys/"
	mkdir -p "$R/repo-new" && cp -R "$NEW/$ALPINE_ARCH" "$R/repo-new/"
	VER2=$(ls "$NEW/$ALPINE_ARCH"/incus-backup-server-[0-9]*.apk | sed 's/.*incus-backup-server-\(.*\)\.apk/\1/')
	before=$(sh0 'cat /etc/incus-backup/server.yaml 2>/dev/null; ls -l /var/lib/incus-backup-server' | sha256sum)
	rc=0
	out=$(sh0 'apk upgrade --repository /repo-new incus-backup-server incus-backup' 2>&1) || rc=$?
	[ $rc = 0 ] && ok "apk upgrade $VER -> $VER2" || bad "apk upgrade: $out"
	[ "$(sh0 'apk info -e -v incus-backup-server')" = "incus-backup-server-$VER2" ] && ok "incus-backup-server is now $VER2" || bad "version after upgrade: $(sh0 'apk info -e -v incus-backup-server')"
	if [ -n "${SMOKE_PG_URL:-}" ]; then
		echo "$out" | grep -q "$VER -> $VER2: applying database migrations" && ! echo "$out" | grep -q WARNING &&
			ok "apk ran post-upgrade: migrations applied" || bad "post-upgrade during apk upgrade: $out"
		echo "$out" | grep -q "the running server is still $VER: rc-service incus-backup-server restart" &&
			ok "post-upgrade tells that the running server is still $VER" || bad "post-upgrade did not mention the running server: $out"
		(exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && ok "upgrade did not stop the running server" || bad "the server stopped during the upgrade"
	else
		echo "$out" | grep -q 'not initialised' && ok "apk ran post-upgrade: no-op without server.yaml" || bad "post-upgrade during apk upgrade: $out"
	fi
	after=$(sh0 'cat /etc/incus-backup/server.yaml 2>/dev/null; ls -l /var/lib/incus-backup-server' | sha256sum)
	[ "$before" = "$after" ] && ok "upgrade left server.yaml and the state untouched" || bad "upgrade changed server.yaml or the state"
fi

# ── removal: pre-deinstall stops a RUNNING server, data stays ────────────
sh0 'touch /var/lib/incus-backup-server/keep-me /var/lib/incus-backup/keep-me'
rc=0; out=$(sh0 'apk del incus-backup-server incus-backup' 2>&1) || rc=$?
[ $rc = 0 ] && ok "apk del" || bad "apk del: $out"
echo "$out" | grep -q 'state in /var/lib/incus-backup-server' && ok "server pre-deinstall ran" || bad "server pre-deinstall: $out"
if [ -n "$server_running" ]; then
	echo "$out" | grep -q 'Stopping incus-backup-server' && ok "removal stopped the server through its init script" || bad "removal did not stop the server: $out"
	wait_gone $spid "pre-deinstall stopped the running server"
	if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then bad "port $port still open after apk del"; else ok "port closed after apk del"; fi
fi
check "apk del kept the server state" test -e "$R/var/lib/incus-backup-server/keep-me"
check "apk del kept the client state" test -e "$R/var/lib/incus-backup/keep-me"
check "apk del kept the incus-backup account" sh0 'getent passwd incus-backup'
check "binaries are gone" sh0 '! test -e /usr/bin/incus-backup-server && ! test -e /usr/bin/incus-backup'
# The -openrc/-doc/-bash-completion packages only depend on their install_if
# condition: apk removes them together with the main package.
left=$(sh0 'apk info -q 2>/dev/null | grep "^incus-backup" || true' | tr '\n' ' ')
[ -z "$left" ] && ok "no incus-backup* package left after removing the two main ones" || bad "still installed: $left"
check "no init script left behind" sh0 '! test -e /etc/init.d/incus-backup-server && ! test -e /etc/init.d/incus-backup'

echo
if [ $fails = 0 ]; then
	echo "smoke test PASSED ($VER, Alpine $ALPINE_VERSION $ALPINE_ARCH, root $R)"
else
	echo "smoke test FAILED: $fails check(s)"
	exit 1
fi
