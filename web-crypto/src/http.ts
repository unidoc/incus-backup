import { FetchError } from './errors.js';
import { checkChunkId, checkGroup, checkSnapshot } from './reader.js';
import type { Fetcher } from './types.js';

export interface HttpFetcherOptions {
  /** fetch implementation (default globalThis.fetch). */
  fetch?: typeof fetch;
  /** Upper bound of one object's size in bytes (default 72 MiB: the largest chunk blob plus margin). */
  maxBytes?: number;
}

/**
 * Fetches repository objects from the management server's API:
 *
 *   GET {base}/api/v1/snapshots/{group}/{snap}/index
 *   GET {base}/api/v1/snapshots/{group}/{snap}/manifest
 *   GET {base}/api/v1/chunks/{id}
 *
 * Plain GETs with the session cookie (credentials: 'include'), no custom headers (so no CORS
 * preflight and nothing a CSRF token would protect), no request body. Only ids that passed the
 * format checks are ever put into a URL; no key-derived secret is ever sent.
 */
export class HttpFetcher implements Fetcher {
  private f: typeof fetch;
  private maxBytes: number;
  constructor(
    readonly baseUrl: string = '',
    opts: HttpFetcherOptions = {},
  ) {
    this.f = opts.fetch ?? globalThis.fetch.bind(globalThis);
    this.maxBytes = opts.maxBytes ?? 72 << 20;
  }

  private url(path: string): string {
    return this.baseUrl.replace(/\/+$/, '') + path;
  }

  private async get(path: string, signal?: AbortSignal): Promise<Uint8Array> {
    const res = await this.f(this.url(path), { method: 'GET', credentials: 'include', signal, redirect: 'error' });
    if (!res.ok) {
      throw new FetchError(`GET ${path}: HTTP ${res.status}`, res.status);
    }
    const len = Number(res.headers.get('content-length') ?? '0');
    if (len > this.maxBytes) {
      await res.body?.cancel();
      throw new FetchError(`GET ${path}: ${len} bytes is more than the ${this.maxBytes} accepted`);
    }
    if (!res.body) return new Uint8Array(await res.arrayBuffer());
    // read with a running bound, so a server that lies about (or omits) the length cannot exhaust memory
    const reader = res.body.getReader();
    const parts: Uint8Array[] = [];
    let n = 0;
    for (;;) {
      const r = await reader.read();
      if (r.done) break;
      n += r.value.length;
      if (n > this.maxBytes) {
        await reader.cancel();
        throw new FetchError(`GET ${path}: more than the ${this.maxBytes} bytes accepted`);
      }
      parts.push(r.value);
    }
    if (parts.length === 1) return parts[0]!;
    const out = new Uint8Array(n);
    let o = 0;
    for (const p of parts) {
      out.set(p, o);
      o += p.length;
    }
    return out;
  }

  async index(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array> {
    checkGroup(group);
    checkSnapshot(snap);
    return this.get(`/api/v1/snapshots/${group}/${snap}/index`, signal);
  }
  async manifest(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array> {
    checkGroup(group);
    checkSnapshot(snap);
    return this.get(`/api/v1/snapshots/${group}/${snap}/manifest`, signal);
  }
  async chunk(id: string, signal?: AbortSignal): Promise<Uint8Array> {
    checkChunkId(id);
    return this.get(`/api/v1/chunks/${id}`, signal);
  }
}
