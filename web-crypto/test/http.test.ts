// HttpFetcher against a real HTTP server serving the fixture at the documented paths.
import { createReadStream, statSync } from 'node:fs';
import { createServer, type IncomingHttpHeaders, type Server } from 'node:http';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { FetchError, HttpFetcher, openKey, RepoReader, type CatalogSnapshot } from '../src/index.js';
import { FIX, fixture, readAll, sha256hex, snapOf } from './helpers.js';
import { readFileSync } from 'node:fs';

const f = fixture();
const cat = snapOf('catalog');
let server: Server;
let base = '';
const seen: { method: string; url: string; headers: IncomingHttpHeaders }[] = [];

beforeAll(async () => {
  server = createServer((req, res) => {
    seen.push({ method: req.method!, url: req.url!, headers: req.headers });
    const u = new URL(req.url!, 'http://x');
    if (u.pathname === '/api/v1/chunks/' + 'f'.repeat(64)) {
      // a hostile server: claims a huge body
      res.writeHead(200, { 'content-length': String(100 << 20) });
      res.end();
      return;
    }
    const p = join(FIX, decodeURIComponent(u.pathname));
    if (!u.pathname.startsWith('/api/v1/') || p.includes('..')) {
      res.writeHead(404).end();
      return;
    }
    try {
      const st = statSync(p);
      res.writeHead(200, { 'content-type': 'application/octet-stream', 'content-length': st.size });
      createReadStream(p).pipe(res);
    } catch {
      res.writeHead(404).end('no');
    }
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const a = server.address();
  base = `http://127.0.0.1:${typeof a === 'object' && a ? a.port : 0}`;
});
afterAll(() => new Promise<void>((r) => server.close(() => r())));

describe('HttpFetcher', () => {
  it('browses and reads through the documented API paths', async () => {
    const key = await openKey(f.recoveryKey);
    const reader = new RepoReader(key, new HttpFetcher(base + '/'));
    const s = (await reader.openSnapshot(cat.group, cat.snap)) as CatalogSnapshot;
    expect((await s.tree.list('')).length).toBeGreaterThan(5);
    const got = await readAll(s.readFile('big/random-30MiB.bin'));
    expect(sha256hex(got)).toBe(sha256hex(readFileSync(join(FIX, cat.source!, 'big/random-30MiB.bin'))));
    const paths = seen.map((x) => x.url);
    expect(paths).toContain(`/api/v1/snapshots/${cat.group}/${cat.snap}/index`);
    expect(paths).toContain(`/api/v1/snapshots/${cat.group}/${cat.snap}/manifest`);
    expect(paths.some((p) => /^\/api\/v1\/chunks\/[0-9a-f]{64}$/.test(p))).toBe(true);
    for (const r of seen) {
      expect(r.method).toBe('GET');
      expect(r.headers['content-type']).toBeUndefined();
      expect(r.headers['authorization']).toBeUndefined();
    }
  });

  it('reports HTTP errors and refuses oversized bodies', async () => {
    const h = new HttpFetcher(base);
    await expect(h.chunk('0'.repeat(64))).rejects.toThrow(FetchError);
    await expect(h.chunk('0'.repeat(64))).rejects.toMatchObject({ status: 404 });
    await expect(h.chunk('f'.repeat(64))).rejects.toThrow(/more than/);
    await expect(h.chunk('../../etc/passwd')).rejects.toThrow(/not a chunk id/);
    await expect(h.index(cat.group, 'x')).rejects.toThrow(/not a snapshot id/);
  });
});
