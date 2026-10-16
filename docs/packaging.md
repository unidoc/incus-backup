# Packaging, installation and operation on Alpine Linux

This covers the Alpine packages (built by `unidoc-aports` from the files in
`packaging/`), the release build (`scripts/release.sh`), and what a server
or client host needs around them: users, files, ports, PostgreSQL, upgrades,
rollback, and backing up the backup server itself.

Each claim below is labelled **[built]** when it was checked on the real
packages from a real `abuild` run (see "How this was validated"), or
**[design]** when it is true by construction but was not exercised.

## Packages

| Package | Contents | Depends |
|---|---|---|
| `incus-backup` | `/usr/bin/incus-backup` (client), `/var/lib/incus-backup` (0700) | nothing (static binary) |
| `incus-backup-openrc` | `/etc/init.d/incus-backup`, `/etc/conf.d/incus-backup`, `/etc/logrotate.d/incus-backup` | `logrotate`; installed automatically with `openrc` |
| `incus-backup-doc` | client man pages, README, design/format/operations docs, `client.yaml.example` | installed automatically with `docs` |
| `incus-backup-server` | `/usr/bin/incus-backup-server`, `/var/lib/incus-backup-server` (incus-backup:incus-backup 0750), the `incus-backup` system account | nothing (static binary; PostgreSQL is external) |
| `incus-backup-server-openrc` | `/etc/init.d/incus-backup-server`, its conf.d and logrotate files | `logrotate`; installed automatically with `openrc` |
| `incus-backup-server-doc` | server man pages, `server.yaml.example`, this document | installed automatically with `docs` |
| `incus-backup-bash-completion`, `incus-backup-server-bash-completion` | bash completion of each program | installed automatically with `bash-completion` |

* The client package never pulls in PostgreSQL. The server package does not
  depend on `postgresql` either: the database can be on another host. On a
  single box, install it yourself (below).
* The web UI is compiled into `incus-backup-server` (`go:embed` of
  `internal/server/webui/dist`). There is no separate web package.
* Both binaries are static (`CGO_ENABLED=0`), so they also run outside
  Alpine. They exist for `x86_64` and `aarch64`, and the packages for both
  were built with abuild **[built]**. The aarch64 build ran in an aarch64
  Alpine root under qemu-user (see "How this was validated").

## Install

```sh
# UniDoc package repository (once per host; see unidoc-aports README)
wget -P /etc/apk/keys/ https://pkg.unidoc.io/keys/unidoc-aports.rsa.pub
echo "https://pkg.unidoc.io/v$(cut -d. -f1,2 /etc/alpine-release)/main" >> /etc/apk/repositories
apk update

apk add incus-backup-server        # the repository server
apk add incus-backup               # on every Incus host (the client)
```

Each package prints its next steps when it is installed.

## Users and permissions

| Path | Owner | Mode | Notes |
|---|---|---|---|
| `incus-backup` user/group | system (`adduser -S`), shell `/sbin/nologin`, home `/var/lib/incus-backup-server`, no password | | created by the server package's pre-install; never removed |
| `/etc/incus-backup/` | root | 0755 | shared by client and server; the files carry the protection |
| `/etc/incus-backup/server.yaml` | incus-backup | 0600 | written by `incus-backup-server init`; may hold the database password; the init script re-asserts owner and mode on each start |
| `/etc/incus-backup/client.yaml`, `key.json` | root | 0600 | written by `incus-backup init` |
| `/var/lib/incus-backup-server/` | incus-backup:incus-backup | 0750 | state: `ca.key`, `server.key` and `secrets.key` (0600, re-asserted at each start), `ca.crt`, `server.crt`, sync credentials, `admin.sock` |
| `/var/lib/incus-backup-server/admin.sock` | incus-backup:incus-backup | 0660 | local admin API (used by the CLI and the web UI backend): members of the `incus-backup` group can administer the server |
| `/var/lib/incus-backup/` | root | 0700 | client state and spool |
| `/var/log/incus-backup/` | root | 0755 | `server.log` (incus-backup, 0640), `client.log` (root, 0640) |
| datastore (`--datastore`, e.g. `/srv/incus-backup`) | incus-backup | as created by the server | encrypted chunks only; `init` hands it to the service account |

The server daemon runs as `incus-backup` with `no_new_privs`, so it never
gains any capability. The client daemon runs as root (see "Client service").

## Ports

| Port | What | Who connects |
|---|---|---|
| 8007/tcp | agent API, mutual TLS (private CA of the server) | clients (`incus-backup`) and sync peers |
| 8443/tcp | web UI (HTTPS) | administrators' browsers |
| `admin.sock` (unix) | local admin API | root and the `incus-backup` group on the server host |

Open 8007 to the Incus hosts. Restrict 8443 to the admin network.

## PostgreSQL on the same host (Alpine)

```sh
apk add postgresql postgresql-client     # Alpine 3.24: resolves to postgresql18
rc-update add postgresql
rc-service postgresql start              # the first start runs initdb (auto_setup)

su postgres -c "createuser --pwprompt incus_backup"
su postgres -c "createdb --owner incus_backup incus_backup"
```

**[built]**, in an Alpine 3.24 root under proot:

* `apk add postgresql` installs `postgresql18`, and the service is called
  `postgresql`. That is the name the server's init script `use`s and
  orders itself `after`.
* The first `start` ran initdb.
* `su postgres -c createuser/createdb` created the role and the database.
  `--pwprompt` is interactive and was replaced by `ALTER ROLE ... PASSWORD`
  in the test.

The configuration is in `/etc/postgresql/` (`pg_hba.conf`,
`postgresql.conf`). After initdb it **trusts every local and every
localhost TCP connection**: any local user can connect as any role,
including `postgres`. Tighten it before you store anything. Put these lines
first and change the `trust` lines to `scram-sha-256` (or `peer` for
`local`):

```
host    incus_backup    incus_backup    127.0.0.1/32    scram-sha-256
host    incus_backup    incus_backup    ::1/128         scram-sha-256
```

then `rc-service postgresql reload`. The URL is
`postgres://incus_backup:PASSWORD@127.0.0.1/incus_backup?sslmode=disable`. A
database on another host should use `sslmode=verify-full`.

## First run of the server

```sh
incus-backup-server init \
  --database-url 'postgres://incus_backup:PASSWORD@127.0.0.1/incus_backup?sslmode=disable' \
  --datastore /srv/incus-backup \
  --san backup.example.net            # every name/IP clients use (repeatable)
rc-update add incus-backup-server
rc-service incus-backup-server start
incus-backup-server token create HOST --scope backup    # enrolment token for a client
```

`init` creates the CA and the server certificate, migrates the schema,
creates the first repository and writes `server.yaml`. Run it as root: it
hands the state, the datastore and `server.yaml` to `incus-backup`.

### Web UI

The web UI is served on port 8443 by the same daemon.

The daemon logs a one-time **setup token** at start while no user exists
(`no user exists yet: open the web UI and create the first administrator with
this one-time setup token`). Print it again with:

```sh
incus-backup-server user setup-token
```

Open `https://SERVER:8443`, enter the token and create the first
administrator, or create one on the command line with
`incus-backup-server user create NAME --role admin --password-stdin`. The web
listener uses a self-signed certificate by default (`web.mode` in
`server.yaml`, `docs/operations.md`). The rest of the first run (onboarding,
policies, notifications, a restore test) is `docs/operations.md`, section 1.

## First run of a client

```sh
incus-backup init --server backup.example.net:8007 --token TOKEN   # prints the RECOVERY KEY once: store it offline
incus-backup doctor
rc-update add incus-backup
rc-service incus-backup start
```

## Services (OpenRC)

Both services run under `supervise-daemon`, which really supervises the
process: "started" means it is still running, and a crash is respawned.

| | `incus-backup-server` | `incus-backup` |
|---|---|---|
| runs as | `incus-backup:incus-backup`, `no_new_privs`, umask 027 | root, `no_new_privs`, umask 077 |
| depends | `need net localmount`, `use postgresql logger dns`, `after postgresql zfs-mount firewall` | `need localmount net`, `use incusd logger dns`, `after incusd zfs-mount` |
| refuses to start | without `server.yaml` (says how to run `init`) | without `client.yaml` (says how to run `init`) |
| stop | SIGTERM, 60 s, then SIGKILL | SIGTERM (cancels the running backup, which cleans up), 120 s, then SIGKILL |
| respawn | every 10 s, never gives up (`respawn_max=0`) | every 30 s, never gives up |
| log | `/var/log/incus-backup/server.log` | `/var/log/incus-backup/client.log` |

* Respawning never stops by default because a PostgreSQL outage longer than a
  few attempts must not leave the server down until somebody notices. For a
  hard limit set `INCUS_BACKUP_SERVER_RESPAWN_MAX`/`_PERIOD` (client:
  `INCUS_BACKUP_RESPAWN_*`) in conf.d. The scripts deliberately override the
  generic `respawn_delay=2 respawn_max=5 respawn_period=1800` that Alpine's
  `/etc/rc.conf` sets for every service. A first version used
  `${respawn_max:-0}`, and supervise-daemon really got rc.conf's 5 **[built]**.
* `/etc/conf.d/*` hold only OpenRC settings (config path, log path, extra
  arguments, stop and respawn timing). They are not configuration of the
  programs: that is `server.yaml` (bootstrap only) plus PostgreSQL, and
  `client.yaml`.
* Logs are rotated weekly with `copytruncate`, keeping 8. Neither program
  reopens its log on a signal, and supervise-daemon holds the file open.
* `start_pre` warns about `*.apk-new` files. apk never overwrites a modified
  file under `/etc`, so a new conf.d from an upgrade lands next to it.

### Health check

`rc-service incus-backup-server healthcheck` runs `incus-backup-server status`
as the service user. That checks that the configuration loads, PostgreSQL
answers and the repository is readable. It does **not** talk to the running
daemon. Periodic checks are off by default. When `healthcheck_timer` is set
in conf.d, supervise-daemon restarts the server after a failed check. A
restart interrupts running backup sessions and cannot fix an unreachable
PostgreSQL. With several repositories, set
`INCUS_BACKUP_SERVER_HEALTH_REPOSITORY`, otherwise `status` refuses to pick
one and the check always fails.

### Client service and capabilities

The client daemon runs as root for these reasons:

* the Incus socket is full Incus administration (the daemon creates and
  deletes snapshots);
* it reads every file of container and volume snapshots (`CAP_DAC_READ_SEARCH`);
* it mounts ZFS snapshots read-only with mount(2) (`CAP_SYS_ADMIN`);
* the key is root-only.

It never executes other programs, so `no_new_privs` is set. Setting
`INCUS_BACKUP_CAPABILITIES` in `/etc/conf.d/incus-backup` narrows the
capability bounding set. This is **not** the default and **not** validated
on a real host: a missing capability makes backups fail. Test with a manual
`incus-backup run` after changing it.

## Upgrade

```sh
su postgres -c "pg_dump -Fc incus_backup" > /root/incus_backup-$(date +%F).dump   # before every upgrade
apk upgrade incus-backup-server
rc-service incus-backup-server restart
```

The server package's post-upgrade step runs `incus-backup-server migrate`
with the new binary:

* it runs as the service user;
* it uses the database in `server.yaml`, never a `DATABASE_URL` from the
  shell;
* it has a timeout (`INCUS_BACKUP_MIGRATE_TIMEOUT`, default 300 s);
* migrations are transactional and serialised by an advisory lock;
* it is skipped when the server was never initialised or has no
  `database_url`;
* it never fails the upgrade. On error it prints what to run, and the
  server also migrates on its next start.

It does **not** restart the service. Until the restart, the old daemon
keeps running against the migrated schema, so restart promptly.

Clients: `apk upgrade incus-backup && rc-service incus-backup restart`.
Upgrade the server first. Mixed client/server versions were not tested for
these packages **[design]**.

## Rollback

The UniDoc repository serves only the latest build of each package. Two ways
back:

* **Binary only, schema unchanged** (no new migration in the release notes):
  `apk add ./incus-backup-server-OLD.apk`, using a package kept from
  `/var/cache/apk` or rebuilt from the old tag. Alternatively, replace
  `/usr/bin/incus-backup-server` with the release binary of the old version
  (verify `SHA256SUMS` and its `.minisig`). Then restart.
* **Schema changed:** an older binary refuses a database that a newer
  release migrated ("the database was migrated by a newer release"). Restore
  the dump taken before the upgrade
  (`pg_restore --clean -d incus_backup FILE`), then install the old binary.
  Whatever ran in between is in the datastore, and
  `incus-backup-server reconcile` brings PostgreSQL back in line with it.

## Uninstall

`apk del incus-backup-server` stops a running service first, then removes
the program and its `-openrc`/`-doc`/`-bash-completion` subpackages. It
**never** deletes data.

The stop lives in the `-openrc` package's pre-deinstall. apk removes that
package before the main one, so a stop in the main package's script alone
found no init script any more and left the server running. The smoke test
caught this **[built]**.

These all stay:

* `/var/lib/incus-backup-server`
* `/etc/incus-backup`
* the datastore
* the PostgreSQL database
* the `incus-backup` account

`apk del incus-backup` likewise keeps `/etc/incus-backup` (including the
key) and `/var/lib/incus-backup`. Run `rc-update del incus-backup-server`
(or `incus-backup`) before removing, otherwise a dangling runlevel symlink
is left behind.

## Backing up the server itself

What a rebuilt server needs, in order of importance:

| What | Why | Without it |
|---|---|---|
| `/var/lib/incus-backup-server/secrets.key` | seals the S3 credentials and sync secrets stored in PostgreSQL | every stored credential must be entered again |
| `/var/lib/incus-backup-server/ca.key` + `ca.crt` | the private CA that every enrolled client trusts and authenticates with | every client must be enrolled again |
| `server.key`/`server.crt`, sync credentials (same directory) | current identity | can be re-issued from the CA |
| PostgreSQL dump (`pg_dump -Fc incus_backup`) | users, tokens, audit, hosts and their certificate records, policies, schedules, sync jobs, notifications, traffic rules, sealed credentials | snapshots are rebuildable from the datastore (`reconcile`); everything else is lost: every host enrols again, the rest is re-created by hand (`config export` is a readable reference, there is no import; `docs/operations.md` 4.2) |
| `/etc/incus-backup/server.yaml` | bootstrap | trivial to rewrite |
| the datastore | the backups themselves | protect with sync to a second server, not with a copy of this host |

```sh
tar -C / -czf /root/ibs-state-$(date +%F).tgz var/lib/incus-backup-server etc/incus-backup/server.yaml
su postgres -c "pg_dump -Fc incus_backup" > /root/ibs-db-$(date +%F).dump
incus-backup-server config export > /root/ibs-config-$(date +%F).yaml
```

The state archive contains private keys. Store it encrypted and offline.
The server holds no payload key, so none of this can decrypt backups.
Clients keep their recovery keys themselves.

## Building

### Release (`scripts/release.sh VERSION`, `just release VERSION`, `just dist`)

The script refuses to run unless VERSION matches `version.txt` and is
apk-valid (`1.2.3`, `1.2.3_rc1`). It produces these files in `dist/`:

* `incus-backup-VERSION.tar.gz`: the source with `vendor/` (every Go module)
  and the **built** web UI. The Alpine build needs no network and no
  node/npm. The web UI is built from `web/` (`npm run crypto`, which builds
  the web-crypto WebAssembly module and library that a fresh clone lacks,
  then `npm ci && npm run build`) when `web/package.json` exists. The
  release refuses a placeholder dist
  (no `index.html`, or a `PLACEHOLDER` file) unless
  `ALLOW_PLACEHOLDER_WEBUI=1`.
* `incus-backup[-server]-VERSION-linux-{amd64,arm64}`: built **from the
  extracted tarball** with `GOPROXY=off -mod=vendor`, so a tarball that
  misses something fails here. The script also checks each binary:
  * it is statically linked (`file`, plus `readelf`: no `PT_INTERP`, no
    `NEEDED`);
  * it has the right machine type;
  * `version` and `--version` print VERSION (amd64 natively, arm64 under
    `qemu-aarch64` when available: `QEMU_AARCH64=...`).
* `SHA256SUMS`, `RELEASE_NOTES.md` (the section of `CHANGELOG.md`, or a
  stub), and `BUILDINFO` (Go version, flags, `SOURCE_DATE_EPOCH`).
* `aports/incus-backup/`, also packed as `incus-backup-VERSION-aports.tar.gz`:
  the APKBUILD with `pkgver` and `sha512sums` filled in, plus its files (see
  "Handoff to unidoc-aports").
* `*.minisig`, when a key is given. This follows the alpine-zfsboot
  convention: every asset gets its own signature and `SHA256SUMS` is not
  signed. In CI the key content comes from the environment variable
  `INCUS_BACKUP_MINISIGN_KEY`, which the release job maps from a secret of
  the GitHub **Environment** `signing` (not a repository secret). That
  environment is restricted to `v*` tags, and a tag ruleset limits who may
  create those tags. `MINISIGN_PUBLIC_KEY` makes the script verify every
  signature before it finishes. The key is generated with
  `minisign -G -W`, locally, never in CI.

Reproducibility: the tarball is sorted, owned by 0:0, dated
`SOURCE_DATE_EPOCH` (default: the last commit) and gzipped with `-n`. The
binaries use `-trimpath -buildvcs=false -buildid=`. The APKBUILD uses exactly
these Go flags, so with the same Go version the binaries inside the apk are
byte-identical to the release binaries.

### Alpine packages

`packaging/APKBUILD` is the source of `dist/aports/incus-backup/APKBUILD`.
Notes on it:

* `source=` is the release tarball (`releases/download/v$pkgver/...`), not
  GitHub's tag archive, because the tag archive has neither `vendor/` nor
  the built web UI.
* `GOFLAGS` are set in `build()`: abuild's default `-buildmode=pie` would
  produce a dynamically loaded executable.
* `GOTOOLCHAIN=local` (Alpine's default) makes the build fail when Alpine's
  `go` is older than `go.mod`. The escape hatch is `export GOTOOLCHAIN=auto`
  plus `options="net"`, as unidoc-incus does.
* `check()` runs the 13 test packages that need neither PostgreSQL nor
  an S3 service (the list predates the in-process S3 fake). The others use embedded-postgres, which downloads a glibc
  PostgreSQL at test time: that cannot work offline or on musl, and those
  tests fail rather than skip without a database. They must be run with
  `just test` against a real PostgreSQL. CI does that on every
  push and pull request (`.github/workflows/ci.yml`, with a PostgreSQL
  service; S3 is in-process), and `release.yml` runs the whole CI workflow on the
  tagged commit before it builds anything (`docs/development.md`).
  `checkdepends="tzdata"`: the retention tests load `Europe/Berlin` from
  the system.
* **Former flake in `check()` (fixed):**
  `internal/format TestEscrowRoundTripAndEveryFailureMode` used to fail
  about 1 run in 8, and with it the whole abuild:
  * measured: 25 of 300 runs failed; the "typo" decoded to the same bytes
    in 253 of 2000 tries;
  * cause: the test's "typo" replaced the last base32 character, which
    carries 2 unused bits that the non-strict decoder ignores, so the
    "typo" often decoded to the same key;
  * it was a test bug, with no security impact (the decoded key is
    identical);
  * the test now mutates a non-final character (`len(priv)-6`).
* `pkgusers`/`pkggroups` let the package itself carry
  `incus-backup:incus-backup` ownership of `/var/lib/incus-backup-server`.

### Handoff to unidoc-aports

* Take `incus-backup-VERSION-aports.tar.gz` **from the published release**
  (it is `dist/aports/incus-backup/` of the CI run whose tarball was
  published, signed like every asset) and unpack it into
  `unidoc-aports/main/`. The `sha512sums` in it belong to exactly that
  tarball.
* Do **not** add incus-backup to unidoc-aports' `check-updates.yml` as it
  is. That workflow:
  * hashes `archive/refs/tags/v$latest.tar.gz`, which is not this
    package's `source=`;
  * only bumps `pkgver`, so it would never carry changed init, conf.d or
    logrotate files from `packaging/`.

  It needs a release-asset mode first, for example downloading
  `dist/aports` from the release.

`scripts/release-apk.sh [dist]` (`just package`) builds them with the real
abuild, signed with a throwaway key, into `dist/apk/`. On Alpine it uses the
host's abuild. Elsewhere it uses a throwaway Alpine root (minirootfs keys +
`apk-tools-static`, whose signature is checked) entered with `proot`. That
needs no root, no user namespaces and no container runtime.

`scripts/smoke-install.sh [dist]` installs the packages into a fresh Alpine
root and checks them. Its checks are listed in the script header.

## How this was validated

**[built]**: sandbox, x86_64 Debian host without root or user namespaces.
The tools were a throwaway Alpine 3.24 root under proot, abuild 3.17,
apk-tools 3.0.8 and Go 1.26.8 (Alpine's).

* **abuild.** `abuild -d -r` built all eight packages from the release
  tarball offline: client, server, and their `-openrc`, `-doc` and
  `-bash-completion` subpackages. `check()` passed: 13 test packages, about
  20 s. Several builds failed on the escrow flake described above and
  passed on retry.
* **Fixes from the first builds.** The first builds found three problems,
  all fixed:
  * the retention tests need `tzdata` (now `checkdepends`);
  * abuild's strip changed the binaries (now `!strip`);
  * Alpine's `/etc/rc.conf` respawn defaults silently replaced the
    scripts' respawn settings.
* **Signatures and install.** Every package passes `apk verify` against the
  throwaway key. `apk add` with signature checking and the maintainer
  scripts running installed them into a fresh root. `-openrc` and
  `-bash-completion` came in through `install_if`, and `-doc` only once
  `docs` was installed.
* **`scripts/smoke-install.sh`: 134 checks passed.** That run, from a fresh copy of the tree with this patch applied and driven by `just package` and `just smoke-install`, had a real
  PostgreSQL 16 and a real `apk upgrade` from 0.1.0-r0 to 0.1.0-r1. The
  checks:
  * **Package metadata:** arch, dependencies (none on PostgreSQL), each
    install script in the right package, and the ownership and modes in
    the tar headers.
  * **The installed root:** the account (system uid, nologin, no
    password), the modes on disk, and the gzipped man pages rendering
    with mandoc.
  * **Binaries:** version, `--version` and `--help`.
  * **Init scripts:** `sh -n`, `openrc-run describe`, `depend()`,
    `rc-update add`, and `logrotate -d`.
  * **post-upgrade no-ops:** without `server.yaml`, and with an empty
    `database_url`.
  * **A real `init`:** `ca.key`, `server.key` and `server.yaml` are 0600,
    and no non-certificate file is world readable.
  * **post-upgrade with a database:** it migrated the real database, run
    by hand and again by `apk upgrade`, and the upgrade left `server.yaml`
    and the state untouched.
  * **The real service:** `openrc-run start` brought up supervise-daemon
    with `--no-new-privs --user incus-backup --respawn-delay 10
    --respawn-max 0 --retry TERM/60/KILL/5 --umask 027`. The mTLS port
    answered with the server's certificate from its private CA, and the
    log went to `server.log` (0640). `openrc-run stop` ended both
    processes and closed the port.
  * **The client service:** the start is refused without `client.yaml`.
    With one, supervise-daemon runs the client as root with
    `--no-new-privs --respawn-delay 30 --respawn-max 0
    --retry TERM/120/KILL/5 --umask 077`. The daemon itself exits without
    Incus.
  * **`healthcheck()`:** it passes against the running server, and fails
    for a repository that does not exist (negative control).
  * **`apk upgrade` while the server runs:** the server keeps running, and
    post-upgrade says it is still the old version.
  * **`apk del` while the server runs:** the `-openrc` pre-deinstall stops
    it and the port closes. The binaries and every subpackage are gone; the
    state and the account are kept.
* **Release binaries.** They are statically linked (`file` and `readelf`),
  and the arm64 ones ran under qemu-aarch64 (`version`, `--version`,
  `--help`).
* **Reproducibility.** Two release runs into different directories gave
  identical `SHA256SUMS`, tarball included. The binaries inside the apk
  are byte-identical to the release binaries (same sha256).
* **Signing.** minisign signing and verification ran with a throwaway key,
  both as a key file and as key content in the environment. A wrong
  public key and a tampered file are both rejected.
* **aarch64.** This was checked on an earlier revision of this packaging:
  before the `-openrc` pre-deinstall scripts and the pg_hba warning in
  post-install. All later changes are arch-independent shell. abuild ran in
  an aarch64 Alpine 3.24 root under proot with
  `-q qemu-aarch64`, using Alpine's arm64 go. It built all eight packages
  and `check()` passed, in about 45 minutes under emulation. They pass
  `apk verify`. The server binary in the aarch64 apk is byte-identical to
  the arm64 binary that `scripts/release.sh` cross-compiles on amd64.
  This was a manual run, not scripted. The commands:
  `apk.static --usermode --arch aarch64 --root R add alpine-sdk go tzdata`
  over the aarch64 minirootfs, then
  `proot -q qemu-aarch64 -r R` with the same builder setup as
  `release-apk.sh`. unidoc-aports builds aarch64 natively on its arm64
  runner.
* **Web UI.** A stub `go:embed` package was built twice from the release
  tarball and as an apk: once with a placeholder dist (the release refuses
  it without `ALLOW_PLACEHOLDER_WEBUI=1`) and once with a real
  `npm ci && npm run build` dist. The built-UI marker is inside the
  binary, and the apk binary equals the release binary.

**[design]** (not exercised in the sandbox):

* a real boot (runlevels, `need`/`use` ordering against a real PostgreSQL
  service) and crash respawn;
* the account the service really runs as: under proot setuid is faked, so
  the `--user incus-backup` argument was checked, not the effective uid;
* the client daemon doing real work (needs Incus) and its optional
  capability restriction;
* `pg_hba.conf` and the `su postgres` commands on a real Alpine host (the
  `createuser`/`createdb` client commands were run, over TCP, against a
  real server);
* the GitHub release workflow and signing environment
  (`.github/workflows/release.yml`), and the CI workflow it calls
  (`.github/workflows/ci.yml`): both were linted with actionlint and their
  steps run locally (`docs/development.md`), but neither has run on GitHub;
* the **git-checkout path** of `scripts/release.sh`: the file list from
  `git ls-files`, `SOURCE_DATE_EPOCH` from the last commit, and the
  dirty-tree warning. The sandbox tree is not a git checkout, so every run
  above took the directory path. The reproducibility results cover that
  path only.

Under proot, file *ownership* inside the installed root is not real (proot
fakes chown). Ownership was therefore checked in the package metadata (the
tar headers apk records) and in the install scripts, not on disk.
