# Web console: Protection & recovery

The pages under "Protection" (Snapshots, Restore, Verification) and "Operations / Jobs", with the
zero-knowledge file browser. They are built on `web-crypto/` (see `docs/web-crypto.md`) and the host
jobs (see `docs/jobs.md`).

## Pages

| route | file | what |
|---|---|---|
| `/snapshots` | `views/backups/SnapshotList.vue` | Every snapshot of the repository. It has verification tiles and filters kept in the URL (host, project, object, kind, policy, protection, verification, date range). The table shows size, changed/uploaded bytes, duration, full/incremental read, a verification badge, a lock and a notes indicator. Bulk protect/unprotect needs the operator role. Bulk forget needs the admin role and a typed confirmation; protected snapshots are refused and listed with the reason. |
| `/snapshots/:group/:snap` | `SnapshotDetail.vue` | Metadata, previous/next snapshot, and "what changed" (changed and uploaded bytes, size delta, read mode, base snapshot). It also shows integrity: the verification history and the quarantined chunks that affect this snapshot. There are protect and notes controls, the restore tests of the object, a forget button (admin, typed id) and every restore action that fits the kind. |
| `/snapshots/:group/:snap/browse` | `Browser.vue` | The zero-knowledge file browser (below). |
| `/restore` | `views/restore/Restore.vue` | Restore flows as host jobs: `restore_instance`, `restore_volume`, `guest_files`, `fs_extract`. Each flow is a wizard (object+snapshot, options, review, live progress). `?action=&group=&snap=&instance=&project=` preselects. |
| `/verify` | `views/verify/Verify.vue` | Shows coverage (recent/stale/never/failed) and quarantined chunks. You can verify on the server (`POST /verify`, mode new/stale/all) or start a host deep verify (job `deep_verify`). It lists what needs attention and the verify/scrub history. |
| `/jobs`, `/jobs/:id` | `views/jobs/JobList.vue`, `JobDetail.vue` | Live list with filters in the URL. Detail page: progress, log with follow, params (the browser's public key shortened), timeline, result, cancel (operator). |
| `/restore/tests` | `views/jobs/RestoreTests.vue` | A restore-confidence gauge and the latest test per object (passed/failed/overdue/never, compared with the policy's `restore_test_schedule`). Also failure details, the history per object, and "Run now" (operator, method deep/extract/full). |

Shared pieces: `components/protect/` holds `JobPanel` (a live job via `useJob`: SSE `job` events
plus polling), `KeyGate`, `FileInfo`, `VerifyBadge`, `PIcon` (extra icons) and `useSnapRows`.
`lib/protect/` holds the pure logic with unit tests: crypto glue, files, save, snapshots, jobs.
API functions and types live in `api/sections/protect.ts`.

## The file browser and the key

1. **Key step.** The user pastes the recovery key (`IBR1-…`) or loads a text file that holds it. A
   pasted host key file (`key.json`) or escrow key is recognised, and the page explains where the
   recovery key comes from (`incus-backup key recovery`).
   * The string goes straight to `openKey()` inside the WebAssembly module (in a worker), and the
     field is cleared.
   * The page keeps only the key handle, in component memory: not in a store, the URL or storage.
   * `key.domainId` is compared with the repository's `domain_id`. If they differ, the page says
     "This key belongs to another repository" and drops the key.
2. **Open.** The manifest, index and catalog are fetched through `ApiFetcher`. It uses the console's
   API client, so `?repository=` is sent, and it checks every id format before any URL is built. They
   are then authenticated in the module. A failure is explained (`explainCryptoError`): wrong key vs
   tampering (told apart by the domain id), missing chunk, offline repository, module failed to load.
3. **Browse.**
   * Breadcrumb, sortable columns (folders first, natural names, nanosecond mtimes), icons, and
     symlink and hard-link targets.
   * A filter for the current list, and a glob/substring search over the subtree (`tree.find`, first
     1000 matches).
   * File info: mode, uid/gid, mtime/ctime, inode, xattr names and the recorded SHA-256.
   * Previews of text (≤ 512 KiB) and images (≤ 8 MiB, as a `data:` URL because the CSP has no
     `blob:`). A preview reads the **whole** file, so the SHA-256 is checked before anything is shown.
4. **Download.** A single file goes through `readFile`; a multi-select, folder or everything goes
   through `extractZip`. Each download shows progress, bytes/s and a cancel button, with fetch and
   cache statistics in the header.
   * Small downloads are collected and handed to the browser only after the stream closed normally.
   * Above 256 MiB the page streams to disk through `showSaveFilePicker` when the browser has it, and
     aborts the file on an error.
   * A failed integrity check fails the download ("Integrity check failed: the file was not saved").
5. **Wipe.** "Lock and wipe the key", leaving the page and `pagehide` all close the snapshot and the
   key and call `zeroize()`.

Disk (block) snapshots show an explanation and a link to the `guest_files` recovery job. Instance
snapshots link to `restore_instance`. Archive-format (tar) filesystem snapshots link to `fs_extract`.

## Sealed deliveries (guest_files, fs_extract)

1. On "Start" the page calls `generateSealKeypair()` and sends the public key (standard base64) as
   `recipient_public_key`. The private key stays in component memory.
2. When the job succeeds, "Decrypt and save" streams `GET /jobs/{id}/delivery` (not buffered) through
   `unsealStream` and saves the tar.
3. A truncated or altered stream fails ("The delivery was altered or truncated"). Nothing is saved.
4. Leaving the page asks for confirmation (`beforeunload` and the in-page back button), because the
   key cannot be recovered. The server deletes the delivery one hour after the job ends.

## Roles in the UI

| action | role needed | otherwise |
|---|---|---|
| the browser and restores | restore | the route meta hides the pages |
| cancel a job, protect, notes, verify, deep verify, restore test "Run now" | operator | button disabled with a tooltip |
| forget | admin | button not shown |

Checks use `hasRole()`. The perms mirror in `api/perms.ts` is not touched by this section (see the
report).

## Build

`just web` runs `npm run crypto`, which does three things:
* runs `web-crypto/scripts/build-wasm.sh`;
* runs `npm ci` in `web-crypto`;
* runs `tsc` to build `web-crypto/dist`.

`npm ci && npm run build` follow. The web package depends on `@incus-backup/web-crypto` as
`file:../web-crypto` (types only). The plugin `web/build/crypto.ts` ships the built files (see
`docs/api-protect.md`).

## Tests

* `npm test`: `lib/protect/*.test.ts` (files, snapshots, jobs, crypto glue), 33 tests.
* `e2e/protect-demo.spec.ts` (project `demo`) runs against the seeded demo. Its host-job parts need a
  demo whose hosts run the job executor with every type allowed.
  * Key entry, then browse, preview, and a single-file download compared byte for byte with the
    demo's source file (SHA-256 equal to the seed's record).
  * A folder ZIP: `zipfile.testzip()` passes and every file equals the source.
  * A multi-select ZIP, and a glob search.
  * Lock.
  * Every request URL, header and body is scanned for the key and for 10-character windows of it.
  * Another repository's valid key: "belongs to another repository". A typo key: invalid. A
    `key.json`: explained.
  * A disk snapshot: not browsable, and it offers `guest_files` with the snapshot preselected.
  * `fs_extract` end to end. The key pair is made in the page, the real host executor seals the
    files, and the page unseals and saves. The tar's files equal the source. The job then appears on
    the job pages with its log.
  * Restore test "Run now" through to success, a server verification run, the failed filter, bulk
    protect/unprotect as operator, and the admin's forget refusing protected snapshots.
  * Screenshots of every page in light/dark at 1440×900 and 390×844, with no horizontal scroll on
    the phone and no console or CSP error.
* Tamper detection cannot be staged against a real server in e2e. It is covered by web-crypto's
  tests (flipped, truncated or substituted chunks, a swapped index or manifest, sealed-stream
  alteration).
