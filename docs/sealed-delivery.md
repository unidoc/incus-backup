# Sealed deliveries (IBS2), browser ZIP names, and browser key hygiene

This document covers three things that protect what reaches a person's machine from a backup:

- the authenticated delivery format of host jobs (`guest_files`, `fs_extract`);
- the safe name mapping of ZIP files made in the browser;
- what the console does with keys in the page, and where that stops.

It also lists the security audit entries for raw reads and deliveries.

## 1. Threat model of a delivery

A delivery is a tar file that a host makes for a person: files recovered from a VM disk, or
extracted from a filesystem snapshot. It travels like this:

1. The browser asks for it with `POST /api/v1/jobs`.
2. The server hands the job to the host.
3. The host uploads the sealed stream.
4. The server stores it.
5. The browser downloads it with `GET /api/v1/jobs/{id}/delivery`.

The server sees everything in the job parameters, including the recipient public key.

| A malicious or compromised server can | IBS1 (before) | IBS2 (now) |
|---|---|---|
| read the files | no | no |
| alter, truncate or reorder a delivery | detected | detected |
| make its own tar, seal it to the public key it knows, and serve it as "restored files" | **accepted** | **refused**: frames do not authenticate without `Keys.Delivery` |
| serve another job's genuine delivery | accepted if sealed to the same key | refused: the job id is bound |
| run the job with other parameters (another object, snapshot or paths) and serve that genuine delivery | accepted | refused: the request digest is bound |
| withhold or delay a delivery | yes | yes (availability is not protected) |

What IBS2 guarantees:

- A delivery the browser accepts was made by someone who holds the repository key.
- It was made for this job id and for exactly the parameters this page sent.
- Nothing in it was changed, dropped, reordered or appended on the way.

What it does **not** guarantee:

- It does not prove which host made the delivery. Every backup host of a key domain holds the
  same master key, so any of them, or anyone with the recovery key, can make a delivery the
  browser accepts. Losing one host of a domain loses confidentiality and delivery integrity for
  the whole domain. This is a design property of key domains.
- It does not cover the code the page runs. Whoever serves the console's JavaScript and
  WebAssembly can read the pasted recovery key (see `web-crypto/README.md`, "Threat model"). IBS2
  defends against a datastore, relay or database compromise with honest code delivery.

To accept a delivery, the browser must hold the repository key, so the Restore page asks for the
recovery key before it decrypts. The key is held in memory only and checked against the
repository's key domain. It is the same KeyGate as the file browser.

## 2. Byte format (normative)

```
stream     = "IBS2" | ephemeral X25519 public key (32) | data frame* | trailer frame
frame      = length (u32 big endian, 16 .. 1 MiB + 16) | XChaCha20-Poly1305(k_stream, nonce, plaintext, aad)
nonce      = frame counter (u64 big endian, from 0, data frames and trailer share it), zero padded to 24 bytes
aad        = "IBS2" | 0x00 for a data frame, 0x01 for the trailer
data frame = 1 .. 1 MiB of plaintext (never empty)
trailer    = total plaintext bytes (u64 BE) | number of data frames (u64 BE) | transcript (32)
transcript = SHA-256( header | every data frame exactly as sent: length prefix and ciphertext )

k_delivery = HKDF-SHA256(ikm = master key, salt = "incus-backup/v1", info = "delivery-auth")      (format.Keys.Delivery)
k_auth     = HKDF-SHA256(ikm = k_delivery, salt = "incus-backup/v1",
                         info = "incus-backup sealed delivery v2 auth" | u16 BE len(job id) | job id
                                | request digest (32) | recipient public (32) | ephemeral public (32))
k_stream   = HKDF-SHA256(ikm = X25519(ephemeral, recipient) | k_auth, salt = ephemeral public | recipient public,
                         info = "incus-backup sealed stream v2")

request digest = SHA-256("incus-backup job request v1" 0x00 | job type | 0x00 | canonical parameters)
```

Notes on the derivation:

- The canonical parameters are the JSON that `jobspec.Validate` returns: paths cleaned, defaults
  applied, unknown fields refused.
- The browser runs the same Go function in WebAssembly on the parameters it sent. The host runs
  it on the parameters it received.
- `Validate` is idempotent on its own output, so the server re-canonicalising the parameters
  does not change the digest (`internal/jobspec/delivery_test.go`).
- The recipient public key is part of the parameters. It is therefore bound twice: once in the
  digest, and once in `k_auth`.

The reader enforces these rules:

- It refuses `IBS1` with a message that says to update the host.
- It refuses an empty data frame.
- It refuses a trailer whose length, count or transcript does not match the frames received.
- It refuses any byte after the trailer.
- It returns end-of-stream only after the trailer verified.

**Holdback.** The browser adapter (`internal/webcore` `UnsealPush`) keeps back the plaintext of
the newest data frame until the next frame or the trailer has authenticated. This has two
consequences:

- The last piece of a delivery is never released before the whole stream verified.
- A stream cut before its trailer has released at most all but its last frame. It then fails with
  "truncated" (test: `web-crypto/test/seal.test.ts`, "holds back the last frame").

**Pinned and cross-checked.**

- `internal/format/testdata/sealed_v2.json` pins three vectors.
- `web-crypto/test/ibs2-spec.test.ts` reimplements this section without the Go code: Node's
  X25519, HKDF and ChaCha20-Poly1305, plus a hand-written HChaCha20 checked against the
  draft-irtf-cfrg-xchacha test vector. It decrypts and verifies the vectors, which proves that this
  text describes the code. It also has a negative control (an altered vector, a wrong job id).

### Where the parts live

| Part | Code |
|---|---|
| format | `internal/format/sealedbox.go` (`NewSealWriter`, `NewSealOpener`, `NewSealReader`, `SealBinding`) |
| request digest | `internal/jobspec/delivery.go` (`RequestDigest`) |
| host | `internal/client/jobs/executor.go` `deliver`: target keys, job id, digest of the received params |
| WebAssembly | `internal/webcore/seal.go`, `cmd/ibwasm`: `sealNew`/`sealImport`/`sealDrop`, `requestDigest`, `unsealOpen(key, seal, jobId, digest)` |
| TypeScript | `web-crypto/src/seal.ts`: `generateSealKeypair(): SealKey`, `unsealStream(sealed, { key, seal, request })` |
| console | `web/src/views/restore/Restore.vue` |

The console flow:

1. Restore generates the one-time key inside the module.
2. It sends `recipient_public_key` and keeps the exact parameters it sent.
3. It asks for the recovery key through KeyGate.
4. It streams the delivery through `unsealStream`.
5. It saves the file only when the stream closed normally. A blob is handed over after the end;
   a File System Access writable is committed by `close()` and discarded by `abort()`.

### Migration

IBS2 changes the delivery wire format. A host and a console must both be at this version.

- An old host's IBS1 delivery is refused by the new console, which names the cause.
- An old console cannot read IBS2.

Deliveries live for one hour, so nothing persistent needs migrating. `jobspec.DeliveryResult.Format`
is now `"tar+IBS2"`.

## 3. Browser ZIP names (W2/W3)

ZIP entry names come from guest file names, so they are hostile input. Every entry name passes
through `ZipNamer` in `web-crypto/src/zip.ts`, inside `zipStream`, so no caller can bypass it.

**Per path component:**

| Input | Stored as | Reason reported |
|---|---|---|
| `""` | `_` | empty name |
| `.` / `..` | `_` / `__` | dot name |
| C0 controls, DEL, C1 controls (U+0080..U+009F) | `_` per character | control character |
| `\` | `_` | backslash |
| bidi controls U+202A..U+202E, U+2066..U+2069, U+200E, U+200F, U+061C | `_` | bidirectional control |
| lone UTF-16 surrogate | `_` | invalid unicode |
| first component `X:...` (drive letter) | `X_...` | drive letter |
| longer than 255 UTF-8 bytes | first 200 bytes + `~` + 8 hex (FNV-1a of the original) + the extension if it is 16 bytes or less | name too long |

**Per path:**

- A leading `/` is removed (absolute path), and empty components are dropped.
- Parents are mapped before their children. A renamed directory renames everything below it.
- Two stored paths must never be the same for an extracting filesystem. Paths are compared
  case-insensitively, after Unicode NFC, ignoring trailing dots and spaces (Windows).
- On a clash, the later entry gets `~2`, `~3` and so on before its extension:
  `Readme.txt` then `README.TXT` becomes `README.TXT`, `Readme~2.txt` (the catalog's byte order
  decides which one comes first).
- A stored path longer than 4096 UTF-8 bytes is left out and reported through `onSkip`.

**Every rename is reported.** It goes to `onRename(original, stored, reasons)`, and
`RepoReader.extractZip` adds `RENAMED-ENTRIES.txt` to the archive with one JSON-quoted line per
rename. Nothing changes silently.

**Symbolic links are never stored as links.** A link inside an archive can redirect where an
extractor writes later entries, for example `ssh -> /root/.ssh` followed by
`SSH/authorized_keys` on a case-insensitive filesystem.

- `extractZip` option `symlinks`: `'skip'` (default, reported through `onSkip`) or `'note'`.
- With `'note'`, each link becomes a file `<name>.symlink.txt` that names the target.
- `zipStream` refuses `type: 'symlink'` entries unless the caller passes `allowSymlinks: true`.
  Only the ZIP64 unit test does.

**Modes.** Only the `0o777` permission bits are stored. Setuid, setgid and sticky never travel.
Devices and fifos are not stored (reported).

**Caps** (`ZIP_LIMITS`). A caller can lower them, never raise them:

| Cap | Limit |
|---|---|
| entries | 200 000 |
| one file | 64 GiB |
| whole archive (content) | 256 GiB |
| one stored path | 4096 bytes |

`extractZip` checks entries and bytes of the selection before the first byte. Both errors name
the way out: choose less, or use "Extract files on the host" (`fs_extract`). Each central-directory
record stays in memory until the end, so the entry cap is also the memory cap (about 60 MB at
200 000 entries).

**The catalog side.** `fscat.Decode` refuses a name below the root that is `""`, `.`, `..`, or
contains `/` or NUL (`internal/fscat/names_test.go`). No real Linux tree has those. A hostile or
damaged catalog fails before any name reaches the page.

**Tests.** `web-crypto/test/hostile.test.ts`:

- It uses a real catalog snapshot built by the fixture builder from a hostile tree:
  - `..\..\AppData\evil.bat`, control and escape characters, a bidi override;
  - `C:/win.ini`, `Readme.txt`/`README.TXT`;
  - a symlink `ssh -> /root/.ssh` next to `SSH/authorized_keys`;
  - a 254-byte name, setuid/setgid/sticky.
- Checks: `unzip -t`, Python `zipfile`, an extract with `unzip` and with `zipfile.extractall`, and
  that every extracted path lies under the target.
- Plus unit tests of the mapping for what Linux cannot hold.

## 4. Snapshot identity in the browser (W4)

`webcore.OpenSnapshot` checks two identities:

- **The manifest's own object.** `GroupID(manifest.project, manifest.object.kind,
  manifest.object.name)` must equal the group the manifest was opened under. A manifest sealed under
  another object's group is refused.
- **The object the page shows.** It is passed as `expect` (`RepoReader.openSnapshot(group, snap,
  { expect: { project, kind, name } })`, and `Browser.vue` passes the server listing's fields).
  The group must be that object's keyed group id, so a server that lists object B's snapshots under
  object A's name is refused before anything is shown.

Once a snapshot is open, the file browser names it from the decrypted manifest (project and object
name), not from the listing.

Limit: when the listing row for the snapshot is missing (the server omits or delays it), the page has
no claimed identity to check. The first check (manifest object versus group) still runs, and the
label then comes from the manifest only.

Tests:

- `internal/webcore/seal_test.go` `TestOpenSnapshotChecksIdentity`: swapped manifests and a
  mislabelled group.
- `web/e2e/security.demo.spec.ts`: genuine snapshots open in custody and in the synced offsite
  repository; a listing rewritten to another object's name is refused.

## 5. Key hygiene in the browser (W6) and its limits

**What the console does:**

- **One-time delivery keys** are generated inside the WebAssembly module (`sealNew`), and only the
  public key leaves it. The page holds a handle (`SealKey`), and `close()` drops the key in the
  module. `importSealKey` exists for interoperability and tests; it wipes the bytes it was given.
- **The recovery key text** goes from the input field to the module once. The field is cleared
  after every attempt, successful or not.
- **Derived sub-keys** live in the module. `close()` and `zeroize()` overwrite them, including the
  new `Delivery` sub-key.
- **Lock or leave.** When the file browser locks, the page is left or hidden, or the Restore page
  unmounts, the console calls `zeroize()` and then **terminates the module's Web Workers**
  (`web-crypto` `shutdown()`). The browser reclaims their whole memory, and the next use starts
  fresh workers.

**Residual limits:**

- JavaScript strings cannot be wiped. The pasted key text, and any copy the browser made of it
  (the input element, the clipboard, an autofill history if a browser ignores
  `autocomplete="off"`), lives until garbage collected.
- Go gives no guarantee that the runtime made no copy of a key (garbage collector moves, stack
  copies, `crypto/ecdh`'s internal buffers). Only terminating the worker ends those.
- `crypto/ecdh` private keys keep their bytes internally and offer no wipe. Dropping the reference
  (and terminating the worker) is all that is possible.
- Devtools, extensions with page access, crash dumps and swap are outside the page's control.
- **Raw fetch caps (W5).** The console's `ApiFetcher` reads index, manifest and chunk bodies with
  a cap: chunk 64 MiB + 4 KiB, which is `format.MaxChunkSize` plus framing; index 256 MiB; manifest
  16 MiB (the server's own upload limits). A larger body is cancelled mid-stream with an error.
  `web-crypto`'s `HttpFetcher` already capped at 72 MiB.

## 6. Security audit (A1)

On top of one entry per state-changing request, the management API audits the following
(`internal/server/mgmt/audit.go`).

**Refused requests:**

- Kinds:
  - a role refusal (403, reason `role <role>`);
  - a failed CSRF or origin check (reason `csrf`);
  - a cross-site login or setup (actor `anonymous`, reason `cross-site request`).
- The first of a kind (actor, action, reason, IP) in a window is written at once, `ok: false`.
- Repeats in the window become one summary entry, `detail.repeated = n`, when the window ends.

**Delivery downloads** (`GET /jobs/{id}/delivery`):

- One entry each: action `download delivery`, `detail {job, bytes, status}`.

**Raw repository material** (index, manifest, chunks):

- Never one row per request. Instead, one summary per actor, repository and snapshot per window
  (one minute): action `read repository material`, target `<repository>:<group>/<snapshot>`.
- The detail holds the counts `index`, `manifest`, `chunk` and `failed`, plus `requests`, `bytes`,
  `from` and `to`.
- Chunk URLs carry no snapshot. Chunks are attributed to the snapshot whose index or manifest the
  same actor last fetched from that repository (`chunks_attributed` counts them). Chunks fetched
  before any snapshot was opened are summarised under the target `<repository>:chunks`.

**Volume.** Rows are bounded:

- at most one row per (actor, action, reason, IP) and two per window for refusals;
- one per (actor, repository, snapshot) per window for raw reads;
- one per delivery download.

Summaries still pending are written at shutdown (`Server.FlushAudit`).

**Token actors.** A token's actor names its owner: `token:ci (by anna)`. Token names are only
unique per user. The new `auth.Principal.Owner` field carries the owner.

**Not done.** There is no hash chain or other tamper evidence on `audit_log`. The database owner
can still delete rows.
