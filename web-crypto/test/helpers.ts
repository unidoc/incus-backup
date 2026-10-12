import { createHash, hkdfSync } from 'node:crypto';
import { lstatSync, readdirSync, readFileSync, readlinkSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Fetcher } from '../src/index.js';

export const FIX = fileURLToPath(new URL('../.cache/fixture/', import.meta.url));

export interface Fixture {
  recoveryKey: string;
  wrongRecoveryKey: string;
  masterHex: string;
  domainId: string;
  snapshots: { label: string; project: string; kind: string; object: string; group: string; snap: string; format: string; source?: string }[];
  chunks: number;
  sealPrivateHex: string;
  sealPublicHex: string;
  sealJobId: string;
  sealType: string;
  sealParams: string;
  sealed: Record<string, { file: string; plain: string; size: number }>;
  escrowFile: string;
  escrowPrivate: string;
  notes: { xattr: { path: string; name: string; value: string } | null; mtime: { path: string; ns: string } };
}

export function fixture(): Fixture {
  return JSON.parse(readFileSync(join(FIX, 'fixture.json'), 'utf8')) as Fixture;
}

export function snapOf(label: string) {
  const s = fixture().snapshots.find((x) => x.label === label);
  if (!s) throw new Error(`no snapshot ${label}`);
  return s;
}

export interface Call {
  method: 'index' | 'manifest' | 'chunk';
  args: string[];
}

/** A fetcher over the fixture files that records every call and can tamper with responses. */
export class FakeFetcher implements Fetcher {
  calls: Call[] = [];
  active = 0;
  maxActive = 0;
  delayMs = 0;
  tamper?: (method: Call['method'], args: string[], data: Uint8Array) => Uint8Array;
  redirect?: (method: Call['method'], args: string[]) => string[] | undefined;

  constructor(readonly dir = FIX) {}

  private async read(method: Call['method'], args: string[], path: string[], signal?: AbortSignal): Promise<Uint8Array> {
    this.calls.push({ method, args });
    this.active++;
    this.maxActive = Math.max(this.maxActive, this.active);
    try {
      if (this.delayMs) await new Promise((r) => setTimeout(r, this.delayMs));
      if (signal?.aborted) throw signal.reason ?? new Error('aborted');
      const p = this.redirect?.(method, args) ?? path;
      let d: Uint8Array = new Uint8Array(readFileSync(join(this.dir, "api/v1", ...p)));
      if (this.tamper) d = this.tamper(method, args, d);
      return d;
    } finally {
      this.active--;
    }
  }
  index(group: string, snap: string, signal?: AbortSignal) {
    return this.read('index', [group, snap], ['snapshots', group, snap, 'index'], signal);
  }
  manifest(group: string, snap: string, signal?: AbortSignal) {
    return this.read('manifest', [group, snap], ['snapshots', group, snap, 'manifest'], signal);
  }
  chunk(id: string, signal?: AbortSignal) {
    return this.read('chunk', [id], ['chunks', id], signal);
  }
  chunkCalls(): number {
    return this.calls.filter((c) => c.method === 'chunk').length;
  }
}

export async function readAll(s: ReadableStream<Uint8Array>): Promise<Uint8Array> {
  const parts: Uint8Array[] = [];
  let n = 0;
  const r = s.getReader();
  for (;;) {
    const x = await r.read();
    if (x.done) break;
    parts.push(x.value);
    n += x.value.length;
  }
  const out = new Uint8Array(n);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

export function sha256hex(b: Uint8Array): string {
  return createHash('sha256').update(b).digest('hex');
}

export interface SrcEntry {
  kind: 'dir' | 'file' | 'symlink' | 'fifo';
  mode: number;
  size: number;
  link?: string;
  ino: number;
  nlink: number;
}

/** The source tree as the OS sees it (independent of the catalog code). */
export function sourceTree(root: string): Map<string, SrcEntry> {
  const out = new Map<string, SrcEntry>();
  const rec = (rel: string) => {
    const p = rel ? join(root, rel) : root;
    const st = lstatSync(p);
    const kind = st.isDirectory() ? 'dir' : st.isSymbolicLink() ? 'symlink' : st.isFIFO() ? 'fifo' : 'file';
    const e: SrcEntry = { kind, mode: st.mode & 0o7777, size: kind === 'file' ? st.size : 0, ino: st.ino, nlink: st.nlink };
    if (kind === 'symlink') e.link = readlinkSync(p);
    out.set(rel, e);
    if (kind === 'dir') for (const n of readdirSync(p)) rec(rel ? rel + '/' + n : n);
  };
  rec('');
  return out;
}

/** The sub-keys of the fixture's master key, derived independently with Node's HKDF. */
export function derivedKeys(masterHex: string): Buffer[] {
  const m = Buffer.from(masterHex, 'hex');
  return ['chunk-encryption', 'chunk-id', 'manifest-encryption', 'delivery-auth'].map((info) => Buffer.from(hkdfSync('sha256', m, 'incus-backup/v1', info, 32)));
}

/** Every printable form of a secret that must never appear anywhere. */
export function secretForms(f: Fixture): string[] {
  const forms = new Set<string>();
  const rk = f.recoveryKey;
  forms.add(rk);
  forms.add(rk.toLowerCase());
  forms.add(rk.replace(/-/g, ''));
  forms.add(rk.replace(/-/g, '').toLowerCase());
  // every 10-character window of the key body (catches partial echoes)
  const body = rk.replace(/-/g, '').slice(4);
  for (let i = 0; i + 10 <= body.length; i += 5) forms.add(body.slice(i, i + 10));
  for (const k of [Buffer.from(f.masterHex, 'hex'), ...derivedKeys(f.masterHex)]) {
    forms.add(k.toString('hex'));
    forms.add(k.toString('hex').toUpperCase());
    forms.add(k.toString('base64'));
    forms.add(k.toString('base64url'));
    forms.add(k.toString('hex').slice(0, 16));
  }
  return [...forms];
}

/** True when needle bytes occur in hay. */
export function containsBytes(hay: Uint8Array, needle: Uint8Array): boolean {
  outer: for (let i = 0; i + needle.length <= hay.length; i++) {
    for (let j = 0; j < needle.length; j++) if (hay[i + j] !== needle[j]) continue outer;
    return true;
  }
  return false;
}
