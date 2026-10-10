# Proxmox Backup Server compared with incus-backup

> **Implementation status (2026-10-08).** This document is the research record. Its ranked adopt-list was implemented
> and tested; the comparison text below describes the code as it was when it was written. Adopted: (1) corrupt-chunk
> quarantine, failed snapshots never a base, healing, sync `resync-corrupt`; (2) cumulative PBS retention with per-policy
> time zones and an exact preview; (3) browsing and single-file restore (catalog; VM disks through an isolated recovery
> VM); (4) metadata change detection with ctime/inode and a periodic full-data run; (5) design/format reconciled, `gc_pins`
> replaced by the session `base_snapshot_id` pin plus in-memory session pins; (6) one writer per group in PostgreSQL and
> renewable leases instead of a fixed 6 h TTL; protected snapshots, notes, per-snapshot verify state with
> verify-new / re-verify-after-N-days; sync `verified-only`, `transfer-last`, rate limit; S3 without COPY-onto-self;
> repository ownership (`owner.json`); offline escrow key. Still not adopted (by decision): namespaces, tape, maintenance
> modes, owner-per-group (single key domain per repository makes it an integrity nicety). See `docs/design.md` section 17.

Status: research note (2026-10-08). Read-only study of PBS 4.2.8 (`proxmox-backup` at commit
`78f77d53`, plus `proxmox-backup-qemu` and `proxmox-backup-restore-image` in `temp/`), compared
with `design.md`, `format.md` and the code in `internal/`. Nothing here changes the contract;
items marked **adopt** are proposals.

Path abbreviations: `PB/` = `temp/proxmox-backup/`, `PQ/` = `temp/proxmox-backup-qemu/`,
`DS` = `PB/pbs-datastore/src/datastore.rs`, `CS` = `PB/pbs-datastore/src/chunk_store.rs`,
`ENV` = `PB/src/api2/backup/environment.rs`, `MOD` = `PB/src/api2/backup/mod.rs`,
`BW` = `PB/pbs-client/src/backup_writer.rs`, `CR` = `PB/pbs-client/src/pxar/create.rs`.
The pxar crate itself is not in the tree (`pxar = "1.0.1"`, `PB/Cargo.toml`), so pxar internals
are taken from `PB/docs/file-formats.rst` and from how the client calls it.

## 0. The `[M]` claims in design.md, settled

| design.md says [M] | PBS source says |
|---|---|
| Fixed 4 MiB chunks for block objects (4.4) | True. `FIXED_CHUNK_SIZE = 4096*1024` (`PB/pbs-datastore/src/fixed_index.rs:20`); the server always uses it (`MOD:create_fixed_index`); the store accepts 64 KiB..4 MiB (`CS:verify_chunk_size`). |
| GC uses atime marking (10) | True. Phase 1 `utimensat(UTIME_NOW)` on every referenced chunk (`CS:cond_touch_path`, `DS:index_mark_used_chunks`); phase 2 deletes chunks with atime < cutoff (`CS:sweep_unused_chunks`, `cond_sweep_chunk`). Cutoff = 24 h 5 min, moved back to *oldest live writer − 300 s* (`DS:garbage_collection_impl`). |
| HTTP/2 upgrade (8) | True. `GET /api2/json/backup` with `Upgrade: proxmox-backup-protocol-v1`, then H2 on the same connection (`BW:BackupWriter::start`, `MOD:upgrade_to_backup_protocol`). |
| pxar-style purpose-built archive (5) | True, and since PBS 3.2 a split metadata/payload variant exists for change detection (section 3). |

## 1. Datastore and chunk store

**Layout.** `<base>/.chunks/<4 hex>/<64 hex digest>` with all 65 536 prefix dirs created up front
(`CS:create`, `digest_to_prefix`). Snapshots live at `<base>[/ns/<n>...]/<type>/<id>/<RFC3339 time>/`
holding `*.fidx`, `*.didx`, `*.blob` and the manifest `index.json.blob`. The group dir holds an
`owner` file and optional `notes`; a snapshot can hold `.protected` (`PB/pbs-datastore/src/backup_info.rs:31`).

**Chunk insert** (`CS:insert_chunk_nolock`), under one store mutex:
- existing file with the same size: touch atime, report duplicate;
- existing **zero-length** file: overwrite (heals a crash mid-write; also used as S3 cache marker);
- encrypted chunk arriving over an unencrypted one with the same digest: refused (attack/collision);
- both encrypted and uncompressed but different sizes: refused; otherwise keep the smaller one;
- write is `replace_file` (tmp + rename); fsync per chunk only with `sync-level=file`.

**Durability.** Default `sync-level=filesystem`: one `syncfs(2)` at the end of each backup, before
the task reports OK (`ENV:finish_backup` -> `DS:try_ensure_sync_level`; `PB/docs/storage.rst:638-660`).

**Indexes.** `.fidx`: 4096-byte header (magic, uuid, ctime, sha256 over the digest array, size,
chunk size) + 32-byte digests. `.didx`: header + (end offset, digest) pairs. Both are written as
`*.tmp_fidx` / `*.tmp_didx` and renamed on close (`fixed_index.rs:FixedIndexWriter::create/close`,
`dynamic_index.rs:close`). Our single `IBIX` index with a `kind` byte covers both.

**Snapshot visibility.** There is **no tmp dir and no rename** at snapshot level. The final directory
is created at session start (`DS:create_locked_backup_dir`); a snapshot counts as *finished* once
`index.json.blob` exists (`backup_info.rs:BackupInfo::is_finished`). If a session ends unfinished,
the server deletes the dir (`MOD:upgrade_to_backup_protocol`, end-of-session `remove_backup_dir(force)`);
after a crash the half-written dir stays, and prune deletes old unfinished ones unless newer than the
last good one (`PB/pbs-datastore/src/prune.rs`, `keep_unfinished`; `PB/docs/maintenance.rst:72`).

**Manifest is mutable.** Verify state, notes and upload stats live in the manifest's `unprotected`
JSON (not covered by the signature) and are rewritten under a manifest lock by atomic replace
(`backup_info.rs:update_manifest`). Our manifest is encrypted and immutable; this state belongs in
`commit.json`-adjacent files or PostgreSQL.

**Locking.** All locks are flock files under `/run/proxmox-backup/locks/<store>/...`
(`backup_info.rs:DATASTORE_LOCKS_DIR`): exclusive group lock and exclusive snapshot lock for the
writer, a **shared lock on the previous snapshot** so prune cannot remove the base during the backup
(`MOD`, `last.backup_dir.lock_shared()`), per-chunk locks for S3 (`CS:lock_chunk`), and a
process-wide chunk-store lock: writers hold it shared, GC holds it exclusive-ish via `try_exclusive_lock`
only to refuse running next to an *old daemon* whose writer start times are unknown (`DS:garbage_collection`).
`oldest_writer()` (`CS:oldest_writer`) returns the start time of the oldest shared holder.

**GC** (`DS:garbage_collection_impl`):
1. Refuse if the datastore is nested in another one (`ensure_not_nested`).
2. **atime safety check**: insert/touch a 4 MiB zero chunk, sleep 1 s, touch again, require that
   atime moved (`CS:check_fs_atime_updates`). Aborts GC on filesystems that ignore atime updates.
3. cutoff = phase-1 start − 24 h 5 min (tunable `gc-atime-cutoff`); if the oldest writer is older,
   cutoff = oldest writer − 300 s. The 24 h comes from `relatime` semantics (`maintenance.rst:247`).
4. Phase 1 (`DS:mark_used_chunks`): first walk the whole tree for *every* `.fidx/.didx`, then iterate
   namespaces/groups/snapshots **newest snapshot first**, retrying a group up to 10 times and then
   taking the group lock when the newest snapshot vanished (race with backup+prune of the previous
   last snapshot). Index files found by the walk but not by the iteration are still marked, with a
   warning. A move journal covers renames during phase 1 (`PB/pbs-datastore/src/move_journal.rs`).
   An LRU (1 M digests) avoids repeated `utimensat`. EPERM anywhere aborts GC ("might prune too many").
5. Phase 2: iterate `.chunks`, delete if atime < cutoff, count "pending" if < oldest writer.
   `.bad` copies are kept while their digest is still referenced (`CS:cond_touch_bad_chunks`).

**Verify** (`PB/src/backup/verify.rs`): per snapshot, decode every chunk (CRC, decompress, and for
unencrypted chunks the digest), shared sets of already-verified and corrupt digests across the job,
result written into the manifest as `verify_state`. A corrupt chunk is **renamed to
`<digest>.<n>.bad`** (`DS:rename_corrupt_chunk`), so it is "missing" from then on: the next backup that
produces that digest uploads it again and the datastore heals itself. Verify jobs can skip snapshots
verified recently (`verify_filter`, `ignore_verified`, `outdated_after`); `verify-new` verifies each
snapshot right after the backup (`ENV:verify_after_complete`).

**Prune** (`prune.rs:compute_prune_info`, `mark_selections`): rules in order last, hourly, daily,
weekly, monthly, yearly; **a period already covered by a snapshot kept by an earlier rule does not use
up a slot of the later rule** (`already_included`), and periods are computed in **local time**
(`strftime_local`). Protected snapshots are never removed. Prune removes snapshot dirs only; GC frees
chunks.

## 2. Backup protocol and incremental reuse

**Session start** (`MOD:upgrade_to_backup_protocol`): privilege check on the namespace; group created
and exclusively locked (0 s timeout, so one writer per group); **owner check** (owner file written with
`create_new` on first backup, only the owner or its tokens may add snapshots); previous = newest
*finished* snapshot, **dropped if its manifest `verify_state` is `failed`** (no fallback to an older
one); new backup time must be strictly greater than that previous one; shared lock on previous.
HTTP/2 with 32 MiB windows.

**Writers** (`ENV`): up to 256 fixed/dynamic writers per session, each closed with chunk count, size and
the client's index checksum, all re-checked by the server. `finish` (`ENV:finish_backup`) requires no
open writers, at least one file, **the base snapshot still exists**, a manifest whose every file was
uploaded in this session with a matching checksum; then writes the manifest, `syncfs`, drops locks.

**Known chunks.** The server keeps an in-memory whitelist per session: chunks uploaded in the session
plus every digest of any previous index the client *downloaded* via `previous` (`MOD:download_previous`
-> `ENV:register_chunk`). Appending a digest not on the list fails with "no such chunk"
(`ENV:lookup_chunk`). The server never stats or touches reused chunks; their safety rests on the
shared lock of the previous snapshot (no prune) plus the oldest-writer GC cutoff.
Client side (`BW:download_previous_fixed_index/_dynamic_index`): the index is downloaded, its checksum
and size **recomputed locally** ("not trusted") and checked against the previous manifest, then all
digests become known and are never uploaded again.

**Chunk upload** (`PB/src/api2/backup/upload_chunk.rs`): body must equal the announced encoded size;
the server re-checks the CRC and, for unencrypted chunks only, decodes and checks the digest
(`data_blob.rs:verify_unencrypted`).

**Fixed-index reuse for VMs** (`MOD:create_fixed_index`, `reuse-csum`): if the client sends the
previous index checksum and it matches, the server copies the whole previous digest array
(`FixedIndexWriter::clone_data_from`) and the client appends only the changed positions.

**proxmox-backup-qemu** (`PQ/src/commands.rs`): QEMU owns the dirty bitmap; this library only decides
whether the bitmap may be trusted. `check_incremental` requires (a) the server's last manifest lists
the device with **the index checksum this same QEMU process wrote last time** (process-global
`PREVIOUS_CSUMS`, carried across live migration via `proxmox_export_state`), (b) same encryption mode,
(c) same key fingerprint as cached locally. Any mismatch -> full backup (QEMU then ignores/resets the
bitmap [M: QEMU side, not in the tree]).
`register_image` re-verifies count = ceil(size/4 MiB) and the checksum before using the previous index.
A zero chunk is always uploaded first so its digest is known; `write_data(NULL)` means zero chunk.
Only a full-size chunk may be a zero chunk; the last chunk may be short.

## 3. Host filesystems ("storage volumes"): pxar and change detection

**pxar** (`PB/pbs-client/src/pxar/mod.rs`, `CR:get_metadata`, `CR:add_entry_to_archive`): ENTRY
(mode, chattr flags, uid, gid, mtime ns) + xattrs, POSIX ACLs (access and default), file
capabilities, quota project id, then payload/symlink/device; directories end in a GOODBYE hash table
for random access. No atime/ctime/inode stored. Hardlinks: second occurrence is a HARDLINK record with
the offset of the first ENTRY. Sparse files are read fully; holes are recreated only on extract
(`extract.rs:sparse_copy`). Mount points are stored as empty dirs unless `--all-file-systems` /
`--include-dev`; `.pxarexclude` per directory; virtual filesystems skipped; nodump **not** honoured
(flag defined, never used).

**Chunker** (`PB/pbs-datastore/src/chunker.rs`): Buzhash (casync port), 64-byte window, average
4 MiB (min avg/4, max avg*4). `PayloadChunker` accepts *suggested* boundaries at file starts if the
resulting chunk stays within min/max.

**Change detection modes** (`backup_specification.rs:BackupDetectionMode`):
- *legacy* (default): one `.pxar.didx` + `catalog.pcat1.didx`; reads every byte, dedups by chunk.
- *data*: split archive `.mpxar.didx` (metadata, files carry `PAYLOAD_REF{offset,size}`) +
  `.ppxar.didx` (payloads); still reads every byte.
- *metadata*: like data, but uses the previous snapshot's `.mpxar` as reference
  (`proxmox-backup-client/src/main.rs:prepare_reference`). Only if the previous snapshot is split,
  same crypt mode and validly signed; otherwise full read.

**Metadata-mode reuse** (`CR:is_reusable_entry`): a regular file is reusable when the whole pxar
metadata (mode, flags, uid, gid, **mtime ns**, xattrs, ACLs, fcaps, project id) and the size equal the
previous entry. **ctime and inode are deliberately not compared** (`technical-overview.rst:214-222`:
vzdump-style staging changes them every time). Reusable files are queued in a look-ahead cache of open
fds (`look_ahead_cache.rs`, default 512, raised to the fd limit) as long as their old payloads form
one contiguous range. On flush, the chunks of the previous `.ppxar` covering the range are looked up;
if `padding / (range + padding) > 0.1` (`CHUNK_PADDING_THRESHOLD`), everything is re-read and
re-encoded; otherwise the files become `PAYLOAD_REF`s and the old chunks are **injected** into the new
payload stream without reading the data (`CR:inject_chunks_at_current_payload_position`,
`PB/pbs-client/src/inject_reused_chunks.rs`): the chunk stream is cut at the injection point, the
chunker is reset, and the server only receives digests to append. Wasted "padding" bytes from
partially reused chunks are stored as dead content inside the new archive.

**Catalog** (`PB/pbs-datastore/src/catalog.rs:CatalogWriter`): post-order directory tables with name,
type, size and mtime (seconds), uploaded as its own dynamic archive (512 KiB chunks). Used by
`catalog shell`, the web file browser and file-restore. **Split archives do not write it**: the
`.mpxar` itself is the browsable catalog.

**Block level.** `name.img:<file|blockdev>` produces a `.img.fidx` by reading the whole device
(`main.rs:backup_image`); incrementality is only known-chunk dedup (read everything, upload little).
There is **no dirty-bitmap or snapshot-diff path outside QEMU**; grep finds no "bitmap" in the Rust
client.

Documented pitfalls: metadata mode reuses a changed file whose size and mtime were preserved or forged
(`technical-overview.rst:204-207`); mixing legacy and split archives dedups badly (153-155); the
padding threshold is described as "10 % of reused content" but computed over range + padding.

## 4. Encryption, namespaces, sync, retention, S3, tape

**Keys** (`PB/pbs-key-config/src/lib.rs:KeyConfig::with_key/decrypt`): 32-byte AES key wrapped with
AES-256-GCM under scrypt (n=65536, r=8, p=1) or PBKDF2; `kdf: None` = plaintext key file for
unattended use. Fingerprint = keyed hash of a constant (`PB/pbs-tools/src/crypt_config.rs:fingerprint`),
stored in the manifest so a client can tell "wrong key" from "corrupt".
**Chunk id** = `SHA256(plaintext || id_key)` with `id_key = PBKDF2(enc_key, "_id_key", 10)`
(`crypt_config.rs:CryptConfig::new/compute_digest`); unencrypted chunks use plain SHA-256.
**Blob** (`PB/pbs-datastore/src/file_formats.rs`, `data_blob.rs:encode`): 8-byte magic | CRC32 |
[IV 16 | GCM tag 16] | zstd-then-AES-GCM data, **empty AAD** (`crypt_config.rs:data_crypter`), so the
digest is not bound into the ciphertext; the client re-hashes after decrypt instead.
**Master key**: optional RSA public key on the client; every backup uploads `rsa-encrypted.key.blob`
(the session key wrapped with RSA PKCS#1 v1.5, `pbs-key-config:rsa_encrypt_key_config`), so the
holder of the offline RSA private key can restore everything. `paperkey.rs:generate_paper_key` prints
keys as text/QR. **Manifest**: plaintext JSON, HMAC-SHA256 signature over everything except
`unprotected` (`manifest.rs:json_signature/check_signature`). The server sees archive names, sizes and
checksums of every snapshot.
**Keyless server checks**: CRC32 on load, index/blob sha256 and size against the manifest, crypt-mode
consistency. Nothing about encrypted chunk contents; on S3 even the CRC is skipped in verify
(`verify.rs`, `DataBlob::from_raw`).

**Namespaces**: `<store>/ns/<a>/ns/<b>/...`, depth up to 7 below root, ACLs per namespace path with
optional propagation (`PB/src/backup/hierarchy.rs:check_ns_privs_full`; `docs/storage.rst:515`). Each
backup group has one owner (user or token).

**Sync** (`PB/src/server/pull.rs`, `push.rs`, `sync.rs`). Pull: list remote snapshots, drop in-progress
ones (`sync.rs:filter_out_in_progress`), skip anything older than the local last successful snapshot
but always re-sync the newest; seed a "known" set with all chunks of the last local snapshot unless it
failed verify; per remote index, `cond_touch_chunk` each digest and download only missing ones
(`pull.rs:pull_index_chunks`, 20 downloads, 4 writer threads that run `verify_unencrypted`+`insert_chunk`).
Archives go to `*.tmp` -> checksum vs manifest -> rename; the manifest is renamed into place last.
Unchanged snapshots are detected by comparing raw manifest bytes. Options: `transfer-last N`,
`remove-vanished` (protected snapshots kept), `encrypted-only`, `verified-only`, `resync-corrupt`
(re-pull snapshots whose local verify failed), group filters, `rate-in`/`rate-out`, and optional
decryption or encryption while syncing. Push reuses the normal backup-writer protocol on the remote
(`push.rs`, `BackupWriter::start`), so pushed snapshots are owned by the remote user and anything not
newer than the target's newest snapshot is skipped.

**S3** (`PB/pbs-datastore/src/s3.rs`, `DS`): chunks at `<store>/.chunks/<4 hex>/<digest>`, metadata
under `.cnt/`. A **mandatory local cache directory** holds snapshot metadata and an LRU of chunks;
uploads use put-if-absent (`upload_no_replace_with_retry`) under a per-chunk flock. GC never touches S3
objects: phase 1 bumps atime on **local zero-size marker files**; phase 2 lists the bucket and deletes
objects whose local marker is older than the cutoff (batched, chunk locks held); phase 3 sweeps
markers (`DS:garbage_collection_impl`). A `.in-use` object with the hostname prevents two PBS instances
from adopting one bucket (`PB/src/api2/config/datastore.rs`); `s3-refresh` rebuilds local metadata from
the bucket.

**Tape** (`PB/src/tape/`): chunk archives + snapshot archives per media set, a media catalog
(digest -> file number), LTO hardware encryption with keys in a local JSON. Not relevant to v1.

## 5. Restore paths

- **Whole image / archive**: `proxmox-backup-client restore` (`dump_image`), or for VMs the qemu
  library: `proxmox_restore_image` (sequential, zero chunks skipped via the zero digest) and
  `read_image_at[_async]` for random access through a 64-chunk shared LRU (`PQ/src/restore.rs`,
  `shared_cache.rs`). **Live restore**: QEMU boots the VM on a PBS-backed block device that reads
  chunks on demand while the disk is copied in the background [M: only the `read_image_at_async` API
and a `PQ/debian/changelog` entry are in the tree; the QEMU/qemu-server side is not].
- **Map an image**: `proxmox-backup-client map` exposes a `.img.fidx` as a FUSE file and attaches a
  loop device (`PB/pbs-fuse-loop/src/fuse_loop.rs`, `loopdev.rs`).
- **Mount a file archive**: FUSE over a pxar accessor (`PB/pbs-pxar-fuse/src/lib.rs:Session::mount`).
- **Single-file restore from VM disks** (`PB/proxmox-file-restore`): boots a tiny QEMU VM (kernel +
  initramfs from `proxmox-backup-restore-image`, ZFS built in) whose disks are the backup images via
  the QEMU `pbs` block driver, read-only; talks to `proxmox-restore-daemon` over vsock with a ticket.
  The daemon mounts ext4 (noload), xfs (norecovery), btrfs, ntfs3, ..., imports ZFS pools and activates
  LVM, then lists and extracts files as zip/tar.zst (`proxmox-restore-daemon/src/proxmox_restore_daemon/disk.rs`,
  `api.rs`). The VM isolates the host kernel from hostile guest filesystems.

## 6. What PBS does that we do not, ranked by value for Incus

1. **Quarantine corrupt chunks so the next backup heals them.** PBS renames a corrupt chunk to
   `.bad` (`DS:rename_corrupt_chunk`), so it is "missing" and is uploaded again; it also refuses to
   use a verify-failed snapshot as incremental base (`MOD`, `VerifyState::Failed => None`) and pull
   can `resync-corrupt`. Ours: `Store.Verify` only *reports* `chunk-corrupt`
   (`internal/datastore/maintenance.go`), and `Missing`/`PutChunk` treat any existing object as good
   (`internal/datastore/store.go`). A bad blob therefore looks present forever: every later backup
   that produces the same id skips the upload, and the VM path keeps patching onto it. design.md 9
   promises "client re-uploads the chunk", which the code cannot do today. **Adopt**: verify moves a
   blob with a bad trailer to `chunks-bad/` (keyless), records it in PostgreSQL and marks affected
   snapshots; the client's `restore-test run`/`verify` (which checks the AEAD and HMAC id) reports ids
   the server then quarantines; the client forces a full (deduplicated) read of any object whose base
   snapshot is marked bad.
2. **Retention semantics that match what operators expect from PBS.** `internal/retention/retention.go`
   claims PBS semantics but evaluates every rule independently over the same newest periods and in
   UTC. PBS (`prune.rs:mark_selections`, `already_included`) lets each later rule start where the
   earlier ones stopped, and uses local time. Example with one backup a day: `keep-last 3, keep-daily 7`
   keeps 7 days with us and 10 with PBS. **Adopt** the cumulative rule (or rename the claim), decide the
   timezone explicitly (server local time or a per-policy zone), and add a "prune simulator" style
   dry-run (`docs/maintenance.rst:76`) on top of the existing preview.
3. **Single-file restore and browsing.** PBS can browse and extract single files from file archives
   (catalog or `.mpxar`) and from VM disks (restore VM). We can only restore a whole volume or
   instance (`internal/client/engine/restore.go`). **Adopt in two steps**: (a) client-side `ls`/`extract`
   for filesystem objects by streaming our tar index (needs a small per-snapshot file catalog with
   archive offsets, encrypted like the manifest, so the server still learns no paths); (b) for VM
   disks, `incus-backup mount` that serves a snapshot as a read-only NBD export from the chunk store,
   attached to a throwaway Incus VM (the PBS isolation idea without building our own restore image).
4. **Metadata change detection for filesystem objects** (design.md 5 "Incremental", block-level
   research option 2). PBS's design shows what matters: compare full metadata (mode, uid/gid, mtime ns,
   xattrs, ACLs, size). PBS deliberately skips ctime and inode because vzdump-style staging copies
   change them every run. **That reason does not apply to us**: we read successive ZFS snapshots of the
   same dataset, where an unchanged file should keep its inode number and ctime (only `st_dev`
   differs). So we should also compare `ctime_ns` and `inode`, as block-level research option 2 already
   lists. That closes PBS's main documented hole (content changed, size and mtime preserved) [O: confirm
   ctime/inode stability across Incus ZFS snapshots on the real host]. Then reuse *whole old chunks* by injecting them into the new stream with a forced chunker cut and reset;
   re-encode when wasted padding exceeds a threshold (10 %); fall back to a full read when the base is
   missing, of a different format, unsigned, or failed verify. Our tar stream would need the payloads
   separated from headers (PBS split `.mpxar`/`.ppxar`) or per-file chunk boundaries for this to work.
   Also copy PBS's honesty: metadata mode misses content changes with preserved size+mtime, so offer a
   periodic data-mode run (PBS documents the risk but does not schedule it; we should).
5. **Hold the base snapshot during a backup (low value for us, listed here only for completeness).**
   It cannot corrupt a new snapshot: `Commit` stats every chunk the session did not see, and
   `retention.Plan` always keeps the newest snapshot. PBS takes a shared lock on the previous snapshot so
   prune/forget cannot remove it mid-backup, and `finish` fails if it vanished (`ENV:finish_backup`).
   We rely on `Commit` stat-ing every reused chunk (stronger for chunks), but a prune between base
   selection and commit still removes the base snapshot itself. Cheap to add as a row in a pin table.
   Note: design.md 10/16 describe `gc_pins`, but no such table or code exists in `migrations/` or
   `internal/`; GC safety today comes from `grace >= 2*SessionTTL` with a fixed cutoff, which is
   sound (cutoff fixed at GC start, touch-if-older-than-1 h, sessions capped at 6 h and never
   renewed). Either implement pins or remove them from the design.
6. **Snapshot protection and notes.** `.protected` (never pruned, `backup_info.rs:destroy`) and
   per-snapshot/group notes. Useful for "keep the pre-upgrade backup"; both belong in PostgreSQL plus a
   marker file in the snapshot dir so a rebuilt control plane still honours protection.
7. **Verify scheduling with state**: per-snapshot verify result, skip recently verified, re-verify after
   N days (`verify.rs:verify_filter`), verify right after backup (`verify-new`), and a shared
   verified-chunk set so a job reads each chunk once. Our `Verify` re-reads every chunk every time and
   keeps no per-snapshot state.
8. **Owner per group and monotonic snapshot time.** PBS refuses a snapshot from another owner and any
   time not newer than the last one (`MOD`). We bound future skew (`BeginSession`, `MaxClockSkew`) and
   pin the retention policy, but a backup-scope host can still create a snapshot in a group that was
   first written by another host (`storage_objects ... ON CONFLICT DO UPDATE SET host_id=...`). With one
   key domain per repository that is mostly an integrity concern (a compromised host can add a
   misleading newest restore point for someone else's object). **Adopt with care**: "owner = first
   host, moving it is an admin action" would turn every instance migration between cluster members
   (one client per member, design.md 15.6) into an admin task. Make the owner a host *group* (the
   cluster), with single hosts as groups of one.
9. **Sync options**: `transfer-last`, `verified-only`, `resync-corrupt`, group filters and rate limits.
   Our engine copies everything committed. `verified-only` and rate limits matter most for WAN mirrors.
10. **S3 GC without touching objects.** PBS keeps a local marker per chunk and never rewrites S3
    objects; our `S3Backend.Touch` copies the object onto itself (`internal/datastore/backend_s3.go`),
    which costs one COPY request per touched chunk (at most hourly per chunk, thanks to `touchAfter`)
    and on some providers creates a new version or resets storage-class transitions. Consider an index
    of last-seen times in PostgreSQL or a local marker store, with the datastore listing still
    authoritative. Also adopt the `.in-use` bucket marker so two servers never adopt one bucket.
11. **Offline master key (RSA "escrow")** and a paper/QR export of the key. We have a printable recovery
    key, which covers the main case; an asymmetric escrow lets a host back up without being able to
    *restore* other hosts' data, which matters if hosts ever get separate key domains in one repository.
12. **Maintenance modes** (`read-only`, `offline`, `docs/maintenance.rst:338`) and **namespaces**. Our
    projects/hosts already give structure; namespaces only matter if several tenants share a server.

Not worth adopting: tape; the 65 536 pre-created chunk dirs (slow `create`, `verify_chunkstore`);
atime-based marking (see 7); legacy single-stream pxar plus a separate catalog (PBS itself moved away).

## 7. Where our design is better

- **Commit marker written last, nothing visible before.** PBS creates the final snapshot dir at session
  start and treats "manifest exists" as finished; a crash leaves half-written dirs that prune and sync
  must filter (`keep_unfinished`, `filter_out_in_progress`). Ours writes `index`, `manifest`, then
  `commit.json` create-if-absent, and readers ignore anything without it.
- **Integrity without the key.** Our blobs carry a SHA-256 trailer (format.md 4); PBS has CRC32 only,
  and its S3 verify path skips even that. Our AEAD also binds the chunk id in the AAD; PBS uses empty
  AAD and relies on re-hashing after decryption.
- **Server learns less.** PBS manifests are plaintext (archive names, sizes, file list); ours are
  encrypted and group ids are HMACs. PBS chunk ids for encrypted data are keyed like ours.
- **GC marking does not depend on atime.** PBS needs `relatime` reasoning, a 24 h 5 min cutoff and a
  runtime atime self-test (`CS:check_fs_atime_updates`) because some filesystems ignore atime. We touch
  mtime, which every filesystem and S3 honour. We also abort GC on any unreadable index, as PBS does on EPERM.
- **Commit checks reused chunks.** PBS never stats reused chunks (whitelist + locks only); our `Commit`
  stats every chunk not seen in the session and refuses with `ErrMissingChunks`.
- **Durable acks.** `FSBackend.Put` is tmp + fsync + link/rename + parent-dir fsync for every object,
  stronger than PBS's default single `syncfs`. This matters because our client deletes bitmap B1 on the ack.
- **Admin credential never on a host; retention set by the server.** In PBS a backup user with
  `Datastore.Prune` on its own groups can prune; the "backup only" role exists but is a configuration
  choice, not a structural property.
- **Dirty-bitmap trust is explicit and multi-target.** PBS trusts a bitmap only if the server's last
  index checksum equals the one *this QEMU process* wrote (`PQ:check_last_incremental_csum`); a backup
  to a second PBS or a restart forces a full read. Our generations (`ib-<snapshot id>`, oldest committed
  base across targets, design.md 3.2.1) handle several targets without full reads.
- **Block-level for containers is not possible in PBS either.** Its only non-QEMU block path reads the
  whole device (`backup_image`); our planned zvol send-range or `zfs diff` accelerators (block-level
  research options 3 and 4) go beyond PBS.

## 8. PBS pitfalls to avoid, and doc/code mismatches found on our side

- **Mismatch: snapshot commit.** format.md 8 and design.md 7.1/8 say snapshots are built in `tmp/` and
  renamed, with a `.complete` file. The code (`internal/datastore/store.go:Session.Commit`) writes into
  `groups/` and uses `commit.json` as the marker (which also works on S3). The docs should follow the code.
- **Mismatch: S3 and sync are listed as v1 non-goals in design.md 1** but `backend_s3.go` and
  `internal/server/syncer` exist.
- **Mismatch: `gc_pins`** (see 6.5) and **retention "follows PBS"** (see 6.2).
- PBS metadata mode silently misses changes with preserved size and mtime; mixing archive formats in
  one datastore dedups badly. If we add metadata mode, version the archive format per snapshot and
  schedule a periodic full read.
- PBS keeps the *larger or first* copy of a duplicate chunk and cannot check encrypted chunks
  server-side, so a host with the key can poison a chunk id that other hosts dedupe against. The same
  holds for us within one key domain (first write wins, `PutChunk`). Only client-side verify, which
  re-checks the HMAC id, detects it. With item 1, a poisoned chunk becomes quarantined and re-uploaded.
- PBS keeps exactly one writer per group (0 s group lock). Our `Service.BeginSession` only refuses an
  existing snapshot id; two runs for the same group can be open at once and would both use the same
  base. Add a one-open-session-per-group check (a partial unique index on `sessions`).
- Long sessions: PBS handles them through the oldest-writer cutoff; we cap sessions at 6 h
  (`SessionTTL`), so a first backup with `--max-freeze 3h` plus a slow upload can expire. Either
  renew sessions on activity (and keep grace > renewed lifetime + `touchAfter`) or document the cap.
