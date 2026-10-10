# Memory model

Rule: backup, restore, verification, synchronisation and GC are streamed. Their buffers are sized
by configuration (workers, chunk size, batch sizes, budgets), never by the size of the object. What
does grow with an object or a repository is listed below with measured bytes per entry; none of it
holds payload data.

Every number on this page was measured by a real execution (`go test`, `scripts/membench.sh`) on
the tree that includes the performance work (`docs/performance.md`), not estimated. Client and
server ran in ONE process in those tests (the test server is in-process), so every block-path
number is client + server together.

## How to measure

- The suite runs the memory tests at small sizes on every `go test ./...` (about 30 s in total):
  `internal/client/engine/mem_test.go`, `internal/datastore/mem_test.go`,
  `internal/datastore/budget_test.go`, `internal/format/mem_test.go`,
  `internal/fscat/scan_ahead_test.go`, `internal/server/syncer/large_index_test.go`. They fail
  when a phase exceeds its bound, when the live heap grows with the object size, or when a sparse
  restore allocates per hole.
- `scripts/membench.sh` (make it executable: `chmod +x`) runs the same tests at large sizes (sizes
  come from the environment, nothing is skipped) and prints one table: 1/4/8 GiB VM disks (25 %
  data), 64 GiB/1 TiB/4 TiB sparse disks, 30k/300k-file trees, 50k/200k-chunk repositories, a
  1M-entry index. About 5 minutes, about 5 GB of temporary disk, one process at a time with
  GOMEMLIMIT=1GiB.
- `internal/memprobe` samples per phase: `HeapInuse` (peak, every 5 ms), the live heap after each
  GC (`/gc/heap/live:bytes`), and the peak process RSS (`VmHWM`, reset through
  `/proc/self/clear_refs` at the start of the phase), each relative to the phase's start.
  "heap in use" includes garbage the collector has not reclaimed yet (with the default GOGC it can
  reach about twice the live heap); "live" is what is really retained. Catalog, index and repository
  measurements run at GOGC=10 (`StartTight`), so that heap in use follows the live structures; their
  per-entry costs are the slope between two sizes, so fixed buffers cancel out.
- The VM disk in the block tests is procedural (`nbd.Export.Content`): a 4 TiB disk costs no memory
  in the test double, and restore checks every byte against the generator.

## What bounds what (defaults)

| path | bounded by | default |
|---|---|---|
| VM capture (NBD read, encode, spool) | pipelined read batch of `captureReadAhead` chunks + job queue of 2 x `Workers` + one chunk per encoder; 4 MiB buffers pooled, zstd encoders and scratch pooled | 8 + 8 + 4 chunks, ~80 MiB |
| backpressure on the NBD reader | the bounded job queue (the reader blocks when the encoders fall behind) | - |
| upload to a target | `UploadWorkers` concurrent PUTs (one spooled blob each); "which are missing" in batches of 2,000 ids, at most 2 batches asked or answered-but-unconsumed at once; a batch's hex ids exist only while it is asked | 16 x 4 MiB per target |
| server: chunk PUT | one body per request, read into one buffer when its length is declared, but never more than 16 MiB allocated before the bytes arrive; HTTP/2 windows 32 MiB per connection, 8 MiB per stream | 16 bodies per uploading client |
| client: responses | chunk and index responses carry Content-Length and are read into one buffer (at most 16 MiB ahead of the data) | - |
| filesystem backup (catalog) | runs handed over during the walk (`fscat.ScanOptions.OnRun`) and dropped after encoding; the concurrent walk may run at most `fscat.DefaultScanAhead` (64k) entries ahead of the in-order assembly (a parked listing never blocks a worker); packs <= 2.25 MiB, CDC chunks <= 8 MiB; the catalog is encoded in 1 MiB pieces straight into the chunker | - |
| restore (block, filesystem, tar) | read-ahead window of 8 chunks, `Workers` fetches; zero chunks are a shared read-only buffer (no allocation per hole) | ~8 decoded chunks |
| client deep verify / restore test deep | `Workers` fetches; the set of chunks already checked is per object and capped (`engine.VerifySeenMax` = 1M ids) | - |
| server: verify job | `DefaultVerifyParallelism` blobs; chunk states in a two-generation cache capped at `datastore.VerifyCacheMax` (1M) plus every corrupt id exactly; the all-chunks scrub remembers nothing | ~90 MB at the cap |
| server: GC mark | live set keyed by 8 bytes of each id; indexes read in parallel within `datastore.MarkIndexBudget` (256 MiB of index file + parsed entries) | - |
| server: GC sweep | candidates deleted in batches of 4,096 while the listing goes on | - |
| server: sharded `chunks/` listing (GC, verify, usage) | at most `parallelism` shards in flight AND no new shard while `datastore.ListBufferMax` (512k) listed keys wait for delivery | ~65 MB |
| server: session pins | 8 bytes per pinned chunk id + map overhead | - |
| sync (server to server) | `Concurrency` transfers; missing ids per batch of 2,000, transferred before the next batch | 4 x one blob |
| deliveries | streamed into 32 MiB parts (one part buffer + one copy per active upload) | 64 MiB per upload |

The spool is DISK, not memory: it holds the encrypted dirty chunks of one VM (or one filesystem
object) between the freeze and the upload and is deleted after that batch. There is no spool size
limit in the configuration (nothing to honour); `doctor` reports its free space.

## Measured

Measured on 2026-10-08 (4 vCPU shared with other agents, GOMEMLIMIT=1GiB, default `Workers` 4 and
`UploadWorkers` 16, client and server in one process). "before" is the tree with the performance
work but without this change (the measurement harness added); "after" is this change. Heap = peak
`HeapInuse` delta, live = peak live-heap delta, RSS = peak RSS delta, all MiB.

### VM disk, 25 % data / 75 % holes (flat in the disk size)

| disk | phase | before heap / live / RSS | after heap / live / RSS |
|---|---|---|---|
| 1 GiB | backup | 437 / 221 / 435 | 426 / 220 / 444 |
| 4 GiB | backup | 355 / 141 / 261 | 342 / 158 / 310 |
| 8 GiB | backup | 418 / 171 / 355 | 336 / 140 / 273 |
| 1 GiB | restore | 140 / 45 / 13 | 141 / 44 / 68 |
| 4 GiB | restore | 170 / 52 / 2 | 181 / 52 / 0 |
| 8 GiB | restore | **495 / 248** / 114 | 169 / 56 / 4 |
| 1 / 4 / 8 GiB | verify (client deep) | 86-106 / 8-23 | 68-92 / 8-12 |
| 1 / 4 / 8 GiB | restore test deep | 134-171 / 39-57 | 108-173 / 32-49 |

Backup is dominated by the 16 upload workers (16 spooled blobs on the client, 16 bodies plus the
HTTP/2 windows on the server) and the capture read-ahead; it does not grow with the disk. Restore of
the 8 GiB disk allocated 15.1 GB before and 6.2 GB after (3 x the 2 GiB of data).

### Sparse VM disk (64 MiB of data): the entry-count axis

| disk (index entries) | phase | before heap / live | after heap / live |
|---|---|---|---|
| 64 GiB (16k) | restore | 169 / 73 (65.8 GB allocated, 6 s) | 73 / 12 (0.2 GB) |
| 256 GiB (64k) | restore | 251 / 115 (262 GB allocated, 25 s) | - |
| 1 TiB (256k) | restore | (not run: ~1 TB of allocation) | 83 / 14 |
| 4 TiB (1M) | backup | - | 338 / 135, RSS 351 |
| 4 TiB (1M) | restore | - | 122 / 24 (0.36 GB allocated) |
| 4 TiB (1M) | verify | - | 247 / 88 |
| 4 TiB (1M) | restore test deep | - | 250 / 88 |

Before the change every zero chunk of a restore allocated a fresh 4 MiB buffer, even when it was
then skipped. At 4 TiB the per-entry costs are the index (file and parsed, on client and server) and
the capture bookkeeping: linear by design, roughly 100 B per 4 MiB of disk.

### Filesystem catalog (small files), slope per catalog entry

| phase | before B/entry | after B/entry | after, 300k files: heap / live / RSS |
|---|---|---|---|
| backup (full) | 860 | 359 | 160 / 147 / 120 |
| backup (incremental, previous catalog loaded) | 928 | 612 | 192 / 151 / 178 |
| open + ls | 374 | 378 | 106 / 106 / 0 |
| extract (whole restore) | 427 | 376 | 118 / 103 / 38 |

Before: the concurrent walk ran ahead over the whole tree, and the run layout (a path per file) of
the whole tree and the whole encoded catalog were held next to the tree. After: the walk stays
within 64k entries of the assembly, runs are processed during the assembly, the catalog is encoded
in 1 MiB pieces into the chunker, and the previous catalog is dropped after the walk. The tree
itself (~220 B per entry, `fscat.Entry` plus names; two trees in an incremental run) remains.

### Repository maintenance (server), slope per chunk

| phase | before B/chunk | after B/chunk | after, 200k chunks / 316 snapshots: heap MiB |
|---|---|---|---|
| GC (mark + sweep) | 146 | 90 | 18.6 |
| session pins (a session that asked about every chunk) | 67 | 28 | 7.5 |
| verify, all chunks | 140 | 144 | 29.5 |

At 200k chunks the whole `chunks/` listing still fits the listing buffer (512k keys), so most of the
GC and verify slope is the listing; it stops growing at the buffer bound (~65 MB). The live set
itself costs ~15 B per chunk (measured on the tree without the sharded listing). Verify's state
cache stops growing at 1M chunks.

### Index of a 4 TiB disk (1M distinct entries)

| | before | after |
|---|---|---|
| index file | 40 MiB (40 B/entry) | same |
| ParseIndex | 40 MiB (40 B/entry) | same |
| `ChunkIDs` (distinct ids, in index order) | 149 B/entry peak | 110 B/entry peak, ~75 live |

## Known linear costs (by design; no payload)

| structure | where | cost | 4 TiB VM disk (1M entries) |
|---|---|---|---|
| index file + parsed index | client (backup, restore, verify), server (session, sync) | 40 B per entry each | 40 MB + 40 MB |
| distinct chunk ids of an index (`ChunkIDs`) | upload, sync, reconcile, invariants | ~75 B live / ~110 B peak per distinct id | ~110 MB transient |
| capture bookkeeping (positions to read, ids per key domain) | client backup | 1 B + 32 B per entry per key domain | ~33 MB |
| set of chunk ids in a deep restore test | client | ~50 B per distinct id | ~50 MB |
| session pins | server, per open session | ~20-40 B per pinned id | ~20-40 MB |
| GC live set | server, per GC run, whole repository | ~15 B per live chunk | 10M chunks: ~150 MB (extrapolated) |
| verify chunk-state cache | server | ~70-90 B per chunk up to the cap (1M) | capped |
| catalog tree | client (backup, browse, restore, restore test), web UI WASM | ~360-380 B per entry (~610 B during an incremental backup: two trees) | 1M files: 0.4-0.6 GB |
| snapshot lists (`Snapshots`, sync list, admin lists) | client, server, mgmt | one JSON row per snapshot | 100k snapshots: tens of MB |

An index is limited to 256 MiB (6.7M entries, a 27 TiB disk). A sync snapshot PUT carries the index
base64-encoded; its body limit is now derived from the index limit. It was the 32 MiB JSON limit,
which made every block object above about 2.4 TiB impossible to synchronise (fixed, covered by
`TestSyncCarriesAnIndexOfAMillionEntries`). One such PUT costs about 3 x the index size on the
receiving server, transiently.

Targets: with default settings the block paths stayed at <= 445 MiB peak RSS delta for client AND
server together, at every disk size measured (1 GiB to 4 TiB); the client's share is roughly half
(the 16 upload bodies, HTTP/2 windows and index copies are on the server side). The filesystem
catalog path is the one that grows: a tree of about 1M entries crosses 512 MiB in an incremental
backup. The server-side 256 MiB per-session target is not measured separately; its parts are: one
chunk body per concurrent PUT (16 by default), the session's index (40 B per entry), pins (~30 B
per chunk).

## Not measured (read only)

- deliveries (`internal/server/jobs`): 32 MiB parts, one part buffer plus one copy per active
  upload;
- mgmt lists and overview, reconcile (one row per snapshot, no chunk sets);
- the web UI's WASM catalog (`internal/webcore`: one parsed tree per open catalog, the same ~380 B
  per entry as `open + ls`); a browser-side chunk cache is not part of the Go tree;
- guest-file recovery (`guestfiles`, `recovery`): uses the block restore stream;
- the GC live set beyond 200k chunks (the 10M figure is extrapolated);
- verify's prefetch of the next 8 snapshots' indexes (8 indexes, up to 8 x 80 MB for 4 TiB disks).

## Behaviour changes that come with the bounds

- Client deep verify (`incus-backup verify`) deduplicates chunks per object, not across the whole
  repository: a chunk shared by two objects is fetched once per object (counts change).
- Server verify: `VerifyStats.Chunks` can count a chunk twice once `VerifyCacheMax` is exceeded.
- Sync: `ChunksReused` is counted per batch, so a run that fails half-way reports what it compared
  until then.
- Upload: the next "which are missing" question goes out only when an answer has been consumed
  (still 2 in flight, as before; answers no longer pile up ahead of slow uploads).

## Tunables

- client `Engine.Workers` (default 4) and `Engine.UploadWorkers` (default 16): capture buffers scale
  with the first, upload buffers (client and server) with the second, 4 MiB each.
- `fscat.ScanOptions.Ahead` (default 64k entries): how far the concurrent walk may run ahead.
- `engine.VerifySeenMax`, `datastore.VerifyCacheMax`: caps of the "already checked" sets (re-reading
  after the cap is extra I/O, never a skipped check).
- `datastore.MarkIndexBudget` (256 MiB), `datastore.ListBufferMax` (512k keys).
- GOMEMLIMIT: the service units may set it (for example 512MiB client, 1GiB server); with the bounds
  above it only changes how eagerly garbage is collected.
