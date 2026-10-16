# Real-host validation plan (Benfica, Cikarang, real Incus/ZFS, real S3, real network)

All planned core backup functionality is implemented and sandbox-tested (`go test ./...`: real
PostgreSQL, an in-process S3 test server (conformance-checked against versitygw), real mutual TLS, headless Chromium, a faithful fake of Incus). This is the single
plan for what the sandbox cannot prove. Every phase says what to run, what to record and when it passes.
Stop at the first red result and paste the output back: every `FAIL` names the assumption in the code that
is wrong.

We don't yet know where the biggest risk is. The gate checklist at the end (section 21) is therefore
ordered so that the unknowns that would invalidate the most other results, or that cannot be worked around
by configuration, are run first. That order is a plan, not a ranking of risk.

## 0. Ground rules (these keep production safe)

* **Scratch only.** Everything runs in the Incus project **`ibgate`** (`scripts/gates/common.sh` refuses to
  start if it already exists and only ever deletes that project). Never point a gate at project `custody`
  or at the Bosch VM.
* **Scratch repositories only.** Each phase uses its own throw-away repository on the server (`ibgate*`),
  with a throw-away client key (the gates create it). A repository is pinned to one key, so never reuse the
  production repository or key.
* **A scratch server database.** Use a separate PostgreSQL database (and state directory) for the gate
  server. A database created by a pre-release build before the schema consolidation is refused at start
  ("the database was migrated by a newer release (unknown migration …)"): drop and recreate it.
* Nothing here needs the production key, and nothing here writes to a production repository.
* Build once, use the same binaries everywhere: `bin/*-linux-{amd64,arm64}` (`SHA256SUMS`).
  `incus-backup-server version` and `incus-backup version` must agree. Phase 15 needs a separate, patched
  build; never install it outside the scratch setup.

## 1. Preparation (20 minutes)

1. **Server (Cikarang):** PostgreSQL reachable, a fresh database, the server initialised
   (`incus-backup-server init ...`; do not touch the production server). Create the scratch repositories:

   ```sh
   incus-backup-server repo create ibgate     --datastore /srv/incus-backup-gate        # filesystem
   incus-backup-server repo create ibgate-fs  --datastore /srv/incus-backup-gate-fs
   # a REAL S3 bucket that exists only for this test (any S3-compatible provider)
   export INCUS_BACKUP_S3_SECRET_KEY=...        # never on the command line
   incus-backup-server repo create ibgate-s3 --s3-endpoint s3.example.com --s3-region eu-central-1 \
       --s3-bucket ibgate-test --s3-prefix run1 --s3-access-key AKIA...
   incus-backup-server repo probe ibgate-s3
   ```

   `repo probe` must print `ok` for: connect + bucket, put, put if-absent, get, stat, list, delete, open
   repository. **FAIL on "put if-absent" means this service does not honour conditional writes: do not use
   it.**
2. A second server (Amsterdam) for phases 7-8 (any machine; it can be the same host with another PostgreSQL
   database, state directory and ports). Tokens: `incus-backup-server -r REPO token create NAME --scope
   backup|sync-read|sync-write`.
3. Check the admin socket exists and is private: `ls -l /var/lib/incus-backup-server/admin.sock` (mode
   `srw-rw----`).
4. Web console: `incus-backup-server user setup-token`, create an administrator at `https://SERVER:8443`.

## 2. VM backup, NBD and the frozen view (G2, G2a) - 45 minutes

```sh
IBIN=/usr/bin/incus-backup SERVER=cikarang:8007 TOKEN=ibt1.... sh scripts/gates/g-e2e.sh run      # repo ibgate
NBDPOC=/root/nbdpoc/nbdpoc-linux-amd64 sh scripts/gates/g-freeze.sh run 600
NBDPOC=/root/nbdpoc/nbdpoc-linux-amd64 QUOTA_TEST=1 sh scripts/gates/g-freeze.sh run 600
```

Pass: every `PASS`; run 2 reports `incremental`; `gate-d2` (holes) restores byte exact; the restored VM root
disk and raw volume hashes equal the source. Record: NBD throughput (the RATE column of the first run),
overlay growth in `g-freeze.sh`, what the guest saw when the overlay could not grow.

## 3. ZFS filesystem snapshots and metadata incrementality (G3, G3a, G3b) - 30 minutes

```sh
IBIN=... SERVER=cikarang:8007 TOKEN=<token for ibgate-fs> sh scripts/gates/g-fs.sh run
```

What it proves on real ZFS: the snapshot read path works; **a second run on an unchanged volume reads
`0 B`** (inode and ctime are stable across successive Incus ZFS snapshots, G3b); a run after changing one
large and one small file reads about that much; a rewrite that restores size and mtime is still read; the
restored volume equals the source file by file; `fs ls/cat/extract` return the right bytes; hard links,
modes and symlinks survive.

**If G3b fails** (the unchanged run read data): the backup is still correct (it falls back to reading) but
slow. Diagnose:

```sh
zfs list -t snapshot | grep gate-fs
# in two snapshots of the volume, compare one untouched file:
stat -c 'inode=%i ctime=%Z mtime=%Y' /<mount>/.zfs/snapshot/<snap1>/small/f1 /<mount>/.zfs/snapshot/<snap2>/small/f1
```

Paste the output. Fallback: `filesystem.trust_ctime: false`. Also time the stat walk of your largest real
container tree (`time find <rootfs> -xdev -printf ''`); if it takes minutes, the `zfs diff` accelerator
becomes worth building.

## 4. Restore (VM, volume, container, whole instance) - covered by phases 2 and 3

Additionally restore the gate VM as a copy next to itself (`restore instance gate-vm --as gate-vm-copy`),
start it, and confirm the copy's NIC has a different MAC/IP than the original (`--keep-network` keeps
them). Restore the gate container as an instance (`restore instance gate-ct --as gate-ct-copy`) and start
it.

## 5. Single files out of a VM disk with the CLI (G3c) - 15 minutes

`g-fs.sh` step 9 does it with `RECOVERY_IMAGE=images:alpine/edge/cloud`. Pass: the marker file comes out;
no `ibk-rec-*` VM or volume remains; `doctor` reports no leftovers. Then, by hand, on a gate VM whose disk
uses another filesystem (xfs, btrfs; an LVM guest is NOT supported by the stock image: say so if you need
it), repeat `incus-backup restore guest-files instance/NAME/root /etc/hostname --to DIR`. Check **during**
the run that the recovery VM has no NIC (`incus config show ibk-rec-... --expanded --project ibgate`: no
`eth0`, `profiles: []`) and that the backup host mounted nothing (`mount | grep -i ibk` is empty,
`losetup -a` unchanged). Record the time it takes.

Also check, on the recovery VM while it runs: `incus config show ibk-rec-... --expanded --project ibgate`
must show `security.guestapi: "false"`, a `cloud-init` disk device, the guest disk with `readonly: "true"`,
root with `boot.priority: "10"`. If the VM never writes a result, cloud-init did not run: look at the VM
console log. Try a guest disk that carries the SAME image as the recovery image: the recovery VM must still
boot from its own root disk and the marker file must still come out.

## 5a. One daemon, forwarding, ownership (15 minutes)

1. With the daemon running, `incus-backup-server gc`, `prune` and `verify` must work (they are forwarded and
   appear under `task list`); stop the daemon, run `gc --dry-run`, start `incus-backup-server serve` twice:
   the second must refuse ("already serving").
2. As root, run a CLI command; `ls -ln /var/lib/incus-backup-server` must show the service account as owner
   of what it created.
3. Enrol a second backup host and try `incus-backup run` of the SAME object from it: it must be refused until
   `object transfer` is run.

## 6. Corruption, quarantine and healing - 20 minutes (scratch repository only)

On the SERVER, with the `ibgate` repository after phase 2 (for S3 use `aws s3 cp`/`mc` to flip a byte):

```sh
d=/srv/incus-backup-gate/chunks; f=$(find $d -type f | head -1)
printf '\xff' | dd of="$f" bs=1 seek=40 conv=notrunc          # flip one byte of one chunk
incus-backup-server -r ibgate verify                           # reports chunk-corrupt, QUARANTINES it
incus-backup-server -r ibgate corrupt                          # lists it with the affected snapshots
incus-backup-server -r ibgate snapshot list --object ibgate/instance/gate-vm/root    # VERIFIED column: failed
ls /srv/incus-backup-gate/quarantine/                          # the bad blob is kept aside
```

On the HOST: `incus-backup run` must report **`full read`** for the affected objects, and then on the server
`corrupt` must show the chunk `healed`, `verify` must be clean and the snapshot state `ok` again. Restore the
old snapshot: it must work. The `corruption` alert must fire and resolve. Repeat with the repository on S3,
and with `sync --resync-corrupt` (phase 8).

## 7. S3 for real (G4e) - 60 minutes

Use the repository `ibgate-s3` from phase 1.

1. `sh scripts/gates/g-e2e.sh run` against `ibgate-s3` (a token for it): the same PASS list as phase 2.
2. **No object is rewritten:** with bucket versioning on, `aws s3api list-object-versions` must show exactly
   one version per key; request metrics must show no COPY requests.
3. **Interrupted upload:** start `incus-backup run` of a VM with a large first backup, `kill -9` the server
   (or pull the network) mid-upload, restart it; the next run resumes (missing chunks only), the snapshot is
   never visible half-done, `verify` is clean.
4. GC on S3: `incus-backup-server -r ibgate-s3 gc --dry-run`, then real; `verify` clean after.
5. **Ownership:** on a second server, `repo create other --s3-bucket ibgate-test --s3-prefix run1 ...` must be
   refused (`belongs to another incus-backup server`); `repo attach --adopt` is the explicit takeover. Test
   the disaster path: drop the first server's PostgreSQL database, `repo attach ibgate-s3 ... --adopt`,
   `reconcile`: snapshots, **protection and notes** must come back.
6. Credentials: `credential list` shows the access key id but never the secret; `psql -c 'select * from
   credentials'` shows only ciphertext; rotate the key at the provider and run `credential rotate`, then
   `repo probe`.
7. TLS: an endpoint with a private CA works with `--s3-ca-file`; a wrong certificate is refused.
8. **Conditional write probe on this provider:** `repo probe` must report put-if-absent `ok`, and a second
   create of the same `commit.json` must be refused (watch the server log of phase 7.3: no "overwrote"
   warning). Record the provider and its documented `If-None-Match` support.
9. **Presence set:** after a first backup, run the same backup again and record the wall time of the run (and, with `--debug`, of the
   "which chunks are missing" phase if the log shows it) with and without a server restart in between (the set is
   rebuilt from a listing on first use). Then delete one referenced chunk object **behind the server's
   back** (`aws s3 rm`), run a backup of an object that reuses it: the first commit must be refused (409,
   missing chunk), the client's retry must upload it, the snapshot must commit, `verify` must be clean.
   Record request counts per backup from the provider's metrics (HEAD, PUT, LIST) and compare with
   `docs/performance.md` §1.

## 8. Multi-target and server-to-server sync - 60 minutes

1. **Multi-target:** client policy with `targets: [cikarang, amsterdam]`, `commit_required: all`. Run, stop
   Amsterdam, run again (the report must say Cikarang committed, Amsterdam did not; the bitmap generations
   of the lagging target are kept), start Amsterdam, run: it catches up incrementally. Restore from
   Amsterdam alone with Cikarang switched off.
2. **Sync, all four pairings:** push FS->S3, push S3->FS, pull FS<-S3, pull S3<-FS (`sync add ...
   --direction push|pull`, `sync run`, `sync status`). After each: `verify` on the destination, restore from
   the destination alone.
3. Options: `--verified-only`, `--transfer-last 2`, `--rate-limit 5MB` (it must stay within ~10 % of the
   limit), `--resync-corrupt` after corrupting the destination as in phase 6 (`repaired 1`), and a corrupt
   SOURCE (it must not propagate: `skipped ... corrupt`).
4. Interrupt a sync (firewall the peer mid-transfer): the destination shows no half snapshot; the next run
   resumes.
5. Retention independence: a short policy on the source, prune there, sync again: the destination keeps its
   history.

## 9. Retention, protection, GC and long backups - 30 minutes

1. `policy set test --keep-last 3 --keep-daily 7 --timezone Atlantic/Reykjavik`, assign it, create >10
   snapshots, `prune --dry-run`, then `prune`: the numbers must be identical; with one backup per day the
   result is 10.
2. `snapshot protect`, `prune`, `snapshot forget` (refused), `snapshot unprotect`.
3. **Long backup vs GC:** start the first backup of a large VM; while it uploads run `incus-backup-server gc
   --grace 12h` on the same repository several times: the backup must finish and `verify` must be clean.
   Meanwhile `prune`: the base snapshot of the running session survives.
4. **Leases:** start a backup of an object and, while it runs, a second `run --object` of the same object:
   the second one reports "already running". `kill -9` the first client; within ~15 minutes the slot frees.
5. **Maintenance mode:** `repo mode ibgate read-only --reason gate`; `incus-backup run` must exit 0 with the
   objects `deferred` and read nothing (no NBD session in `incus monitor`); `repo mode ibgate online`; the
   next run backs them up.

## 10. Disaster recovery - 45 minutes

1. Host lost: remove the client's `state_dir` and `key_file`. Recreate the key from the **recovery key**
   (`incus-backup key import`), `init --server ... --token ...`, `list`, `restore instance ... --volumes`.
2. Same, but with the **escrow key** and WITHOUT the recovery key: `incus-backup init ... --escrow-public
   IBP1-...` first, later on a clean host `init ... --escrow-private-file FILE`: the key is fetched and
   unwrapped; a wrong private key writes nothing. `key check` against the recovery key must report the same
   domain.
3. Server lost, **with** dump and state backup (`docs/operations.md` 4.2-4.3): restore both on a fresh
   machine; hosts must reconnect without re-enrolment; users, policies, channels and credentials are there.
4. Server lost, database **without** a dump: fresh database, `migrate`, `repo attach --adopt`, `reconcile`;
   re-create policies by hand from `config export` (there is no import); every host must enrol again (record
   the exact refusal a host sees before that). Snapshots, protection and notes must come back.

## 11. Web console and onboarding on real hardware - 30 minutes

1. First administrator with the setup token; enable TOTP; sign in again with a fresh code (the sandbox never
   observed a successful fresh-code login).
2. **Hosts -> Onboarding**: create the token, run the printed command on a fresh gate host, see the host
   appear online.
3. Browse a container snapshot of `ibgate-fs` in the file browser with the gate recovery key, download one
   file and a ZIP; compare with the source (`sha256sum`). In the browser's network panel: only GETs of
   ciphertext, no request carries the key.
4. Run `incus-backup doctor` and `incus-backup status` on the host **while** a backup of a container runs
   (in a loop, every second). Pass: the backup succeeds and its snapshot restores equal to the source. This
   checks that the stale-mount cleanup every CLI command runs at start (`CleanStaleMounts`) does not unmount
   the snapshot a running backup is reading (it unmounts every entry of `<state_dir>/mnt`; found by code
   reading, not observed).

## 12. Restore tests on real ZFS (G7) - 30 minutes

```sh
incus-backup restore-test run --object instance/gate-vm/root --project ibgate --mode full
incus-backup restore-test run --object volume/gate-fs --project ibgate --mode full
incus-backup restore-test run --object instance/gate-ct/root --project ibgate --mode extract
incus-backup restore-test list
```

Pass: every test `passed`; for the VM disk `expected_sha256` equals `volume_sha256`; no `ibk-rt-*` volume
remains (`incus storage volume list --project ibgate`); the server shows the results (**Protection ->
Restore**), and a deliberately corrupted chunk (phase 6) makes a `deep` test fail and raises
`restore_test_failed`. Record: time and throughput of the NBD write and the read-back, scratch-pool space
used. Also queue one from the console (a `restore_test` job) and cancel one while it runs: the scratch volume
must be gone.

## 13. Incus upstream behaviours (dependent volumes, snapshot unmount) - 45 minutes

Two behaviours of Incus that incus-backup relies on and does not patch.

**(a) Restore of instances with dependent volumes** (upstream Incus PR 4137: a restore must take dependent
volumes from the matching snapshot, not from their current state).

1. Gate VM `gate-dep` with a custom block volume attached as a dependent disk (`dependent=true`) and a
   custom filesystem volume. Write marker A to the root disk and both volumes, `incus-backup run`; write
   marker B, `run` again.
2. Restore the first run **next to the original** (`restore instance gate-dep --project ibgate --snapshot
   <run-1 instance snapshot> --as gate-dep-copy --volumes`). Record: the volumes already exist, so they are
   not restored and the copy attaches them with marker B (documented limit, design §10); record whether
   Incus accepts a second instance on a dependent volume at all. Delete `gate-dep-copy` afterwards.
3. Delete `gate-dep` and both of its custom volumes (everything stays in project `ibgate`), then restore
   the **first** run under the original names (`restore instance gate-dep --project ibgate --snapshot
   <run-1 instance snapshot> --volumes`). Pass: root and both volumes carry marker A (the volumes come from
   the snapshot recorded for the same run, `snap:<ref>` in the instance manifest), not the newer marker B.
4. Set `user.backup.exclude=true` on the filesystem volume, write marker C, run, then delete the instance
   and its volumes again and restore that run. Record whether the instance still references the volume and
   which of its snapshots restore used (expected from the code: the newest one, because the run recorded
   none).
5. Incus alone: `incus snapshot create gate-dep s1`, write marker D, `incus snapshot restore gate-dep s1`.
   Record whether the dependent volume returned to s1 (behaviour of PR 4137) and the Incus version.
6. Consistency: the root disk and the dependent block volume of one run come from one frozen view; check
   with ordered write counters as in the PoC (no inversion).

**(b) Snapshot device removal on unmount** (upstream Incus PR 4104). `Real.SnapshotPath` mounts a ZFS
snapshot under `<state_dir>/mnt/snap-*` when Incus has not mounted it, and its release retries the unmount
20 times at 500 ms, then gives up silently (the next CLI start cleans leftovers).

1. 50 back-to-back `incus-backup run --object gate-ct` and `--object gate-fs` (container running, volume
   attached to the running container), and 20 more while `incus snapshot create/delete` runs on the same
   volume in a loop.
2. After each run: `mount | grep "$STATE/mnt/snap-"` must be empty; `zfs list -t snapshot | grep ibk-` must be
   empty (our snapshot was deleted); the client log must contain no "busy" error from deleting the
   snapshot.
3. Record any run that took noticeably longer than the others (the unmount retries do not log).

Pass: no leftover mount, no leftover `ibk-*` snapshot, no failed run. Any failure here is a bug to fix
before production, not a configuration matter.

## 14. File recovery jobs through the console (guest_files, fs_extract) - 30 minutes

On the gate host set `jobs.allow: [backup_now, restore_test, deep_verify, guest_files, fs_extract]` and
restart the daemon. In the console (**Protection -> Restore**):

1. `guest_files` from `gate-vm` (`/etc/hostname`, a directory): the job runs the recovery VM, the delivery
   downloads, the console verifies it with the recovery key, the files equal the source.
2. `fs_extract` from `gate-ct`.
3. With `jobs.allow` without `guest_files`: the job fails at once with "job type guest_files is not enabled
   on this host".
4. Stop the host daemon mid-job: the job is re-queued after its 5-minute lease and completes once the daemon
   is back; partial deliveries are gone (`deliveries/` under the datastore).

Pass: all of the above; the server's datastore never holds plaintext (`grep -r` for a marker string under
`deliveries/` finds nothing).

## 15. Certificate renewal over weeks (G8) - 3 to 4 weeks, unattended

Lifetimes are compile-time constants (`internal/pki/lifecycle.go`); there is no flag. Build a **scratch-only**
server and client with shortened values, for example `ClientValidity = ServerValidity = 21 days`,
`RenewBefore = 7 days`, `RenewGrace = 3 days`, and label the binaries clearly. On a scratch server and two
gate hosts, plus one sync peer:

1. Leave them running with hourly backups for 3-4 weeks. Pass: every host renewed once (`incus-backup cert
   status`: renewal time, no error), the server certificate was reissued without a restart, no backup
   failed for a certificate reason, `cert_expiry` warned only in the expected window.
2. Switch one host off for longer than its certificate's remaining life but less than `RenewGrace` after
   expiry: it must renew when it comes back. Switch another off beyond `RenewGrace`: it must be told to
   enrol again.
3. `ca rotate`; wait until `ca status` shows nothing pending; `ca promote`. Pass: hosts and the sync peer
   keep working without re-enrolment; a host that was off during the whole rotation is cut off after
   `promote` (or blocks it without `--force`).

Never run these binaries against the production server or hosts.

## 16. Notifications to real services (G9) - 30 minutes

1. Channels: SMTP to a real mail server (STARTTLS on 587 and implicit TLS on 465, with authentication), ntfy
   (ntfy.sh or self-hosted, with a token), Gotify, a webhook with `hmac_secret`. Press **Test** on each.
2. A rule for `backup_failed` and `host_offline`; stop the gate host's daemon for 2 h: `host_offline` fires,
   one message per channel; start it again: one "resolved" message.
3. Quiet hours and a silence: nothing is sent inside; held messages go out after.

Pass: every channel receives firing and resolved messages; the HMAC signature verifies with the documented
Python snippet; SMTP refuses a server without STARTTLS; record delivery latency.

## 17. Memory under real load (G11) - 60 minutes

1. On the backup host and the server: `scripts/membench.sh` with the default sizes; compare with
   `docs/memory.md`.
2. Real daemons: record peak RSS (`grep VmHWM /proc/$(pidof incus-backup)/status`, the same for
   `incus-backup-server`) during (a) the first backup of the largest gate VM, (b) an incremental backup of
   the largest real container tree you can copy into `ibgate`, (c) a server `verify --mode all` and a `gc`
   on the gate repository, (d) three hosts uploading at once.

Pass: block paths stay under about 450 MiB per side; the catalog path stays within ~610 B per file; the
server does not grow with repository size beyond the documented per-chunk costs.

## 18. Performance on real hardware (G11) - 60 minutes

Compare with `docs/performance.md` (sandbox numbers): NBD read throughput (phase 2), capture throughput (read bytes and
duration of the run summary), upload throughput to the server over the real WAN at the default 16 workers,
"which are missing" and commit times on the real S3 provider (phase 7.9), `verify --mode all` and `gc`
durations on the S3 repository, the filesystem scan rate of the largest container. Record each number with
the machine, the RTT and the load. Pass: no number is worse than the sandbox's for the same RTT; otherwise
record the bottleneck.

## 19. Traffic control on a real network (G10) - 30 minutes

1. A rule `upload`, `scope: host`, 5 MiB/s for the gate host; a 1 GiB first backup: the measured upload
   rate (`GET /api/v1/traffic/current`, the host's interface counters) stays within ±10 % of the limit.
2. A `global` rule shared by two gate hosts: they share it fairly.
3. A rule with a window that starts in 5 minutes: the rate changes when the window starts, without a restart.
4. A `download` rule during a restore.

Pass: within ±10 % (the sandbox's tolerance was ±20 % on a loaded machine); no backup fails because of a rule.

## 20. ACME on a real domain (G12) - 30 minutes

The server runs as an unprivileged user without capabilities, so it cannot bind ports 443 or 80. ACME with
TLS-ALPN-01 needs the domain's port 443 to reach the web listener: forward it (e.g. firewall DNAT 443 ->
8443), or use HTTP-01 with `acme_http_listen: ":8080"` and DNAT 80 -> 8080.

```yaml
web_listen: ":8443"
web:
  mode: acme
  domains: [backup-gate.example.com]
  email: ops@example.com
  acme_directory: https://acme-staging-v02.api.letsencrypt.org/directory   # staging first
```

Pass: a staging certificate, then a production one; **Configuration -> Certificates** shows it with its
expiry; a browser connects without a warning. Also check `web.mode: files` reload: replace the files and
see the new certificate within seconds, without a restart.

## 21. Gate checklist

Run in this order. Each line passes only on the criterion given; any FAIL stops the list until it is
understood. "Blocking" means production use waits for it.

| # | Gate | Phase | Pass criterion | Blocking |
|---|---|---|---|---|
| 1 | VM backup and restore through real NBD and bitmaps (G2) | 2 | every `PASS` of `g-e2e.sh`; restored hashes equal the source; run 2 incremental | yes |
| 2 | Frozen-view hold cost (G2a) | 2 | overlay growth and guest behaviour recorded; no guest I/O error before `max_freeze` | yes |
| 3 | ZFS snapshot read path and whole-container restore (G3) | 3, 4 | `g-fs.sh` all `PASS`; restored tree equal file by file | yes |
| 4 | Snapshot unmount vs Incus device removal (PR 4104) and CLI stale-mount cleanup during a backup | 13b, 11.4 | no leftover mount or snapshot in 70 runs; backups during `doctor` loops restore equal | yes |
| 5 | Dependent volumes on restore (PR 4137) | 13a | run-1 restore after deleting the instance and volumes carries marker A everywhere; limits of steps 2 and 4 recorded | yes |
| 6 | Restore tests full mode on ZFS (G7) | 12 | all `passed`, hashes equal, no `ibk-rt-*` left | yes |
| 7 | ctime/inode stability (G3b) | 3 | unchanged run reads `0 B` (else `trust_ctime: false`, recorded) | no |
| 8 | Recovery VM with a real image (G3c), CLI and console | 5, 14 | files equal source; no NIC, nothing mounted on the host, no leftovers | yes for `guest_files` |
| 9 | Real S3 provider (G4e), conditional writes, presence set | 1, 7 | probe all `ok`; one version per key; external deletion caught at commit; verify clean | yes for S3 repositories |
| 10 | Corruption and healing | 6 | quarantined, full read, healed, verify clean, alert fired and resolved | yes |
| 11 | Disaster recovery, host and server | 10 | restores from recovery key and from escrow; server rebuilt with and without a dump | yes |
| 12 | Multi-target and sync | 8 | all four pairings verify and restore standalone; options behave | yes for sync users |
| 13 | Retention, GC, leases, maintenance mode | 9 | dry run equals apply; long backup survives GC; deferred runs read nothing | yes |
| 14 | Web console on real hardware | 11 | setup, TOTP with a fresh code, onboarding, browse and download equal source | yes for console users |
| 15 | Notifications to real services (G9) | 16 | every channel receives firing and resolved | no |
| 16 | Memory under real load (G11) | 17 | within the documented bounds | no |
| 17 | Performance on real hardware (G11) | 18 | numbers recorded, none worse than the sandbox at equal RTT | no |
| 18 | Traffic control on a real network (G10) | 19 | within ±10 % | no |
| 19 | Certificates over weeks and CA rotation (G8) | 15 | renewals without failures; rotation without re-enrolment | yes before the first expiry (2 years) |
| 20 | ACME on a real domain (G12) | 20 | staging and production certificates, reload works | no |
| 21 | Packaging on a real Alpine boot (G5) | `docs/packaging.md` | services start at boot in order, respawn, run as the service user | yes |
| 22 | Passkeys and security keys with real authenticators (G13) | `docs/authentication.md` | register and sign in with a hardware key and a platform passkey in Chrome, Firefox and Safari over HTTPS on the real name; cloned-counter refusal is not testable on hardware (sandbox only) | yes for WebAuthn users |
| 23 | OIDC against a real identity provider (G13) | `docs/authentication.md` | sign in through Keycloak or Authentik: group to role and repository scope, refused unverified e-mail, break-glass `user local-login on` works | yes for SSO users |

Not implemented, deliberately deferred to after the first real-host validation: ZFS pool and SMART health
of the disks under a datastore (with alerts), and mounting a snapshot on a host (`mount`/`map` over the
NBD code). Neither blocks the gates above.

Paste back every FAIL line and every number marked "record". All green means the sandbox claims hold on
real hardware.
