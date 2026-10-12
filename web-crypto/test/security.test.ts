// No key material ever leaves the module: not in any fetcher argument, not in any URL, header or
// body the HttpFetcher sends, not in any error message, not in any console output.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { generateSealKeypair, HttpFetcher, openKey, RepoReader, unsealStream, type CatalogSnapshot } from '../src/index.js';
import { containsBytes, derivedKeys, FakeFetcher, FIX, fixture, readAll, secretForms, snapOf } from './helpers.js';

const f = fixture();
const cat = snapOf('catalog');
const forms = secretForms(f);
const rawKeys = [Buffer.from(f.masterHex, 'hex'), ...derivedKeys(f.masterHex)];

const logged: string[] = [];
beforeAll(() => {
  for (const m of ['log', 'info', 'warn', 'error', 'debug'] as const) {
    vi.spyOn(console, m).mockImplementation((...a: unknown[]) => {
      logged.push(a.map(String).join(' '));
    });
  }
});
afterAll(() => vi.restoreAllMocks());

function assertClean(what: string, s: string) {
  for (const x of forms) expect(s.includes(x), `${what} contains key material`).toBe(false);
}

describe('no key material leaves the module', () => {
  it('every fetcher argument and every HTTP request is free of key material', async () => {
    const requests: { url: string; init: RequestInit }[] = [];
    const files = new FakeFetcher();
    // a fetch() stand-in that serves the fixture and records everything it was given
    const fakeFetch = (async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      requests.push({ url, init });
      const p = new URL(url).pathname.replace(/^\/api\/v1\//, '');
      try {
        return new Response(readFileSync(join(FIX, 'api/v1', p)), { status: 200 });
      } catch {
        return new Response('not found', { status: 404 });
      }
    }) as typeof fetch;
    const http = new HttpFetcher('https://gui.example', { fetch: fakeFetch });
    const recorder = {
      index: (g: string, s: string, sig?: AbortSignal) => (files.calls.push({ method: 'index', args: [g, s] }), http.index(g, s, sig)),
      manifest: (g: string, s: string, sig?: AbortSignal) => (files.calls.push({ method: 'manifest', args: [g, s] }), http.manifest(g, s, sig)),
      chunk: (id: string, sig?: AbortSignal) => (files.calls.push({ method: 'chunk', args: [id] }), http.chunk(id, sig)),
    };
    const key = await openKey(f.recoveryKey);
    const reader = new RepoReader(key, recorder);
    for (const s of f.snapshots) await reader.openSnapshot(s.group, s.snap);
    const snap = (await reader.openSnapshot(cat.group, cat.snap)) as CatalogSnapshot;
    await readAll(snap.readFile('big/random-30MiB.bin'));
    await readAll(snap.extractZip(['small', 'hard']));
    await readAll(snap.readFile('perms/secret.txt', { start: 2, end: 5 }));
    expect(files.calls.length).toBeGreaterThan(20);
    for (const c of files.calls) {
      for (const a of c.args) {
        assertClean(`${c.method} argument`, a);
        expect(a).toMatch(/^[0-9a-f-]+$/); // only ids ever reach the fetcher
      }
    }
    expect(requests.length).toBe(files.calls.length);
    for (const r of requests) {
      assertClean('URL', r.url);
      expect(r.init.method).toBe('GET');
      expect(r.init.body).toBeUndefined();
      expect(r.init.headers).toBeUndefined();
      expect(r.init.credentials).toBe('include');
      expect(r.url).toMatch(/^https:\/\/gui\.example\/api\/v1\/(chunks\/[0-9a-f]{64}|snapshots\/[0-9a-f]{32}\/[0-9]{14}-[0-9a-f]{8}\/(index|manifest))$/);
    }
  });

  it('error messages of every failure mode never contain key material', async () => {
    const msgs: string[] = [];
    const grab = async (p: Promise<unknown>) => {
      try {
        await p;
      } catch (e) {
        msgs.push(String((e as Error).message), String((e as Error).stack));
      }
    };
    const typo = f.recoveryKey.slice(0, -2) + (f.recoveryKey.endsWith('A') ? 'BB' : 'AA');
    await grab(openKey(typo));
    await grab(openKey(f.recoveryKey + 'AAAA'));
    await grab(openKey(f.recoveryKey.replace('IBR1', 'IBR2')));
    await grab(openKey(f.recoveryKey.slice(0, 20) + '!!!' + f.recoveryKey.slice(23)));
    const key = await openKey(f.recoveryKey);
    const tampered = new FakeFetcher();
    tampered.tamper = (_m, _a, d) => {
      d[d.length >> 1] ^= 1;
      return d;
    };
    await grab(new RepoReader(key, tampered).openSnapshot(cat.group, cat.snap));
    const good = new FakeFetcher();
    const snap = (await new RepoReader(key, good).openSnapshot(cat.group, cat.snap)) as CatalogSnapshot;
    good.tamper = (_m, _a, d) => {
      d[10] ^= 1;
      return d;
    };
    await grab(readAll(snap.readFile('big/medium-700KiB.bin')));
    await grab(readAll(snap.readFile('nope')));
    const wrong = await openKey(f.wrongRecoveryKey);
    await grab(new RepoReader(wrong, new FakeFetcher()).openSnapshot(cat.group, cat.snap));
    const seal = await generateSealKeypair();
    const req = { jobId: 'j', type: 'fs_extract', params: { project: 'p', object: 'volume/v', paths: ['a'], recipient_public_key: Buffer.from(seal.publicKey).toString('base64') } };
    await grab(readAll(unsealStream(new Uint8Array(200), { key, seal, request: req })));
    const bogus = new Uint8Array(200);
    bogus.set(new TextEncoder().encode('IBS2'));
    await grab(readAll(unsealStream(bogus, { key, seal, request: req })));
    await grab(readAll(unsealStream(bogus, { key: wrong, seal, request: req })));
    await seal.close();
    await grab(readAll(unsealStream(bogus, { key, seal, request: req })));
    await key.close();
    await grab(new RepoReader(key, new FakeFetcher()).openSnapshot(cat.group, cat.snap));
    expect(msgs.length).toBeGreaterThanOrEqual(20);
    for (const m of msgs) assertClean('error message', m);
  });

  it('nothing was logged, and no fetched object holds raw key bytes', async () => {
    for (const l of logged) assertClean('console output', l);
    expect(logged).toEqual([]);
    // sanity of the leak detector itself: the plaintext index and chunks never embed a key
    const idx = new Uint8Array(readFileSync(join(FIX, 'api/v1/snapshots', cat.group, cat.snap, 'index')));
    for (const k of rawKeys) expect(containsBytes(idx, new Uint8Array(k))).toBe(false);
  });

  it('the leak detector would notice a leak (negative control)', () => {
    expect(() => assertClean('control', `x${f.recoveryKey.toLowerCase()}y`)).toThrow();
    expect(() => assertClean('control', derivedKeys(f.masterHex)[0]!.toString('base64'))).toThrow();
    expect(() => assertClean('control', f.recoveryKey.replace(/-/g, '').slice(9, 19))).toThrow();
  });
});
