# Operations

How to install, run, watch, maintain, recover and harden incus-backup. The contract is
`docs/design.md`; package details are in `docs/packaging.md`; every API route is in `docs/api.md`.
Commands below are the real ones (`--help` of each command shows all flags).

## What the pieces hold

| | Host (`incus-backup`) | Server (`incus-backup-server`) |
|---|---|---|
| encryption key / recovery key | **yes** | never |
| backup contents, file names, VM configuration | readable | encrypted, unreadable |
| host, project, policy, instance and volume **names**, sizes, times | yes | yes (control plane) |
| paths typed into a file-recovery job | yes | yes (design §19) |
| administrative authority | never | people with the `admin` role (web UI, API, local CLI) |
| infrastructure credentials (S3 keys, mail and webhook secrets) | never | sealed in the secret store (PostgreSQL holds ciphertext; the sealing key is `secrets.key`, a 0600 file) |

A compromised **server** reveals what exists and when it changes, not any data, as long as the code it
serves to browsers is honest (design §18.3). A compromised **host** can write new backups (it holds the
key and a backup-scope certificate) but cannot prune, forget or delete committed history.

## 1. First run: from install to a tested restore

### 1.1 Install

```sh
apk add incus-backup-server            # on the server (docs/packaging.md: UniDoc repository, PostgreSQL)
apk add incus-backup                   # on every Incus host
```

PostgreSQL can be on the same box or elsewhere (`docs/packaging.md`, "PostgreSQL on the same host").
Tighten `pg_hba.conf` before storing anything.

### 1.2 Initialise the server

```sh
incus-backup-server init \
  --database-url 'postgres://incus_backup:PASSWORD@127.0.0.1/incus_backup?sslmode=disable' \
  --datastore /srv/incus-backup --repository custody \
  --san backup.example.net               # every name/IP hosts use (repeatable)
rc-update add incus-backup-server && rc-service incus-backup-server start
```

`init` migrates the schema, creates the private CA and server certificate in the state directory,
creates the first repository and writes `server.yaml` (mode 0600). It refuses a state directory that is
already initialised ("state directory already initialised"). Never run it with a **new** state directory
on a working server: a new CA orphans every enrolled host.

Optional `server.yaml` keys (bootstrap only; see `examples/server.yaml`):

| key | default | meaning |
|---|---|---|
| `web_listen` | `:8443` | the web UI and `/api/v1` over HTTPS; `off` disables |
| `web.mode` | `self` | `self` (self-signed, renewed), `files` (`cert`, `key`; reloaded when they change), `acme` (`domains`, `email`, `acme_directory`, `acme_http_listen`) |
| `admin_socket` | `<state_dir>/admin.sock` | local admin API; `off` disables |
| `metrics_listen` | off | plain-HTTP `GET /metrics` (`docs/metrics.md`) |
| `metrics_unauthenticated` | false | only on a private address |

### 1.3 First administrator (setup token)

While no user exists the daemon logs a one-time setup token at start ("no user exists yet: open the web
UI and create the first administrator with this one-time setup token"). Print it again any time:

```sh
incus-backup-server user setup-token
```

Open `https://backup.example.net:8443`, enter the token, choose a name and a password (at least 12
characters). Or create the user on the command line:
`incus-backup-server user create anna --role admin --password-stdin`. Turn on two-factor authentication
under Profile right away; the setup checklist on the dashboard asks for it.

### 1.4 Onboard hosts

In the web UI, **Infrastructure -> Hosts -> Onboarding** creates a one-time token and prints the exact
command for the host. On the command line:

```sh
incus-backup-server -r custody token create benfica --scope backup    # valid 24 h (--ttl)
```

On the host:

```sh
incus-backup init --server backup.example.net:8007 --token ibt1.…     # shows the RECOVERY KEY once
incus-backup doctor
rc-update add incus-backup && rc-service incus-backup start
```

Store the recovery key in your password manager before you confirm; `incus-backup key check` re-checks a
typed copy later. Optionally add an offline escrow key now (section 4.1).

A second host of the same repository must use the **same key** (one key domain per repository): import the
key first (`incus-backup key import` from the recovery key), then `init`. A host with a different key is
refused and told how to fix it.

### 1.5 Policies and selection

```sh
incus-backup-server -r custody policy set nightly --schedule 03:00 \
    --keep-last 3 --keep-daily 14 --keep-weekly 8 --keep-monthly 12 --timezone Atlantic/Reykjavik
incus-backup-server -r custody policy assign benfica nightly --project custody
```

Or in the UI (**Configuration -> Policies**, with the exact retention preview). On the host, Incus keys
select and exclude (precedence: exclude > key on the object > key on a using instance > server assignment
> `default_policy`):

```sh
incus config set bosch user.backup.policy nightly
incus config set scratch user.backup.exclude true
incus storage volume set default biggi user.backup.policy nightly
incus-backup plan        # what is selected, which policy, and why
incus-backup run         # back up now (the first backup of a big VM: --max-freeze 3h)
```

### 1.6 Notifications

**Configuration -> Notifications**: add a channel (SMTP, webhook, Slack/Discord webhook, ntfy, Gotify),
press **Test** (the receiver's answer is shown), then add a rule (which alert types, minimum severity,
quiet hours). Set the base URL under **Settings** so messages link to the console. There is no CLI for
channels and rules; `incus-backup-server alerts list` and `alerts ack` work from the shell.
`docs/notifications.md` explains every alert.

### 1.7 Prove a restore

Restore tests run on their own (each policy's `restore_test_schedule`, default monthly), but run one now:

```sh
incus-backup restore-test run --object instance/bosch/root --project custody --mode full
incus-backup restore-test list
```

or **Protection -> Restore** in the UI (a `restore_test` job for the host). The dashboard's setup
checklist is complete when a channel was tested, a rule exists, a host and a policy exist, a restore test
passed, every admin has 2FA and the base URL is set.

## 2. The key

* `incus-backup init` creates one random master key per key domain and shows the **recovery key**
  (`IBR1-…`). The key file (`/etc/incus-backup/key.json`, 0600) is **not** the thing to back up; the
  recovery key is.
* All servers of one host share the key by default (one capture, byte-identical chunks). A host can use a
  **different key per server** (`servers.<name>.key_file`): chunks are then encrypted once per key, but the
  volume is still read only once.
* A passphrase-protected key file: `init --passphrase-file FILE` (the daemon then needs `passphrase_file`
  in `client.yaml`). To change the passphrase, recreate the file from the recovery key with `key import
  --passphrase-file` (there is no `key passwd`).
* The master key **cannot be rotated** inside a repository. If a key leaks: create a new repository, enrol
  the hosts with a new key, let the old repository age out under its retention, then destroy it.
* Losing the key, the recovery key *and* the escrow private key means every backup is permanently
  unreadable. By design.

## 3. Restore, browse, recover single files

```sh
incus-backup list                                            # what the server holds
incus-backup list instance/bosch/root --project custody     # restore points of one object
incus-backup restore instance bosch --project custody --as bosch-restored --volumes --start
incus-backup restore volume volume/biggi --project custody --to default/biggi-copy
incus-backup restore files instance/web/root --to /mnt/recover          # no Incus needed
incus-backup fs ls -l instance/web/root etc                              # browse (catalog, no download)
incus-backup fs extract instance/web/root etc/nginx --to /tmp/x          # SHA-256 verified
incus-backup restore guest-files instance/bosch/root /etc/hostname --to /tmp/x \
    --image images:alpine/edge/cloud --pool default                     # VM disk, via a recovery VM
incus-backup verify --last 1                                             # download + decrypt
```

* `restore instance --as` next to the original drops NIC MAC/IP unless `--keep-network`.
  `--volumes` restores referenced custom volumes from the snapshot taken in the same run; a volume that
  **already exists** is not restored and the copy attaches the existing one (design §10).
* **In the browser:** **Protection -> Snapshots** opens a filesystem snapshot in the file browser after
  you paste the recovery key (it stays in the page, design §18) and downloads files or a ZIP.
  **Protection -> Restore** queues host jobs: whole instances and volumes (`restore_instance`,
  `restore_volume`), files from VM disks (`guest_files`) and host-side extraction (`fs_extract`); the
  result is sealed to a key generated in your browser and checked with the recovery key.
* **Jobs the host accepts** are its own decision (`client.yaml`):

  ```yaml
  jobs:
    # disabled: true                # take no jobs from any server
    allow: [backup_now, restore_test, deep_verify, restore_instance, restore_volume, guest_files, fs_extract]
  ```

  Without `allow`, only `backup_now`, `restore_test` and `deep_verify` run. `guest_files` and `fs_extract`
  hand file contents to the holder of the job's recipient key: allow them only where that is accepted.
* Guest-file recovery needs an image with cloud-init, `tar` and drivers for the guest's filesystem. Scratch
  objects `ibk-rec-*` (recovery) and `ibk-rt-*` (restore tests) are removed on every path; `doctor` reports
  leftovers.

## 4. Disaster recovery

### 4.1 The host is gone

1. New Incus host with `incus-backup` installed.
2. Put the key back: `printf '%s\n' IBR1-… | incus-backup key import`.
3. Create a token on a surviving server: `incus-backup-server -r custody token create newhost --scope backup`.
4. `incus-backup init --server … --token …` (a wrong key is refused with a clear message).
5. `incus-backup list`, then `incus-backup restore instance NAME --project P --volumes --start`.
6. If the new host has another name, move the objects:
   `incus-backup-server -r custody object transfer P/NAME newhost`.

**With the escrow key instead of the recovery key:** skip step 2 and run `incus-backup init --server …
--token … --escrow-private-file FILE`: it fetches the wrapped key, opens it with the offline private key
and writes the key file (a wrong key writes nothing). Set up escrow beforehand with `incus-backup key
escrow generate` (keep the PRIVATE half offline) and `incus-backup key escrow upload --public IBP1-…` (or
`init --escrow-public`). Replacing a stored blob needs an administrator: `incus-backup-server escrow clear`.

### 4.2 Backing up the server itself

| What | Why | Without it |
|---|---|---|
| PostgreSQL (`pg_dump -Fc incus_backup`) | users, API tokens, audit, hosts and their certificate records, policies, assignments, schedules, sync jobs, notifications, traffic rules, settings, sealed credentials, task and job history | backups survive (datastore), but every host and sync peer must **enrol again** and every user, policy, channel and credential must be **re-created by hand** |
| `<state_dir>/secrets.key` | unseals the credentials stored in PostgreSQL | every stored S3, mail and webhook secret must be entered again (the server says so; it never silently creates a new key) |
| `<state_dir>/ca.crt`, `ca.key` (and `ca-next.*` during a rotation) | the CA every host and peer trusts | every host must enrol again with a new token |
| `<state_dir>/server.crt`, `server.key`, `sync/`, web TLS files | current identity, sync-peer credentials | re-issued from the CA; sync jobs re-enrol |
| `/etc/incus-backup/server.yaml` | bootstrap | rewrite it |
| `incus-backup-server -r REPO config export` | policies, assignments and sync jobs as readable YAML | there is **no import**: it is the reference for re-creating them by hand |
| the datastore | the backups themselves | protect it with a second server (`sync` or a second target), not with a copy of this host |

```sh
su postgres -c "pg_dump -Fc incus_backup" > /root/ibs-db-$(date +%F).dump
tar -C / -czf /root/ibs-state-$(date +%F).tgz var/lib/incus-backup-server etc/incus-backup/server.yaml
incus-backup-server -r custody config export > /root/ibs-config-custody-$(date +%F).yaml
```

The state archive holds private keys: store it encrypted and offline. None of it can decrypt backups. Run
it after every change of users, policies or credentials, and at least daily.

### 4.3 Restoring the server

**With the database dump and the state directory** (the normal case):

1. Install the same or a newer `incus-backup-server` (never older: the schema guard refuses a database a
   newer release migrated).
2. Restore the state directory and `server.yaml`; create the database and `pg_restore -d incus_backup FILE`.
3. Start the service; it migrates if the binary is newer. Hosts reconnect with their certificates.
4. If the datastore holds snapshots newer than the dump: `incus-backup-server -r REPO reconcile` (also
   scheduled daily).

**Database lost, state directory and datastore intact** (the rebuild path):

1. Create a fresh database, keep the old `server.yaml` (or rewrite it with the old `state_dir` and the new
   `database_url`) and run `incus-backup-server migrate`. Do not run `init`: it refuses an initialised state
   directory, and a new state directory would mean a new CA.
2. Attach each repository: `incus-backup-server repo attach custody --datastore /srv/incus-backup --adopt`
   (or the `--s3-…` flags; `--adopt` is needed because the new database has a new server identity, which
   `owner.json` does not name). Then `incus-backup-server -r custody reconcile`: snapshots, protection,
   notes and verification state come back.
3. Create the first administrator (`user setup-token`), then re-create policies and assignments (from the
   export), S3 credentials, notification channels, traffic rules and settings.
4. Every host and sync peer **enrols again** with a new token (hosts keep their key; host certificate
   records were in the lost database). Their objects are claimed back on the first run.

**Only the datastore survived:** install, `init` with a new state directory and a throw-away first
repository (`--repository scratch --datastore /srv/scratch`), then `repo attach … --adopt` the real
datastore and `reconcile` as above; every host enrols again. Remove the scratch repository afterwards
(section 4.4).

### 4.4 Moving or retiring a repository

1. Stop the backups to it: remove it from policies, then `incus-backup-server repo mode NAME offline`.
2. **Detach** (forget it here, keep every byte; attach it later here or elsewhere):
   `incus-backup-server repo detach NAME --clear-owner --force-hosts host1,host2`
3. Or **destroy** (delete every backup in it, then forget it):
   `incus-backup-server repo destroy NAME --confirm NAME --force-hosts host1,host2`
   On S3 every object under the repository's prefix is deleted (nothing outside it); on a filesystem the
   directory's contents are deleted (the directory stays). It runs as a task; if it fails, the repository
   stays offline with the reason.

`--force-hosts` must list every enrolled host and sync peer: their enrolment goes with the repository.

A decommissioned instance keeps its newest snapshot forever under retention. Delete the whole object:
`incus-backup-server -r custody object delete PROJECT/NAME --confirm PROJECT/NAME [--force-protected]`; the
next GC frees the chunks.

## 5. Several servers

* **Host to many:** add servers with repeated `incus-backup init`. A policy's `targets` and
  `commit_required` (`all`|`any`) say where it goes. With `all`, a generation advances only for targets
  that committed; a lagging target keeps its base bitmap and catches up next run.
* **Server to server:** `incus-backup-server -r custody sync add NAME --direction push|pull --remote
  HOST:8007 --token TOKEN --schedule 02:00`. The token is a `sync-write` (push) or `sync-read` (pull)
  token created on the peer (`token create NAME --scope sync-write`). Options (also `sync set`):
  `--verified-only`, `--resync-corrupt`, `--transfer-last N`, `--group`, `--rate-limit 20MB`. A snapshot
  the source marks as failed is never transferred. Retention is independent per repository (deletions are
  never propagated). `sync status NAME` shows lag, skipped and repaired counts.
* Each server is a complete standalone restore source.

## 6. S3 datastores

```sh
export INCUS_BACKUP_S3_SECRET_KEY=…       # or --s3-secret-file; never a flag
incus-backup-server repo create offsite --s3-endpoint s3.example.com --s3-region eu-central-1 \
    --s3-bucket backups --s3-prefix ib --s3-access-key AK
incus-backup-server repo probe offsite
```

* The bucket must exist. Creation refuses a service that does not honour conditional writes
  (`If-None-Match`) and a bucket another server owns (`repo attach --adopt` is the explicit takeover).
* `repo probe` re-runs the checks (connectivity, TLS, bucket, put/get/stat/list/delete, put-if-absent).
  The required permissions are those operations on `<prefix>/*`. `--s3-ca-file` for a private CA.
* The secret is sealed (`credential list` shows names and access key ids only; `credential rotate NAME`
  replaces it; `--s3-credential NAME` reuses one).
* Do **not** enable lifecycle expiry on the bucket. No request ever rewrites an existing object.
* Free space cannot be known on an object store: `capacity` shows usage only, and no capacity alert fires.
* The server keeps a presence set of chunk ids in memory (80 B per id, at most about 2 M ids) to answer
  "which chunks are missing" quickly; commits always ask the bucket (design §24.2).

## 7. Maintenance windows

* **Repository modes** (admin): `incus-backup-server repo mode custody read-only --reason "disk
  replacement"` lets restores and browsing continue and refuses every write; `offline` refuses every
  access (take the storage away). `incus-backup-server repo mode custody online` ends it.
* Hosts ask the mode at the start of a run and **defer** the objects (run status `deferred`, exit 0); the
  next run after the repository is online backs them up. Deferred runs neither raise nor resolve
  `backup_failed`; `backup_missed` still fires if the window outlasts the schedule. Silence it for the
  window in **Operations -> Alerts** (silences).
* **Housekeeping schedules** per repository (`maintenance show|set`): capacity `every 1h`, reconcile
  `daily`, prune `04:00`, gc `05:00`, verify `daily` (new + stale snapshots), scrub `monthly` (every chunk),
  GC grace 24 h (at least 12 h). Move them out of the backup window, for example
  `incus-backup-server -r custody maintenance set --gc 13:00`.
* **Traffic windows:** **Operations -> Traffic control** limits host uploads/downloads per host or shared,
  in weekly windows (`mon-fri 08:00-18:00`), see `docs/traffic.md`. Sync jobs have `--rate-limit`.

## 8. Tasks, server health and logs

* Every long server operation is a task with a log, progress and cancel (**Operations -> Tasks**):

  ```sh
  incus-backup-server task list --state active      # or --state failed, --kind gc
  incus-backup-server task show ID -f               # follow the log until the task ends
  incus-backup-server task cancel ID                # stops at the next check; ends as "cancelled"
  ```

  `gc`, `prune`, `verify`, `reconcile` and `sync run` on the command line wait for the result; with a
  daemon running they are handed to it and run there as tasks. Only one destructive task runs per
  repository; a second is refused with the id of the first. A cancelled GC leaves nothing half-deleted.
  Tasks that ran when the daemon restarted are marked failed: run them again. History: 30 days, at most
  2000 tasks.
* **Configuration -> Server status**: load, CPU, memory, uptime, the state directory and filesystem
  repositories' disks, PostgreSQL (version, size, connections, migrations), listeners and the clock
  difference to your browser, with a week of history.
* **Configuration -> Server status -> Logs**: the daemon's last 5000 lines (info and above), secrets
  redacted; the service log is `/var/log/incus-backup/server.log`.
* **Support report** (status page, admin): a `.tar.gz` with version, redacted configuration, status,
  repositories, recent tasks, alerts, certificates, settings and log; no passwords, keys or tokens; host
  names replaced unless you ask. Read it before you send it anywhere.
* **Update notice:** off by default; the server contacts nothing until `update_check` is turned on
  (status page or `PUT /api/v1/settings`).
* Host side: `incus-backup status`, `incus-backup doctor`, `incus-backup cert status`, and
  `/var/log/incus-backup/client.log`.

## 9. Monitoring

* The built-in alert engine (`docs/notifications.md`) covers missed and failed backups, offline hosts,
  failed maintenance, corruption, sync failure and lag, capacity, unreachable S3, certificate expiry,
  failed restore tests and login attacks.
* **Prometheus:** `GET https://SERVER:8443/api/v1/metrics` with a `readonly` API token, or the dedicated
  `metrics_listen`. Metric list and example alert rules: `docs/metrics.md`.
* OpenRC health check: `rc-service incus-backup-server healthcheck` (`docs/packaging.md`).

## 10. Certificates

Nothing expires silently and nothing needs a restart (`docs/certificates.md`).

| Certificate | Lifetime | Renewal |
|---|---|---|
| host / sync peer | 2 years | automatic when < 45 days remain (host daemon at start and daily; sync before each run); now: `incus-backup cert renew` |
| server (agent API) | 2 years | automatic (< 45 days, SAN change, CA promotion), hot-reloaded |
| private CA | 10 years | `ca rotate`, then `ca promote` (below) |
| web listener | | mode `self`: renewed automatically; `files`: replace the files (reloaded); `acme`: automatic |

* Check: `incus-backup-server ca status` (exits non-zero when something is critical), `incus-backup cert
  status`, **Configuration -> Certificates**, the `cert_expiry` alert and `incus_backup_certificate_days_left`.
* **Add a name or address** hosts use: `incus-backup-server ca sans backup.example.net 192.0.2.10`
  (reissued and served without a restart). Hosts pin the CA, so nothing changes for them.
* **A host that was off for long:** a certificate that expired less than 30 days ago still renews; after
  that, enrol again with a new token (the key stays).
* **CA rotation** (in the CA's last year, when `ca status` warns): `incus-backup-server ca rotate`; wait
  until `ca status` lists no pending identity (hosts renew at their next daily check, peers at their next
  sync run; a day or two); then `incus-backup-server ca promote`. `--force` cuts off the stragglers, which
  must enrol again. New tokens pin the new CA after promotion.

## 11. Upgrades

```sh
su postgres -c "pg_dump -Fc incus_backup" > /root/incus_backup-$(date +%F).dump   # before every upgrade
apk upgrade incus-backup-server && rc-service incus-backup-server restart
apk upgrade incus-backup && rc-service incus-backup restart                         # every host
```

* The package's post-upgrade step migrates (transactional, serialised by an advisory lock); the daemon
  migrates again on start.
* **Schema guard:** a binary refuses to run against a database that names a migration it does not know
  ("the database was migrated by a newer release"). To go back, restore the dump taken before the upgrade
  and install the old package (`docs/packaging.md`, "Rollback"). Afterwards, `reconcile` brings PostgreSQL
  in line with backups made in between.
* **Upgrade server, hosts and browsers together** when the release notes say so. 0.1.0 changes the
  sealed-delivery format to IBS2: an older host's delivery is refused by the new console, and an older
  console cannot read IBS2 (`CHANGELOG.md`).
* Development databases created before the pre-release schema consolidation are refused by the guard;
  recreate them (`CHANGELOG.md`).

## 12. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `incus-backup run`: "another backup of this object is already running" (409) | one writer per object: a second run, or a client killed less than 15 min ago | wait for the first run, or for its lease to run out (15 min) |
| run status `deferred`, nothing uploaded | the repository is `read-only` or `offline` | `repo mode NAME online`; the next run catches up |
| every run of a VM reads the disk in full | missing/inconsistent bitmap, VM restarted with in-memory bitmaps, failed verification of the base, server/local state mismatch | expected fail-closed behaviour; check `incus-backup doctor` |
| a VM backup aborts after `max_freeze` | the frozen view was held too long (large first backup, slow disk) | run once with `--max-freeze 3h`; check spool disk space |
| "this object belongs to another host" (403) | the instance moved to another host | `incus-backup-server object transfer PROJECT/NAME NEWHOST` |
| `init`: "… already belongs to key domain …" | the repository was created with another key | `incus-backup key import` the original recovery key, new token, `init` again |
| host requests fail with 401 "enrol again" | certificate expired more than 30 days ago, or issued by a CA that is no longer trusted | new token, `incus-backup init` again (the key stays) |
| `cert_expiry` warning for a host below 45 days | renewal is failing (`renew.json`) | `incus-backup cert status`, then `cert renew`; check that the server is reachable |
| job failed: "job type X is not enabled on this host" | `jobs.allow` in the host's `client.yaml` | add the type to `jobs.allow` if accepted, restart the host daemon |
| the Restore page refuses a delivery as IBS1 | the host runs a version before 0.1.0 | upgrade the host, run the job again |
| `serve`: "another incus-backup-server is already serving this database" | a second daemon on the same database | run exactly one daemon per database |
| start refused: "the database was migrated by a newer release" | an older binary, or a pre-consolidation development database | install the newer binary, restore the matching dump, or recreate the development database |
| "credential cannot be unsealed (wrong secrets.key for this database?)" | `secrets.key` lost or from another installation | restore `secrets.key` from the state backup, or `credential rotate` every credential |
| `repo create`/`attach`: "belongs to another incus-backup server" | `owner.json` names another server | if that server is gone for good: `repo attach --adopt` |
| `repo probe` fails at put-if-absent | the S3 service ignores `If-None-Match` | do not use that service |
| GC frees nothing for days | GC grace (24 h), chunks pinned by open sessions, or a failing GC | `gc --dry-run`; `task list --kind gc --state failed` |
| `corruption` alert | verify found corrupt chunks (quarantined) | the next backup heals it (full read); `corrupt` shows healed; or `sync --resync-corrupt` |
| `backup_missed` without errors | the host daemon is not running, the object lost its policy, or the repository is in maintenance | `incus-backup status`, `incus-backup plan`, `rc-service incus-backup status` |
| browser: "wrong key" | the pasted key belongs to another key domain | use the recovery key of that repository's hosts |
| full restore test fails for lack of space | the scratch volume lands in a full pool | set `recovery.pool` in `client.yaml` |
| no web UI | `web_listen: off`, firewall, or a port conflict | check `server.yaml` and the log; `ss -ltnp` |

## 13. Capacity planning

* **Datastore:** deduplicated, compressed chunks. **Storage -> Capacity** and `incus-backup-server
  capacity` show growth per day (7/30/90-day trends) and the projected full date on filesystems. Keep 20 %
  free; the capacity alert fires at 90 % or 14 days before full.
* **PostgreSQL:** small (about one row per snapshot plus history; Server status shows its size).
* **Server memory** (`docs/memory.md`): per uploading host up to 16 request bodies of 4 MiB plus HTTP/2
  windows (32 MiB per connection); per GC about 15 B per live chunk; verify up to ~90 MB at its cap; the S3
  presence set 80 B per chunk id (at most ~160 MiB). Measured in the sandbox only: plan 1 GiB plus
  ~256 MiB per concurrently uploading host until real numbers exist.
* **Host memory:** block paths stayed under ~450 MiB for client and server together in the sandbox; a
  container with about 1 M files needs ~0.6 GB during an incremental backup (catalog trees).
* **Host spool (disk):** a VM batch's encrypted dirty chunks wait in `spool_dir` between freeze and upload;
  the first backup of a VM can need up to its allocated size. `doctor` reports free space.
* **Host CPU:** 4 encode workers by default (`performance.encode_workers`); compressing compressible data is
  the main cost (about 400 MB/s of text-like plaintext on 4 cores in the sandbox).
* **Network:** 16 upload workers per target reached 150-200 MB/s up to 80 ms RTT in the sandbox; a real
  WAN and real S3 providers are unproven (`docs/performance.md`).

## 14. Security hardening checklist

- [ ] PostgreSQL: `pg_hba.conf` with `scram-sha-256`, no `trust` lines; `sslmode=verify-full` for a remote
      database; the role owns only the `incus_backup` database.
- [ ] Firewall: 8007/tcp only from Incus hosts and sync peers; 8443/tcp only from the admin network;
      `metrics_listen` only on loopback or a management network.
- [ ] The agent API runs directly (no TLS-terminating proxy or WAF; at most L4 pass-through): client
      certificates are the identity.
- [ ] The web listener has a trusted certificate (`web.mode: files` or `acme`), so people do not learn to
      click through warnings.
- [ ] Every administrator has TOTP; few administrators; others get `operator`, `restore` or `readonly`,
      restricted to their repositories where possible.
- [ ] API tokens with the lowest role, an expiry and a repository restriction; review **Users & tokens**.
- [ ] The service account's group (admin socket) has no other members.
- [ ] `jobs.allow` on each host lists only what you accept; `guest_files`/`fs_extract` only where handing
      files to the holder of a browser key is acceptable.
- [ ] Recovery key in a password manager, an offline escrow key, `key check` once a year.
- [ ] State directory, `secrets.key`, CA keys and a PostgreSQL dump backed up encrypted and offline.
- [ ] A second server (`sync` or a second target); `verify` and `scrub` schedules on.
- [ ] Notifications tested; `base_url` set; the audit log reviewed.
- [ ] Paste the recovery key only into a console you trust: whoever serves the web UI's code can read it
      (design §18.3). Otherwise use the host CLI.
- [ ] S3: a bucket and key for incus-backup only, permissions on `<prefix>/*`, no lifecycle expiry.
- [ ] Keep `update_check` off if the server must not make outbound connections.

## 15. One daemon, the admin socket, ownership

* Exactly one `incus-backup-server serve` runs per database (an advisory lock). Destructive CLI commands
  (`gc`, `prune`, `snapshot forget`, a quarantining `verify`, `credential rotate`, `sync run`, `repo
  destroy`, `object delete`) are forwarded to it over the admin socket when it runs, and executed locally
  under the same lock when it does not. Run the CLI as root or as the service account.
* The admin socket (mode 0660, state directory) gives **full administration** to every member of its group.
* Objects belong to the host that first backed them up (`object transfer` moves them).

## 16. Retention, verification, protection and healing

* Which retention applies to an object is fixed on the server (admin assignment, else the policy the object
  was first backed up with; `object retention` pins one). A host cannot shorten its own history.
* Retention decides exactly like PBS, with periods cut in the **policy's time zone** and the server's
  commit time (`docs/retention.md`). `prune --dry-run` prints exactly what the next run keeps and removes.
* `snapshot protect --object P/N SNAPSHOT` makes a snapshot immune to retention and `forget` (it survives a
  database rebuild); `snapshot notes`, `object notes`, `snapshot history`.
* GC removes unreferenced chunks older than the grace (`maintenance set --gc-grace`); a running backup of
  any length is safe (its chunks are pinned).
* **Verification:** scheduled (`verify` daily, `scrub` monthly) and on demand (`verify --mode
  all|new|stale`). A corrupt chunk is **quarantined**, its snapshots are marked failed, and the next backup
  reads everything again and heals it; `corrupt` then shows it healed.
* A backup session is a lease (15 min of silence ends it), so a multi-hour first backup is fine.

## 17. TLS, nginx and WAFs

Run the server **directly** on its agent port. Every endpoint except enrolment demands a client
certificate from the server's own CA, TLS 1.3 only. A TLS-terminating proxy would have to re-validate client
certificates and forward identity in headers, moving the trust boundary into the proxy for no gain. Use an
L4 pass-through (`stream` with `ssl_preread`) or the firewall. Enrolment (the only unauthenticated
endpoint) is rate limited per address and needs a 256-bit one-time token. If the web listener sits behind
a reverse proxy, the proxy must preserve `Host` and `Origin` (CSRF check) and is trusted with sessions.

## 18. Command reference

Server: `incus-backup-server --help` lists `alerts`, `audit`, `ca`, `capacity`, `config`, `corrupt`,
`credential`, `escrow`, `gc`, `host`, `init`, `jobs`, `maintenance`, `migrate`, `object`, `openapi`,
`policy`, `prune`, `reconcile`, `repo`, `serve`, `snapshot`, `status`, `sync`, `task`, `token`, `user`,
`verify`. `-r NAME` selects the repository when there is more than one. `alerts list`, `audit list`,
`jobs list`, `task list`, `task show` and `ca status` take `--json`. Notifications, settings, traffic
rules and API tokens are managed through the web UI or the API (`docs/api.md`).

Host: `incus-backup --help` lists `cert`, `daemon`, `doctor`, `fs`, `init`, `key`, `list`, `plan`,
`restore`, `restore-test`, `run`, `status`, `verify`.
