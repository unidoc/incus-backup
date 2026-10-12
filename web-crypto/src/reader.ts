// RepoReader: opens snapshots through a Fetcher and decrypts them locally.
import { childController, LRU, raceSignal, Semaphore } from './cache.js';
import type { Core, FileRef, ObjectIdentity, SnapshotInfo } from './core.js';
import { abortError, IbError, IntegrityError, throwIfAborted } from './errors.js';
import type { KeyHandle } from './key.js';
import type { EntryInfo, EntryKind, FetchProgress, Fetcher, Manifest, ReadOptions, ReaderOptions, TreeStats, ZipOptions } from './types.js';
import { ZIP_LIMITS, zipStream, type ZipEntry, type ZipRename, type ZipStreamOptions } from './zip.js';

const GROUP_RE = /^[0-9a-f]{32}$/;
const SNAP_RE = /^[0-9]{14}-[0-9a-f]{8}$/;
const ID_RE = /^[0-9a-f]{64}$/;

/** Throws unless s is a group id (32 lowercase hex). Keeps arbitrary strings out of fetch URLs. */
export function checkGroup(s: string): void {
  if (!GROUP_RE.test(s)) throw new IbError(`not a group id: ${JSON.stringify(s)}`);
}
export function checkSnapshot(s: string): void {
  if (!SNAP_RE.test(s)) throw new IbError(`not a snapshot id: ${JSON.stringify(s)}`);
}
export function checkChunkId(s: string): void {
  if (!ID_RE.test(s)) throw new IbError(`not a chunk id: ${JSON.stringify(s)}`);
}

interface Flight {
  promise: Promise<Uint8Array>;
  ctrl: AbortController;
  waiters: number;
}

/** Fetches, verifies, decrypts and caches chunks; shared by every snapshot of one reader. */
class ChunkStore {
  readonly cache: LRU<Uint8Array>;
  private sem: Semaphore;
  private flights = new Map<string, Flight>();
  readonly stats: FetchProgress = { chunksFetched: 0, bytesFetched: 0, cacheHits: 0, inFlight: 0 };

  constructor(
    private key: KeyHandle,
    private fetcher: Fetcher,
    private opts: Required<Pick<ReaderOptions, 'cacheBytes' | 'concurrency'>> & ReaderOptions,
  ) {
    this.cache = new LRU(opts.cacheBytes);
    this.sem = new Semaphore(opts.concurrency);
  }

  private report(): void {
    this.stats.inFlight = this.sem.active;
    this.opts.onProgress?.({ ...this.stats });
  }

  /** The plaintext of chunk id (size bytes); "" is the all-zero chunk. Callers must not modify it. */
  async get(id: string, size: number, signal?: AbortSignal): Promise<Uint8Array> {
    throwIfAborted(signal);
    if (id === '') return new Uint8Array(size);
    const hit = this.cache.get(id);
    if (hit) {
      this.stats.cacheHits++;
      this.report();
      return hit;
    }
    let f = this.flights.get(id);
    if (!f) {
      const ctrl = new AbortController();
      const flight: Flight = { ctrl, waiters: 0, promise: undefined as unknown as Promise<Uint8Array> };
      flight.promise = (async () => {
        checkChunkId(id);
        await this.sem.acquire(ctrl.signal);
        let pt: Uint8Array;
        try {
          this.report();
          const blob = await this.fetcher.chunk(id, ctrl.signal);
          throwIfAborted(ctrl.signal);
          this.stats.chunksFetched++;
          this.stats.bytesFetched += blob.byteLength;
          pt = await this.key.core.decodeChunk(this.key.check(), blob, id);
        } finally {
          this.sem.release();
          this.report();
        }
        if (pt.byteLength !== size) throw new IntegrityError(`chunk ${id} holds ${pt.byteLength} bytes, the index says ${size}`);
        this.cache.set(id, pt);
        return pt;
      })();
      flight.promise.then(
        () => this.flights.delete(id),
        () => this.flights.delete(id),
      );
      this.flights.set(id, flight);
      f = flight;
    }
    const fl = f;
    fl.waiters++;
    try {
      return await raceSignal(fl.promise, signal);
    } finally {
      fl.waiters--;
      if (fl.waiters === 0 && signal?.aborted) {
        fl.ctrl.abort(signal.reason);
        this.flights.delete(id);
      }
    }
  }
}

/** A piece of payload, and whether it is part of an all-zero chunk. */
interface Piece {
  data: Uint8Array;
  zero: boolean;
}

/** Opens snapshots of one key domain. */
export class RepoReader {
  /** @internal */ readonly store: ChunkStore;
  /** @internal */ readonly readAhead: number;

  constructor(
    readonly key: KeyHandle,
    readonly fetcher: Fetcher,
    opts: ReaderOptions = {},
  ) {
    const concurrency = opts.concurrency ?? 6;
    this.readAhead = Math.max(1, opts.readAhead ?? concurrency);
    this.store = new ChunkStore(key, fetcher, { ...opts, cacheBytes: opts.cacheBytes ?? 64 << 20, concurrency });
  }

  /** Reader-wide fetch statistics. */
  get stats(): FetchProgress {
    return { ...this.store.stats };
  }

  /** Drops every cached plaintext chunk. */
  clearCache(): void {
    this.store.cache.clear();
  }

  /**
   * Fetches and verifies a snapshot's manifest and index, and for catalog snapshots its catalog.
   * Rejects with IntegrityError when anything fails to authenticate (wrong key, tampering).
   */
  async openSnapshot(group: string, snap: string, opts: { signal?: AbortSignal; expect?: ObjectIdentity } = {}): Promise<OpenedSnapshot> {
    checkGroup(group);
    checkSnapshot(snap);
    const { signal } = opts;
    const [manifest, index] = await Promise.all([this.fetcher.manifest(group, snap, signal), this.fetcher.index(group, snap, signal)]);
    throwIfAborted(signal);
    // the module checks that the manifest's object hashes to `group` and, with `expect`, that `group`
    // is the object the caller shows (the server's listing chooses the labels)
    const info = await this.key.core.openSnapshot(this.key.check(), group, snap, manifest, index, opts.expect);
    const base = { group, snap, manifest: info.manifest as Manifest, kind: info.kind, size: info.size };
    if (info.format !== 'catalog') {
      const unsupported =
        info.format === 'archive'
          ? 'archive format cannot be browsed'
          : info.format === 'block'
            ? 'block device snapshots cannot be browsed as files'
            : 'instance snapshots hold configuration only; browse their storage objects';
      return { ...base, format: info.format, browsable: false, unsupported } as UnsupportedSnapshot;
    }
    const snapObj = new CatalogSnapshotImpl(this, info, base);
    await snapObj.load(signal);
    return snapObj;
  }

  /** @internal Pieces of [from, to) of the index's byte space, with bounded read-ahead. */
  async *range(info: SnapshotInfo, from: number, to: number, signal?: AbortSignal): AsyncGenerator<Piece> {
    if (from >= to) return;
    const ends = info.ends;
    // first entry whose end is after `from`
    let lo = 0;
    let hi = ends.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (ends[mid]! > from) hi = mid;
      else lo = mid + 1;
    }
    const first = lo;
    if (first >= ends.length) throw new IbError(`offset ${from} is outside the snapshot`);
    const { ctrl, dispose } = childController(signal);
    const q: Promise<Uint8Array>[] = [];
    let next = first;
    const startOf = (i: number) => (i === 0 ? 0 : ends[i - 1]!);
    const fill = () => {
      while (q.length < this.readAhead && next < ends.length && startOf(next) < to) {
        const i = next++;
        const p = this.store.get(info.ids[i]!, ends[i]! - startOf(i), ctrl.signal);
        p.catch(() => undefined); // observed when awaited; silence rejections of unread read-ahead
        q.push(p);
      }
    };
    try {
      for (let i = first; i < ends.length && startOf(i) < to; i++) {
        fill();
        const pt = await raceSignal(q.shift()!, signal);
        const s = startOf(i);
        const a = Math.max(from, s) - s;
        const b = Math.min(to, ends[i]!) - s;
        // a copy: consumers may transfer or detach what they receive, the cache must stay intact
        yield { data: pt.slice(a, b), zero: info.ids[i] === '' };
      }
    } finally {
      ctrl.abort(abortError(signal));
      dispose();
    }
  }
}

export interface SnapshotBase {
  group: string;
  snap: string;
  /** The decrypted manifest (meta holds fs.format, fs.payload_size, ...). */
  manifest: Manifest;
  /** manifest.object.kind: 'block' | 'filesystem' | 'instance'. */
  kind: string;
  /** Logical size of the snapshot's index byte space. */
  size: number;
}

export interface UnsupportedSnapshot extends SnapshotBase {
  format: 'archive' | 'block' | 'instance';
  browsable: false;
  /** Why it cannot be browsed, e.g. 'archive format cannot be browsed'. */
  unsupported: string;
}

export interface Tree {
  /** Number of entries including the root. */
  readonly count: number;
  readonly stats: TreeStats;
  /** Children of a directory (the entry itself when it is not a directory). */
  list(path: string): Promise<EntryInfo[]>;
  stat(path: string): Promise<EntryInfo>;
  /** Paths below root whose base name matches glob (Go path.Match syntax: * ? [a-z]). */
  find(root: string, glob: string): Promise<string[]>;
  /** Pre-order walk of the subtree at root (root included), fetched from the module in batches. */
  walk(root?: string, opts?: { batch?: number; signal?: AbortSignal }): AsyncGenerator<EntryInfo>;
  /** Entries and total file bytes at and below path. */
  subtree(path: string): Promise<{ entries: number; bytes: number }>;
}

export interface CatalogSnapshot extends SnapshotBase {
  format: 'catalog';
  browsable: true;
  tree: Tree;
  /**
   * Streams a regular file (hard links resolved) or a byte range of it, fetching only the chunks
   * that cover the range. A whole-file read is checked against the catalog's SHA-256 BEFORE its
   * last bytes are delivered: the stream errors (IntegrityError) instead of closing on mismatch.
   */
  readFile(path: string, opts?: ReadOptions): ReadableStream<Uint8Array>;
  /**
   * Streams a ZIP of the given files/directories (all when empty); every file is SHA-256 verified
   * before its last bytes are written. The second argument is a ZipOptions or just an AbortSignal.
   */
  extractZip(paths: string[], opts?: ZipOptions | AbortSignal): ReadableStream<Uint8Array>;
  /**
   * A safe download name: one path gives its base name (plus ".zip" when zip is true, e.g. for a
   * directory); none or several give "<object>-<snapshot>.zip". zip defaults to paths.length !== 1.
   */
  suggestedFilename(paths?: string[], zip?: boolean): string;
  /** Forgets the catalog inside the module. */
  close(): Promise<void>;
}

export type OpenedSnapshot = CatalogSnapshot | UnsupportedSnapshot;

/**
 * A pull-driven stream over gen. cancel() aborts ctrl first, so a generator waiting on a stalled
 * fetch is released at once (awaiting gen.return() alone would queue behind that wait).
 */
function streamOf(gen: AsyncGenerator<Uint8Array>, ctrl: AbortController, dispose: () => void): ReadableStream<Uint8Array> {
  return new ReadableStream<Uint8Array>(
    {
      async pull(c) {
        try {
          const r = await gen.next();
          if (r.done) {
            dispose();
            c.close();
          } else c.enqueue(r.value);
        } catch (e) {
          dispose();
          try {
            c.error(e);
          } catch {
            // already cancelled
          }
        }
      },
      cancel(reason) {
        ctrl.abort(reason instanceof Error ? reason : abortError());
        dispose();
        gen.return(undefined).catch(() => undefined);
      },
    },
    { highWaterMark: 0 },
  );
}

function isSignal(x: unknown): x is AbortSignal {
  return typeof AbortSignal !== 'undefined' && x instanceof AbortSignal;
}

function clean(p: string): string {
  return p
    .split('/')
    .filter((x) => x !== '' && x !== '.')
    .join('/');
}

function sanitize(s: string): string {
  return s.replace(/[\\/:*?"<>|\x00-\x1f]+/g, '_').replace(/^\.+/, '_') || 'download';
}

function mtimeSec(e: EntryInfo): number {
  return Number(BigInt(e.mtimeNs) / 1000000000n);
}

class CatalogSnapshotImpl implements CatalogSnapshot {
  readonly format = 'catalog' as const;
  readonly browsable = true as const;
  readonly group: string;
  readonly snap: string;
  readonly manifest: Manifest;
  readonly kind: string;
  readonly size: number;
  tree!: Tree;
  private cat = 0;
  private closed = false;

  constructor(
    private r: RepoReader,
    private info: SnapshotInfo,
    base: SnapshotBase,
  ) {
    this.group = base.group;
    this.snap = base.snap;
    this.manifest = base.manifest;
    this.kind = base.kind;
    this.size = base.size;
  }

  private get core(): Core {
    return this.r.key.core;
  }

  private catalog(): number {
    if (this.closed) throw new IbError('the snapshot was closed');
    return this.cat;
  }

  async load(signal?: AbortSignal): Promise<void> {
    const buf = new Uint8Array(this.info.catalogSize);
    let o = 0;
    for await (const p of this.r.range(this.info, this.info.payloadSize, this.info.size, signal)) {
      buf.set(p.data, o);
      o += p.data.length;
    }
    if (o !== buf.length) throw new IntegrityError('the catalog is shorter than the manifest says');
    const c = await this.core.catalogOpen(buf);
    this.cat = c.cat;
    const self = this;
    this.tree = {
      count: c.count,
      stats: c.stats,
      list: (p) => self.core.list(self.catalog(), p),
      stat: (p) => self.core.stat(self.catalog(), p),
      find: (root, glob) => self.core.find(self.catalog(), root, glob),
      subtree: (p) => self.core.subtree(self.catalog(), p),
      walk: (root = '', o = {}) => self.walk(root, o.batch ?? 1000, o.signal),
    };
  }

  private async *walk(root: string, batch: number, signal?: AbortSignal): AsyncGenerator<EntryInfo> {
    const w = await this.core.walkOpen(this.catalog(), root);
    let done = false;
    try {
      while (!done) {
        throwIfAborted(signal);
        const r = await this.core.walkNext(w, batch);
        done = r.done;
        for (const e of r.entries) yield e;
      }
    } finally {
      if (!done) await this.core.walkClose(w);
    }
  }

  /** Content pieces of a resolved file range, hash-verified when it is the whole file. */
  private async *fileData(ref: FileRef, start: number, end: number, signal?: AbortSignal, onProgress?: (d: number, t: number) => void): AsyncGenerator<Uint8Array> {
    if (ref.payOff + ref.size > this.info.payloadSize) throw new IntegrityError(`${ref.path}: content lies outside the payload`);
    const whole = start === 0 && end === ref.size && ref.size > 0;
    let h: number | undefined = whole ? await this.core.hashNew() : undefined;
    const total = end - start;
    let done = 0;
    let held: Uint8Array | undefined;
    try {
      for await (const p of this.r.range(this.info, ref.payOff + start, ref.payOff + end, signal)) {
        if (h !== undefined) {
          if (p.zero) await this.core.hashZeros(h, p.data.length);
          else await this.core.hashUpdate(h, p.data);
        }
        // hold back the newest piece: the last one is delivered only after the digest matched
        if (held) yield held;
        held = p.data;
        done += p.data.length;
        onProgress?.(done, total);
      }
      if (done !== total) throw new IntegrityError(`${ref.path}: read ${done} bytes, expected ${total}`);
      if (h !== undefined) {
        const sum = await this.core.hashSum(h);
        h = undefined;
        if (sum !== ref.sha256) throw new IntegrityError(`${ref.path}: content does not match its recorded SHA-256`);
      }
      if (held) yield held;
    } finally {
      if (h !== undefined) await this.core.hashSum(h).catch(() => undefined);
    }
  }

  readFile(path: string, opts: ReadOptions = {}): ReadableStream<Uint8Array> {
    const self = this;
    const { ctrl, dispose } = childController(opts.signal);
    const signal = ctrl.signal;
    async function* gen(): AsyncGenerator<Uint8Array> {
      throwIfAborted(signal);
      const ref = await self.core.resolveFile(self.catalog(), path);
      const start = opts.start ?? 0;
      const end = Math.min(opts.end ?? ref.size, ref.size);
      if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || start > end) {
        throw new RangeError(`bad range [${start}, ${end}) for a file of ${ref.size} bytes`);
      }
      yield* self.fileData(ref, start, end, signal, opts.onProgress);
    }
    return streamOf(gen(), ctrl, dispose);
  }

  extractZip(paths: string[], optsOrSignal: ZipOptions | AbortSignal = {}): ReadableStream<Uint8Array> {
    const self = this;
    const opts: ZipOptions = isSignal(optsOrSignal) ? { signal: optsOrSignal } : optsOrSignal;
    const { ctrl, dispose } = childController(opts.signal);
    const signal = ctrl.signal;
    // the caller may lower the caps, never raise them
    const limits = { ...ZIP_LIMITS };
    for (const k of Object.keys(limits) as (keyof typeof limits)[]) limits[k] = Math.min(opts.limits?.[k] ?? ZIP_LIMITS[k], ZIP_LIMITS[k]);
    const renames: ZipRename[] = [];
    const enc = new TextEncoder();
    async function* entries(): AsyncGenerator<ZipEntry> {
      let roots = [...new Set((paths.length ? paths : ['']).map(clean))].sort();
      // a root inside another selected root is already covered
      roots = roots.filter((r) => !roots.some((o) => o !== r && (o === '' || r.startsWith(o + '/'))));
      let total = 0;
      let count = 0;
      for (const r of roots) {
        const st = await self.tree.subtree(r);
        total += st.bytes;
        count += st.entries;
      }
      // fail before the first byte when the selection cannot fit the caps
      if (count > limits.maxEntries) {
        throw new IbError(`the selection holds ${count} entries; a ZIP made in the browser holds at most ${limits.maxEntries}. Choose fewer folders, or extract them on the host (Restore, "Extract files on the host").`);
      }
      if (total > limits.maxTotalBytes) {
        throw new IbError(`the selection holds ${total} bytes; a ZIP made in the browser holds at most ${limits.maxTotalBytes}. Choose fewer files, or extract them on the host (Restore, "Extract files on the host").`);
      }
      let done = 0;
      for (const root of roots) {
        for await (const e of self.tree.walk(root, { signal })) {
          throwIfAborted(signal);
          if (e.path === '') continue;
          const base = { name: e.path, mode: e.mode & 0o777, mtime: mtimeSec(e) };
          switch (e.kind as EntryKind) {
            case 'dir':
              yield { ...base, type: 'dir' };
              break;
            case 'file':
            case 'hardlink': {
              const ref = await self.core.resolveFile(self.catalog(), e.path);
              if (e.kind === 'hardlink') total += ref.size; // subtree() counts a hard link as 0 bytes
              const before = done;
              yield {
                ...base,
                type: 'file',
                size: ref.size,
                data: () =>
                  self.fileData(ref, 0, ref.size, signal, (d) => {
                    done = before + d;
                    opts.onProgress?.(done, total);
                  }),
              };
              break;
            }
            case 'symlink':
              if (opts.symlinks === 'note') {
                const body = enc.encode(
                  `This was a symbolic link in the backup.\nTarget: ${e.link ?? ''}\nIt is not stored as a link: a link inside an archive can redirect where an extractor writes files.\n`,
                );
                yield { name: `${e.path}.symlink.txt`, type: 'file', mode: 0o644, mtime: base.mtime, size: body.length, data: async function* () { yield body; } };
              } else {
                opts.onSkip?.(e.path, e.kind, 'symlinks are not stored in archives');
              }
              break;
            default:
              opts.onSkip?.(e.path, e.kind as EntryKind, `a ${e.kind} cannot be stored in a ZIP file`);
          }
        }
      }
      // every renamed entry is listed in the archive itself: nothing changes silently
      if (renames.length) {
        const lines = renames.map((r) => `${JSON.stringify(r.original)} -> ${JSON.stringify(r.stored)} (${r.reasons.join(', ')})`);
        const body = enc.encode(
          `Entries of this archive stored under another name than in the backup\n(characters unsafe on other systems, or names that would collide when extracted):\n\n${lines.join('\n')}\n`,
        );
        yield { name: 'RENAMED-ENTRIES.txt', type: 'file', mode: 0o644, mtime: Math.floor(Date.now() / 1000), size: body.length, data: async function* () { yield body; } };
      }
    }
    const zopts: ZipStreamOptions = {
      forceZip64: opts.forceZip64,
      limits,
      onRename: (r) => {
        renames.push(r);
        opts.onRename?.(r);
      },
      onSkip: (name, reason) => opts.onSkip?.(name, 'file', reason),
    };
    return streamOf(zipStream(entries(), zopts), ctrl, dispose);
  }

  suggestedFilename(paths: string[] = [], zip = paths.length !== 1): string {
    const ps = paths.map(clean);
    if (ps.length === 1 && ps[0] !== '') {
      const base = sanitize(ps[0]!.split('/').pop()!);
      return zip ? base + '.zip' : base;
    }
    const obj = sanitize((this.manifest.object?.name ?? 'snapshot').replace(/\//g, '-'));
    return `${obj}-${this.snap}.zip`;
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    await this.core.catalogClose(this.cat);
  }
}

