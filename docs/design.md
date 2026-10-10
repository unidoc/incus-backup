# incus-backup: design

Status: DRAFT 4 (2026-10-09). This document is the contract the code is written against; when
implementation evidence changes an assumption, the document changes first.

The system described here is implemented. Its tests pass against real PostgreSQL, a real S3 service
(MinIO), real mutual TLS, a real browser (headless Chromium for the web UI and the WebAssembly
decryption) and a faithful fake of Incus (dirty bitmaps, frozen NBD views, snapshots, a disposable
recovery VM). What has not been proven against a real Incus/ZFS host, a real S3 provider, a real
mail server or a real ACME CA is marked **unproven** where it is described, listed in section 14 and
planned in `docs/real-host-validation.md`. All planned core backup functionality is implemented and
sandbox-tested.

Evidence labels used below:

- **[V]** verified in this project (Incus 7.5.1 source, or a real run on the Benfica host, see
  `incus/temp/torun-nbd-bitmap-poc.sh` and its logs).
- **[T]** proven by a test in this tree (named where it matters), not on a real host.
- **[M]** from memory of how Proxmox Backup Server (PBS) or a library works, not re-checked.
- **[O]** open question. Needs a decision, a spike or a real-host run.

Contents: 0 principle · 1 goals · 2 concepts · 3 components (3.1 control plane, 3.2 multiple targets,
3.3 sync) · 4 VM disks · 5 filesystems · 6 selection · 7 repository and cryptography (7.6 S3) ·
8 protocol · 9 failure semantics · 10 retention, GC, verify, restore (10.1 restore tests) ·
11 configuration · 12 management plane · 13 quality bar · 14 validation gates · 15 open questions ·
16 schema · 17 PBS comparison · 18 zero-knowledge web UI · 19 host jobs · 20 certificates ·
21 maintenance modes and tasks · 22 notifications · 23 traffic control · 24 performance architecture ·
25 memory model.

## 0. Principle

**Incus provides the primitives. incus-backup provides the backup system.**

incus-backup uses only what a stock Incus (7.5.1 or newer) offers through its API. **No Incus fork and
no QEMU patch is needed.** All Incus access goes through one package, `internal/client/incusx`
(`Real` for production, `fake` for tests); nothing talks QMP, nothing uses `incus export`.

| Incus primitive | What incus-backup uses it for | Where |
|---|---|---|
| NBD export of a running VM on a **frozen view** (`GET /1.0/instances/{name}/nbd`): the first connection freezes a copy-before-write view of all disks at once | consistent, agentless capture of every disk of a VM (4.1) | `InstanceNBD` |
| **Persistent dirty bitmaps** (`POST /1.0/instances/{name}/bitmaps`, per-volume `/bitmaps`) | incremental VM backups by generation (4.3) | `CreateInstanceBitmap`, `CreateBitmap`, `Bitmaps`, `DeleteBitmap` |
| Per-volume NBD (`writable` for restore, read-only for read-back) | VM disk restore, full restore tests, reading the recovery VM's output | `VolumeNBD` |
| **Snapshots** of storage volumes | consistent capture of container roots and filesystem volumes (5) | `CreateSnapshot`, `DeleteSnapshot`, `SnapshotPath` |
| **Storage volumes** | restore targets, scratch volumes of restore tests and guest-file recovery | `CreateVolume`, `VolumeExists`, `DeleteVolume`, `FilesystemPath` |
| **Instance API** | discovery (projects, instances, devices, config keys `user.backup.*`), restore of whole instances | `Info`, `Projects`, `Instances`, `Volumes`, `CreateInstance`, `Start/StopInstance` |
| A **disposable recovery VM** (an ordinary VM from an image, no network, no profile, cloud-init seed) | single files out of VM disks without mounting a guest filesystem on the host (5.4) | `CreateRecoveryVM`, `WaitStopped`, `DeleteInstance` |

Qualifications, so the statement stays exact:

- **Filesystem snapshot read path.** When Incus has not mounted a snapshot, `Real.SnapshotPath` mounts
  the ZFS snapshot dataset itself, read-only, with mount(2), under the client's own state directory (or
  reads `.zfs/snapshot`). That path is ZFS-specific; filesystem volumes on other drivers work only when
  Incus mounts the snapshot, and block-backed filesystem volumes (`zfs.block_mode`) are not supported yet
  (5.3). **[O: real ZFS, gate G3]**
- The NBD and bitmap client methods need the Incus client library and server **7.5.1 or newer** [V].
- Two upstream Incus behaviours are relied on, not patched, and **must still be verified** on the real
  host (not done yet): restore of instances with dependent volumes (upstream PR 4137) and snapshot device
  removal on unmount (upstream PR 4104). See `docs/real-host-validation.md`, phase 13. "No fork" holds
  on that basis.

## 1. Goals

1. Back up Incus **VMs, containers and custom storage volumes** to a remote repository,
   incrementally, without stopping or pausing any guest.
2. **Zero-knowledge server for payloads.** The server stores opaque encrypted data in a directory or
   an S3 bucket and never holds a payload key. (It does hold *infrastructure* credentials such as an S3
   access key or a mail password; those sit in a sealed secret store, see 7.5.) It *does* know
   operational names (hosts, projects, policies, instance and storage-object names) and job state,
   because it runs the control plane (3.1). It never sees volume contents, file names inside volumes
   (except paths an operator types into a file-recovery job, 19), VM config/secrets or chunk contents.
3. PBS-style repository: chunks addressed by hash, deduplicated, no incremental chains. Deleting any
   backup never breaks another one.
4. **Incus native** (section 0). What to back up is declared with ordinary Incus config keys (6).
5. Very easy to set up: server `init`, a setup token for the first web login, an onboarding command
   per host, set a key on an instance, done.
6. Backup **and restore**, plus `verify` and scheduled restore tests (10.1), from day one. A backup
   that was never restored is not a backup.
7. Single static Go binaries (`CGO_ENABLED=0`), Alpine/OpenRC first; the web UI is compiled into the
   server binary.

Non-goals (v1): application-consistent snapshots (no guest agent is ever installed in guests,
crash-consistent is the contract), tape, a server that can read backup contents (the web UI decrypts
in the browser, section 18), master-key rotation inside a repository (7.3).

## 2. Concepts

| Concept | Meaning |
|---|---|
| **instance** | An Incus VM or container. Pure compute topology. |
| **storage object** | A unit of data that is backed up: a VM disk, a container root filesystem, a custom volume, and an instance object holding the instance configuration. Has a stable id. |
| **backup snapshot** | One point-in-time capture of one storage object in the repository (a manifest + index + chunks). |
| **backup run** | One execution that captures a set of storage objects and records which instances reference them. |
| **repository** | One datastore (a directory or an S3 bucket/prefix) with its own key domain, managed by one server. |
| **host** | An enrolled Incus installation running the client (`incus-backup`). |
| **task** | A long server-side operation (GC, verify, retention, sync run...) with a log (21). |
| **host job** | Work the management plane asks a host to do, because only the host has the key and Incus (19). |

Rule: **the unit of backup is the storage object, never the instance.** A custom volume `biggi`
mounted in containers A and B and attached to VMs C and D is captured **once** per run. Instance
manifests only *reference* `biggi` (and record the snapshot id of `biggi` taken in the same run).
Restore recreates `biggi` once and reattaches it.

Run algorithm (per project):

```
discover instances (Incus API)
  -> read backup selection from config keys and server assignments
  -> resolve attached storage -> unique set of storage objects
  -> for each object: snapshot ONCE, capture ONCE
  -> write instance manifests that reference objects (and their snapshot ids of this run)
  -> commit run
```

## 3. Components

```
incus-backup         client / CLI / daemon, runs on the Incus host, holds the key
incus-backup-server  repository server, runs anywhere, holds no key; serves
                     - the agent API (mutual TLS, :8007) for hosts and sync peers
                     - the management API /api/v1 and the web UI (HTTPS, :8443; and the unix admin socket)
                     - optionally a Prometheus listener (metrics_listen)
ibwasm               the client's decryption code compiled to WebAssembly, run in the browser (18)
ibdemo               development only: a complete seeded stack in one process (docs/dev-demo.md)
```

Go layout follows `incus-sync`: module `github.com/unidoc/incus-backup`, `cmd/...`, `internal/...`,
cobra for the CLI, `gopkg.in/yaml.v3`, `Justfile` with `CGO_ENABLED=0` builds, `version.txt`, `docs/`,
`examples/`. The web UI (`web/`, Vue 3 + Vite) is built into `internal/server/webui/dist` and embedded
with `go:embed`; `web-crypto/` is the TypeScript library around the WebAssembly module.

### 3.0 Hosts and topology

Every Incus installation is a **host** of the server, whether it runs on bare metal (Benfica) or inside
a VM that runs its own Incus. A host enrols once, then discovers projects, instances, the storage graph
and unique storage objects itself. Policies are assigned on the server per host, project or object.
The web UI shows per-host health, counts of VMs/containers/volumes, last backup time and
protected-versus-total objects, with warnings for unprotected objects.

### 3.1 Control plane (PostgreSQL) and backup plane (datastore)

```
incus-backup-server
  |-- PostgreSQL  = control plane: hosts and certificates, policies, schedules, retention, assignments,
  |                 runs, tasks, host jobs, per-object state, capacity, GC/verify state, users, roles,
  |                 audit, notifications, traffic rules, sealed infrastructure secrets
  `-- datastore   = backup plane: encrypted chunks, indexes, encrypted manifests, commit markers,
                    protection markers, notes, verification records, escrow blob
                    (a directory, or an S3 bucket + prefix)
```

Rules:

- **PostgreSQL is a required server dependency and the only control-plane implementation.** SQLite,
  flat-file, YAML and in-memory control planes are explicitly unsupported. (The *datastore* is
  pluggable, see 7.5; only the control plane has a single implementation.) It is present in
  development and tests from the first commit.
- **Server configuration is data, not files.** Repositories, S3 settings, policies, assignments,
  schedules, retention, verification, sync jobs, notes, protection, users, notifications, traffic rules
  and settings are stored in PostgreSQL and changed through one service layer, exposed as the
  `incus-backup-server` CLI, the management API and the web UI (12). `server.yaml` holds only what is
  needed before PostgreSQL can be reached plus the listener setup (11). Secrets are sealed in the secret
  store; PostgreSQL holds ciphertext only and nothing ever returns a secret after it was stored.
- **PostgreSQL is never the source of truth for repository content.** Which chunks belong to which
  snapshot lives in the datastore's indexes and manifests, and so do protection, notes and
  verification state. Losing PostgreSQL must not lose backups.
- **PostgreSQL can be rebuilt** from the datastore (`reconcile`: snapshots, times, sizes, protection,
  notes, verification state) plus hosts re-announcing their name -> group-id mapping [T]. What is
  **lost** with PostgreSQL and cannot be rebuilt from the datastore: users, API tokens, the audit log,
  host enrolments and their certificate records (every host and sync peer must enrol again), policies,
  assignments, schedules, sync jobs, notification channels/rules, traffic rules, settings, sealed
  credentials, task/job history. `incus-backup-server config export` writes policies, assignments and
  sync jobs as YAML; there is **no import** (re-creation is manual). Hence: back up PostgreSQL itself
  (`docs/operations.md`, "Backing up the server").
- Conventions copied from `gorisk` and `isms`: `pgx/v5`, timestamp-named SQL files in `migrations/`
  (`YYYYMMDDHHMMSS_name.sql`), a `schema_migrations` table, migrations embedded in the binary, `serve`
  migrates on start and `migrate` runs it standalone, **one migration per released version** (the
  pre-release history is consolidated into `20261008000000_initial_schema.sql`), `devenv/` for a local
  dev database. The test helper **fails instead of skipping** when no database is available, and starts
  `embedded-postgres` itself when `INCUS_BACKUP_TEST_DATABASE_URL` is unset.
- **Schema guard.** After migrating, the server refuses to run when `schema_migrations` names a
  migration it does not know (`db.checkNotNewer`): an older binary never runs against a schema a newer
  release changed [T].

**Client pull model.** The host asks the server rather than being pushed to:

```
who am I, is my repository online?  -> GET /v1/whoami (identity from the client certificate, repository mode)
what should I back up?              -> GET /v1/desired-state (policies, assignments)
last committed snapshot?            -> per-object snapshot lists
run, upload, commit                 -> sessions (8)
report status and inventory         -> POST /v1/runs, PUT /v1/inventory
any work for me?                    -> GET /v1/jobs/next (long poll, 19)
```

**Where "what to back up" is decided** (decided, see 15): `user.backup.exclude` > `user.backup.policy`
on the object > the key on an instance that uses it > server assignment (object > project > host) >
`default_policy`. `incus-backup plan` and `doctor` print the effective result and which rule won.

**What the server can make a host do.** The host validates whatever the server sends: it only ever
backs up objects it can see in its own Incus. Beyond scheduled backups, the server can only *ask* a
host to do work through **host jobs** (19), and **the host decides what it accepts** with `jobs.allow`
in `client.yaml`. Without that key a host accepts only `backup_now`, `restore_test` and `deep_verify`,
which create no Incus objects (a full restore test creates and always deletes its own scratch volume)
and hand out no data. `restore_instance` and `restore_volume` (create objects, never overwrite) and
`guest_files`/`fs_extract` (hand file contents to the holder of a recipient key) must be allowed
explicitly; `jobs.disabled: true` refuses every job. No backup-scope credential and no job can delete
anything.

### 3.2 Multiple targets (client -> several independent servers)

A host can back up the same objects to several independent servers ("targets"). Each target is a
**complete standalone repository** with its own PostgreSQL and datastore. If every other target is
lost, any one target alone is enough to restore (given the recovery key).

```
Benfica: capture once -> chunk/hash/compress once -> encrypt (once per key domain)
            |-> Cikarang   (PostgreSQL + datastore)
            `-> Amsterdam  (PostgreSQL + datastore)
```

Rules:

- **One capture per object per run.** One NBD freeze, one dirty extent map, one read of the volume,
  whatever the number of targets.
- Chunk -> hash -> compress happen once on plaintext. Encryption and the keyed chunk id are computed
  **per key domain**. Targets sharing a key domain (the default) receive byte-identical blobs. Targets in
  different key domains cost an extra encryption pass, never a second read.
- A policy lists targets and a commit requirement (`targets`, `commit_required: all|any`, default all).
- Each server applies its **own** retention for the policy the object is governed by. Retention is
  independent per target.
- The client keeps, per (object, target), the id of the last snapshot that target **committed**
  (confirmed by the target, never assumed).

#### 3.2.1 Generations with several targets

Bitmaps are named `ib-<snapshot-id>`. Let `Lt` be the last committed snapshot id of target `t`. The
client retains one bitmap per distinct `Lt` among the required targets, plus the current run's.

```
run N+1:
 1. create ib-<N+1> (before the freeze)
 2. open NBD (freeze)
 3. base = the OLDEST committed snapshot among required targets
    (Lmin); extent map E = extents of ib-<Lmin>
 4. re-read every chunk position touched by E from the frozen view;
    new index = index(Lmin) patched with those chunks
 5. for each target: ask which chunk ids it lacks, upload only those,
    upload index + manifest, commit
 6. for each target that acked: Lt := N+1
 7. delete ib-X for every X no required target still has as its Lt
```

Why this is safe: positions outside E have not changed since Lmin, so index(Lmin) is correct there for
every target; every chunk any target lacks sits at a position inside E. A target that was down simply
keeps its older `Lt`; the next run re-reads a larger E and sends it what it lacks. No full read is
needed unless a target's bitmap was lost or is older than the retention of retained bitmaps (then: full
read, still deduplicated).

`commit_required: all` -> the generation of a lagging target is never discarded. `any` -> succeeds when
one target commits; targets that fall too far behind (their bitmap dropped, `max_target_lag`) do a full
read when they return.

### 3.3 Server-to-server synchronization (push and pull)

An `incus-backup-server` can synchronize committed backups to or from another `incus-backup-server`,
**without the Incus host and without any payload key**. Repositories hold encrypted chunks, indexes and
manifests, so replication copies opaque repository objects.

```
Benfica -> Cikarang --push--> Amsterdam            (initiator: Cikarang)
Benfica -> Cikarang <--pull-- Amsterdam            (initiator: Amsterdam)
```

- **Both directions are user-facing and the initiator really initiates.** One shared sync engine
  implements both.
- Repository-aware, not filesystem replication: `list committed snapshots -> diff against destination
  -> ask which chunk ids are missing -> transfer only those blobs -> transfer index/manifest -> verify ->
  atomically commit`. A partially synchronized snapshot is never visible as a restore point.
- Resumable and idempotent: state is re-derived by comparing snapshot lists and chunk ids.
- Own restricted scopes (`sync-read` on the source, `sync-write` on the destination). A sync peer never
  receives admin authority. Its credential is a client certificate of the peer's CA, renewed like a
  host's (20).
- **Retention is independent.** Replication does not propagate deletions (no `remove-vanished` mode).
- PostgreSQL is not replicated. Datastores are authoritative for payload.
- GC honours in-flight syncs: chunks received by a sync are pinned (a pin-only session per repository,
  renewed by activity) until their snapshots commit, in addition to the grace period.
- **Operational controls** (per job, in PostgreSQL): `verified-only`, `resync-corrupt` (repair
  destination snapshots that failed verification from a source copy that has not failed itself; the
  destination re-verifies what it repaired), `transfer-last N`, a group filter (group ids), and a rate
  limit on blob traffic.
- **Corrupt content is never propagated as healthy.** A snapshot the source marks as failed is not
  transferred, whatever the options; every blob is also checked keylessly (SHA-256 trailer) in flight.
- Sync state, runs, bytes, chunks reused/transferred, skipped and repaired counts and replication lag
  live in PostgreSQL and are exposed through `sync status`, the API and the UI. A sync run is a task (21).

```
incus-backup-server -r custody sync add amsterdam --remote backup2.example.net:8007 \
    --token <sync-write token from amsterdam> --direction push --schedule "02:00"
incus-backup-server -r custody sync run amsterdam
incus-backup-server -r custody sync status amsterdam
```

## 4. Capturing a VM disk (block storage object)

Proven on the Benfica host with Incus 7.5.1 on ZFS, 12 sessions, guest writing continuously. [V]

### 4.1 Mechanism

Incus exposes all disks of a running VM over one NBD session (`GET /1.0/instances/{name}/nbd`). The
first connection freezes a copy-before-write view of **all disks atomically** (one QMP transaction).
Dirty bitmaps (`POST /1.0/instances/{name}/bitmaps`, per-volume `/bitmaps`) record writes. The tool
never talks QMP directly.

### 4.2 Facts the design depends on

- Freeze point = first NBD connection. All connections of a backup must stay open together. When the
  last one closes the session ends and the next connection freezes again. [V]
- Frozen view is stable: two full reads differ in 0 blocks while the guest writes. [V]
- Multi-disk freeze is consistent (ordered-write counters never inverted). [V]
- Exported bitmaps are **live**: a bitmap keeps accumulating writes after the freeze (measured 59 MB ->
  102 MB in 20 s on a busy disk). Harmless: the extent list read from the frozen view is a superset. [V]
- `base:allocation` and `qemu:dirty-bitmap:<name>` are served via NBD structured replies and
  `BLOCK_STATUS`. [V]
- **Persistent bitmaps exist only on the root disk** (Incus creates the qcow2 metadata layer only for
  root). Extra raw ZFS volumes only get in-memory bitmaps, which vanish when the VM stops. [V]
- The instance-level bitmap call covers root plus disks with `dependent=true` only, in one transaction;
  when it fails (a dependent raw volume cannot hold a persistent bitmap) the client falls back to one
  call per disk. Other volumes need the per-volume call. [V, T]
- `bitmap list/show/delete` need a running VM. [V]
- Persistent root bitmap **content** survives a clean restart (`SUBSET_OK`, 0 missing). The in-memory
  bitmap on a raw ZFS volume was gone, as predicted. [V]

### 4.3 Generation protocol (fail closed)

(Single-target view. With several targets the base is the oldest committed snapshot among required
targets, see 3.2.1.)

Bitmap name is `ib-<snapshot-id>` where `snapshot-id` identifies the last **committed** backup snapshot
of that object.

```
state: B1 = ib-<last committed id>
1. create B2 = ib-<new run id> on every disk, BEFORE opening NBD
2. open NBD (all exports of the VM, one connection each, held open)
3. walk B1 ONCE -> extent map E (fixed; never walk B1 again)
4. read ONLY the 4 MiB chunks touched by E from the frozen view
   (all other chunk hashes are copied from the previous index)
5. encrypt, upload, commit snapshot on the server
6. on commit ack: delete B1; B2 is now current
7. on any failure before 6: delete nothing; B1 stays authoritative
```

Order rule: B2 must exist before the freeze, otherwise writes between "freeze" and "B2 created" are
lost. Negative control proved the checker catches this. [V]

Fall back to a **full read** of that disk (still deduplicated: only unknown chunks are uploaded) when
any of these hold:

- `ib-<last id>` missing, `inconsistent`, or not recording;
- the server's latest snapshot id for the object differs from local state (server is the source of
  truth, local state is a cache);
- the previous snapshot failed verification on any target (it is never a base);
- the VM's metadata image was dropped (copy/restore changes identity);
- the disk has only an in-memory bitmap and the VM was restarted since;
- the disk is a shared block volume attached to more than one VM (a per-VM bitmap cannot see other VMs'
  writes);
- offline changes were made to the volume.

Tracking is an optimisation. Correctness never depends on it.

### 4.4 Chunks

Fixed 4 MiB chunks for block objects [M: PBS does the same]. Bitmap granularity is 64 KiB, so one
dirty extent touches whole 4 MiB chunks that get re-hashed. All-zero chunks and `base:allocation` holes
are recorded as "zero" in the index and never uploaded.

### 4.5 Known cost

Measured volume read in the synthetic worst-case PoC (heavy random writer): 2.35 % of 13 GiB per run.
Real VMs will be lower. **NBD throughput through the Incus API proxy was never measured.** [O] The NBD
client pipelines up to 16 requests (24); whether that matches QEMU behind the Incus proxy is measured on
the real host (gate G2).

### 4.6 Frozen-view hold time (production-safety risk, OPEN)

Incus implements the frozen view as a `blockdev-backup sync=none` job whose target is a qcow2 overlay
created in the VM's config-volume directory (`prepareEphemeralSnapshot`, `driver_qemu.go:8899`), sized
like the disk and unlinked immediately (accessed by fd). While the NBD session is open, every guest
overwrite of a not-yet-copied block first copies the old block into that overlay, so the overlay grows
with guest write volume for as long as the session is held. [V: read from source]

Not known: which volume's free space bounds it, and what QEMU does to **guest writes** when the overlay
cannot grow (no `on-cbw-error` or similar is set by Incus [V: grep found no such option]). All PoC
sessions lasted seconds. A real backup over a WAN could hold the session for hours. [O]

Design consequences (fail closed, applied regardless of the test outcome):

- The client **never uploads while holding the freeze longer than needed**: it reads dirty chunks from
  the frozen view as fast as the local read allows, hashes/compresses/encrypts into a local disk spool,
  and releases the NBD session as soon as all required bytes are read. Upload continues afterwards from
  the spool.
- `max_freeze` (default 15 m, `run --max-freeze` overrides it) per run. The FIRST backup of a large VM
  can legitimately need hours: run it once with `--max-freeze 3h`. The spool is per VM batch and deleted
  as soon as the batch is uploaded. Exceeding the limit aborts the run, closes the session (which
  cancels the job) and keeps B1. Never wait for a slow network while frozen.
- `doctor` warns if the config volume has less free space than a configurable fraction of the disk size.
- **Gate G2a (scratch VM on Benfica):** hold a session open with a slow reader against a busy writer,
  record overlay growth, record what the guest observes when the limit is reached. Results go here.

## 5. Capturing a filesystem (container root, filesystem volume)

An Incus snapshot `ibk-<snapshot id>` is taken (and always deleted afterwards) and mounted read-only the
way Incus mounts it. Two formats exist; the **catalog format ("fs2") is the default**, the archive
format ("tar") stays as the correctness fallback (`filesystem.format: tar`).

### 5.1 Catalog format (fs2)

One snapshot is ONE dynamic chunk index with two byte ranges:

```
[ payload: the bytes of every regular file, concatenated in walk order ][ catalog ]
```

The **catalog** (`internal/fscat`, magic `IBCT`) records for every entry its mode, uid/gid, nanosecond
mtime **and ctime**, inode, size, symlink target, device numbers, **all extended attributes (so POSIX
ACLs and file capabilities)** and, for files, the payload offset and a **SHA-256 of the content**. It is
chunked and encrypted like data, so the server learns no file name. The manifest records the layout
(`fs.format`, `fs.payload_size`, `fs.catalog_size`, `fs.mode`, `fs.last_full`, `fs.base`).

**Runs.** The payload is cut into *runs* that start and end on chunk boundaries: one run per large file
(>= 256 KiB, chunked with content-defined chunking), and *packs* of small files in walk order (a pack
ends at 2 MiB, or after 512 KiB at a file whose path hashes to 0 mod 8, so adding or removing a file
disturbs only the packs up to the next name-defined boundary).

**Change detection.** The next backup walks the live tree and compares each file with the previous
catalog: kind, mode, uid, gid, size, mtime (ns), xattrs and, by default, **ctime (ns) and inode**. A run
whose files are all unchanged and whose old bytes are contiguous maps onto whole chunks of the previous
snapshot; those chunk ids are written into the new index and **the file contents are never read**.

Why ctime and inode, unlike PBS (`pbs-client/src/pxar/create.rs:is_reusable_entry`): successive
snapshots of one ZFS dataset keep both for an untouched file, and ctime cannot be set from user space,
so a rewrite that restores size and mtime is still detected (`fscat` has a test that demonstrates it
with and without the comparison). `filesystem.trust_ctime: false` disables it for sources where they are
not stable. [O: stability across Incus ZFS snapshots, gate G3b]

**Trust and fallbacks.** Metadata equality is a very strong hint, not proof. The run reads every file
again (full-data mode, still deduplicated) when: there is no usable previous catalog snapshot; the
previous snapshot of ANY target failed verification; a target lacks the previous snapshot; the last
full-data run is older than `filesystem.full_every` (default 7 days); a reused chunk turns out to be
missing on a target at upload (the run is repeated in full-data mode automatically); or the previous
snapshot is not a catalog snapshot. Several key domains share one scan.

**Zero-length and sparse files** are handled (holes become all-zero chunks without blobs). Hard links,
symlinks, devices and fifos are recorded; sockets are skipped.

**Restore and browsing** (`restore`, `fs ls|find|stat|cat|extract`, and the web UI, 18): the catalog is
fetched (and nothing else) for browsing; extracting a path fetches only the chunks that hold its bytes;
every extracted file is checked against its recorded SHA-256; a damaged catalog is rejected by its own
digest. Extraction never follows a symlink and refuses unsafe paths.

**Prepared, not implemented:** a `zfs diff S1 S2` accelerator (skip walking unchanged directories)
plugs into the scanner, and zvol / block-mode volumes can go through the VM block pipeline
(`docs/block-level-research.md`). Neither makes ZFS mandatory for the repository format.

### 5.2 Archive format ("tar")

Deterministic PAX tar of the snapshot (`internal/archive`) -> FastCDC (2 MiB average, pinned gear table)
-> encrypt per key domain. It reads every file each run, has no catalog (not browsable) and is
restorable by the same `restore` commands.

### 5.3 Common

- The VM config volume is not backed up: the instance object's manifest carries the configuration and
  restore recreates the instance.
- Shared filesystem volumes are one object, captured once. Concurrent writers make the volume
  crash-consistent only; this is documented, not hidden.
- **Read path** (`Real.SnapshotPath`): the snapshot as Incus mounts it
  (`<var>/storage-pools/<pool>/<type>-snapshots/<name>/<snap>`) when it is mounted; otherwise, on ZFS,
  the snapshot dataset is mounted read-only with mount(2) under the client's state directory (the
  release unmounts it, retrying 20 times at 500 ms intervals, then removes the directory; leftovers of a
  killed run are unmounted by `CleanStaleMounts` at the next start), and finally
  `<mount>/.zfs/snapshot/snapshot-<snap>`. Anything else is a clear error. [O: real ZFS, gate G3; the
  interaction of the retry loop with Incus removing snapshot devices, upstream PR 4104, is phase 13 of
  the real-host plan]

### 5.4 Single files from VM disks

A VM disk is opaque bytes; the only trustworthy way to read a guest filesystem is to let a guest read
it. `restore guest-files` and the `guest_files` host job (`internal/recovery`,
`engine.RestoreGuestFiles`):

1. the snapshot of the chosen disk is written to a scratch block volume (`ibk-rec-<id>-disk`);
2. a second scratch volume (`ibk-rec-<id>-out`) receives the result;
3. a **disposable VM** is created from a configurable image (cloud-init needed) with **no profiles, no
   network, no devices except its root, a NoCloud seed disk and the two volumes**, 512 MiB / 1 CPU,
   `security.guestapi=false`, no secure boot; it boots from its own root disk (`boot.priority`), and the
   guest disk is attached read-only;
4. the script mounts the guest disk **read-only** (`ro,nosuid,nodev,noexec` plus
   `noload`/`norecovery`), chooses the first partition holding the first path (or `--partition`), writes
   a tar of the requested paths behind a 4 KiB header (written last) and powers off on EVERY exit;
   requests through a symlinked ancestor are refused, not guessed;
5. the host deletes the VM, reads the output volume through the per-volume NBD endpoint - **nothing is
   ever mounted on the backup host** - and extracts the tar as hostile input (only directories, regular
   files and symlinks; set-id bits and xattrs dropped; ownership never changed; traversal refused; a size
   limit; fail closed on an unsafe name);
6. scratch volumes and the VM are removed on every path, success or failure.

The guest script is tested against stubbed mount/poweroff and directory "partitions"; orchestration,
hostile-tar handling and cleanup against the fake Incus [T]. A real image, cloud-init and a read-only
attached real disk are gate G3c. **[unproven on a real host]**

## 6. Selecting what to back up (Incus native)

The selection lives in ordinary Incus config keys plus server-side assignments. No separate database on
the host.

```
user.backup.policy = <policy-name>     # on an instance or a custom volume
user.backup.exclude = true             # opt an instance/volume out
```

- Visible with `incus config show`, settable by hand with `incus config set`, and inherited through
  profiles (so a profile can opt a whole class in).
- A volume's own key wins over the key of an instance that attaches it. A shared volume is backed up if
  **any** consumer (or the volume itself) selects it, once.
- Policies (schedule, targets, commit requirement, retention, time zone, verify and restore-test
  schedules) are defined on the **server** (`policy set`, the UI) and delivered to hosts through
  `GET /v1/desired-state` from the host's `control` server. `client.yaml` may define local policies used
  only when the control server does not define the name; retention is never a client setting (10). A key
  without a matching policy is an error reported by `doctor`, never silently ignored.
- Server assignments select by host, project or object; `client.yaml` may also restrict to projects
  (`incus.projects`) and set a `default_policy`.

### 6.1 incus-sync integration

`incus-sync` instance YAML gains one optional block `backup: {policy: nightly}` that sets
`user.backup.policy` like any other declared config. (incus-sync does not manage storage volumes today, so
volume tags are set with `incus storage volume set`.)

## 7. Repository and cryptography

The byte-level specification is `docs/format.md`, pinned by test vectors
(`internal/format/testdata/vectors.json`, re-checked by an independent implementation in
`scripts/verify-vectors.py`).

### 7.1 Layout (server)

```
<datastore>/                     # a directory, or a bucket + prefix: the SAME keys
  datastore.json                 # format version, created time, id
  owner.json                     # which server owns this repository (create-if-absent)
  chunks/<aa>/<bb>/<id-hex>      # one encrypted blob per chunk (immutable)
  quarantine/<id-hex>            # corrupt blobs moved out of chunks/ by verify (kept for inspection)
  escrow/<domain>.ibw            # the master key wrapped to an OFFLINE public key (create-only)
  deliveries/<job>/<attempt>/p-NNNNNN   # sealed host-job deliveries (19), short-lived, outside the object model
  groups/<group-id>/notes        # free text for the object
  groups/<group-id>/<snapshot-id>/
      index                      # list of chunk ids (+ offsets/sizes), plaintext ids
      manifest                   # encrypted manifest + MAC
      commit.json                # commit marker, written LAST, create-if-absent
      protected                  # marker: retention and forget refuse to remove the snapshot
      notes                      # free text
      verify.json                # last verification state and a short history
```

A snapshot exists if and only if its `commit.json` exists. There is no directory rename and no `tmp/`
in the object model (the filesystem backend keeps a private `tmp/` for its own atomic writes).
GC, verify, scrub, reconcile, the orphan cleaner and capacity list only `chunks/`, `groups/` and
`quarantine/`, so deliveries never confuse them [T].

`group-id` is an opaque HMAC of the object name, so the datastore carries no names and stays meaningful
without PostgreSQL. Chunk ids are visible to the server (it needs them for dedup and GC); contents are
not.

### 7.2 Chunk blob

```
id    = HMAC-SHA256(k_id, plaintext)         # keyed, so ids do not leak content equality outside this key domain
blob  = magic | version | nonce | XChaCha20-Poly1305(k_enc, nonce, zstd(plaintext), aad = version | id) | SHA-256 trailer
```

- XChaCha20-Poly1305 with a 24-byte random nonce, from `x/crypto`. No home-made primitive.
- Compress then encrypt. Compression ratio side channels are accepted and documented.
- Every blob ends with a **plaintext SHA-256 trailer over the preceding bytes**, so servers verify
  integrity and sync verifies transfers **without any key**. The AEAD tag and the HMAC chunk id can only
  be checked with the key (host, or the browser, 18).

### 7.3 Keys

- One random 32-byte **master key** per key domain, created at the first client `init`. Sub-keys via
  HKDF-SHA256: `k_enc`, `k_id`, `k_manifest`, `k_delivery` (authenticates sealed deliveries, 18) and the
  non-secret domain id.
- Key file on the client host: by default unprotected (mode 0600, for unattended daemons); optionally
  wrapped with a passphrase (Argon2id; `init --passphrase-file`, `key import --passphrase-file`).
- A **recovery key** (`IBR1-…`, printable, with checksum) is shown once at `init`; `init` refuses to finish
  until the operator confirms it was stored (`--i-stored-the-recovery-key` non-interactively). `key check`
  re-checks a typed copy. Losing every copy (and the escrow key) loses all data.
- **Changing the passphrase** of a key file: there is no `key passwd`; recreate the file from the recovery
  key with `key import --passphrase-file` (it refuses to overwrite an existing key file). **[gap]**
- **Master-key rotation is not possible inside a repository** (one key domain per repository; chunk ids
  depend on the key). The rotation path is a new repository with a new key, backed up afresh by the
  hosts, while the old repository ages out under its retention. Sync cannot re-encrypt (it never holds
  a key).
- **Offline escrow** (`internal/format/escrow.go`, `key escrow ...`): an X25519 key pair is generated once;
  its PRIVATE half is kept offline. The master key is wrapped to the PUBLIC half (ephemeral-static ECDH,
  HKDF-SHA256, XChaCha20-Poly1305, key domain and both public keys in the AAD) and the blob is stored on
  the servers, create-only. They cannot open it. A new host needs only a token and the private key
  (`init --escrow-private-file`). The browser library can open an escrow blob (`openEscrowKey`), but the
  web console accepts only the recovery key: unwrap an escrow blob on a host first (`key escrow unwrap`).
- **Key domain (decided).** One key domain per repository; every host enrolled in a repository shares its
  key, which gives cross-host deduplication. The cost: any enrolled host can decrypt that repository and
  can produce deliveries the browser accepts (18). Separate repositories give isolation.

### 7.4 What the server learns

**Plaintext on purpose (control plane):** host names, project names, policy names, instance and
storage-object names, schedules, run times and durations, success/failure, logical size, uploaded size,
dirty %, dedup ratio, restore-point history, user names and roles, audit entries.

**Also observable:** chunk count and sizes, the plaintext chunk-id graph (dedup structure), timing
patterns, which encrypted objects a browser fetched (audited, 12.3), and, for file-recovery jobs, the
paths an operator typed and their per-path `ok`/`missing` status (19).

**Never plaintext:** volume contents, file names and paths inside volumes (other than typed recovery
paths), VM/instance configuration and secrets, chunk contents, key material.

Consequence: a compromised server reveals the infrastructure topology (what exists and when it changes)
but not any data, as long as the code it serves to browsers is honest (18.3). An opaque-names mode
(server shows ids only) is not v1. [O]

### 7.5 Pluggable datastore backends (filesystem and S3)

The repository logic (`internal/datastore`) talks to a small object-store interface (`Backend`: Put with
optional create-if-absent, Get, Stat, List, ListPrefixes, Delete). It needs no rename, directory,
append, lock or touch primitive, so the object model is byte-for-byte the same on every backend. An
in-memory backend with strict object semantics runs the whole datastore suite next to the filesystem and
S3 backends, which keeps POSIX assumptions out of the repository logic. Below the object model,
`ModeBackend` enforces the repository's maintenance mode (21) so no code path can write around it.

* **filesystem**: tmp file + fsync + rename (link for create-if-absent), directory fsync.
* **s3**: the S3 API directly (minio-go; no FUSE/s3fs/rclone), see 7.6.
* **No object is ever rewritten to keep it "alive".** GC grace uses the object's own write time. What a
  running upload relies on is tracked by the server: every session *pins* the chunks it saw present or
  uploaded (in memory, with the PostgreSQL session row), and every commit pins its whole index while a GC
  runs. A multi-hour backup can outlive the grace period; S3 sees no COPY requests and no new versions.
* **Ownership.** `owner.json` (create-if-absent) names the server (`server_identity`) that owns the
  repository; a second server pointed at the same directory or bucket is refused (`repo attach --adopt` is
  the explicit disaster-recovery override; `repo detach --clear-owner` releases it). PostgreSQL also records
  the repository's `datastore.json` id and refuses a different datastore behind the same path or bucket.
* **Credentials** are infrastructure secrets, unrelated to payload keys: the access/secret key (and
  notification secrets) are sealed (XChaCha20-Poly1305, bound to the row id) with `secrets.key`, a 0600 file
  in the state directory. The database alone reveals no credential; no API ever returns one.
* Capacity: usage is counted from listings (hourly sample by default); free space and a fill forecast are
  reported as *unavailable* on object stores.
* Multi-target and server-to-server sync mix backends freely.
* Immutability: chunks are write-once; GC, retention, forget and deletes requested by an administrator are
  the only deleters and run server-side; backup-scope credentials can never delete.

### 7.6 S3 specifics

- Any S3-compatible service: endpoint, region, bucket (must exist), prefix, path-style, TLS with an optional
  private CA (`--s3-ca-file`); `--s3-no-tls` and `--s3-insecure-skip-verify` exist for LAN tests only.
- **Commit marker by conditional write.** `commit.json` and other create-only objects use
  `If-None-Match: *`. `repo create` and `repo probe` check connectivity, bucket, put, put-if-absent,
  get, stat, list and delete, and refuse a service that overwrites despite `If-None-Match` [T against
  MinIO; real providers **unproven**, real-host phase 7].
- **No HEAD before a chunk PUT** on S3: the conditional PUT answers "exists" itself.
- **Bounded parallelism.** Existence checks (Missing and Commit), GC mark/sweep, verify reads and listings
  run with up to 64 requests in flight per repository operation (`datastore.DefaultRequestParallelism`);
  the transport keeps 2 x that many idle connections per host. `chunks/` is listed as parallel shards
  delivered in exact key order (24).
- **Presence set** (S3 only): an in-memory set of chunk ids known to exist, built from a listing and from
  this process's own puts and answers; it answers only "definitely stored" for **Missing**. It is never
  used by **Commit**, which asks the backend about every chunk after pinning them (24.2).
- Bucket lifecycle expiry must be off (GC needs to be the only deleter); versioning/object lock are the
  operator's choice (GC needs delete permission).
- The server never needs bucket listing outside its prefix; the required permissions are put, get, head,
  list and delete on `<prefix>/*`.

## 8. Protocol

The agent API: HTTPS with mutual TLS (TLS 1.3, HTTP/2), on `listen` (default `:8007`). Every endpoint
except enrolment demands a client certificate signed by the server's private CA (20).

- Repository creation and client enrolment are **separate operations**: `incus-backup-server init`
  creates the state (CA, server certificate), migrates PostgreSQL and creates the first repository;
  `repo create` adds more. `token create HOST --scope backup` (or the web UI's onboarding, which prints the
  whole command) creates a one-time 256-bit token that also pins the CA fingerprint. `incus-backup init`
  uses the token, pins the CA and receives a client certificate.
- **Host scopes:** **backup** (create sessions, upload chunks, commit, read its own objects for restore,
  take its own jobs; can never delete or alter history), **sync-read** and **sync-write** (peer servers,
  3.3). **The administrative authority never exists on a backed-up host.** A compromised host can fill the
  repository with junk but cannot erase history. This is the ransomware-resistance property.
- **People** never use the agent API. They use the management API (12) with roles; administration over the
  network is possible for `admin` users and admin-role API tokens, never for a host certificate.

Session and other host routes:

```
GET  /v1/whoami                      identity, repository, repository mode (online/read-only/offline)
GET  /v1/desired-state               policies and assignments for this host
POST /v1/sessions                    -> session id (declares group, snapshot id, time, previous id); 503 in maintenance
POST /v1/sessions/{sid}/missing      -> ids the server lacks (batch query)
PUT  /v1/sessions/{sid}/chunks/{id}  idempotent upload, trailer verified
PUT  /v1/sessions/{sid}/index
PUT  /v1/sessions/{sid}/manifest
POST /v1/sessions/{sid}/commit       -> verify chunks exist, write index + manifest, then commit.json (create-if-absent)
POST /v1/sessions/{sid}/heartbeat    renew the lease
GET  /v1/groups/{group}/snapshots[/{snap}/index|manifest], GET /v1/chunks/{id}   restore reads
POST /v1/runs, POST /v1/runs/{id}/finish, PUT /v1/inventory                       reporting
PUT  /v1/escrow, GET /v1/escrow      the wrapped master key (create-only; opaque to the server)
POST /v1/enroll                      one-time token -> certificate (the only unauthenticated route; rate limited)
GET  /v1/cert, POST /v1/renew, POST /v1/renew/confirm                             certificate renewal (20)
GET  /v1/jobs/next, POST /v1/jobs/{id}/progress|finish, GET /v1/jobs/{id}/cancel-requested,
PUT  /v1/jobs/{id}/delivery, POST /v1/restore-tests                               host jobs (19)
```

**One daemon, one owner of the pins.** Session pins, the "one destructive task per repository" rule and
the writer slots live in the memory of the serving process, so exactly one `serve` may run per database
(PostgreSQL advisory lock, taken for its whole life; a second one refuses to start). Every destructive
operation (GC, retention, forget, a quarantining verify, credential rotation, a sync run, a repository
destroy, an object delete) needs that lock: the daemon has it; the CLI obtains it only while no daemon
runs, and otherwise forwards the request to the daemon's admin socket (it then runs there as a task,
21). Read-only work (dry runs, previews, report-only verify, listings) is never blocked.

**Objects belong to the host that first backed them up.** The object's owner, project, name, kind and
retention policy are pinned at first sight; another backup-scope host gets 403 on that group id. An
unowned object (created by sync or rebuilt after a disaster) is claimed by the first host that announces
it. An administrator moves an object with `object transfer`.

**Sessions are leases.** Every request on a session renews a 15 minute inactivity lease (a client busy
without talking to the server heartbeats every Lease/3). A multi-hour first backup never expires; a dead
client is reclaimed one lease after its last sign of life. PostgreSQL admits **exactly one open session
per (repository, object group)** (a partial unique index), so two writers can never race on one object; a
second writer gets 409. The session's `previous_id` snapshot is **pinned** against retention and `forget`
while the session is open.

**Maintenance mode** is checked when a session opens (503, before the host reads anything) and again at
commit (503). Hosts ask `whoami` at the start of a run and **defer** objects whose every target is in
maintenance (21).

Sync endpoints (peer scopes):

```
GET  /v1/sync/snapshots?after=<cursor>   committed snapshots (group, time, snapshot id, index digest)
POST /v1/sync/chunks/missing             ids the destination lacks
GET  /v1/chunks/{id}                     blob (sync-read)
PUT  /v1/sync/chunks/{id}                blob (sync-write), idempotent, trailer verified
PUT  /v1/sync/snapshots/{group}/{snap}   index + manifest, then atomic commit
```

## 9. Commit, crash recovery, failure semantics

| Failure | Result |
|---|---|
| Client crash before commit | The lease runs out (15 min without activity); the session and its pins are reclaimed, the writer slot frees. B1 untouched. Next run repeats. |
| Two clients back up the same object at once | PostgreSQL admits one open session per object group; the second gets 409. |
| Retention or forget while a backup builds on the base snapshot | The base is pinned for the lifetime of the session and survives. |
| GC runs during a long backup | Chunks the session saw present or uploaded are pinned however old; a snapshot committed after GC's mark phase pins its whole index for the rest of that run. |
| Two servers adopt one repository | `owner.json` (create-if-absent) refuses the second; `--adopt` is explicit. |
| Network drop mid-upload | Resume: ask `missing`, upload rest. |
| Server crash mid-commit | Snapshot visible only once `commit.json` exists (written last). Otherwise absent; leftovers are orphans cleaned later. |
| Commit acked, client crashes before deleting B1 | Next run sees `ib-<old>` plus server snapshot id mismatch -> full read for that disk, deduplicated. Safe. |
| Incus restarted / VM restarted | Persistent root bitmap used if valid; others full read. |
| Bitmap `inconsistent` or missing | Full read. |
| Chunk corrupted on server | `verify` (keyless, SHA-256 trailer) moves the blob to `quarantine/`, so it no longer answers "present" to dedup checks; the affected snapshots are marked failed and recorded (`corrupt_chunks`). A failed snapshot is never an incremental base: the next backup reads everything again (deduplicated), re-uploads the chunk and the record flips to healed. `sync --resync-corrupt` repairs a destination from a healthy source. |
| A chunk deleted behind the server's back (S3 presence set says "present") | Commit asks the backend about every chunk and refuses (409, missing chunks); the ids are evicted from the presence set and the client's retry uploads them. Never a visible snapshot with a missing chunk. [T] |
| One required target down, others committed | The committed targets advance `Lt`. The down target keeps its older `Lt`; its bitmap is retained; next run re-reads a larger extent map. Nothing discarded. |
| All required targets not committed | No `Lt` changes; all bitmaps retained. |
| Sync interrupted | Partial transfers are invisible; re-run compares chunk ids and resumes. Destination snapshot is committed atomically only after verification. |
| Sync source loses a snapshot to retention mid-sync | The sync of that snapshot fails cleanly (not committed); other snapshots proceed. |
| Frozen view held too long | Client aborts at `max_freeze`, closes the session, keeps B1. |
| Compromised host dates snapshots into the future to displace history | Refused beyond 5 minutes of skew; retention buckets use the SERVER's commit time (a synced snapshot keeps its source server's commit time, clamped to now). `keep-last` alone can still be flooded: use daily/weekly rules. |
| A target is unreachable for days | Its base generation is kept from the client's memory (`state.json`) until `max_target_lag`; the new generation is kept only if some target committed it; at most 8 `ib-*` bitmaps are ever carried. |
| A reused base chunk vanished from a server | The upload reports `ErrLocalChunkMissing`, that base generation is dropped, the next run reads the disk in full (deduplicated) and heals the repository. |
| Repository put in read-only/offline maintenance during a run | Sessions opened after the change get 503 before anything is read; a commit refused by the change gets 503; the objects are **deferred** (run status `deferred`, not a failure). |
| Server daemon restarts while a task runs | The task is marked failed ("server restarted while the task was running"); destructive tasks stop between steps, so nothing is half-deleted; run it again. |
| Host daemon dies while running a job | The job's 5-minute lease runs out; the server re-queues it while attempts remain (restores are never retried) and deletes partial deliveries. |
| `restore --as` next to the running original | NIC `hwaddr`, static addresses are dropped unless `--keep-network`; same-name disaster recovery keeps them. |
| Disk full on server | Commit refused cleanly; no partial snapshot. |

Cross-object consistency: each storage object has its own snapshot instant, recorded in the manifest.
Consistency **across** objects of one run is not guaranteed (except all disks of one VM, which share one
frozen view). Documented limit.

## 10. Retention, GC, verify, restore

- **Retention authority.** Which policy governs an object is decided by the server: an admin assignment
  (object > project > host) wins, otherwise the policy pinned when the object was first seen
  (`storage_objects.retention_policy`); a backup-scope host can never change it later (`object retention
  OBJECT POLICY` pins one explicitly). Unknown policy = keep everything.
- **Retention semantics are PBS-exact** (`docs/retention.md`): the decisions equal a literal port of PBS
  `compute_prune_info`/`mark_selections` (`pbs-datastore/src/prune.rs`) over 24 000 random configurations
  including ISO weeks that straddle month and year ends [T: `internal/retention/pbsref_test.go`]. Rules run
  in the order last, hourly, daily, weekly, monthly, yearly; each keeps the newest snapshot of each of the N
  most recent periods no earlier rule covered; **rules are cumulative**; older snapshots of a period a rule
  kept are removed for good (later rules skip them, PBS `PruneMark::Remove`); protected snapshots are
  never removed and use no slot.
- **Documented input deviations** (the algorithm is the same, what goes in differs): periods are cut in the
  **policy's IANA time zone** (PBS: the server's local zone); the snapshot time is the **server's commit
  time** (PBS: the client-set backup time); equal times are ordered by snapshot id; the newest snapshot is
  never removed (a safety net that never changes a decision while a rule is active); a snapshot pinned by
  an open session counts as protected; an unknown time zone or policy keeps the whole group. With no rule at
  all everything is kept.
- `prune --dry-run` (and `POST /prune?dry_run=1`) returns exactly the plan that will be applied, per
  snapshot with the reason; the web UI's preview is a TypeScript port held identical to the Go code by
  shared vectors [T: `webvectors_test.go` + vitest].
- **Protected snapshots, notes.** `snapshot protect` writes a marker in the repository; retention and
  `forget` refuse protected snapshots and snapshots a running backup builds on. Notes (snapshot and object)
  are repository objects too. Both survive a PostgreSQL rebuild.
- **Deleting a whole object** (`object delete PROJECT/NAME --confirm PROJECT/NAME [--force-protected]`,
  admin) forgets every snapshot of it as a task; the chunks are freed by the next GC.
- **GC** is server-side mark and sweep over the plaintext indexes, needing no key: mark every committed
  index (an unreadable index aborts the whole run); candidates are unreferenced chunks older than the grace
  period (default 24 h, minimum 12 h; age is the object's write time); each candidate is re-checked under
  the sweep lock (batches of 64) and skipped when a running session pins it or a session that ended since
  the GC began did. One destructive task at a time per repository.
- **Verify** (server, no key): reads every chunk blob's SHA-256 trailer, every index and manifest against
  the commit marker's digests, and that every referenced chunk exists; **within one job each chunk is read
  at most once**. Modes: `all` (every snapshot and every chunk object, the "scrub"), `new` (snapshots never
  verified), `stale` (never verified, or older than the policy's `reverify_days`). The result is recorded
  per snapshot (`verify.json`, history of the last 20, mirrored in PostgreSQL). Corrupt chunks are
  quarantined (9). Scheduled: `verify` daily (new + stale) and `scrub` monthly by default. There is no
  verification right after each commit; a new snapshot waits for the next `verify` run. `incus-backup
  verify` (client) and the `deep_verify` job additionally decrypt and recompute the keyed chunk ids, which
  proves restorability.
- **Restore (VM):** recreate the instance from the stored configuration, recreate volumes, write chunks into
  them through Incus's per-volume NBD endpoint (`writable`) [V: endpoint in 7.5.1]. **Restore
  (filesystem):** recreate the volume and extract with exact ownership/xattrs/ACLs. **Single files:**
  `fs extract` (filesystem backups, 5.1), `restore guest-files` (VM disks, 5.4), and in the web UI (18, 19).
- **Instance restore and referenced volumes.** `restore instance --volumes` restores each referenced custom
  volume from the snapshot recorded in the instance manifest for the same run (`snap:<ref>`). Two limits,
  both documented and on the real-host plan (phase 13): a referenced volume that **already exists** under
  the target project is not restored and the restored instance attaches the existing volume (its current
  state); and when the run did not capture the volume (it failed, or another policy covers it), the manifest
  names no snapshot and restore takes the volume's **newest** snapshot. [O]

### 10.1 Restore tests

A backup that was never restored is not a backup, so restore tests are scheduled, recorded and alerted on.

- **Methods** (`incus-backup restore-test run --mode`, the `restore_test` job): `deep` (download, decrypt
  and recompute the keyed id of every distinct chunk; lists every bad chunk), `extract` (filesystem catalog
  snapshots: restore into a scratch directory, every file checked against its SHA-256 while written and
  again from disk, entries counted against the catalog), `full` (block objects: restore through the normal
  NBD write path into a scratch volume `ibk-rt-*`, hash the decrypted stream, read the volume back over a
  new read-only NBD connection and compare; filesystem objects: restore into a scratch volume and check like
  extract). None touches the original object; scratch objects are deleted on every path.
- **Schedule:** the client daemon plans tests itself from the policy's `restore_test_schedule` (default
  `monthly`): every (object, target) pair is tested within the period, a rotating share per day; method
  `extract` for filesystems, `deep` for disks with `full` every 4th test [T: fake clock, 60 objects, 4
  months]. Policies defined only in `client.yaml` have no restore-test schedule.
- **Records:** locally (`restore-tests.json`) and on the server (`restore_tests`, via `POST
  /v1/restore-tests`; accepted only for objects the reporting host owns). The UI shows the history; the
  setup checklist asks for a passed test; `restore_test_failed` alerts on the latest failure.
- **Unproven:** a full restore test on real ZFS (scratch volume, NBD write and read-back speed). Real-host
  phase 12.

## 11. Configuration and setup experience

**Server.** `/etc/incus-backup/server.yaml` is bootstrap only (`internal/server/config`):

```yaml
listen: ":8007"                                   # agent API (mutual TLS)
state_dir: /var/lib/incus-backup-server           # CA, server cert, secrets.key, web TLS, admin.sock, sync credentials
database_url: postgres://incus_backup@localhost/incus_backup   # or INCUS_BACKUP_DATABASE_URL / DATABASE_URL
sans: [backup.example.net]                        # bootstrap SANs of the server certificate (later: ca sans)
# admin_socket: /var/lib/incus-backup-server/admin.sock   # default; "off" disables
# web_listen: ":8443"                             # web UI + /api/v1 over HTTPS; "off" disables
# web:                                            # TLS of the web listener
#   mode: self                                    # self (default, self-signed, renewed) | files | acme
#   cert: /etc/ssl/backup.crt                     # mode files (watched, reloaded on change)
#   key: /etc/ssl/backup.key
#   domains: [backup.example.com]                 # mode acme (TLS-ALPN-01 on the web listener; needs :443)
#   email: ops@example.com
#   acme_directory: https://acme-v02.api.letsencrypt.org/directory
#   acme_http_listen: ":80"                       # optional HTTP-01 and http->https redirect
# metrics_listen: 127.0.0.1:9187                  # optional plain-HTTP GET /metrics
# metrics_unauthenticated: false                  # true only on a private address
```

Everything else is stored in PostgreSQL and managed with the CLI, the management API or the web UI.

**Client.** `/etc/incus-backup/client.yaml` (written by `incus-backup init`; see `examples/client.yaml`):
host name, state and spool directories, key file, `max_freeze`, `max_target_lag`, `verify_schedule`, the
Incus socket and projects, `servers` (each a complete repository, optionally with its own `key_file`),
`control` (the server that supplies policies), optional local `policies`/`default_policy`, `filesystem`
(`format`, `full_every`, `trust_ctime`), `recovery` (`image`, `pool`, `memory`, `cpu`), `performance`
(`encode_workers`, `upload_workers`) and `jobs` (`disabled`, `allow`).

Setup (the web UI covers every server step; see `docs/operations.md` for the full flow):

```
server$ incus-backup-server init --database-url ... --datastore /srv/incus-backup --san backup.example.net
server$ rc-service incus-backup-server start            # logs a one-time setup token; or: user setup-token
browser: https://backup.example.net:8443 -> create the first administrator with the setup token
browser: Hosts -> Onboarding: copy the printed `incus-backup init --server ... --token ...` command
host$   incus-backup init --server backup.example.net:8007 --token ...   # makes keys, prints the recovery key
host$   incus config set <instance> user.backup.policy nightly
host$   incus-backup doctor && incus-backup run
```

`doctor` checks Incus reachability and version, the API extensions needed (NBD, bitmaps), server
reachability and CA pin, certificate state, key present, selection resolves, spool space, leftovers of
recovery VMs, and warns about shared block volumes and in-memory bitmaps. The client contains its own
scheduler (Alpine has no systemd). Packaging: OpenRC scripts + apks built for `unidoc-aports`
(`docs/packaging.md`).

## 12. Management plane

One management API (`internal/server/mgmt`, `/api/v1`, contract in `docs/api.md` with a route table
generated from the route metadata and an OpenAPI 3.1 document) serves every human and automation
client: the server CLI, the web UI and scripts. There is no business logic in handlers; they call the
service layer that the CLI also uses.

### 12.1 Listeners

| listener | who | authentication | default |
|---|---|---|---|
| agent API (`listen`) | hosts, sync peers | mutual TLS with the server's private CA (20) | `:8007` |
| web listener (`web_listen`) | browsers, automation | session cookie + CSRF header, or `Authorization: Bearer` API token; TLS per `web.mode` | `:8443` |
| unix admin socket (`admin_socket`) | the server CLI, local scripts | none: whoever can open the socket (mode 0660, owner and group of the service account) is `local-admin` | `<state_dir>/admin.sock` |
| metrics listener (`metrics_listen`) | Prometheus | API token unless `metrics_unauthenticated` | off |

Hosts never use `/api/v1`; people never use the agent API. The two authentication systems are never
mixed.

### 12.2 People, roles, scopes

- **Users** (`internal/server/auth`): argon2id password hashes (at least 12 characters), optional TOTP with
  ten one-time recovery codes, sessions of 30 minutes idle / 12 hours absolute, five failed logins lock an
  account for growing intervals, 20 login attempts per minute per address. The first administrator is
  created with a one-time **setup token** (logged at start while no user exists, or `user setup-token`).
- **Roles:** `readonly` < `restore` < `operator` < `admin`, plus `none` for users who have only
  per-repository roles. Every route has an entry in `mgmt/perms.go`; a route without one panics at start
  and a matrix test checks every route against every role [T].
- **Repository scopes** (`docs/permissions.md`): a user can be restricted to some repositories with a role
  per repository; administrators are never restricted. Enforcement is one function (`authorize`) before every
  handler, driven by each route's declared repository binding; lists, the overview and the SSE stream are
  filtered per item; another repository's job or alert id answers 404 [T].
- **API tokens:** role at most the owner's, optional expiry, optionally restricted to repositories (then
  without global rights); rights are recomputed per request, so demoting the owner limits the token at once.
  The audit actor of a token names its owner (`token:ci (by anna)`). Secrets are shown once.

### 12.3 Audit

Every state-changing request is audited (actor, action, target, address, result; bodies are never
logged), as are sign-ins, refused requests (role, CSRF/origin, cross-site login/setup; repeats summarised
per window), sealed-delivery downloads and reads of raw repository material (one summary per actor,
repository and snapshot per minute). There is no hash chain: the database owner can delete rows.

### 12.4 API contract and live updates

- JSON in and out, strict decoding (unknown fields are 400), 1 MiB bodies, errors `{"error": ...}`, 503 for
  repositories in maintenance.
- Long operations answer **202** with a task (21); `?wait=1` keeps the synchronous form (the CLI's forwarding
  uses it).
- `GET /openapi.json` (any signed-in principal) and `incus-backup-server openapi`: OpenAPI 3.1 generated from
  the route metadata (`routes_meta.go`), with each route's role and repository binding; a test fails when a
  route lacks metadata or `docs/api.md`'s generated table is stale [T]. The web UI renders it at
  Configuration -> API.
- `GET /events`: Server-Sent Events (`job`, `alert`, `run`, `repository`, `host`, `task`), filtered per
  subscriber.

### 12.5 Web UI

The Vue 3 console is compiled into the server binary and served on the web listener with a strict CSP
(`script-src 'self' 'wasm-unsafe-eval'`, `frame-ancestors 'none'`) and HSTS on HTTPS. It covers the
dashboard (problems list, protection coverage, setup checklist), hosts and onboarding, objects, snapshots,
the zero-knowledge file browser and restore (18), verification, tasks, jobs and restore tests, sync,
alerts and notifications, traffic control, repositories and capacity, policies with the exact retention
preview, users and tokens with repository access, certificates, settings, server status and logs, the API
explorer and the audit log. Its end-to-end tests run with Playwright against the demo stack (`ibdemo`) and
a real server [T]. The CLI remains complete for server operations; notifications, settings, traffic rules
and API tokens have no dedicated CLI commands (API or UI only).

## 13. Quality bar

1. This document, then a **byte-level format spec** with test vectors (done: `docs/format.md`, vectors
   re-verified by an independent implementation; `docs/sealed-delivery.md` for IBS2 with its own vectors and
   an independent TypeScript re-implementation).
2. Fuzz the chunker, index/manifest parsers and blob decoder (`just fuzz`).
3. Crash-injection tests at every step of section 4.3 and section 8 (datastore commit points, with a
   negative control).
4. Tests fail, never skip, when PostgreSQL or MinIO is missing.
5. Automated restore-and-compare: the engine tests restore and compare byte by byte against the fake; the
   gates (`scripts/gates/`) do it on a real host.
6. Independent review of the cryptographic construction before 1.0 (now including escrow and IBS2). [O]
7. A format version in every file; no silent format changes; the compressed chunk encoding is pinned by a
   golden test.

## 14. Validation gates

These are **evidence checkpoints, not implementation phases.** A gate is passed when its evidence exists.
Gates that need the real Benfica/Incus/ZFS host, a real S3 provider, a real mail server or a public
domain are run by the operator from `scripts/gates/` and `docs/real-host-validation.md`; the sandbox
cannot run Incus.

| Gate | Evidence required | State (2026-10-09) |
|---|---|---|
| G0 format | Byte-level spec with test vectors; vectors pass in Go; fuzzers run clean | **passed** |
| G1 server | PostgreSQL schema + migrations, crash-injection at every commit step green, DB rebuild from datastore passes | **passed** |
| G2 VM | Real VM backed up and restored byte-identical; NBD throughput measured | engines **passed against the fake** and the real NBD protocol in the PoC; **real end-to-end pending** (`g-e2e.sh`) |
| G2a freeze-hold | Long hold vs busy writer, overlay growth and guest behaviour recorded | script ready (`g-freeze.sh`); **result pending**; mitigations implemented |
| G3 filesystem | Container and volume restored with exact ownership/xattrs/ACLs; snapshot read path verified on Incus ZFS | round trips **passed**; **snapshot read path and whole-container restore pending** |
| G3a catalog incrementality | Second run reads only changed files; restore equals source; forged-mtime rewrite detected | **passed** against the fake |
| G3b ctime/inode on ZFS | Successive Incus ZFS snapshots keep inode and ctime of an untouched file | **pending** |
| G3c guest recovery | A real recovery VM mounts a real guest filesystem read-only and returns the files | **passed** against stubs and the fake; real VM **pending** |
| G4 retention | GC never removes a live chunk; prune honours policy; decisions equal PBS | **passed** (incl. differential test against a PBS port) |
| G4c integrity | Corruption quarantined, recorded, never a base, healed | **passed** on filesystem and S3 (MinIO) |
| G4d concurrency | One writer per object, leases, base pinned | **passed** |
| G4a multi-target | One capture to two servers; lagging target keeps generations | **passed** (two real servers in-process) |
| G4b sync | Push and pull, resume, partial never visible, independent retention, options | **passed** (every filesystem/S3 pairing) |
| G4e S3 | Real S3-compatible service: conditional commit, presence set, GC, verify, restore, sync | **passed against MinIO**; a real provider **pending** |
| G5 integration | `doctor`, OpenRC/apk packaging, incus-sync block, setup flow | apks **built** with abuild and smoke-installed (134 checks, `docs/packaging.md`); a real Alpine boot and the client daemon on a real host **pending** |
| G6 management plane and UI | Roles, scopes, audit, every route, browser decryption | **passed** in the sandbox (route/role matrix, scope matrix, Playwright against the demo and a real server, WebAssembly in headless Chromium) |
| G7 host jobs and restore tests | Jobs claimed, fenced, leased, sealed deliveries verified; restore-test methods | **passed** against the fake and real mTLS; full restore test on real ZFS and `guest_files` via a real recovery VM **pending** |
| G8 certificates | Renewal, overlap, lost answers, CA rotation | **passed** with a driven clock and real TLS; renewal over real weeks and rotation across machines **pending** |
| G9 notifications | Delivery to each channel kind | webhook **passed** end to end; SMTP (against an in-process fake SMTP server), ntfy and Gotify **passed in unit tests**; real servers **pending** |
| G10 traffic control | Rates within tolerance, windows, rule changes | **passed** on loopback (±20 %); a real network **pending** |
| G11 performance and memory | Numbers of `docs/performance.md` and `docs/memory.md` on real hardware | sandbox numbers recorded; real hardware **pending** |
| G12 web TLS | `self`, `files` reload, ACME issuance | `self` and `files` **passed**; ACME on a real domain **pending** |

## 15. Open questions

Closed during implementation:

- Key spelling `user.backup.policy` / `user.backup.exclude` (used by incus-sync).
- Policy precedence: exclude > key on the object > key on a using instance > server assignment (object >
  project > host) > `default_policy`.
- Retention is applied by the server from an admin-set policy; administration over the network exists for
  people with roles (12), never for hosts.
- Key domain: one per repository.
- Root persistent bitmap content survives a clean restart [V, PoC].
- VM restore write path: per-volume NBD with `writable` into a freshly created stopped volume [V; real run
  pending in G2].
- Multi-user RBAC and a web UI: implemented (12, 18).

Still open (each is exercised by a gate script or `docs/real-host-validation.md`):

1. Cost and failure mode of a very long frozen view (4.6) - `g-freeze.sh`.
2. Snapshot read path for container/filesystem objects on real ZFS (5.3) - `g-e2e.sh`, `g-fs.sh`.
3. Whole-container restore into a freshly created Incus container (`FilesystemPath` needs the volume
   mounted) - `g-e2e.sh`.
4. NBD throughput through the Incus API proxy, with pipelining.
5. Independent review of the cryptographic construction before 1.0 (escrow wrap and IBS2 included).
6. Incus clusters (multiple members, remote storage): run one client per member; untested.
7. Stateful VMs (memory snapshots): out of scope for v1.
8. ctime/inode stability across Incus ZFS snapshots (G3b).
9. The recovery VM in real life (G3c).
10. A real S3 provider (G4e): conditional writes, listing behaviour, request rates.
11. Name check of `incus-backup` against existing projects.
12. Instance restore with dependent/existing volumes and a missing `snap:<ref>` (10), compared with
    upstream Incus PR 4137 semantics.
13. Snapshot unmount retry against Incus removing snapshot devices (5.3, upstream PR 4104).

Closed since draft 2: block-level backup of filesystem volumes (file-level catalog now, `zfs diff` and
zvol block-mode as later accelerators, `docs/block-level-research.md`).

## 16. PostgreSQL schema outline (control plane)

Authoritative DDL: `migrations/20261008000000_initial_schema.sql` (one file; the pre-release history was
consolidated, see `CHANGELOG.md`). Tables by domain:

- **Identity and storage:** `server_identity` (recorded in every repository's `owner.json`), `credentials`
  (sealed secrets: S3, SMTP, tokens, webhooks), `repositories` (backend, non-secret S3 settings, credential
  reference, datastore id, key domain, maintenance mode and reason), `server_settings` (key/value: SANs,
  base URL, advertise address, instance name, update check, ...)
- **Hosts:** `hosts` (identity, scope, current certificate fingerprint, last seen, last renewal),
  `host_certs` (every issued certificate of an identity), `enrollment_tokens`
- **Policies:** `policies` (schedule, targets, commit requirement, keep rules, IANA time zone,
  `reverify_days`, verify and restore-test schedules), `policy_assignments`
- **Objects and runs:** `storage_objects` (host, project, name, kind, group id, pinned retention policy,
  notes), `snapshots` (a cache of the repository's own metadata incl. protection, notes, verification),
  `sessions` (lease, base snapshot pin; partial unique index: one open session per repository+group),
  `backup_runs` (incl. `deferred` and `objects_deferred`)
- **Integrity and housekeeping:** `snapshot_verifications`, `corrupt_chunks`, `capacity_samples`,
  `maintenance_settings`, `maintenance_runs` (linked to their task)
- **Tasks and system:** `tasks` (partial unique index: one exclusive task per repository), `task_log`,
  `system_samples`
- **Sync:** `sync_jobs`, `sync_runs`, `sync_snapshot_state`
- **People:** `users`, `user_sessions`, `api_tokens`, `user_repo_scopes`, `api_token_repos`, `audit_log`,
  `auth_setup`
- **Notifications:** `notify_settings`, `notify_conditions`, `notify_channels`, `notify_rules`,
  `notify_alerts`, `notify_alert_events`, `notify_rule_state`, `notify_silences`, `notify_deliveries`,
  `notify_probe_state`
- **Host jobs:** `host_jobs`, `host_job_log`, `restore_tests`, `job_deliveries`
- **Traffic:** `traffic_rules`
- `schema_migrations` (created by the migration runner)

PostgreSQL is **not** the source of truth for payload, snapshot or chunk relationships, nor for protection,
notes and verification state. There is no `gc_pins` table: the pin is the open session row plus the
datastore's in-memory session pins (7.5).

## 17. What was taken from Proxmox Backup Server (source comparison)

`docs/pbs-comparison.md` compares this design with the PBS source (v4.2.8); the README has the
feature matrix with what is missing. Adopted (each with tests): quarantine and healing of corrupt chunks
and the rule that a failed snapshot is never a base; PBS-exact retention with explicit time zones; one
writer per group; protected snapshots and notes; per-snapshot verify state with verify-new and
re-verify-after-N-days; sync `verified-only`, `resync-corrupt`, `transfer-last` and rate limits; traffic
control with time windows; metadata change detection (comparing ctime and inode too) and padding-free chunk
reuse; an `owner` marker; S3 without rewriting objects; offline key escrow; maintenance modes; tasks with
logs; browsing and single-file restore for filesystem backups and (through an isolated VM) for VM disks.
Kept different on purpose: SHA-256 keyless blob trailers (PBS: CRC32), encrypted manifests (PBS:
plaintext), atomic visibility by commit marker, bitmap generations across several targets, browser-side
decryption of encrypted snapshots (PBS cannot browse encrypted snapshots in its UI).

## 18. Zero-knowledge web UI

### 18.1 Browser decryption

The server serves only ciphertext; the browser decrypts. The operator pastes the recovery key into the page; it goes once into a WebAssembly module
(`cmd/ibwasm`, the Go code of `internal/webcore`, which reuses `internal/format` and `internal/fscat`)
running in Web Workers, is checked against the repository's key domain, and never leaves the page. The page
fetches the encrypted manifest, index and chunks through `GET /api/v1/snapshots/{group}/{snap}/index|manifest`
and `GET /api/v1/chunks/{id}` (role `restore` on that repository, audited as summaries), decrypts the
manifest, checks the index against the manifest's digest, decodes the catalog and streams files (each
verified against its SHA-256 before the last piece is released) or a ZIP (hostile names mapped safely,
symlinks never stored as links). `docs/web-crypto.md` and `docs/sealed-delivery.md` have the details.

Tests: functional and tampering tests in Node and headless Chromium; every request, URL, header, body,
error message and console line is searched for key material with a negative control; a Playwright run as
the `restore` user listed and downloaded files with only ciphertext GETs [T].

### 18.2 What the browser cannot do itself: IBS2 authenticated delivery

Files from VM disks (`guest_files`) and host-side extraction (`fs_extract`) need the host (Incus, the
recovery VM). The browser generates a one-time X25519 key pair inside the module, sends only the public key
with the job, and the host streams the result sealed to it in **IBS2**: XChaCha20-Poly1305 frames under a
stream key derived from X25519 **and** an authentication key derived from the repository's `k_delivery`,
bound to the job id and the digest of the canonical job parameters, closed by a trailer with frame count,
length and a transcript hash. The server stores and relays opaque parts and can neither read nor forge,
alter, truncate, reorder or substitute a delivery, nor serve another job's or other parameters'
delivery [T: vectors, an independent TypeScript implementation, malicious-server cases]. IBS1 (unauthenticated)
is refused with a message to update the host.

### 18.3 Threat model and its honest limit

- Protected: payload confidentiality against a compromised datastore, database or server process that
  serves honest code; delivery integrity as above; snapshot identity (a manifest opened under another
  object's group, or a listing that relabels object B as A, is refused).
- **Not protected: whoever serves the JavaScript and WebAssembly can read the pasted key.** A compromised
  or malicious server (or a compromised build) can ship code that exfiltrates the key. The browser
  decryption protects against compromise of stored data and of the relay path, not against compromise of
  code delivery. Operators who do not accept that use the CLI on a host (`fs`, `restore guest-files`) and
  never paste the key into a browser.
- Any holder of a key domain's master key (every enrolled host of that repository, or the recovery key)
  can produce deliveries the browser accepts; IBS2 does not prove which host made one.
- Key hygiene limits: JavaScript strings cannot be wiped; the module zeroises its keys and its workers are
  terminated on lock or leave, but runtime copies, devtools, extensions and swap are outside the page's
  control.

## 19. Host jobs

The server never holds a payload key, so work that needs it (backup now, restore tests, deep verify,
restores, file recovery) runs on a host, queued by the management plane (`docs/jobs.md`):

- **Queue:** `host_jobs` in PostgreSQL; the host long-polls `GET /v1/jobs/next` over its client certificate
  and only ever sees its own jobs (others are 404). A claim is `FOR UPDATE SKIP LOCKED` [T: 8 pollers, 20
  jobs, each handed out once]; every claim increments `attempt` and every host write carries it (a revived
  stale attempt gets 409).
- **Leases:** a running job is a 5-minute lease renewed by progress reports every 30 s; an expired lease is
  re-queued while attempts remain, else failed; restores are never retried; queued jobs expire after 24 h.
- **Gating on the host:** `jobs.allow` / `jobs.disabled` (3.1). The executor re-validates parameters with
  the same strict `internal/jobspec` the server uses.
- **What reaches the server:** messages and log lines written by the executor; engine errors only when
  made of ids, object names and counts; no file name of a volume except typed recovery paths and their
  per-path status. Logs are capped (1000 lines per job).
- **Execution:** one heavy job at a time, never beside a scheduled run; up to two light jobs (`fs_extract`)
  beside it; cancellation reaches the engine and NBD streams; `backup_now` does not move the schedule.
- **Deliveries:** IBS2 streams (18.2) stored as 32 MiB parts under `deliveries/` of the repository's
  datastore, size and SHA-256 computed while streaming, handed out only after the job succeeded, deleted
  after one hour.

## 20. Certificates

`docs/certificates.md` is the full specification.

- A private CA (ECDSA P-256, 10 years, `MaxPathLen=0`) per server signs the server certificate and every
  host and sync-peer certificate (2 years). Clients pin the CA by fingerprint (from the token); host names
  are never checked.
- `host_certs` holds every certificate an identity was issued; a request is authenticated when the
  certificate chains to a trusted CA, is valid at the service clock, is a known non-revoked fingerprint and
  the identity is enabled.
- **Renewal** with less than 45 days left (or when the signing CA changed): hosts renew in the daemon (at
  start and daily, `incus-backup cert renew`), sync peers before each run, the server certificate in the
  daemon (every minute; SAN changes too) with hot reload of the listener. The protocol (`/v1/renew`,
  `/v1/cert`, `/v1/renew/confirm`) keeps the old certificate valid until the new one is confirmed, handles
  lost answers and concurrent renewals, and is crash-safe on disk [T, driven clock]. A certificate expired
  more than 30 days ago must enrol again.
- **CA rotation:** `ca rotate` (a new CA signs new certificates; both trusted; clients pick up the bundle on
  renewal), then `ca promote` once nothing is pending (`--force` cuts off the stragglers) [T, real TLS].
- Lifetimes and thresholds are compile-time constants (`internal/pki/lifecycle.go`); there is no runtime
  knob to shorten them for a test. **Unproven:** renewal over real weeks and rotation across machines.
- The web listener has its own certificate (`web.mode`: self-signed and renewed, files watched and
  reloaded, or ACME; 11). `cert_expiry` alerts and `incus_backup_certificate_days_left` cover all of them.

## 21. Maintenance modes and tasks

- **Repository modes** (`repo mode NAME online|read-only|offline --reason`, admin): `read-only` lets
  restores and browsing continue while every write (backups, retention, GC, verify state, quarantine, sync
  receive) is refused; `offline` refuses every datastore access. Enforced by `datastore.ModeBackend` below
  the object model. Hosts defer objects instead of failing (8); deferred runs neither raise nor resolve
  `backup_failed`, but `backup_missed` still fires if maintenance outlasts the schedule. Detach and destroy
  need `offline`.
- **Tasks** (`internal/server/tasks`): every long server-side operation (manual and scheduled GC,
  verify/scrub, retention, reconcile, capacity samples, sync runs, probes, repository destroys, object
  deletes) is a task with state, progress, a log (at most 5000 lines), a result and cancel. The API answers
  **202** with the task; the CLI follows or waits. One **exclusive** (destructive) task per repository,
  enforced by a partial unique index; a second gets 409 naming the first; the scheduler skips a busy
  repository and retries the next minute. Cancelling stops at the next check (a GC between sweep batches:
  nothing half-deleted [T]); a cancelled run does not raise `maintenance_failed`. Tasks left running by a
  previous daemon are marked failed at start. History: 30 days, at most 2000 tasks.
- **Default schedules** (`maintenance set`): capacity `every 1h`, reconcile `daily`, prune `04:00`, gc
  `05:00`, verify `daily`, scrub `monthly`, GC grace 24 h.
- **Server health:** `GET /system/status` and samples (load, CPU, memory, disks of the state directory and
  filesystem repositories, PostgreSQL, listeners, clock) every minute for 7 days; the daemon's last 5000 log
  lines (redacted) in the UI; a redacted support report; an opt-in update check (off by default; the server
  contacts nothing until enabled).

## 22. Notifications

`docs/notifications.md` is the full specification. The engine (`internal/server/notify`) works like a
monitoring system:

- An **alert** is a state with a stable key that fires, stays open while its condition holds and resolves
  by itself. Conditions are evaluated every minute over the control plane: `backup_missed`,
  `backup_failed`, `host_offline`, `maintenance_failed`, `corruption`, `sync_failed`, `sync_lag`,
  `capacity_warning`, `repository_unreachable`, `cert_expiry`, `restore_test_failed`; events
  (`login_security`, `job_failed`, `update_available`, ...) are emitted by other parts of the server.
- **Rules** route alerts to **channels** (SMTP, webhook in JSON/Slack/Discord form with an optional HMAC
  signature, ntfy, Gotify) with severity and glob filters, reminders, quiet hours (critical bypass),
  escalation, "resolved" messages, dedupe, flapping detection, silences and a daily digest.
- **Delivery** is a durable queue in PostgreSQL: retries with backoff, dead letters, per-channel rate
  limits, at-least-once, exactly one evaluator at a time (advisory lock). Channel secrets are sealed;
  SMTP never authenticates without TLS; HTTP channels never follow redirects.
- Thresholds are data (`notify_conditions`, `notify_settings`), changed through the API/UI; there is no
  configuration file and no CLI for channels and rules.
- **Unproven:** delivery to a real SMTP server, ntfy and Gotify instance (webhook delivery was exercised
  end to end).

## 23. Traffic control

`docs/traffic.md` is the full specification. Rules (`traffic_rules`, admin) limit chunk and index uploads
(`upload`) and downloads (`download`) of hosts on the agent API with token buckets: per matching host
(`scope: host`) or shared (`scope: global`), host globs, optional weekly windows in an IANA zone,
strictest rule wins, fair queuing, waits honour the request context, rules re-read every 30 s, and a
database failure never stops a backup (the previous rules stay). `GET /traffic/current` shows live rates.
Sync jobs have their own `rate_limit`. Small JSON calls are never limited; there is no client-side limit.
Measured within the ±20 % tolerance on loopback [T]; a real network is unproven.

## 24. Performance architecture

`docs/performance.md` has the measurements. What is parallel, and what bounds it:

| path | parallel | bound (default) |
|---|---|---|
| VM capture | NBD reads pipelined: up to 16 requests in flight per connection (`nbd.DefaultDepth`), 8 chunks per batch; replies demultiplexed by handle | read-ahead 8 chunks; encode queue 2 x workers |
| encode (HMAC, zstd, XChaCha20, trailer) | `encode_workers` goroutines, one zstd encoder each (pooled) | 4 |
| upload | `upload_workers` concurrent PUTs per target over one HTTP/2 connection (32 MiB connection / 8 MiB stream windows on the server); "which are missing" questions pipelined with uploads | 16 workers; batches of 2000 ids, 2 in flight |
| filesystem scan | lstat/xattr/readlink on a worker pool, one in-order assembly pass (byte-identical result [T]) | 16 workers, 64k entries ahead |
| datastore requests (Missing, Commit checks, GC, verify, listings) | bounded parallel requests per operation | 64 (`DefaultRequestParallelism`) |
| `chunks/` listing | sharded by first-level prefix, delivered in exact key order | parallelism shards and 512k buffered keys |
| verify | read-ahead worker pool, results recorded in sequential order (identical to a sequential job [T]) | 16 blobs |
| GC | parallel mark; sweep in batches of 64 under the write lock, re-checking pins and age | `MarkIndexBudget` 256 MiB |
| PostgreSQL | indexes for every hot query; reconcile prefetches; `last_seen_at` written at most once a minute per host | — |

### 24.1 Invariants the parallelism keeps

- Parallel results are identical to sequential ones (order, duplicates, quarantine, records) [T].
- The GC read lock is taken once per caller, never by workers (a recursive RLock deadlocks once GC waits).
- `Commit` keeps "pin every chunk first, then check every chunk at the backend".

### 24.2 Presence set invariants (S3 only)

- It holds **positives only**; an id not in it is asked of the backend; a "not found" evicts it.
- Deletions by this process remove the id **before** the object is deleted; answers older than a removal
  are dropped (a generation counter); a re-list applies concurrent deletes as tombstones [T].
- It cannot know deletions by someone else, so **only Missing uses it; Commit never does**. A stale
  positive costs one refused commit (409) and one retry; it can never make a snapshot with a missing chunk
  visible [T, MinIO with a plain S3 client deleting behind the server].
- Bounds: built in the background on first use, re-listed every 6 h while used, 80 bytes per id, at most
  2 Mi ids (a larger repository does without it).

## 25. Memory model

`docs/memory.md` has the measurements (`scripts/membench.sh`). Rule: backup, restore, verification,
synchronisation and GC are streamed; their buffers are sized by configuration (workers, chunk size, batch
sizes, budgets), never by the size of an object. What grows with an object or a repository is metadata,
never payload, and is listed with measured bytes per entry: the index (40 B per 4 MiB chunk), distinct chunk
ids (~110 B peak), session pins (~30 B per chunk), the GC live set (~15 B per chunk), the verify cache
(capped at 1 M chunks), and the catalog tree (~380 B per file; ~610 B during an incremental backup).
Measured: block paths stayed at <= 445 MiB peak RSS for client and server together from 1 GiB to 4 TiB disks
[T, sandbox]; the filesystem catalog path crosses 512 MiB around 1 M files in an incremental backup. The
spool is disk, not memory. Memory under real load on real hardware is unproven (real-host phase 17).
