# incus-backup repository format, version 1

Normative. The Go code in `internal/format` implements exactly this, the test
vectors in `internal/format/testdata/vectors.json` pin it byte for byte, and
`scripts/verify-vectors.py` re-checks the vectors with an **independent**
implementation (PyNaCl + `cryptography`). All integers are big endian. Any
change to a byte layout requires a new version number, new vectors and a
migration path; vectors are never regenerated to "make the test pass".

## 1. Primitives

| Use | Primitive |
|---|---|
| Key derivation from the master key | HKDF-SHA256, salt `"incus-backup/v1"` |
| Keyed chunk id, group id | HMAC-SHA256 |
| Authenticated encryption | XChaCha20-Poly1305, 24-byte random nonce |
| Passphrase wrapping | Argon2id (default t=3, m=64 MiB, p=4) |
| Integrity trailers | SHA-256 |
| Compression | zstd (compress, then encrypt) |

## 2. Keys

`master` = 32 random bytes. Sub-keys are `HKDF(master, salt, info)` with 32
output bytes and `info` one of:

| info | key |
|---|---|
| `chunk-encryption` | `k_enc` |
| `chunk-id` | `k_id` |
| `manifest-encryption` | `k_manifest` |
| `delivery-auth` | `k_delivery` (authenticates IBS2 sealed deliveries, `docs/sealed-delivery.md`) |
| `domain-id` | first 8 bytes, hex = **domain id** (non-secret) |

The domain id is shown to servers so a host with a different master key is
rejected. It reveals nothing usable about the key.

### Recovery key

`IBR1-` followed by the base32 (RFC 4648, no padding) of
`master || SHA256("incus-backup recovery key v1" || master)[0:4]`, written in
groups of 5 characters separated by `-`. Decoding is case- and
separator-insensitive and verifies the checksum.

### Key file (JSON)

`protection: "none"` stores the master in base64 (file mode 0600 is the only
protection; needed for unattended schedulers). `protection: "passphrase"`
stores `AEAD(KEK, nonce, master, aad = "incus-backup keyfile v1|" || domain_id)`
with `KEK = Argon2id(passphrase, salt, t, m, p, 32)`.

## 3. Chunk id

`id = HMAC-SHA256(k_id, plaintext)` (32 bytes, hex on disk). The **zero id**
(32 zero bytes) means "all-zero bytes, no blob" and is only valid inside
indexes.

## 4. Chunk blob

```
magic    4   "IBCH"
version  1   0x01
flags    1   bit0 = plaintext was zstd compressed before encryption
reserved 2   0x0000
nonce   24   random
ct       n   XChaCha20-Poly1305(k_enc, nonce, [zstd](plaintext), aad)
trailer 32   SHA-256 over every preceding byte
aad = magic | version | flags | reserved | chunk id (32 bytes)
```

* The trailer lets a **keyless** server (and the sync engine) detect any
  corruption of the stored bytes.
* The AEAD tag and the chunk id can only be checked with the key. After
  decryption the reader **recomputes the HMAC id** and compares it with the
  name the blob was stored under.
* Binding the id into the AAD prevents moving a valid blob to another name.
* Decompressed size is capped at 64 MiB.

## 5. Index (plaintext)

```
magic       4  "IBIX"
version     1  0x01
kind        1  1 = fixed size chunks, 2 = dynamic (content defined)
reserved    2  0
chunk_size  4  fixed: bytes; dynamic: 0
total_size  8  logical object size
count       8
entries     count * ( end u64 | chunk id 32 )
trailer    32  SHA-256 over every preceding byte
```

Entry `i` covers `[end(i-1), end(i))`. Ends strictly increase and the last end
equals `total_size`. For fixed indexes `end(i) = min((i+1)*chunk_size, total)`.
The manifest records `SHA-256` of the whole index file.
Block objects use fixed 4 MiB chunks. Filesystem archives use dynamic chunks.

## 6. Manifest (encrypted)

```
magic "IBMF" | version 0x01 | reserved 3 | nonce 24 |
XChaCha20-Poly1305(k_manifest, nonce, JSON, aad) | trailer 32
aad = "IBMF" | 0x01 0x00 0x00 0x00 | group id | 0x00 | snapshot id
```

Binding the group and snapshot id prevents relocating a manifest. The JSON
carries names, instance/volume configuration, references, statistics and the
index digest. A keyless server never sees any of it.

## 7. Group id and snapshot id

`group id = hex(HMAC-SHA256(k_id, "group\0" project "\0" kind "\0" name)[0:16])`.
The datastore directory tree uses only group ids, so it carries no names.

`snapshot id = YYYYMMDDHHMMSS (UTC) "-" 8 hex`. Only `[0-9a-f-]`, so it is safe
in file names and in bitmap names (`ib-<snapshot id>`).

## 8. Datastore layout (server)

```
datastore.json                                 {format, id, created}
owner.json                                     {server_id, host, since, previous?}   create-if-absent
chunks/<aa>/<bb>/<64 hex id>                   blob (section 4)
quarantine/<64 hex id>                         a blob that failed its trailer, moved out of chunks/
escrow/<16 hex key domain>.ibw                 wrapped master key (section 10), create-only
groups/<group id>/notes                        free text (UTF-8, at most 16 KiB)
groups/<group id>/<snapshot id>/index          index file (section 5)
groups/<group id>/<snapshot id>/manifest       manifest blob (section 6)
groups/<group id>/<snapshot id>/commit.json    server-written commit marker (receipt)
groups/<group id>/<snapshot id>/protected      marker object: the snapshot must not be removed
groups/<group id>/<snapshot id>/notes          free text
groups/<group id>/<snapshot id>/verify.json    {state, at, history[{at,state,chunks,problems}]} (last 20)
```

These are object keys; the layout is identical on a filesystem and in an S3 bucket.
A snapshot **exists only if `commit.json` exists**. The server writes index and manifest first
and `commit.json` last, create-if-absent (the marker carries the index and manifest digests).
Objects without a marker are invisible debris and are never read by restore. `protected`, `notes` and
`verify.json` are server-side metadata written only for committed snapshots and removed with them; they
carry no payload information. A chunk is never rewritten after it was stored; `quarantine/` receives a copy
of a corrupt blob before it is removed from `chunks/`, after which the id is "missing" again and a new upload
of the same plaintext restores it.

## 9. Filesystem snapshots, format "fs2"

A filesystem snapshot has ONE index (dynamic, section 5) over `payload || catalog`. The manifest's `meta` holds
`fs.format="2"`, `fs.payload_size`, `fs.catalog_size` (their sum is the index's total size), `fs.entries`,
`fs.mode` (`full` or `incremental`), `fs.last_full` (RFC 3339 time of the last snapshot in the chain that read
every file), `fs.base` (snapshot the reuse was based on) and `fs.trust_ctime` (`1` when ctime and inode were
compared). A manifest without `fs.format` is the archive format (one PAX tar stream in the index).

**Payload.** The content of every regular file with size > 0, concatenated in catalog pre-order. The payload is
cut into runs that start and end on chunk boundaries: a file of at least 256 KiB is one run chunked with
FastCDC (section 3); smaller files are packed in walk order into one chunk per pack, a pack ending when it
reaches 2 MiB or, from 512 KiB on, after a file whose path has `FNV-1a-32(path) mod 8 = 0`. All-zero chunks are
zero entries without a blob. The catalog is cut with FastCDC and starts on a chunk boundary, so the payload and
catalog ranges never share a chunk.

**Catalog** (`IBCT`, all integers varint / uvarint, strings and byte strings length-prefixed with a uvarint):

```
magic "IBCT" | version 0x01 | reserved 3
entry (the root, a directory, comes first)
SHA-256 over every preceding byte (32)

entry  := kind(1) name mode(uv) uid(uv) gid(uv) mtime_ns(v) ctime_ns(v) inode(uv) xattrs  body
xattrs := count(uv) { name value }*
body   := dir:      entry* 0x00                      children sorted by name, strictly increasing
          file:     size(uv) [ payload_offset(uv) sha256(32) ]    the bracket only if size > 0
          symlink:  target
          hardlink: path of the first link
          char|block: major(uv) minor(uv)
          fifo:     (nothing)
kind   := 1 dir | 2 file | 3 symlink | 4 hardlink | 5 char | 6 block | 7 fifo
```

A name never contains `/` and is not `.` or `..`; nesting is bounded. A decoder rejects a bad digest, unknown
kinds, unsorted or repeated names and trailing bytes. Extraction checks every file against its `sha256`.

## 10. Escrow blob

```
"IBW1" | key domain id (16 ASCII hex) | ephemeral X25519 public (32) | nonce (24) | XChaCha20-Poly1305(master) (32+16)
key  = HKDF-SHA256(ECDH(ephemeral, recipient), salt = ephemeral_pub | recipient_pub, info = "incus-backup escrow key v1")
aad  = "incus-backup escrow v1" | key domain id | recipient_pub | ephemeral_pub
```

The printable keys are `IBE1-` (private) and `IBP1-` (public) followed by base32 groups of five and a four byte
SHA-256 checksum. Unwrapping also checks that the key it opens belongs to the key domain the blob names.
