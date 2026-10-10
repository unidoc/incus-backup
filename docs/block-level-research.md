# Can filesystem volumes be backed up at block level?

> **Status (2026-10-08): decided and implemented.** The recommendation below (section 4) was taken: option 2,
> *catalog + stat walk*, is the default filesystem format ("fs2", `internal/fscat`, design 5.1): encrypted per-snapshot
> catalog with size, mtime, **ctime, inode**, mode, owner and xattrs per file; unchanged files are reused as whole
> chunks without being read; periodic full-data runs; browsing and single-file extraction. Option 3 (`zfs diff`)
> plugs into the scanner later; option 4 (zvol/block-mode through the VM pipeline) is unchanged; option 5 remains
> rejected. Open: the real-ZFS stability of inode and ctime across Incus snapshots (gate G3b,
> `docs/real-host-validation.md`). PBS's own metadata mode was read from source (`create.rs:is_reusable_entry`,
> `CHUNK_PADDING_THRESHOLD`) and differs deliberately: it ignores ctime and inode, and it injects partially reused
> chunks with padding; our runs end on chunk boundaries, so nothing is padded.

Question (from `temp/volumes.txt` / `volumes2.txt`): VM disks are backed up as blocks with
Incus dirty bitmaps. Can containers and custom *filesystem* volumes (ZFS datasets) get the
same treatment (cost proportional to changed bytes, not to the number of files), without the
repository becoming a chain of `zfs send` increments, and with an exact restore?

Evidence labels as in `design.md`: **[V]** verified in the Incus 7.5.1 source or the OpenZFS
header, **[M]** from memory, **[O]** open, needs a real ZFS host (the sandbox has no ZFS).

## 1. The central fact

A ZFS *filesystem* dataset is not an addressable block device. Its content is a set of DMU
objects (dnodes: files, directories, ZAP tables, the master node...) each with its own block
tree. There is no linear byte range "0..N of the dataset" to hash into 4 MiB chunks and to
write back. A **zvol** is different: it is one object whose offsets are the device offsets.

| Object | What the byte-level view is |
|---|---|
| zvol (VM root, custom block volume, or a block-backed filesystem) | linear: send records `(object 1, offset, length)` = device offsets |
| filesystem dataset (default for containers and custom filesystem volumes) | object graph: no linear range exists |

Incus decides which one a volume is: `zfs.block_mode` (custom volumes; or pool-wide
`volume.zfs.block_mode`) makes a filesystem volume a *formatted zvol* [V: `config_options.txt`
`zfs.block_mode`: "Whether to use a formatted zvol rather than a dataset"]. Incus already handles
the snapshot devices of such volumes: it sets `volmode=dev` and `snapdev=visible` on the parent
so `/dev/zvol/<pool>/<...>@snapshot-<name>` appears [V: `driver_zfs_volumes.go` ~3420].

## 2. What the OpenZFS stream gives (and what it would cost to use it)

`zfs send -i S1 S2` is a serialisation of "what changed in object terms" [V: `zfs_ioctl.h`]:
`DRR_OBJECT` (create/modify a dnode: type, block size, bonus buffer), `DRR_FREEOBJECTS`,
`DRR_WRITE(object, offset, logical_size, payload)`, `DRR_FREE(object, offset, length)`,
`DRR_SPILL`, `DRR_WRITE_EMBEDDED`, `DRR_WRITE_BYREF` (dedup only), `DRR_OBJECT_RANGE`,
`DRR_END`; every record is a fixed 312-byte `dmu_replay_record_t` followed by a payload, with
fletcher-4 checksums. Features matter (`LARGE_BLOCKS`, `COMPRESSED`, `EMBED_DATA`, `RAW`,
`LARGE_DNODE`, `SA_SPILL`...): they change the payloads, so the stream must be requested without
the optional features (or all of them handled).

* **For a zvol this is exactly a block-level change list**: object 1 `WRITE` and `FREE` ranges
  are the changed device ranges. Without `-c/-e/-L` the payload is plain data. We do not even
  need the payload: parse only the headers and skip the payload bytes, then read the ranges
  from the snapshot device (`/dev/zvol/...@snapshot-x`) into our 4 MiB chunks. That is the VM
  pipeline unchanged (base index + patched chunks), minus NBD.
* **For a filesystem dataset** the same stream is an object-level delta. To keep "no chain"
  we would have to keep our own *object map* per restore point (per object: its `DRR_OBJECT`
  metadata plus the list of `(offset, length) -> chunk` data blocks), patch it with each delta,
  and on restore **synthesise a complete valid full stream** (BEGIN, OBJECT, WRITE..., END with
  correct checksums and feature flags) and feed it to `zfs receive`. That would give an exact
  ZFS-level restore, but it means re-implementing the producer side of the stream format,
  tied to OpenZFS versions and feature flags, and it can only be validated on a real ZFS host.
  Highest fidelity, highest risk. **Not recommended for v1.**

## 3. The options, honestly

| # | Option | Cost per run | Restore | Chain-free | Risk / effort |
|---|---|---|---|---|---|
| 1 | **Current v1**: snapshot -> deterministic tar -> FastCDC -> dedup | reads every byte, uploads only new chunks | file level, exact POSIX metadata incl. xattrs/ACLs | yes | done; slow for huge trees |
| 2 | **Catalog + stat walk** (PBS "metadata change detection") | `lstat` of every file; reads only changed files | file level (and browsing single files) | yes | moderate; pure Go; portable to dir/btrfs pools |
| 3 | Option 2 accelerated by **`zfs diff S1 S2`** | O(changed): no walk at all | as 2 | yes | moderate; needs the previous snapshot kept (an Incus snapshot `ibk-<id>`); keep periodic full stat-walk as a safety net against any drift |
| 4 | **zvol / block-mode volumes** via send-record ranges (or NBD) | O(changed bytes) | byte-exact volume (any filesystem inside) | yes | small once the stream header parser exists; only for volumes that are zvols |
| 5 | Object map + synthesised full stream (section 2) | O(changed) | exact ZFS dataset restore | yes | very high; version fragile; needs ZFS to test |

Option 1 is correct today, only its running time grows with data size. A real container tree of
tens of GiB is read in minutes; "millions of files" cost the walk plus the read, not memory.

## 4. Recommendation

1. **Do not replace the file-level design with a block-level one for datasets.** A dataset has
   no block level; the only exact-by-block route (option 5) is the riskiest piece of code in
   the whole project and gives little the file-level archive does not.
2. **Option 2 first** (catalog with per-file `size, mtime_ns, ctime_ns, inode, mode, uid, gid`,
   reuse of unchanged data, reading only changed files). It removes the "read everything"
   cost, keeps single-file restore, keeps the repository chain-free, and works on every pool
   driver. Then **option 3** as an accelerator for ZFS pools (`zfs diff` replaces the walk).
   Design needed first: how unchanged *small* files keep their chunks. PBS solves it with a
   separate payload stream; a simple alternative here is per-directory packs of small files
   (reused when the directory's entries are all unchanged) plus per-file CDC for large files.
3. **Option 4 for volumes that are zvols** (`zfs.block_mode`), if the operator wants databases or
   million-file volumes on a real block level: expose them to the VM pipeline (send-record
   ranges against the snapshot device, or Incus per-volume NBD). This needs Incus to snapshot
   such a volume consistently, which it already does; no Incus patch is required if the client
   reads the snapshot device itself.
4. Only if an exact `zfs receive`-level restore (dataset properties, clones, holds) ever matters:
   revisit option 5.

## 5. What would change in Incus (if we want it upstream or in `unidoc-incus`)

Nothing is *required*. A useful, small primitive would be an API that returns "the changed
extents of a volume between two snapshots" for block-backed volumes (backed by `zfs send -i`
header parsing or `zdb` birth times) and "a stable read-only path for a snapshot" (today Incus
only mounts one on demand). The client implements both itself for now (it mounts the snapshot
dataset read-only exactly the way Incus does, `driver_zfs_volumes.go`: "Mount the snapshot
directly (not possible through tools)").

## 6. Experiments that settle the open points (run on the real host)

* Count and time the real trees: `find <container rootfs> -xdev | wc -l` and
  `time find <rootfs> -xdev -printf '' ` for the largest containers/volumes. If a full stat walk
  takes seconds, option 2 is enough and option 3 is unnecessary.
* `zfs diff` accuracy and speed on a snapshot pair of a busy container (do xattr/ACL-only
  changes appear? renames? how long?).
* For zvol-backed volumes: parse a real `zfs send -i` header stream (no payload) and compare the
  ranges with `dd`-written test ranges. This is the only experiment that needs new code
  (`internal/zfssend`, headers only) and it is safe on a scratch zvol in project `ibgate`.

None of these can run in the build sandbox (no ZFS kernel module).
