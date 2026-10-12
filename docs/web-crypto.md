# Browser decryption (web-crypto)

The management GUI is served by the repository server, which must never see a key or plaintext.
So the browser decrypts: the operator pastes the recovery key into the tab, the tab fetches the
encrypted objects through the server API and decrypts them locally. The key stays in the page's
memory, inside the WebAssembly module.

## Components

| Path | What |
|---|---|
| `internal/webcore` | Plain Go, host-tested: key handles (derived sub-keys only, master wiped), snapshot opening (manifest decrypt, index bound to the manifest's `index_digest`, catalog layout checks), chunk decode, catalog browse (`fscat.Decode`, `Lookup`, `List`, `Find`, `Subtree`, resumable pre-order walk), streaming SHA-256, one-time X25519 keys kept inside the module (handles), push-driven verifier over `format.SealOpener` (IBS2, newest frame held back until the next one or the trailer verified), `jobspec.RequestDigest`, snapshot identity checks, escrow unwrap. |
| `cmd/ibwasm` | `GOOS=js GOARCH=wasm` binding of webcore on `globalThis.ibwasm`. Every call is synchronous, returns `{ok}` or `{err}`, recovers panics. Only handles, ids, JSON and requested plaintext cross the boundary. |
| `web-crypto/` | TypeScript ESM package: loads the module (Node: in place; browser page: a pool of module Web Workers), `RepoReader`, streaming ZIP writer, `HttpFetcher`, `unsealStream`. vitest + headless Chromium tests. |
| `internal/webfixture` | Go test that builds the test repository with the real engine (fake Incus, real in-process server over mTLS, PostgreSQL, filesystem datastore) and exports it at the API paths; `sealer/` seals stdin to a public key for the interop test. |

The only change to existing files: `//go:build !js` on `internal/fscat/scan.go`, `extract.go` and
`fscat_test.go` (they use `x/sys/unix`; the browser needs only `catalog.go` and `browse.go`).

Module size (Go 1.26.8, `-trimpath -ldflags="-s -w"`): **5 003 708 bytes raw, 1 426 767 bytes gzip -9** (was 4 853 117 / 1 388 877 before IBS2: `jobspec` validation for the request digest adds about 150 KB)
(budget 6 MiB; the build script fails above it).

## TypeScript API surface

```ts
// keys
function openKey(recoveryKey: string, opts?: { core?: Core }): Promise<KeyHandle>;           // KeyError on bad checksum/prefix/length
function openEscrowKey(wrapped: Uint8Array, escrowPrivateKey: string, opts?: { core?: Core }): Promise<KeyHandle>;
function zeroize(core?: Core): Promise<void>;                                              // wipes every key/catalog/stream in the module
class KeyHandle {
  readonly core: Core;
  readonly domainId: string;                                                              // 16 hex, non-secret
  get closed(): boolean;
  groupId(project: string, kind: 'block' | 'filesystem' | 'instance' | string, name: string): Promise<string>;
  close(): Promise<void>;
}

// repository access
interface Fetcher {
  index(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array>;
  manifest(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array>;
  chunk(id: string, signal?: AbortSignal): Promise<Uint8Array>;
}
class HttpFetcher implements Fetcher {
  constructor(baseUrl?: string, opts?: { fetch?: typeof fetch; maxBytes?: number });     // GET {base}/api/v1/snapshots/{g}/{s}/index|manifest, /api/v1/chunks/{id}
}
interface ReaderOptions { cacheBytes?: number /* 64 MiB */; concurrency?: number /* 6 */; readAhead?: number /* = concurrency */; onProgress?: (p: FetchProgress) => void }
interface FetchProgress { chunksFetched: number; bytesFetched: number; cacheHits: number; inFlight: number }
class RepoReader {
  constructor(key: KeyHandle, fetcher: Fetcher, opts?: ReaderOptions);
  get stats(): FetchProgress;
  clearCache(): void;
  openSnapshot(group: string, snap: string, opts?: { signal?: AbortSignal }): Promise<OpenedSnapshot>;  // IntegrityError on wrong key / tampering
}
type OpenedSnapshot = CatalogSnapshot | UnsupportedSnapshot;
interface SnapshotBase { group: string; snap: string; manifest: Manifest; kind: string; size: number }
interface UnsupportedSnapshot extends SnapshotBase {
  format: 'archive' | 'block' | 'instance'; browsable: false;
  unsupported: string;   // 'archive format cannot be browsed' | 'block device snapshots cannot be browsed as files' | 'instance snapshots hold configuration only; ...'
}
interface CatalogSnapshot extends SnapshotBase {
  format: 'catalog'; browsable: true;
  tree: Tree;
  readFile(path: string, opts?: ReadOptions): ReadableStream<Uint8Array>;
  extractZip(paths: string[], opts?: ZipOptions | AbortSignal): ReadableStream<Uint8Array>;   // [] = everything
  suggestedFilename(paths?: string[], zip?: boolean /* default paths.length !== 1 */): string;
  close(): Promise<void>;
}
interface Tree {
  readonly count: number;          // entries incl. the root
  readonly stats: TreeStats;       // { Files, Dirs, Symlinks, Hardlinks, Special, Bytes }
  list(path: string): Promise<EntryInfo[]>;
  stat(path: string): Promise<EntryInfo>;
  find(root: string, glob: string): Promise<string[]>;                                  // Go path.Match on base names
  walk(root?: string, opts?: { batch?: number; signal?: AbortSignal }): AsyncGenerator<EntryInfo>;  // pre-order, root included
  subtree(path: string): Promise<{ entries: number; bytes: number }>;
}
interface ReadOptions { start?: number; end?: number /* exclusive */; signal?: AbortSignal; onProgress?: (done: number, total: number) => void }
interface ZipOptions { signal?: AbortSignal; onProgress?: (done: number, total: number) => void;
  onSkip?: (path: string, kind: EntryKind, reason: string) => void; symlinks?: 'skip' | 'note' /* never links */;
  onRename?: (r: ZipRename) => void; limits?: Partial<ZipLimits>; forceZip64?: boolean }
interface EntryInfo {
  path: string; name: string; kind: 'dir' | 'file' | 'symlink' | 'hardlink' | 'char' | 'block' | 'fifo';
  mode: number; modeString: string; uid: number; gid: number;
  mtimeNs: string; ctimeNs: string; inode: string;      // decimal strings (do not fit a JS number)
  size: number; sha256?: string; link?: string; major?: number; minor?: number;
  xattrs?: { name: string; value: string /* base64 */ }[]; children?: number;
}
interface Manifest { format: number; snapshot_id: string; previous_id?: string; created: string; host: string; project: string;
  policy: string; object: { name: string; kind: string; size: number }; index_digest: string; config?: unknown; refs?: string[];
  meta?: Record<string, string> /* fs.format, fs.payload_size, fs.catalog_size, fs.entries, fs.mode, ... */; stats: {...} }

// sealed deliveries (format.NewSealWriter, "IBS2", docs/sealed-delivery.md)
class SealKey { readonly publicKey: Uint8Array; readonly closed: boolean; close(): Promise<void> } // private half inside the module
function generateSealKeypair(opts?: { core?: Core }): Promise<SealKey>;
function importSealKey(privateKey: Uint8Array /* wiped */, opts?: { core?: Core }): Promise<SealKey>; // interop, tests
function requestDigest(type: string, params: unknown, opts?: { core?: Core }): Promise<Uint8Array>;  // jobspec.RequestDigest
function unsealStream(sealed: ReadableStream<Uint8Array> | Uint8Array,
                      o: { key: KeyHandle; seal: SealKey; request: { jobId: string; type: string; params: unknown }; signal?: AbortSignal }): ReadableStream<Uint8Array>;
function shutdown(): void; // terminate the shared Core's workers (after zeroize, on lock)

// runtime placement
function configure(c: { mode?: 'auto' | 'worker' | 'direct'; workers?: number; workerUrl?: string | URL; wasmUrl?: string | URL; wasmExecUrl?: string | URL; wasmBytes?: Uint8Array }): void;
function defaultCore(): Core;
function createCore(c?: same as configure): Core;

// errors (all extend IbError; messages never contain key material)
class KeyError, IntegrityError, FetchError /* .status */, GoError
// helpers
function checkGroup(s: string): void; function checkSnapshot(s: string): void; function checkChunkId(s: string): void;
function zipStream(entries: AsyncIterable<ZipEntry>, opts?: { forceZip64?: boolean; allowSymlinks?: boolean; limits?: Partial<ZipLimits>;
                   onRename?: (r: ZipRename) => void; onSkip?: (name: string, reason: string) => void }): AsyncGenerator<Uint8Array>;
// names are always mapped by ZipNamer / safeComponent (docs/sealed-delivery.md section 3); ZIP_LIMITS caps
function crc32(data: Uint8Array, crc?: number): number;
```

## How a snapshot is opened and read
1. `manifest` + `index` are fetched; the module decrypts the manifest (AAD = group + snapshot id),
   checks `SHA-256(index) == manifest.index_digest` (as `engine.Locate` does), parses the index, and
   for `fs.format == "2"` checks `fs.payload_size + fs.catalog_size == index size` and that the
   payload ends on a chunk boundary.
2. The catalog chunks (the index entries after the payload) are fetched, decoded and handed to
   `fscat.Decode` (which checks the catalog's own digest, sorting and names).
3. `readFile` maps `[payOff+start, payOff+end)` onto index entries by binary search and fetches only
   those chunks; all-zero chunks (zero id) are never fetched. A shared LRU (bytes-bounded) and an
   in-flight table de-duplicate fetches; a semaphore bounds concurrent fetches; a per-stream window
   bounds read-ahead. Whole files are hashed in the module and compared with the catalog's SHA-256
   before the last piece is released.

## Tests and measurements
`npm test` in `web-crypto/` (41 tests, about 60 s) builds the module, regenerates the fixture with the
real engine and runs:
- **listing** equals the source tree as the OS reports it (lstat walk: kinds, modes, sizes, symlink
  targets, hard-link grouping, nanosecond mtime, xattr);
- **30 MiB file** streams byte-identical, SHA-256 equal to the source; **range reads** fetch 1-2
  chunks (counted fetcher calls), the tail 1, an empty range 0, an all-zero 3 MiB file 0;
- **wrong key** fails before any chunk fetch; **tampering**: flipped/truncated/substituted chunk,
  catalog chunk, index of another snapshot, manifest of another snapshot, flipped index byte, valid
  chunk served under another id: all `IntegrityError`;
- **ZIP** of the whole tree passes `unzip -t`, extracts to the source bytes (modes, symlinks, hard
  links), Python `zipfile.testzip()`; forced ZIP64; a real 4 GiB + 12345 byte entry with entries
  beyond the 4 GiB offset; 70 000 entries; the fifo is skipped and reported;
- **unseal** of `format.NewSealWriter` output (multi-frame, exactly 1 MiB, small, empty), dribbled
  in random pieces; truncation at header/frame/boundary positions, trailing data, altered frame,
  wrong key all rejected; a key pair generated by the library receives a stream sealed by Go
  (`internal/webfixture/sealer`);
- **concurrency** never exceeds the limit; a stalled consumer causes at most `readAhead` fetches;
  abort and `cancel()` stop fetching (also with a server that never answers);
- **security**: every fetcher argument, URL, header and body, every error message (20+ failure
  modes) and console output are searched for the recovery key (forms and 10-character windows),
  the master key and the three sub-keys (hex, base64); a negative control proves the detector;
- **HttpFetcher** against a local HTTP server at the documented paths, HTTP errors, an oversized body;
- **headless Chromium 156** (Playwright headless shell): the same functional checks in the worker
  pool (page default), a single worker, the page main thread, and the library running inside a
  user Web Worker; no request carried key material; all API requests were plain GETs.

Throughput (MB/s of plaintext; 4-core shared box under heavy load from other jobs, load average
13-20, so single runs vary by 2x; best and worst observed shown):

| | Node 24 (one module) | Chromium worker pool (3) | Chromium 1 worker | Chromium main thread |
|---|---|---|---|---|
| chunk decode (trailer SHA-256 + XChaCha20-Poly1305 + zstd + HMAC id) | 14-30 | 54-88 | 21-42 | 33-42 |
| SHA-256 in the module | 58-93 | 36-87 | 20-47 | 40-95 |
| CRC-32 (JS) | 116-616 | 205-486 | 233-514 | 373-717 |
| readFile 30 MiB end to end (cold cache, verified) | 8-22 | 18-36 | 13-23 | 10-20 |
| ZIP of `big/` (47 MB) | 14-28 | 17-36 | 15-28 | 14-27 |
| unseal | 32-75 | 13-74 | 18-93 | 21-114 |

Go's WebAssembly target has no assembly for SHA-256 or ChaCha20, so a single module decodes at
roughly 30-40 MB/s; the worker pool scales decoding with cores. Whole-file verification hashes in one
worker and caps a single download at roughly the SHA-256 rate.

## Integration notes (done; kept for reference)
- **Routes to add** on the GUI's HTTP server (session-cookie auth, CSRF-less GET, per-repository
  authorization; knowing a chunk id must not be the only gate):
  `GET /api/v1/snapshots/{group}/{snap}/index`, `GET /api/v1/snapshots/{group}/{snap}/manifest`,
  `GET /api/v1/chunks/{id}` returning the raw stored bytes (`application/octet-stream`). These are
  distinct from the host mTLS routes `/v1/groups/{group}/snapshots/{snap}/...` and `/v1/chunks/{id}`.
  The GUI also needs a snapshot listing per group (group ids are computed in the page with
  `key.groupId(project, kind, name)`) and the repository's key domain id (to tell "wrong key" from
  "tampered").
- Serve `web-crypto/dist/` and `web-crypto/wasm/` with the same relative layout (`.wasm` as
  `application/wasm`), CSP `script-src 'self' 'wasm-unsafe-eval'; worker-src 'self'`.
- Sealed deliveries (host → browser) are built: jobs `guest_files`/`fs_extract`, IBS2
  (`docs/sealed-delivery.md`); the console's Restore page asks for the recovery key to verify them.
- Build: `sh web-crypto/scripts/build-wasm.sh` (also run by `npm test`); consider a Justfile target.
  `wasm/ibwasm.wasm` is a build artifact (git-ignored).

## Not done / unproven
- The library's own tests run HttpFetcher against a local static server at the documented paths; the
  console's Playwright tests (`web/e2e`) run it against the real `/api/v1` routes of the demo and a real
  server.
- Instance snapshots are classified as unsupported but no instance snapshot is in the fixture.
- ZIP64 beyond 4 GiB was proven with the ZIP writer directly (synthetic 4 GiB entry), not through a
  catalog snapshot holding a 4 GiB file.
- Throughput numbers are from a heavily loaded shared machine (see above).
- Delivery of the GUI code itself is outside this library: see the threat model in
  `web-crypto/README.md` (whoever serves the JavaScript and wasm can read the pasted key).
