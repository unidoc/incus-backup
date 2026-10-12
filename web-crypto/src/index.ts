// @incus-backup/web-crypto: browse and download incus-backup snapshots in the browser without the
// server ever seeing a key or plaintext. See README.md for the API and the threat model.
export { openKey, openEscrowKey, zeroize, KeyHandle, type KeyOptions } from './key.js';
export {
  RepoReader,
  checkGroup,
  checkSnapshot,
  checkChunkId,
  type OpenedSnapshot,
  type CatalogSnapshot,
  type UnsupportedSnapshot,
  type SnapshotBase,
  type Tree,
} from './reader.js';
export { HttpFetcher, type HttpFetcherOptions } from './http.js';
export { unsealStream, generateSealKeypair, importSealKey, requestDigest, SealKey, type SealKeypair, type DeliveryRequest, type UnsealOptions } from './seal.js';
export { configure, defaultCore, createCore, shutdown, Core, DirectTransport, WorkerTransport, PoolTransport, GoError, type CoreConfig, type CoreMode, type Transport, type ObjectIdentity } from './core.js';
export { IbError, KeyError, IntegrityError, FetchError } from './errors.js';
export { crc32 } from './crc32.js';
export { zipStream, safeComponent, ZipNamer, ZIP_LIMITS, type ZipEntry, type ZipLimits, type ZipRename, type ZipStreamOptions } from './zip.js';
export type {
  EntryInfo,
  EntryKind,
  TreeStats,
  Manifest,
  Fetcher,
  FetchProgress,
  ReaderOptions,
  ReadOptions,
  ZipOptions,
} from './types.js';
