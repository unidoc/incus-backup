// Public types of the web-crypto package.

/** One catalog entry (internal/webcore.EntryInfo). */
export interface EntryInfo {
  /** Path relative to the snapshot root, "" for the root, no leading slash. */
  path: string;
  name: string;
  kind: EntryKind;
  /** Permission bits incl. setuid/setgid/sticky (no file type bits). */
  mode: number;
  /** ls -l style, e.g. "drwxr-xr-x". */
  modeString: string;
  uid: number;
  gid: number;
  /** Nanoseconds since the epoch, decimal (does not fit a JS number). */
  mtimeNs: string;
  ctimeNs: string;
  inode: string;
  /** Bytes, regular files only (0 otherwise). */
  size: number;
  /** Hex SHA-256 of the content, regular files with size > 0. */
  sha256?: string;
  /** Symlink target, or for a hardlink the path of the entry holding the content. */
  link?: string;
  major?: number;
  minor?: number;
  /** Extended attributes (POSIX ACLs are system.posix_acl_*); value is base64. */
  xattrs?: { name: string; value: string }[];
  /** Number of children (directories). */
  children?: number;
}

export type EntryKind = 'dir' | 'file' | 'symlink' | 'hardlink' | 'char' | 'block' | 'fifo';

/** Counts of a catalog (fscat.Stats). */
export interface TreeStats {
  Files: number;
  Dirs: number;
  Symlinks: number;
  Hardlinks: number;
  Special: number;
  Bytes: number;
}

/** Decrypted manifest (format.Manifest JSON). */
export interface Manifest {
  format: number;
  snapshot_id: string;
  previous_id?: string;
  created: string;
  host: string;
  project: string;
  policy: string;
  object: { name: string; kind: 'block' | 'filesystem' | 'instance' | string; size: number };
  index_digest: string;
  config?: unknown;
  refs?: string[];
  meta?: Record<string, string>;
  stats: {
    logical_bytes: number;
    dirty_bytes: number;
    read_bytes: number;
    uploaded_bytes: number;
    full_read: boolean;
    duration_ms: number;
  };
}

/** Where the GUI gets repository bytes from. Every method returns the raw (encrypted or plaintext-index) object. */
export interface Fetcher {
  index(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array>;
  manifest(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array>;
  chunk(id: string, signal?: AbortSignal): Promise<Uint8Array>;
}

/** Reader-wide fetch statistics, reported after every chunk fetch or cache hit. */
export interface FetchProgress {
  chunksFetched: number;
  bytesFetched: number;
  cacheHits: number;
  inFlight: number;
}

export interface ReaderOptions {
  /** Plaintext bytes kept in the chunk LRU (default 64 MiB). */
  cacheBytes?: number;
  /** Concurrent chunk fetches (default 6). */
  concurrency?: number;
  /** Chunks fetched ahead of the consumer by streams (default = concurrency). */
  readAhead?: number;
  onProgress?: (p: FetchProgress) => void;
}

export interface ReadOptions {
  /** First byte (inclusive, default 0). */
  start?: number;
  /** Last byte (exclusive, default the file size). */
  end?: number;
  signal?: AbortSignal;
  onProgress?: (done: number, total: number) => void;
}

export interface ZipOptions {
  signal?: AbortSignal;
  onProgress?: (done: number, total: number) => void;
  /** Called for every entry that is not written (devices, fifos, symlinks with symlinks: 'skip', over-long paths). */
  onSkip?: (path: string, kind: EntryKind, reason: string) => void;
  /**
   * Symbolic links are never stored as links (a link in an archive can redirect where an extractor
   * writes the next entries). 'skip' (default): left out and reported through onSkip; 'note': each
   * becomes a small text file "<name>.symlink.txt" that names its target.
   */
  symlinks?: 'skip' | 'note';
  /** Called for every entry stored under another name than its original (unsafe characters, collisions). */
  onRename?: (r: { original: string; stored: string; reasons: string[] }) => void;
  /** Lower caps than the defaults (ZIP_LIMITS); the defaults are the maximum. */
  limits?: Partial<{ maxEntries: number; maxEntryBytes: number; maxTotalBytes: number; maxNameBytes: number }>;
  /** Write ZIP64 records for every entry even when not needed (tests). */
  forceZip64?: boolean;
}
