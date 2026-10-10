// Core: the typed, asynchronous view of the wasm API. It runs the module either in this realm
// (Node, tests, or code that is already inside a worker) or in a dedicated Web Worker (the
// default in a browser page), so decryption never blocks the page's main thread.
import { IbError, IntegrityError, KeyError } from './errors.js';
import type { EntryInfo, TreeStats } from './types.js';
import { loadWasm, type RawApi, type WasmLocation } from './wasm.js';

/** Moves calls to wherever the module runs. */
export interface Transport {
  call(name: string, args: unknown[], transfer?: Transferable[]): Promise<unknown>;
  terminate?(): void;
}

/** Runs the module in the current realm. */
export class DirectTransport implements Transport {
  private api: Promise<RawApi>;
  constructor(loc: WasmLocation = {}) {
    this.api = loadWasm(loc);
  }
  async call(name: string, args: unknown[]): Promise<unknown> {
    const api = await this.api;
    const f = api[name];
    if (typeof f !== 'function') throw new IbError(`unknown wasm function ${name}`);
    const r = f(...args);
    if (r.err !== undefined) throw new GoError(r.err);
    return r.ok;
  }
}

/** An error message from the Go side (never contains key material). */
export class GoError extends IbError {
  constructor(message: string) {
    super(message);
    this.name = 'GoError';
  }
}

export interface WorkerLocation extends WasmLocation {
  /** URL of dist/worker.js; default: ./worker.js next to this module. */
  workerUrl?: string | URL;
}

/** Runs the module in a dedicated module Web Worker (browser). */
export class WorkerTransport implements Transport {
  private worker: Worker;
  private seq = 0;
  private pending = new Map<number, { resolve: (v: unknown) => void; reject: (e: Error) => void }>();
  private failed: Error | undefined;

  constructor(loc: WorkerLocation = {}) {
    this.worker = new Worker(new URL(loc.workerUrl ?? './worker.js', import.meta.url), { type: 'module', name: 'ibwasm' });
    this.worker.onmessage = (ev: MessageEvent) => {
      const m = ev.data as { id: number; ok?: unknown; err?: string; goErr?: boolean };
      const p = this.pending.get(m.id);
      if (!p) return;
      this.pending.delete(m.id);
      if (m.err !== undefined) p.reject(m.goErr ? new GoError(m.err) : new IbError(m.err));
      else p.resolve(m.ok);
    };
    this.worker.onerror = (ev: ErrorEvent) => {
      this.failed = new IbError(`the crypto worker failed: ${ev.message}`);
      for (const p of this.pending.values()) p.reject(this.failed);
      this.pending.clear();
    };
    const url = (u?: string | URL) => (u === undefined ? undefined : new URL(u, import.meta.url).href);
    this.worker.postMessage({ init: { wasmUrl: url(loc.wasmUrl), wasmExecUrl: url(loc.wasmExecUrl) } });
  }

  call(name: string, args: unknown[], transfer: Transferable[] = []): Promise<unknown> {
    if (this.failed) return Promise.reject(this.failed);
    const id = ++this.seq;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.worker.postMessage({ id, name, args }, transfer);
    });
  }

  /** Calls sent and not yet answered. */
  get load(): number {
    return this.pending.size;
  }

  terminate(): void {
    this.worker.terminate();
    this.failed = new IbError('the crypto worker was terminated');
    for (const p of this.pending.values()) p.reject(this.failed);
    this.pending.clear();
  }
}

/**
 * Several workers, each with its own module instance: chunk decoding (stateless given the key)
 * is spread over all of them, everything stateful (catalogs, walks, hashes, sealed streams) runs
 * in the first. A key is loaded into every worker; callers see one handle.
 */
export class PoolTransport implements Transport {
  private keys = new Map<number, number[]>();
  private nextKey = 0;
  constructor(readonly workers: WorkerTransport[]) {
    if (workers.length === 0) throw new IbError('a pool needs at least one worker');
  }

  private mapped(k: unknown): number[] {
    const hs = this.keys.get(k as number);
    if (!hs) throw new GoError('the key was closed');
    return hs;
  }

  async call(name: string, args: unknown[], transfer: Transferable[] = []): Promise<unknown> {
    const w = this.workers;
    switch (name) {
      case 'loadRecoveryKey':
      case 'loadEscrow': {
        const rs = (await Promise.allSettled(w.map((x) => x.call(name, args)))) as PromiseSettledResult<{ key: number; domainId: string }>[];
        const bad = rs.find((r) => r.status === 'rejected') as PromiseRejectedResult | undefined;
        if (bad) {
          await Promise.all(rs.map((r, i) => (r.status === 'fulfilled' ? w[i]!.call('dropKey', [r.value.key]) : undefined)));
          throw bad.reason;
        }
        const ok = rs as PromiseFulfilledResult<{ key: number; domainId: string }>[];
        const id = ++this.nextKey;
        this.keys.set(
          id,
          ok.map((r) => r.value.key),
        );
        return { key: id, domainId: ok[0]!.value.domainId };
      }
      case 'dropKey': {
        const hs = this.keys.get(args[0] as number);
        this.keys.delete(args[0] as number);
        if (hs) await Promise.all(hs.map((h, i) => w[i]!.call('dropKey', [h])));
        return true;
      }
      case 'zeroize':
        this.keys.clear();
        await Promise.all(w.map((x) => x.call('zeroize', [])));
        return true;
      case 'decodeChunk': {
        const hs = this.mapped(args[0]);
        let best = 0;
        for (let i = 1; i < w.length; i++) if (w[i]!.load < w[best]!.load) best = i;
        return w[best]!.call(name, [hs[best], ...args.slice(1)], transfer);
      }
      case 'groupId':
      case 'openSnapshot':
      case 'unsealOpen': // the stream state and the one-time seal keys live in the first worker
        return w[0]!.call(name, [this.mapped(args[0])[0], ...args.slice(1)], transfer);
      default:
        return w[0]!.call(name, args, transfer);
    }
  }

  terminate(): void {
    for (const x of this.workers) x.terminate();
  }
}

/** Raw snapshot description returned by Core.openSnapshot. */
export interface SnapshotInfo {
  manifest: unknown;
  kind: string;
  format: 'catalog' | 'archive' | 'block' | 'instance';
  indexKind: number;
  chunkSize: number;
  size: number;
  ids: string[];
  ends: Float64Array;
  payloadSize: number;
  catalogSize: number;
  payloadEntries: number;
}

export interface FileRef {
  path: string;
  size: number;
  payOff: number;
  sha256: string;
}

const integrityHints = [
  'authentication failed',
  'failed authentication',
  'identity does not match',
  'IBS1',
  'trailer',
  'sealed stream',
  'does not match',
  'digest mismatch',
  'bad magic',
  'truncated',
  'catalog',
  'index:',
  'manifest:',
  'unsafe name',
  'not sorted',
  'layout',
  'chunk boundary',
  'blob',
  'decompressing',
];

function asIntegrity(e: unknown): never {
  if (e instanceof GoError && integrityHints.some((h) => e.message.includes(h))) throw new IntegrityError(e.message);
  throw e;
}

function copyIfShared(b: Uint8Array): Uint8Array {
  // a view into a larger buffer would transfer (detach) that whole buffer: copy instead
  return b.byteOffset === 0 && b.byteLength === b.buffer.byteLength ? b : b.slice();
}

/** What a server listing claims a snapshot group is (checked against the keyed group id). */
export interface ObjectIdentity {
  project: string;
  kind: string;
  name: string;
}

/** Typed access to the wasm functions. */
export class Core {
  constructor(readonly transport: Transport) {}

  /** Ends the module's workers (every key, catalog and stream inside them is gone). */
  terminate(): void {
    this.transport.terminate?.();
  }

  private c(name: string, args: unknown[], transfer?: Transferable[]): Promise<unknown> {
    return this.transport.call(name, args, transfer);
  }

  async loadRecoveryKey(s: string): Promise<{ key: number; domainId: string }> {
    try {
      return (await this.c('loadRecoveryKey', [s])) as { key: number; domainId: string };
    } catch (e) {
      if (e instanceof GoError) throw new KeyError(e.message);
      throw e;
    }
  }
  async loadEscrow(wrapped: Uint8Array, priv: string): Promise<{ key: number; domainId: string }> {
    try {
      return (await this.c('loadEscrow', [wrapped, priv])) as { key: number; domainId: string };
    } catch (e) {
      if (e instanceof GoError) throw new KeyError(e.message);
      throw e;
    }
  }
  async dropKey(key: number): Promise<void> {
    await this.c('dropKey', [key]);
  }
  async zeroize(): Promise<void> {
    await this.c('zeroize', []);
  }
  async groupId(key: number, project: string, kind: string, name: string): Promise<string> {
    return (await this.c('groupId', [key, project, kind, name])) as string;
  }
  async openSnapshot(key: number, group: string, snap: string, manifest: Uint8Array, index: Uint8Array, expect?: ObjectIdentity): Promise<SnapshotInfo> {
    let r: { info: string; ends: Float64Array };
    try {
      const args: unknown[] = [key, group, snap, manifest, index];
      if (expect) args.push(JSON.stringify({ project: expect.project, kind: expect.kind, name: expect.name }));
      r = (await this.c('openSnapshot', args)) as { info: string; ends: Float64Array };
    } catch (e) {
      asIntegrity(e);
    }
    const info = JSON.parse(r.info) as Omit<SnapshotInfo, 'ends'>;
    return { ...info, ends: r.ends };
  }
  /** Decodes one chunk (the blob is copied, never transferred: the caller keeps it). */
  async decodeChunk(key: number, blob: Uint8Array, id: string): Promise<Uint8Array> {
    try {
      return (await this.c('decodeChunk', [key, blob, id])) as Uint8Array;
    } catch (e) {
      asIntegrity(e);
    }
  }
  async catalogOpen(bytes: Uint8Array): Promise<{ cat: number; count: number; stats: TreeStats }> {
    const b = copyIfShared(bytes);
    let r: { cat: number; count: number; stats: string };
    try {
      r = (await this.c('catalogOpen', [b], [b.buffer as ArrayBuffer])) as { cat: number; count: number; stats: string };
    } catch (e) {
      asIntegrity(e);
    }
    return { cat: r.cat, count: r.count, stats: JSON.parse(r.stats) as TreeStats };
  }
  async catalogClose(cat: number): Promise<void> {
    await this.c('catalogClose', [cat]);
  }
  async list(cat: number, path: string): Promise<EntryInfo[]> {
    return JSON.parse((await this.c('list', [cat, path])) as string) as EntryInfo[];
  }
  async stat(cat: number, path: string): Promise<EntryInfo> {
    return JSON.parse((await this.c('stat', [cat, path])) as string) as EntryInfo;
  }
  async find(cat: number, root: string, pattern: string): Promise<string[]> {
    return JSON.parse((await this.c('find', [cat, root, pattern])) as string) as string[];
  }
  async subtree(cat: number, path: string): Promise<{ entries: number; bytes: number }> {
    return (await this.c('subtree', [cat, path])) as { entries: number; bytes: number };
  }
  async resolveFile(cat: number, path: string): Promise<FileRef> {
    return JSON.parse((await this.c('resolveFile', [cat, path])) as string) as FileRef;
  }
  async walkOpen(cat: number, root: string): Promise<number> {
    return (await this.c('walkOpen', [cat, root])) as number;
  }
  async walkNext(w: number, max: number): Promise<{ entries: EntryInfo[]; done: boolean }> {
    const r = (await this.c('walkNext', [w, max])) as { entries: string; done: boolean };
    return { entries: JSON.parse(r.entries) as EntryInfo[], done: r.done };
  }
  async walkClose(w: number): Promise<void> {
    await this.c('walkClose', [w]);
  }
  async hashNew(): Promise<number> {
    return (await this.c('hashNew', [])) as number;
  }
  async hashUpdate(h: number, data: Uint8Array): Promise<void> {
    await this.c('hashUpdate', [h, data]);
  }
  async hashZeros(h: number, n: number): Promise<void> {
    await this.c('hashZeros', [h, n]);
  }
  async hashSum(h: number): Promise<string> {
    return (await this.c('hashSum', [h])) as string;
  }
  /** A one-time X25519 key pair: the private half stays inside the module (handle `seal`). */
  async sealNew(): Promise<{ seal: number; publicKey: Uint8Array }> {
    return (await this.c('sealNew', [])) as { seal: number; publicKey: Uint8Array };
  }
  /** Moves an existing private key into the module (interoperability, tests). */
  async sealImport(priv: Uint8Array): Promise<{ seal: number; publicKey: Uint8Array }> {
    try {
      return (await this.c('sealImport', [priv])) as { seal: number; publicKey: Uint8Array };
    } catch (e) {
      if (e instanceof GoError) throw new KeyError(e.message);
      throw e;
    }
  }
  async sealDrop(seal: number): Promise<void> {
    await this.c('sealDrop', [seal]);
  }
  /** jobspec.RequestDigest of the parameters this page sent (validated by the same Go code as the host). */
  async requestDigest(type: string, params: string): Promise<Uint8Array> {
    return (await this.c('requestDigest', [type, params])) as Uint8Array;
  }
  async unsealOpen(key: number, seal: number, jobId: string, digest: Uint8Array): Promise<number> {
    try {
      return (await this.c('unsealOpen', [key, seal, jobId, digest])) as number;
    } catch (e) {
      if (e instanceof GoError) throw new KeyError(e.message);
      throw e;
    }
  }
  async unsealPush(u: number, data: Uint8Array): Promise<Uint8Array> {
    try {
      return (await this.c('unsealPush', [u, data])) as Uint8Array;
    } catch (e) {
      if (e instanceof GoError) throw new IntegrityError(e.message);
      throw e;
    }
  }
  async unsealEnd(u: number): Promise<void> {
    try {
      await this.c('unsealEnd', [u]);
    } catch (e) {
      if (e instanceof GoError) throw new IntegrityError(e.message);
      throw e;
    }
  }
  async unsealClose(u: number): Promise<void> {
    await this.c('unsealClose', [u]);
  }
}

export type CoreMode = 'auto' | 'worker' | 'direct';

export interface CoreConfig extends WorkerLocation {
  /** 'auto' (default): Web Workers in a browser page, the current realm elsewhere (Node, inside a worker). */
  mode?: CoreMode;
  /** Number of decoding workers in worker mode (default: hardwareConcurrency - 1, between 1 and 4). */
  workers?: number;
}

let config: CoreConfig = {};
let shared: Core | undefined;

/** Sets where the module and worker are loaded from; call before the first openKey. */
export function configure(c: CoreConfig): void {
  config = { ...c };
  shared = undefined;
}

function inPage(): boolean {
  return typeof window !== 'undefined' && typeof document !== 'undefined' && typeof Worker !== 'undefined';
}

function transportFor(c: CoreConfig): Transport {
  const mode = c.mode ?? 'auto';
  if (!(mode === 'worker' || (mode === 'auto' && inPage()))) return new DirectTransport(c);
  const hc = (globalThis.navigator as { hardwareConcurrency?: number } | undefined)?.hardwareConcurrency ?? 2;
  const n = Math.max(1, Math.min(c.workers ?? Math.min(4, hc - 1), 16));
  const ws = Array.from({ length: n }, () => new WorkerTransport(c));
  return n === 1 ? ws[0]! : new PoolTransport(ws);
}

/**
 * Terminates the process-wide Core's workers, which discards everything inside them (keys, seal
 * keys, catalogs, streams); the next use starts fresh workers. Call after zeroize() when the page
 * locks: a terminated worker's memory is released by the browser, a zeroized one only overwritten
 * where Go allows.
 */
export function shutdown(): void {
  const c = shared;
  shared = undefined;
  c?.terminate();
}

/** The process-wide Core (created on first use). */
export function defaultCore(): Core {
  if (!shared) shared = new Core(transportFor(config));
  return shared;
}

/** A new Core with its own transport (its own workers in worker mode). */
export function createCore(c: CoreConfig = {}): Core {
  return new Core(transportFor(c));
}
