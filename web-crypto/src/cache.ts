import { abortError } from './errors.js';

/** A byte-bounded least-recently-used map. */
export class LRU<V extends { byteLength: number }> {
  private m = new Map<string, V>();
  private bytes = 0;
  constructor(readonly maxBytes: number) {}

  get(k: string): V | undefined {
    const v = this.m.get(k);
    if (v !== undefined) {
      this.m.delete(k);
      this.m.set(k, v);
    }
    return v;
  }

  set(k: string, v: V): void {
    if (v.byteLength > this.maxBytes) return;
    const old = this.m.get(k);
    if (old !== undefined) {
      this.bytes -= old.byteLength;
      this.m.delete(k);
    }
    this.m.set(k, v);
    this.bytes += v.byteLength;
    while (this.bytes > this.maxBytes) {
      const first = this.m.keys().next().value as string;
      this.bytes -= this.m.get(first)!.byteLength;
      this.m.delete(first);
    }
  }

  get size(): number {
    return this.m.size;
  }
  get usedBytes(): number {
    return this.bytes;
  }
  clear(): void {
    this.m.clear();
    this.bytes = 0;
  }
}

/** A counting semaphore whose waits can be aborted. */
export class Semaphore {
  private waiters: { resolve: () => void; reject: (e: Error) => void; signal?: AbortSignal; onAbort?: () => void }[] = [];
  private used = 0;
  constructor(readonly limit: number) {
    if (!(limit >= 1)) throw new RangeError('concurrency must be at least 1');
  }

  get active(): number {
    return this.used;
  }

  acquire(signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) return Promise.reject(abortError(signal));
    if (this.used < this.limit) {
      this.used++;
      return Promise.resolve();
    }
    return new Promise((resolve, reject) => {
      const w: (typeof this.waiters)[number] = { resolve, reject, signal };
      if (signal) {
        w.onAbort = () => {
          const i = this.waiters.indexOf(w);
          if (i >= 0) this.waiters.splice(i, 1);
          reject(abortError(signal));
        };
        signal.addEventListener('abort', w.onAbort, { once: true });
      }
      this.waiters.push(w);
    });
  }

  release(): void {
    const w = this.waiters.shift();
    if (w) {
      if (w.signal && w.onAbort) w.signal.removeEventListener('abort', w.onAbort);
      w.resolve();
    } else {
      this.used--;
    }
  }
}

/** Resolves like p, or rejects as soon as signal aborts. */
export function raceSignal<T>(p: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return p;
  if (signal.aborted) return Promise.reject(abortError(signal));
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(abortError(signal));
    signal.addEventListener('abort', onAbort, { once: true });
    p.then(
      (v) => {
        signal.removeEventListener('abort', onAbort);
        resolve(v);
      },
      (e) => {
        signal.removeEventListener('abort', onAbort);
        reject(e);
      },
    );
  });
}

/** An AbortController that also aborts when parent aborts; dispose() detaches it. */
export function childController(parent?: AbortSignal): { ctrl: AbortController; dispose: () => void } {
  const ctrl = new AbortController();
  if (!parent) return { ctrl, dispose: () => undefined };
  if (parent.aborted) {
    ctrl.abort(parent.reason);
    return { ctrl, dispose: () => undefined };
  }
  const f = () => ctrl.abort(parent.reason);
  parent.addEventListener('abort', f, { once: true });
  return { ctrl, dispose: () => parent.removeEventListener('abort', f) };
}
