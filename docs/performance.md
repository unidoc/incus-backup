# Performance

What was measured, what was changed because of it, and what could not be measured here. Every
number in this file comes from a benchmark in the tree (`just bench`, `just bench-big`); the
"before" column is the same benchmark code run against the unmodified tree (only the benchmark
files and two tiny test shims added). No repository format change: `docs/format.md` and the format
vectors are untouched, and the compressed chunk encoding is now pinned too
(`internal/format/golden_test.go`, digests computed with the old encoder).

## Machine and method

* 4 vCPU of an AMD Ryzen 9 3900 (QEMU VM), 16 GB RAM, one virtual disk (`/tmp` on overlayfs),
  Linux 6.18, Go 1.26.8, MinIO (single node, filesystem mode, same machine), embedded PostgreSQL.
  (MinIO is gone from the test setup: its server is no longer distributed. All numbers in this document are
  historical MinIO measurements; `just bench` now runs against the in-process S3 fake, or any real service via
  `INCUS_BACKUP_TEST_S3_*`, so new numbers are not comparable with these.)
* The machine was **shared with several other build/test agents** the whole time: load averages
  of 6 to 70 on 4 cores. Wall-clock numbers are therefore noisy (spreads are given where several
  runs exist). The evidence that does not depend on the load is the **request counts**
  (`HEAD/op`, `GET/op`, `LIST/op`, `conns`, PostgreSQL transactions) and the RTT-bound results,
  where the time is dominated by waiting for the network, not by the CPU.
* WAN latency is modelled with a TCP delay line (`internal/bench.DelayProxy`): every byte is
  stamped on arrival and released RTT/2 later in each direction, so it adds latency without
  throttling a bulk transfer (its own test checks both). It sits between the server and MinIO for
  the datastore benchmarks and between client and server for the upload benchmarks.
* S3 benchmarks run with `-benchtime 1x`: one op is one whole operation over all N chunks (Missing
  of N ids, Commit of an N-chunk index, a GC, a verification, a usage listing).
* Upload benchmarks use the production HTTP server (`httpapi.Server.NewHTTPServer`, HTTP/2, real
  mutual TLS, PostgreSQL control plane); the test node now uses the same server as production.


## Summary

| What | Before | After | Why |
|---|---|---|---|
| "Which chunks are missing", 11,000 ids, S3 at 20 ms RTT | 244 s | 3.9 s (0.40 s with the presence set) | parallel HEAD (64), presence set |
| Commit existence check, 11,000 chunks, S3 at 20 ms RTT | ≈ 240 s (1 HEAD per chunk, sequential) | 4.0 s | parallel HEAD (64), never cached |
| Same two at 100,000 chunks, 20 ms RTT | ≈ 2,200 s each (extrapolated, sequential) | Missing 35 s (3.5 s with the presence set), Commit 36 s | as above |
| Verify (scrub) 100,000 chunks, S3, 0 ms / 20 ms | 130 s / ≈ 2,150 s (extrapolated) | 38 s / 155 s | read-ahead worker pool, ordered recording |
| GC 100,000 chunks, S3 at 20 ms | 12.2 s | 3.8 s | parallel mark (no marker GETs), sharded listing, batched sweep |
| Chunk upload client→server, 1 GiB, 80 ms RTT, 16 workers | 12.6 MB/s (capped by the HTTP/2 window) | 168 MB/s | HTTP/2 receive windows, 16 upload workers |
| Chunk encode, 4 MiB text-like data, 4 goroutines | 131–147 MB/s (zstd serialised) | 393–410 MB/s | one zstd encoder per goroutine, pooled buffers |
| Filesystem scan, 1M files | 11–31 s | 4.3–5.2 s | concurrent lstat/xattr, sequential assembly |
| `GET /v1/objects` with 104k snapshot rows | 313 s | 53 ms | missing index (`snapshots_object`) |

Details, raw numbers and caveats per area follow.

## 1. S3 request overhead: Missing and Commit

**Before.** `Store.Missing`, `Session.Missing` and the existence check of `Commit` issued one HEAD per
chunk, strictly one after the other (`HEAD/op` = N, `conns` = 1). The cost is N × (RTT + service
time), linear in both:

| ids | 0 ms | 5 ms | 20 ms | 80 ms |
|---|---|---|---|---|
| 1,000 (Missing) | 0.53–0.62 s | 7.3 s | 21.3–22.6 s | 83.0 s |
| 11,000 (Missing) | 5.6 s | 78.2 s | 244 s | ≈ 913 s (extrapolated) |
| 11,000 (Commit) | 7.3 s | 78.5 s | ≈ 240 s (1,000: 24.4 s) | ≈ 910 s (1,000: 82.6 s) |

**Change.** The checks are bounded-parallel (`datastore.DefaultRequestParallelism` = 64, a code
default; `Store.SetRequestParallelism` for tests). Duplicate and zero ids are asked once; the answer
(order, duplicates) is identical to the sequential one (`TestParallelMissingIsBoundedAndIdenticalToSequential`).
The parallel requests run under the caller's single hold of the GC read lock (a worker never takes
it: a recursive RLock deadlocks once GC waits for the write side); `Session.Missing` pins under the
same hold, and `Commit` keeps "pin every chunk first, then check" unchanged. The S3 transport now keeps
`2 × parallelism` idle connections per host (it kept 16 on plain HTTP and **2** on TLS, so most of
32 parallel requests opened a new connection: `conns` = 82 for 32 requests before the fix, 32 after).
`PutChunk` no longer HEADs before its conditional PUT on S3 (the `If-None-Match: *` PUT answers
"exists" itself; one round trip less per uploaded chunk); the filesystem keeps its cheap stat.

**After** (load 2–11, 90 % of the Missing ids present):

| 11,000 ids | 0 ms | 5 ms | 20 ms | 80 ms |
|---|---|---|---|---|
| Missing, par 32 | 1.30 s | 2.59 s | 7.61 s | 28.3 s |
| Missing, par 64 | 1.28 s | 1.60 s | 3.91 s | 14.1 s |
| Missing, par 32, presence set | 0.13 s | 0.26 s | 0.77 s | 2.87 s |
| Missing, par 64, presence set | 0.09 s | 0.15 s | 0.40 s | 1.48 s |
| Commit, par 32 | 1.37 s | 2.64 s | 7.83 s | 28.6 s |
| Commit, par 64 | 1.24 s | 1.62 s | 4.00 s | 14.5 s |

| 100,000 ids | par 32, 0 ms | par 32, 20 ms | par 64, 20 ms | par 32, 80 ms |
|---|---|---|---|---|
| Missing | 13.2 s | 70.4 s | 35.1 s | 259 s |
| Missing, presence set | 1.16 s | 6.9 s | 3.5 s (+ 2.7 s to list 100k chunks once) | 26.1 s |
| Commit | 16.0 s | 70.5 s | 36.5 s | 257 s |

At 0 ms the single local MinIO is the limit (~8,000 HEAD/s on these 4 cores), so more parallelism
does not help there; behind a real RTT the time is RTT-bound and halves from 32 to 64 (hence the
default of 64; at 64 requests per 20 ms that is ~3,200 requests/s, below the per-prefix rates S3
services document). At 80 ms only parallelism 32 was measured at 100k; at 64 it should halve as it
does at 11k and at 20 ms (not measured).

### Is parallel HEAD enough at 100k chunks and 20 ms? The presence set

No: 100,000 ids at 20 ms still cost 35 s per Missing pass at 64, paid by every
incremental backup of a 400 GiB disk, and again by its commit. Hence the **presence set**
(`internal/datastore/presence.go`), a repository-local optimisation the engine never sees:

* **Positives only.** It is loaded from a full listing of `chunks/` (sharded, below), plus every chunk
  this process stores or sees answer a HEAD; an id that is not in it is asked of the backend as before,
  and a "not found" evicts it.
* **Deletions by this process can never leave a stale positive.** GC's sweep and quarantine remove an id
  *before* deleting the object (quarantine also after, since it does not exclude sessions), and a
  positive answer from a request that was in flight when any removal happened is dropped (a generation
  counter; `TestPresenceSetDropsAnswersOlderThanARemoval`). A deletion during a re-list is a tombstone
  applied to the new listing, an upload during it an addition (`TestPresenceSetRebuildHonoursConcurrentDeletesAndPuts`).
* **What it cannot know** is a deletion by someone else (an operator, a lifecycle rule, or a listing that
  still shows an object deleted just before it: the `Backend` contract bounds what a listing may *miss*,
  not what it may still *show*). So it is used **only by Missing, never by Commit**. Commit asks the
  backend about every chunk after pinning them, exactly as before; a stale positive therefore costs one
  refused commit (409, missing chunks), whose ids are evicted from the set, after which the client's
  existing retry (`upload.go`: ask again, upload, commit, at most twice) uploads them. It can never make a
  snapshot with a missing chunk visible. Tests: `TestPresenceSetExternalDeletionIsCaughtAtCommit` (memory
  backend, with a negative control) and `TestS3PresenceSetExternalDeletionCaughtAtCommit` (real MinIO,
  the object removed with a plain S3 client). For server-to-server sync the same case is one failed
  snapshot transfer (409) that the next sync run repeats successfully.
* **Why Commit does not use a listing either.** A LIST started after the pin would be as fresh as a HEAD
  after the pin *if* listings never showed deleted objects; the contract does not promise that and
  S3-compatible services differ, so Commit stays on parallel HEAD (4.0 s per 11k chunks at 20 ms).
* **Bounds.** On for S3 repositories only (filesystem stats are microseconds). Built in the background
  on the first Missing, re-listed every `PresenceRefresh` (6 h, only while used). 80 bytes per id
  (measured), capped at `PresenceMaxEntries` = 2 Mi ids (≈ 160 MiB, twice that during a re-list); a
  larger repository does without it. Listing 100k chunks takes 2.7–3.5 s.
* **Other processes.** A GC run from the admin CLI goes through the daemon when it runs (`fwd.go`), so
  its deletes reach the daemon's set; only a second process writing the same bucket (not a supported
  setup: repositories are owned by one server) would be an "external" deletion, caught at commit.

## 2. Verify, GC and usage on S3

* **Sharded listing** (`listing.go`): `chunks/` is listed as one shallow listing plus its first-level
  prefixes in parallel (bounded window, so memory stays bounded), delivered in exactly the key order of
  one recursive listing (`TestShardedListingMatchesOneListing`, on filesystem, memory and MinIO,
  including junk objects directly below `chunks/`). One recursive S3 listing is a sequential chain of
  1,000-key pages; `LIST/op` in the tables counts listing calls (257 shards after, 1 call of 100+
  pages before).
* **GC mark** lists each group once (no `commit.json` GET per snapshot: marking needs no receipt, and
  marking too much is safe) and reads the indexes in parallel; any unreadable or corrupt index still
  aborts before anything is swept (`TestParallelGCAbortsOnCorruptIndex`). The **sweep** takes the write
  lock once per batch of 64 candidates and re-checks (pins, then age) and deletes the batch in parallel
  under it: the lock is held about two round trips per batch instead of two per candidate. Same result
  as sequential (`TestParallelGCIsIdenticalToSequential`).
* **Verify** reads chunks ahead with a worker pool (`DefaultVerifyParallelism` = 16 blobs in memory at
  most, batches of 4,096 ids) and the index/manifest of the next 8 snapshots, but records every
  result in sequential order, so problems, results, counters, quarantine and `verify.json` records are
  identical to a sequential job (`TestParallelVerifyIsIdenticalToSequential` compares the whole
  repository afterwards on a repository with corrupt and missing chunks, a corrupt index, a missing
  manifest and junk names). A backend error still aborts the job at the point a sequential job would.

| | before 11k (0 / 5 / 20 ms) | after 11k (0 / 5 / 20 / 80 ms) | before 100k (0 / 20 ms) | after 100k (0 / 20 ms) |
|---|---|---|---|---|
| GC (nothing to sweep) | 1.23 / 1.71 / 8.68 s | 0.51 / 0.49 / 0.92 / 1.54 s | 5.8 / 12.2 s | 4.3 / 3.8 s |
| Verify, all chunks | 12.1 / 105 / ≈ 260 s* | 2.4 / 6.0 / 16.5 / 59.5 s | 130 / ≈ 2,150 s* | 38 / 155 s |
| Usage (listing) | 1.37 / 1.82 / 2.54 s | 0.44 / 0.67 / 0.58 / 1.09 s | 4.3 / 10.5 s | 3.5 / 3.4 s |

\* extrapolated from the 1,000-chunk run (23.6 s at 20 ms), the job being strictly sequential.
GC before also made 20 HEADs (one per snapshot marker), after 0. The "before" runs had a load
average of 16–45, the "after" runs 2–11; the RTT-bound columns are the robust ones.

Not changed: Verify's second pass over the snapshots that share a corrupt chunk (rare) and
`CheckInvariants` (tests only) are still sequential.


## 3. Client encode path

Per 4 MiB block chunk the client computes the keyed id (HMAC-SHA256), compresses (zstd, default
level), encrypts (XChaCha20-Poly1305) and appends the SHA-256 trailer. Measured on 4 cores, three
interleaved A/B rounds at load ≈ 7.5, MB/s of plaintext (`BenchmarkEncodeChunk`, `-benchtime 20x`):

| data | before, 1 goroutine | after, 1 goroutine | before, 4 goroutines | after, 4 goroutines |
|---|---|---|---|---|
| incompressible | 357–377 | 431–511 | 417–681 | 1,045–1,186 |
| text-like (≈ 3:1) | 114–129 | 124–126 | 131–147 | 393–410 |
| sparse (90 % zero 4 KiB blocks) | 666–825 | 716–752 | 1,463–1,679 | 2,059–2,096 |
| allocated per chunk (incompressible / text) | 24.2 MB / 7.6 MB | 4.4 MB / 1.5 MB | | |

Parts, one goroutine: HMAC-SHA256 ≈ 1.2–2.0 GB/s, SHA-256 ≈ 1.5–2.0 GB/s, zstd on text ≈ 150 MB/s, on
incompressible data ≈ 1.0–1.1 GB/s. Compression of compressible data is the dominant cost.

**The finding.** All encoding shared one `zstd.Encoder` built with concurrency 1, and klauspost's
`EncodeAll` serialises on its internal encoder: the client's worker pool compressed one chunk at a
time (4 goroutines ≈ 1 goroutine on text). Decoding (restore) had the same problem.

**Change** (`internal/format/blob.go`): every call takes its own encoder/decoder from a free list,
all built with exactly the original options, and compresses into a pooled scratch buffer sized by
`MaxEncodedSize` (the blob itself is still a fresh slice, so callers may reuse their plaintext
buffers, which the capture loop now does). Output is byte-identical: the format vectors pass, a new
golden test pins the SHA-256 of ten compressed blobs computed with the *old* encoder (serially and from
four goroutines at once), and the pooled encoder is compared with a reference encoder at every size
around the block and window boundaries. No compression level, option or skip heuristic was changed
(any of those would change bytes).

**Zero chunks and extents.** The all-zero test (`bytes.Count`) runs at 50–70 GB/s, so a zero chunk
costs ≈ 0.07 ms and is never hashed, encoded or uploaded; holes reported by `base:allocation` are not
even read on a full read, and an incremental read reads only dirty-bitmap extents (unchanged).

Worker split: `Engine.Workers` (encode, default `DefaultEncodeWorkers` = 4: CPU bound and running
next to the guests, so capped) and the new `Engine.UploadWorkers` (default 16, below), both settable in
`client.yaml` (`performance.encode_workers`, `performance.upload_workers`, validated 0–64 / 0–128).

## 4. NBD read path

The NBD client sent one 1–4 MiB read and waited for its reply before the next: one round trip per
request. It now pipelines (`internal/nbd/pipeline.go`): `ReadAt` sends the sub-requests of one call
together and `Conn.ReadBatch` keeps up to `Conn.Depth` (default 16) requests in flight; a
demultiplexer routes structured reply chunks by handle, in any order and interleaving the protocol
allows. `captureDisk` reads 8 chunks (32 MiB) per batch, in position order, into pooled 4 MiB buffers.
Server errors for one request leave the stream usable; protocol violations (unknown handle,
overlapping chunks, which could otherwise hide a range that was never sent) close the connection.
`Close` now interrupts a read stuck on a hung server (before, it waited for the connection lock, so
the max_freeze abort could hang). Tests run against the fake server in sequential mode and in a new
mode that answers concurrently, newest first, with interleaved chunks, plus injected errors.

MB/s reading a 256 MiB random export in 4 MiB units (medians, min–max, 6–11 interleaved rounds,
load 6–18):

| transport | before `ReadAt` | after `ReadAt` | after `ReadBatch` 8 × 4 MiB, depth 4 / 16 |
|---|---|---|---|
| net.Pipe | 1,700 (611–2,080) | 1,252 (556–1,839) | 1,451 / 1,304 |
| loopback TCP | 1,189 (411–1,716) | 1,086 (580–1,681) | 840 / 1,113 |
| delay line, ≈ 2–4 ms RTT | 260 (184–334) | 553 (371–817) | 559 / 700 |
| delay line, ≈ 6–7 ms RTT | 130 (102–142) | 401 (222–445) | 542 / 480 |

The local transports are bounded by the fake server (one request at a time, 1 MiB cap by default,
copies every reply): no difference beyond the noise. Behind latency the gain is 2–4×. The real path
(QEMU's NBD server through the Incus proxy) could not be measured here; QEMU serves requests
concurrently with a 32 MiB request limit, which is what the batching is designed for.

## 5. Upload path client → server

**Findings.**
1. **HTTP/2 flow control capped every upload at one MiB per round trip.** A client multiplexes all of
   its chunk uploads over one HTTP/2 connection, and Go's server default receive window is 1 MiB per
   connection: 46 MB/s at 20 ms and 12.6 MB/s at 80 ms *whatever the number of workers* (table). The API
   server now sets 32 MiB per connection and 8 MiB per stream (`httpapi.NewHTTPServer`; memory is
   only used while a handler does not read, and handlers read each body at once). The table below was
   measured with 64/16 MiB; rerun with the final 32/8 MiB: 181 MB/s at 80 ms and 186–204 MB/s at 20 ms
   with 16 workers (filesystem). They are bounded because a peer without a client certificate can
   connect (for enrolment): it can make the server buffer up to 32 MiB per connection (1 MiB before).
2. **Every authenticated request wrote `hosts.last_seen_at`**: one PostgreSQL commit per chunk, all
   workers of a host on the same row. Now written at most once a minute per host (or when its address
   changes); the lookup itself (a unique index) still runs per request, so disabling a host still takes
   effect immediately.
3. The PUT handler read 4 MiB bodies with `io.ReadAll` (about 20 grow-and-copy steps); it now reads
   into one buffer of the announced Content-Length (the server enforces the length).
4. The client asked "which chunks are missing" in sequential batches of 2,000 before uploading
   anything. The questions are now pipelined with the uploads (2 batches in flight, uploads of a batch
   start as soon as its answer is in; `TestUploadPipelinesMissingQuestionsWithUploads` checks the bound,
   the overlap and the result).
5. The test node used `httptest` without HTTP/2; it now uses the production server, so the tests and
   benchmarks see the windows production sees.

1 GiB of incompressible 4 MiB chunks, nothing present on the server, MB/s (`BenchmarkUpload`; before
at load 6–12, after at load 6–8; one connection in every run):

| store, RTT | before: 1 / 4 / 8 / 16 workers | after: 1 / 4 / 8 / 16 workers |
|---|---|---|
| filesystem, 0 ms | 71 / 162 / 104 / 147 | 76 / 144 / 174 / 213 |
| filesystem, 20 ms | 27 / 44 / 46 / 47 | 66 / 158 / 187 / 168 |
| filesystem, 80 ms | 9.2 / 12.3 / 12.6 / 12.6 | 35 / 117 / 159 / 168 |
| MinIO, 0 ms | 86 / 136 / 157 / 177 | 91 / 133 / 168 / 215 |
| MinIO, 20 ms | 25 / 43 / 45 / 40 | 59 / 133 / 151 / 189 |
| MinIO, 80 ms | 8.6 / 12.3 / 12.5 / 12.5 | 34 / 86 / 117 / 154 |

Default upload workers: **16** (best or equal-best in every row above; costs 16 × 4 MiB of memory).

Incremental upload (50,000 small chunks, all already present: only the "missing" questions and the
commit; `BenchmarkUploadIncremental`):

| | before 0 ms / 20 ms | after 0 ms / 20 ms |
|---|---|---|
| filesystem repository | 0.65 / 1.32 s | 0.27 / 0.63 s |
| MinIO repository | 50.8 / 52.2 s | 4.5 / 5.9 s |

**Server side on a filesystem** (`BenchmarkFSPutChunk`): storing one chunk is trailer check, temp file
write + fsync, link, directory fsync. On this virtual disk: 64 KiB ≈ 7–10 ms serial, 2.2–2.9 ms with 4
in parallel; 4 MiB ≈ 120–190 MB/s. A breakdown for 64 KiB: write+fsync 2.1–2.4 ms, directory fsync
1.5–1.6 ms, link and unlink ≈ 0.04 ms. The directory fsync is required for the chunk to be durable
before it is acknowledged; amortising it (group commit) only helps puts into the *same* directory, and
the `chunks/xx/yy/` fan-out (65,536 leaf directories) makes that rare, so it was left as is. More
parallel uploads overlap the fsyncs, which is where the parallel speed-up above comes from.

## 6. Filesystem scan and catalog

`fscat.Scan` now lists directories and does the per-entry lstat, xattr reads and readlink on a pool
of workers (`ScanOptions.Workers`, default `DefaultScanWorkers` = 16), while one in-order pass still
does everything order dependent (hard-link detection, payload offsets, packs, comparison with the
previous catalog, the choice of the first error), so the result is byte-identical: an equivalence
test runs the original scanner (kept verbatim as a test reference) and the new one over a tree with
hard links, symlinks, fifos, sockets, xattrs, deep and wide directories, with 1–64 workers and
unclean root spellings, and once more with a 1-byte xattr buffer so every xattr call takes the ERANGE
retry path. Xattrs are read with one `Llistxattr` and one `Lgetxattr` per value into pooled buffers;
**no xattr call is skipped** (nothing on a filesystem proves a file has none). The catalog encoder
appends into one pre-sized buffer (1 allocation instead of 2 M per million entries), the decoder
allocates entries in blocks, and the CDC splitter stopped moving ~10 MB after every cut (boundaries
unchanged, tested against the original splitter).

Synthetic trees (64 entries per directory, 1 % symlinks, 0.5 % hard links, 2 % with a user xattr,
files 0–4 KiB plus some of 260 KiB), warm cache, 4–6 interleaved runs at load 8–21:

| | before | after |
|---|---|---|
| full scan, 200k files | 1.24–4.09 s | 0.52–1.02 s |
| full scan, 1M files | 11.1–30.7 s | 4.3–5.2 s |
| unchanged-tree incremental (decode old catalog, scan, encode), 200k | 1.30–2.50 s, 2.80 M allocs | 0.87–1.43 s, 1.96 M allocs |
| same, 1M | 10.6–29.8 s, 14.0 M allocs | 4.9–10.0 s, 9.8 M allocs |
| catalog encode, 1M entries (82 MB) | 450–633 ms, 286 MB allocated | 142–174 ms, 96 MB, 1 alloc |
| catalog decode, 1M entries | 252–307 ms, 2.14 M allocs, 210 MiB live | 220–394 ms, 1.07 M allocs, 208 MiB live |
| CDC splitter | 563–1,050 MB/s | 948–1,328 MB/s |

Worker sweep (200k files): 1 → 148k, 4 → 279k, 8 → 372k, 16 → 366k, 32 → 329k entries/s. 16 was
kept for cold caches, where each call waits for the disk (not measurable here). A 1M-entry catalog
holds ≈ 208 MiB in memory (≈ 218 bytes per entry, the size of `fscat.Entry`, unchanged). The scan
allocates somewhat more transiently (≈ 1.14 GB vs 0.94 GB per 1M-file scan).

## 7. PostgreSQL

Seeded with 104.5k snapshot rows over 10k objects in the measured repository (146k rows in total),
`ANALYZE`d, `EXPLAIN ANALYZE` before and after the indexes that are now
the "performance indexes" section of `migrations/20261008000000_initial_schema.sql` (`BenchmarkControlPlaneQueries`;
`TestPerfIndexesUsed` checks the planner uses them):

| query | before | after |
|---|---|---|
| object list (`GET /v1/objects`, admin objects): snapshot count per object | Seq Scan per object, 313,247 ms | Index Only Scan `snapshots_object`, 53 ms |
| status: objects of one host | 51.8 ms | 0.47 ms (`storage_objects_host`) |
| status: last run of one host | 5.79 ms | 0.01 ms (`backup_runs_host_time`) |
| status: runs in the last 24 h | 5.5 / 9.0 ms | 0.06 / 0.05 ms (`backup_runs_repo_time`) |
| maintenance runs list | 7.08 ms | 0.04 ms (`maintenance_runs_time`) |
| last run of every host | 323.7 ms | 0.23 ms |
| delete a host (FK SET NULL) | 76.9 ms | 53.2 ms (`sessions_host`; the rest is real updates) |
| HostFromCert, snapshot list of a group, session checks | ≤ 0.08 ms (already indexed) | unchanged |

N+1 fixes: reconcile read PostgreSQL state with one query per snapshot and wrote unchanged rows back;
it now prefetches in 3 queries and writes only differences (20,900 snapshots in 1,900 groups: 55,100
transactions → 9–17, 21.5–37.6 s → 4.2–6.1 s). `ListAllSnapshots` (sync) made one query per group, now
one (1,901 → 2–3 transactions; wall time unchanged, it is dominated by reading `commit.json` files).
The migration runner wraps each file in a transaction, so the indexes are built without
`CONCURRENTLY`: on a large existing installation creating `snapshots_object` briefly blocks writes to
`snapshots`.

## Tunables (all code defaults, no server configuration)

| name | default | where |
|---|---|---|
| `datastore.DefaultRequestParallelism` | 64 | concurrent HEAD/GET/DELETE/LIST per repository operation |
| `datastore.DefaultVerifyParallelism` | 16 | chunk blobs a verification holds at once |
| `datastore.PresenceRefresh`, `PresenceMaxEntries` | 6 h, 2 Mi ids | presence set (S3 only) |
| `httpapi` HTTP/2 windows | 32 MiB per connection, 8 MiB per stream | API server |
| `service` last-seen write interval | 1 min | `hosts.last_seen_at` |
| `engine.DefaultEncodeWorkers` / `DefaultUploadWorkers` | 4 / 16 | client; `performance.encode_workers` / `upload_workers` in client.yaml |
| upload "missing" batches | 2,000 ids, 2 in flight | client |
| `nbd.DefaultDepth`, capture read-ahead | 16 requests, 8 chunks | client |
| `fscat.DefaultScanWorkers` | 16 | client |

## What could not be measured here

* **Real hardware paths:** no ZFS, no QEMU NBD server, no Incus proxy, no real WAN or real S3 service.
  Latency is a delay line on loopback (no loss, no bandwidth limit, no TCP slow-start effects of a long
  path), MinIO is one local process on the same 4 cores, the disk is a virtual disk on a shared host.
  The S3 per-prefix request limits of commercial services were not exercised.
* **Load.** Other agents kept the 4 cores busy (load 2–77 over the session). RTT-bound results and
  request counts are reliable; CPU-bound wall-clock numbers carry the spreads shown, and some "before"
  runs had higher load than the matching "after" runs (noted per table).
* **Not measured at full size:** the sequential "before" of 100k-chunk Missing/Commit and of verify at
  20 ms (they would take 40 min to over 2 h each); they are extrapolated from 1k/11k runs, which is safe
  only because the old code is strictly sequential (`conns` = 1, one request per chunk). The 100k
  "after" runs at 0 and 80 ms and the 100k maintenance runs used parallelism 32, not the final 64.
* **Cold caches** (dropping caches needs root) for scans and the filesystem datastore.
* Missing batch size (2,000) was not varied; with the questions pipelined it matters less.
* Two existing tests are flaky on the unmodified tree, unrelated to this work: `TestEscrowRoundTripAndEveryFailureMode`
  (≈ 12 % of runs) and `TestImportKeyRestoresTheSameDomainAndNeverOverwrites` (≈ 20 %): both "mistype" a
  key by changing its last base32 character, which sometimes only changes unused padding bits.

## Rerunning

`just bench` (about 30 minutes) and `just bench-big` (several hours) run everything above; both need PostgreSQL
like the tests (embedded PostgreSQL); S3 is the in-process fake unless `INCUS_BACKUP_TEST_S3_*` names a real service.
Sizes come from `INCUS_BACKUP_BENCH_N`, `_RTT`, `_PAR`, `_PRESENCE`, `_UPLOAD_MB`, `_WORKERS`, `_STORE`,
`_IDS`, `_FILES`, `_ENTRIES`, `_DIR`, `_FSDIR` (see the benchmark files). For a before/after comparison
copy the `*_bench_test.go` files into an old tree (the datastore and engine benchmarks use two small
`perf_knobs_test.go` shims for knobs the old code lacks).


