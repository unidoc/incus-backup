# @incus-backup/web-crypto

Zero-knowledge browsing and single-file download of incus-backup snapshots **in the browser**.
The operator pastes the recovery key (`IBR1-...`) into the tab; the tab fetches the encrypted
manifest, index, catalog and chunks through the server's API and decrypts them locally. The
server never receives the key or any plaintext.

All cryptography and every byte format is the Go code of this repository (`internal/format`,
`internal/fscat`) compiled to WebAssembly (`cmd/ibwasm`, logic in `internal/webcore`). Nothing
is re-implemented in JavaScript except the ZIP container and its CRC-32 (not security relevant).

## API

```ts
import { openKey, RepoReader, HttpFetcher, unsealStream, generateSealKeypair } from '@incus-backup/web-crypto';

const key = await openKey(pasted);              // KeyError "recovery key checksum mismatch (typo?)" on a typo
key.domainId;                                   // non-secret; compare with the repository's recorded domain id
const group = await key.groupId('default', 'filesystem', 'volume/docs');
const reader = new RepoReader(key, new HttpFetcher(''), { concurrency: 6, cacheBytes: 64 << 20 });
const snap = await reader.openSnapshot(group, '20261007120000-0a1b2c3d');
if (!snap.browsable) show(snap.unsupported);    // 'archive format cannot be browsed', block, instance
else {
  await snap.tree.list('etc');                  // EntryInfo[]
  for await (const e of snap.tree.walk('')) {}
  const body = snap.readFile('etc/hosts');      // ReadableStream<Uint8Array>, SHA-256 verified
  const zip = snap.extractZip(['etc', 'root/.bashrc'], abortSignal);
  snap.suggestedFilename(['etc'], true);        // 'etc.zip'
}
await key.close();                              // wipes the sub-keys inside the module
```

The full surface (signatures) is in `docs/web-crypto.md` and in `dist/*.d.ts` after `npm run build`.

### Behaviour the GUI must know
- **A wrong key and tampering look the same** (`IntegrityError: manifest authentication failed ...`).
  Compare `key.domainId` with the domain id the server records for the repository first, to say
  "this key belongs to another repository".
- **Errored stream = failed download.** `readFile` and `extractZip` verify every chunk (keyed id +
  AEAD) and every whole file (catalog SHA-256). The last piece of a file is held back until its
  digest matched; on a mismatch the stream errors instead of closing. Treat only a stream that
  closed normally as complete. A byte range (`start`/`end`) is chunk-verified but has no whole-file
  digest.
- **Cancellation**: every operation takes an `AbortSignal`; `stream.cancel()` aborts in-flight
  fetches at once, also when the server withholds a response.
- **Memory**: streams are pull-driven; at most `readAhead` (default = `concurrency`) decoded chunks
  (≤ 8 MiB each) are in flight per stream, plus the LRU (`cacheBytes`). A ZIP keeps one central
  directory record (~100 bytes + name) per entry until the end.
- **ZIP**: STORE (no compression), UTF-8 names made safe for every extractor (`ZipNamer`, renames
  listed in `RENAMED-ENTRIES.txt`), Unix permission bits only (no setuid/setgid/sticky), symlinks
  never stored as links (`symlinks: 'skip'` default, `'note'` for a text file naming the target),
  caps on entries and sizes (`ZIP_LIMITS`), hard links resolved to their content, devices and fifos
  skipped and reported through `onSkip`. ZIP64 records are written when sizes, offsets or the entry
  count need them (tested with a 4 GiB + 12345 byte entry and 70 000 entries).
- **Workers**: in a browser page the default Core runs the module in a pool of module Web Workers
  (`hardwareConcurrency - 1`, 1..4); chunk decoding is spread over the pool, catalogs, hashes and
  sealed streams live in the first worker. Inside a worker (or in Node) the module runs in place.
  `configure({ mode, workers, workerUrl, wasmUrl, wasmExecUrl })` before the first `openKey`.

## Sealed streams
Hosts encrypt browser-bound results (files recovered from VM disks or extracted from filesystem
snapshots) with `format.NewSealWriter` (IBS2, `docs/sealed-delivery.md`): to a one-time X25519 key
created inside the module, and authenticated with the repository key for one job id and the exact
parameters the page sent. The server that relays it cannot forge or swap it.

```ts
const seal = await generateSealKeypair();       // seal.publicKey goes into the job's recipient_public_key
const params = { project, object, paths, recipient_public_key: b64(seal.publicKey) };
const job = await createJob({ type: 'fs_extract', params });
const key = await openKey(recoveryKey);         // only key holders can make a delivery this accepts
const plain = unsealStream(response.body, { key, seal, request: { jobId: job.id, type: 'fs_extract', params } });
// errors (IntegrityError) on forgery, another job, other parameters, truncation, alteration;
// treat the output as incomplete until the stream CLOSES
await seal.close();                              // when done
```

## Threat model

What the design protects **against a server that stores and relays the data**:

| A malicious or compromised server/datastore can | Result |
|---|---|
| read the key or plaintext | **no**: only ciphertext, the plaintext chunk index (ids and sizes) and opaque ids reach it |
| serve a wrong, altered, truncated or swapped chunk | detected: blob trailer, AEAD bound to the chunk id, keyed id recomputed over the plaintext, length check |
| serve another snapshot's index or manifest | detected: the manifest's AAD binds group and snapshot id; the index must match the manifest's `index_digest` |
| alter the catalog (file names, sizes, digests) | detected: the catalog is ordinary chunk data plus its own SHA-256 |
| forge a file that verifies | **no**: it would need the chunk keys |
| truncate, alter or reorder a sealed delivery | detected (AEAD per frame with a counter nonce, authenticated trailer with length, frame count and transcript hash, no data after it) |
| forge a delivery (seal its own tar to the public key it relays), or serve another job's | **no** (IBS2): frames authenticate only under a key derived from the repository key, the job id and the request digest |
| present object B's snapshots under object A's name | detected when the caller passes `expect` (the console does): the group must be A's keyed group id |
| withhold data / stall | **yes**: it can refuse service; the library only guarantees prompt cancellation |
| hide snapshots | **yes**: snapshot listings are not authenticated by this library |
| observe access patterns | **yes**: which snapshots and chunk ids are fetched, sizes and timing of downloads |

**The decisive limit: whoever serves the GUI's JavaScript and the wasm module can read the pasted
key.** If the same server that relays the data also delivers `index.js`, `worker.js`,
`wasm_exec.js` and `ibwasm.wasm`, a compromised server can deliver modified code. The guarantees
above hold against a datastore, storage, relay or database compromise with honest code delivery.
Mitigations (not built here): Subresource Integrity / a published SHA-256 of the module and scripts
that operators can check, serving the GUI from a separate static origin, a strict CSP.

### Key handling inside the page
- The recovery key string is passed once to the module and not kept by the library. JavaScript
  cannot wipe strings: clear the input field after `openKey`.
- The master key is wiped in Go right after deriving the sub-keys; `close()`/`zeroize()` wipe the
  sub-keys. Intermediate buffers inside `format.DecodeRecoveryKey` and the Go garbage collector's
  copies are not wiped (Go offers no guarantee).
- One-time delivery keys are generated inside the module and never cross to JavaScript (only the
  public key does); `shutdown()` terminates the workers after `zeroize()` so nothing the Go runtime
  copied survives a lock (limits: `docs/sealed-delivery.md` section 5).
- Nothing crosses to JavaScript but handles, the domain id, group ids and the plaintext asked for.
  No key material is logged or put into any error message; no key-derived value is put into a URL,
  header or body. Tested (`test/security.test.ts`, `browser/run.mjs`), including a negative control
  that the detector does catch a leak.
- `HttpFetcher` sends plain `GET`s with `credentials: 'include'`, no custom headers, no body, and
  only identifiers that passed format checks (`[0-9a-f]{32}`, `[0-9]{14}-[0-9a-f]{8}`, `[0-9a-f]{64}`).
  Responses are size-bounded (72 MiB) while they stream in.

## Building and testing

Prerequisites: the Go 1.26 toolchain on `PATH` (same release for `ibwasm.wasm` and `wasm_exec.js`),
PostgreSQL for the fixture (embedded PostgreSQL starts itself, as in every repository test), Node ≥ 20,
`unzip`, `zipinfo`, `python3`, and Playwright's headless Chromium (`chromium_headless_shell-*` in
`~/.cache/ms-playwright`, or `CHROMIUM_PATH`).

```sh
npm install
npm run build          # wasm/ibwasm.wasm (+ wasm_exec.js, Go LICENSE) and dist/
npm test               # builds the wasm, generates .cache/fixture with the REAL engine, runs everything
npm run bench          # Node and Chromium throughput
```

`npm test` runs `scripts/fixture.sh`: the Go test `internal/webfixture` backs up a real directory tree
(unicode names, nested dirs, a 30 MiB random file, zero-filled files, 400 small files, symlinks, hard
links, empty files and dirs, a fifo, an xattr), an archive-format volume and a VM disk with the real
engine into an in-process server and exports the snapshots through the real client API under the
`/api/v1/...` paths. In a container without Chromium's system libraries run `npm run chromium-libs`
once (unpacks the Debian packages into `.cache/chromium-libs`, picked up automatically) or set
`CHROMIUM_LIBS`.

Serving: `.wasm` must be `application/wasm`. The CSP needs `script-src 'self' 'wasm-unsafe-eval'` and
`worker-src 'self'`. Serve `dist/` and `wasm/` with this package's relative layout, or pass explicit
URLs to `configure()` (bundlers do not detect the worker URL by themselves).

`wasm/wasm_exec.js` is copied unmodified from the Go toolchain (BSD license, `wasm/LICENSE.golang`).
