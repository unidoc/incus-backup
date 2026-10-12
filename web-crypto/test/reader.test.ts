import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, lstatSync, writeFileSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { IntegrityError, KeyError, openEscrowKey, openKey, RepoReader, type CatalogSnapshot, type EntryInfo } from '../src/index.js';
import { FakeFetcher, FIX, fixture, readAll, sha256hex, snapOf, sourceTree } from './helpers.js';

const f = fixture();
const cat = snapOf('catalog');

async function openCatalog(fetcher = new FakeFetcher(), opts = {}) {
  const key = await openKey(f.recoveryKey);
  const reader = new RepoReader(key, fetcher, opts);
  const s = await reader.openSnapshot(cat.group, cat.snap);
  if (!s.browsable) throw new Error('not browsable');
  return { key, reader, s: s as CatalogSnapshot, fetcher };
}

async function walkAll(s: CatalogSnapshot): Promise<EntryInfo[]> {
  const out: EntryInfo[] = [];
  for await (const e of s.tree.walk('', { batch: 37 })) out.push(e);
  return out;
}

describe('keys', () => {
  it('rejects a typo with a clear checksum message', async () => {
    const body = f.recoveryKey.split('');
    const i = body.length - 2;
    body[i] = body[i] === 'A' ? 'B' : 'A';
    await expect(openKey(body.join(''))).rejects.toThrow(KeyError);
    await expect(openKey(body.join(''))).rejects.toThrow(/checksum mismatch \(typo\?\)/);
    await expect(openKey('IBX1-AAAAA')).rejects.toThrow(/must start with IBR1/);
    await expect(openKey('IBR1-AAAAA')).rejects.toThrow(/wrong length/);
  });

  it('accepts case, space and dash variations and derives the right domain and group ids', async () => {
    const key = await openKey('  ' + f.recoveryKey.toLowerCase().replace(/-/g, ' ') + '\n');
    expect(key.domainId).toBe(f.domainId);
    expect(await key.groupId(cat.project, cat.kind, cat.object)).toBe(cat.group);
    await key.close();
    expect(key.closed).toBe(true);
    expect(() => key.check()).toThrow(/closed/);
  });
});

describe('escrow', () => {
  it('opens the key from an escrow blob and its offline private key', async () => {
    const wrapped = new Uint8Array(readFileSync(join(FIX, f.escrowFile)));
    const key = await openEscrowKey(wrapped, f.escrowPrivate);
    expect(key.domainId).toBe(f.domainId);
    const s = await new RepoReader(key, new FakeFetcher()).openSnapshot(cat.group, cat.snap);
    expect(s.browsable).toBe(true);
    const bad = wrapped.slice();
    bad[bad.length - 1] ^= 1;
    await expect(openEscrowKey(bad, f.escrowPrivate)).rejects.toThrow(KeyError);
    await expect(openEscrowKey(wrapped, 'IBE1-AAAAA')).rejects.toThrow(KeyError);
  });
});

describe('catalog snapshot', () => {
  it('opens with the manifest and lists exactly the source tree', async () => {
    const { s, fetcher } = await openCatalog();
    expect(s.format).toBe('catalog');
    expect(s.kind).toBe('filesystem');
    expect(s.manifest.snapshot_id).toBe(cat.snap);
    expect(s.manifest.meta?.['fs.format']).toBe('2');
    expect(Number(s.manifest.meta?.['fs.payload_size'])).toBeGreaterThan(30 << 20);
    // opening fetched only the catalog chunks, no payload
    expect(fetcher.chunkCalls()).toBeLessThanOrEqual(2);

    const src = sourceTree(join(FIX, cat.source!));
    const entries = await walkAll(s);
    expect(entries.length).toBe(s.tree.count);
    expect(entries.length).toBe(src.size);
    const byPath = new Map(entries.map((e) => [e.path, e]));
    const firstOfInode = new Map<number, string>();
    for (const e of entries) {
      const o = src.get(e.path);
      expect(o, e.path).toBeDefined();
      expect(e.mode, e.path).toBe(o!.mode);
      if (o!.kind === 'file' && o!.nlink > 1) {
        const first = firstOfInode.get(o!.ino);
        if (!first) {
          firstOfInode.set(o!.ino, e.path);
          expect(e.kind).toBe('file');
        } else {
          expect(e.kind, e.path).toBe('hardlink');
          expect(e.link).toBe(first);
        }
      } else {
        expect(e.kind, e.path).toBe(o!.kind);
      }
      if (e.kind === 'file') {
        expect(e.size, e.path).toBe(o!.size);
        if (e.size > 0) expect(e.sha256, e.path).toBe(sha256hex(readFileSync(join(FIX, cat.source!, e.path))));
      }
      if (e.kind === 'symlink') expect(e.link).toBe(o!.link);
    }
    expect([...firstOfInode.keys()].length).toBe(1);
    expect(byPath.get('other/third-link.txt')?.link).toBe('hard/original.txt');
    // fixed mtime with nanoseconds survives exactly
    expect(byPath.get(f.notes.mtime.path)?.mtimeNs).toBe(f.notes.mtime.ns);
    if (f.notes.xattr) {
      const x = byPath.get(f.notes.xattr.path)?.xattrs?.find((a) => a.name === f.notes.xattr!.name);
      expect(Buffer.from(x!.value, 'base64').toString()).toBe(f.notes.xattr.value);
    }
    expect(byPath.get('special/pipe')?.kind).toBe('fifo');
    expect(s.tree.stats.Files).toBe(entries.filter((e) => e.kind === 'file').length);
  });

  it('lists, stats and finds', async () => {
    const { s } = await openCatalog();
    const root = await s.tree.list('/');
    expect(root.map((e) => e.name)).toContain('Ünïcødé');
    const uni = await s.tree.list('Ünïcødé');
    expect(uni.map((e) => e.name).sort()).toEqual(['emoji 🎉 party.txt', 'ß-straße', '日本語 ファイル.txt'].sort());
    const st = await s.tree.stat('/perms/script.sh');
    expect(st.modeString).toBe('-rwxr-xr-x');
    expect(st.path).toBe('perms/script.sh');
    const txt = await s.tree.find('small', 'f00?.txt');
    expect(txt).toEqual(['small/f000.txt', 'small/f001.txt', 'small/f002.txt', 'small/f003.txt', 'small/f004.txt', 'small/f005.txt', 'small/f006.txt', 'small/f007.txt', 'small/f008.txt', 'small/f009.txt']);
    await expect(s.tree.stat('nope')).rejects.toThrow(/no such file/);
    await expect(s.tree.list('../etc')).rejects.toThrow(/escapes the root/);
    const sub = await s.tree.subtree('deep');
    expect(sub.entries).toBe(9); // deep, a..f, leaf.txt, empty-inside.txt
  });

  it('streams a 30 MiB file byte-identical with the right SHA-256, holding back nothing unverified', async () => {
    const { s, fetcher } = await openCatalog();
    const before = fetcher.chunkCalls();
    const src = readFileSync(join(FIX, cat.source!, 'big/random-30MiB.bin'));
    let progress = 0;
    const got = await readAll(s.readFile('big/random-30MiB.bin', { onProgress: (d) => (progress = d) }));
    expect(got.length).toBe(src.length);
    expect(sha256hex(got)).toBe(sha256hex(src));
    expect(Buffer.compare(Buffer.from(got), src)).toBe(0);
    expect(progress).toBe(src.length);
    expect(fetcher.chunkCalls() - before).toBeGreaterThan(5);
  });

  it('reads files with zero chunks, empty files, unicode names and hard links', async () => {
    const { s, fetcher } = await openCatalog();
    for (const p of ['big/zeros-3MiB.bin', 'big/mixed-zero-hole.bin', 'empty.txt', 'Ünïcødé/日本語 ファイル.txt', 'hard/original.txt', 'perms/secret.txt']) {
      const got = await readAll(s.readFile(p));
      expect(sha256hex(got), p).toBe(sha256hex(readFileSync(join(FIX, cat.source!, p))));
    }
    const n = fetcher.chunkCalls();
    const viaLink = await readAll(s.readFile('other/third-link.txt'));
    expect(Buffer.from(viaLink).toString()).toBe('one inode, three names\n');
    expect(fetcher.chunkCalls()).toBe(n); // cached
    await expect(readAll(s.readFile('deep'))).rejects.toThrow(/not a regular file/);
    await expect(readAll(s.readFile('links/rel'))).rejects.toThrow(/not a regular file/);
  });

  it('a range read fetches only the chunks covering the range', async () => {
    const { s, fetcher } = await openCatalog();
    const src = readFileSync(join(FIX, cat.source!, 'big/random-30MiB.bin'));
    const before = fetcher.chunkCalls();
    const start = 17 * 1024 * 1024 + 12345;
    const got = await readAll(s.readFile('big/random-30MiB.bin', { start, end: start + 1000 }));
    expect(Buffer.compare(Buffer.from(got), src.subarray(start, start + 1000))).toBe(0);
    const used = fetcher.chunkCalls() - before;
    expect(used).toBeGreaterThanOrEqual(1);
    expect(used).toBeLessThanOrEqual(2);
    // the tail of the file
    const b2 = fetcher.chunkCalls();
    const tail = await readAll(s.readFile('big/random-30MiB.bin', { start: src.length - 10 }));
    expect(Buffer.compare(Buffer.from(tail), src.subarray(src.length - 10))).toBe(0);
    expect(fetcher.chunkCalls() - b2).toBe(1);
    // an empty range fetches nothing
    const b3 = fetcher.chunkCalls();
    expect((await readAll(s.readFile('big/random-30MiB.bin', { start: 5, end: 5 }))).length).toBe(0);
    expect(fetcher.chunkCalls()).toBe(b3);
    // an all-zero file is one zero chunk: no blob, no fetch, and still SHA-256 verified
    const b4 = fetcher.chunkCalls();
    const zeros = await readAll(s.readFile('big/zeros-3MiB.bin'));
    expect(zeros.length).toBe(3 << 20);
    expect(zeros.every((x) => x === 0)).toBe(true);
    expect(fetcher.chunkCalls() - b4).toBe(0);
  });

  it('respects the fetch concurrency limit and bounds read-ahead when the consumer stalls', async () => {
    const fetcher = new FakeFetcher();
    const { s } = await openCatalog(fetcher, { concurrency: 3, readAhead: 4 });
    fetcher.delayMs = 15;
    fetcher.maxActive = 0;
    await readAll(s.readFile('big/random-30MiB.bin'));
    expect(fetcher.maxActive).toBeLessThanOrEqual(3);
    expect(fetcher.maxActive).toBeGreaterThan(1);

    // a stalled consumer: after one piece, at most readAhead further chunks are requested
    const f2 = new FakeFetcher();
    const o2 = await openCatalog(f2, { concurrency: 6, readAhead: 2 });
    const before = f2.chunkCalls();
    const r = o2.s.readFile('big/random-30MiB.bin').getReader();
    await r.read();
    await new Promise((x) => setTimeout(x, 200));
    expect(f2.chunkCalls() - before).toBeLessThanOrEqual(1 + 2 + 1);
    await r.cancel();
  });

  it('the chunk cache stays within its byte bound', async () => {
    const { s, reader } = await openCatalog(new FakeFetcher(), { cacheBytes: 8 << 20 });
    await readAll(s.readFile('big/random-30MiB.bin'));
    expect(reader.store.cache.usedBytes).toBeLessThanOrEqual(8 << 20);
  });

  it('cancels: an aborted read rejects and stops fetching', async () => {
    const fetcher = new FakeFetcher();
    const { s } = await openCatalog(fetcher, { concurrency: 2, readAhead: 2 });
    fetcher.delayMs = 30;
    const ac = new AbortController();
    const r = s.readFile('big/random-30MiB.bin', { signal: ac.signal }).getReader();
    await r.read();
    ac.abort();
    await expect(r.read()).rejects.toThrow(/abort/i);
    const n = fetcher.chunkCalls();
    await new Promise((x) => setTimeout(x, 300));
    expect(fetcher.chunkCalls()).toBe(n);
    // an already aborted signal never fetches
    const n2 = fetcher.chunkCalls();
    await expect(readAll(s.readFile('big/medium-700KiB.bin', { signal: AbortSignal.abort() }))).rejects.toThrow(/abort/i);
    expect(fetcher.chunkCalls()).toBe(n2);
    // and openSnapshot honours its signal
    const key = await openKey(f.recoveryKey);
    await expect(new RepoReader(key, fetcher).openSnapshot(cat.group, cat.snap, { signal: AbortSignal.abort() })).rejects.toThrow(/abort/i);
  });

  it('cancel() returns promptly and aborts fetches even when the server withholds data', async () => {
    for (const how of ['readFile', 'extractZip']) {
      const fetcher = new FakeFetcher();
      const { s } = await openCatalog(fetcher, { concurrency: 2, readAhead: 2 });
      let n = 0;
      let aborted = 0;
      const real = fetcher.chunk.bind(fetcher);
      fetcher.chunk = (id: string, signal?: AbortSignal) => {
        if (n++ < 2) return real(id, signal); // two answered: the first piece is held until the second arrives
        // withhold: never answer, only notice the abort
        return new Promise<Uint8Array>((_, reject) => {
          signal?.addEventListener('abort', () => {
            aborted++;
            reject(new Error('aborted by the reader'));
          });
        });
      };
      const stream = how === 'readFile' ? s.readFile('big/random-30MiB.bin') : s.extractZip(['big/random-30MiB.bin']);
      const r = stream.getReader();
      await r.read();
      const pending = r.read(); // waits on a withheld chunk
      pending.catch(() => undefined);
      await new Promise((x) => setTimeout(x, 50));
      const t0 = Date.now();
      await r.cancel();
      expect(Date.now() - t0, how).toBeLessThan(100);
      await new Promise((x) => setTimeout(x, 20));
      expect(aborted, how).toBeGreaterThan(0);
    }
  });

  it('extractZip takes a bare AbortSignal as its second argument', async () => {
    const { s } = await openCatalog();
    const ac = new AbortController();
    const r = s.extractZip(['big'], ac.signal).getReader();
    await r.read();
    ac.abort();
    await expect(
      (async () => {
        for (;;) if ((await r.read()).done) return;
      })(),
    ).rejects.toThrow(/abort/i);
  });

  it('names downloads safely', async () => {
    const { s } = await openCatalog();
    expect(s.suggestedFilename(['Ünïcødé/日本語 ファイル.txt'])).toBe('日本語 ファイル.txt');
    expect(s.suggestedFilename(['deep'], true)).toBe('deep.zip');
    expect(s.suggestedFilename([])).toBe(`volume-docs-${cat.snap}.zip`);
    expect(s.suggestedFilename(['a', 'b'])).toBe(`volume-docs-${cat.snap}.zip`);
  });
});

describe('unsupported snapshots', () => {
  it('archive and block snapshots expose the manifest and a clear reason', async () => {
    const key = await openKey(f.recoveryKey);
    const fetcher = new FakeFetcher();
    const reader = new RepoReader(key, fetcher);
    const a = snapOf('archive');
    const sa = await reader.openSnapshot(a.group, a.snap);
    expect(sa.browsable).toBe(false);
    if (!sa.browsable) {
      expect(sa.format).toBe('archive');
      expect(sa.unsupported).toBe('archive format cannot be browsed');
    }
    expect(sa.manifest.object.name).toBe('volume/docs-tar');
    const b = snapOf('block');
    const sb = await reader.openSnapshot(b.group, b.snap);
    expect(sb.browsable).toBe(false);
    expect(sb.format).toBe('block');
    expect(sb.kind).toBe('block');
    expect(sb.size).toBe(12 << 20);
    expect(fetcher.chunkCalls()).toBe(0);
  });
});

describe('tampering and wrong keys', () => {
  it('a wrong key fails cleanly before any chunk is fetched', async () => {
    const key = await openKey(f.wrongRecoveryKey);
    const fetcher = new FakeFetcher();
    await expect(new RepoReader(key, fetcher).openSnapshot(cat.group, cat.snap)).rejects.toThrow(IntegrityError);
    await expect(new RepoReader(key, fetcher).openSnapshot(cat.group, cat.snap)).rejects.toThrow(/manifest authentication failed/);
    expect(fetcher.chunkCalls()).toBe(0);
  });

  it('detects a tampered payload chunk', async () => {
    const fetcher = new FakeFetcher();
    const { s } = await openCatalog(fetcher);
    fetcher.tamper = (m, _a, d) => {
      if (m === 'chunk') d[100] ^= 1;
      return d;
    };
    await expect(readAll(s.readFile('big/random-30MiB.bin'))).rejects.toThrow(IntegrityError);
  });

  it('detects a tampered, truncated or substituted catalog chunk', async () => {
    for (const how of ['flip', 'truncate', 'other-chunk']) {
      const fetcher = new FakeFetcher();
      let other = '';
      fetcher.tamper = (m, args, d) => {
        if (m !== 'chunk') return d;
        if (how === 'flip') d[d.length - 40] ^= 0x80;
        if (how === 'truncate') return d.subarray(0, d.length - 1);
        return d;
      };
      if (how === 'other-chunk') {
        other = readdirSync(join(FIX, 'api/v1/chunks'))[0]!;
        fetcher.redirect = (m, args) => (m === 'chunk' && args[0] !== other ? ['chunks', other] : undefined);
      }
      const key = await openKey(f.recoveryKey);
      await expect(new RepoReader(key, fetcher).openSnapshot(cat.group, cat.snap), how).rejects.toThrow(IntegrityError);
    }
  });

  it("detects another snapshot's index or manifest served in place of the right one", async () => {
    const key = await openKey(f.recoveryKey);
    const a = snapOf('archive');
    const f1 = new FakeFetcher();
    f1.redirect = (m) => (m === 'index' ? ['snapshots', a.group, a.snap, 'index'] : undefined);
    await expect(new RepoReader(key, f1).openSnapshot(cat.group, cat.snap)).rejects.toThrow(/does not match its manifest/);
    const f2 = new FakeFetcher();
    f2.redirect = (m) => (m === 'manifest' ? ['snapshots', a.group, a.snap, 'manifest'] : undefined);
    await expect(new RepoReader(key, f2).openSnapshot(cat.group, cat.snap)).rejects.toThrow(IntegrityError);
    const f3 = new FakeFetcher();
    f3.tamper = (m, _a, d) => {
      if (m === 'index') d[40] ^= 1;
      return d;
    };
    await expect(new RepoReader(key, f3).openSnapshot(cat.group, cat.snap)).rejects.toThrow(IntegrityError);
  });

  it('rejects ids that are not ids before they reach a URL', async () => {
    const key = await openKey(f.recoveryKey);
    const fetcher = new FakeFetcher();
    const r = new RepoReader(key, fetcher);
    await expect(r.openSnapshot('../../etc', cat.snap)).rejects.toThrow(/not a group id/);
    await expect(r.openSnapshot(cat.group, '20261007120000-xyz')).rejects.toThrow(/not a snapshot id/);
    expect(fetcher.calls.length).toBe(0);
  });
});

describe('zip extraction', () => {
  const tmp = mkdtempSync(join(tmpdir(), 'ibzip-'));

  async function zipTo(s: CatalogSnapshot, paths: string[], name: string, opts = {}) {
    const skipped: string[] = [];
    const bytes = await readAll(s.extractZip(paths, { onSkip: (p) => skipped.push(p), ...opts }));
    const file = join(tmp, name);
    writeFileSync(file, bytes);
    return { file, skipped, bytes };
  }

  function compareExtracted(dir: string, srcRoot: string, prefix = '') {
    const src = sourceTree(srcRoot);
    let files = 0;
    for (const [p, e] of src) {
      if (prefix && !(p === prefix || p.startsWith(prefix + '/'))) continue;
      if (p === '') continue;
      const out = join(dir, p);
      if (e.kind === 'fifo' || e.kind === 'symlink') {
        expect(() => lstatSync(out), p).toThrow(); // never stored: links can redirect extraction
        continue;
      }
      const st = lstatSync(out);
      if (e.kind === 'dir') expect(st.isDirectory(), p).toBe(true);
      if (e.kind === 'file') {
        expect(Buffer.compare(readFileSync(out), readFileSync(join(srcRoot, p))), p).toBe(0);
        expect(st.mode & 0o777, p).toBe(e.mode & 0o777);
        files++;
      }
    }
    return files;
  }

  it('the whole tree unzips (unzip -t, unzip -x) to the source bytes; fifos are skipped and reported', async () => {
    const { s } = await openCatalog();
    let last = 0;
    let lastTotal = -1;
    const { file, skipped } = await zipTo(s, [], 'all.zip', {
      onProgress: (d: number, t: number) => {
        last = d;
        lastTotal = t;
      },
    });
    expect(skipped.sort()).toEqual(['links/abs', 'links/dangling', 'links/dirlink', 'links/rel', 'special/pipe']);
    const t = execFileSync('unzip', ['-t', file], { encoding: 'utf8' });
    expect(t).toMatch(/No errors detected/);
    const dir = mkdtempSync(join(tmp, 'x-'));
    execFileSync('unzip', ['-q', file, '-d', dir]);
    const files = compareExtracted(dir, join(FIX, cat.source!));
    expect(files).toBeGreaterThan(400);
    expect(last).toBe(lastTotal);
    expect(last).toBe((await s.tree.subtree('')).bytes + 2 * 23); // plus the two hard links' content
    // python's zipfile agrees (CRC of every member)
    const py = execFileSync('python3', ['-c', 'import zipfile,sys;z=zipfile.ZipFile(sys.argv[1]);print(z.testzip(), len(z.namelist()))', file], { encoding: 'utf8' });
    expect(py.trim().startsWith('None')).toBe(true);
  });

  it('a selection with ZIP64 records forced is still valid', async () => {
    const { s } = await openCatalog();
    const { file } = await zipTo(s, ['deep', 'Ünïcødé', 'big/medium-700KiB.bin', 'other', 'links'], 'sel64.zip', { forceZip64: true });
    expect(execFileSync('unzip', ['-t', file], { encoding: 'utf8' })).toMatch(/No errors detected/);
    const z = execFileSync('zipinfo', ['-v', file], { encoding: 'utf8' });
    expect(z).toMatch(/PKWARE 64-bit sizes/);
    const zb = readFileSync(file);
    expect(zb.readUInt32LE(zb.length - 98)).toBe(0x06064b50); // ZIP64 end of central directory
    expect(zb.readUInt32LE(zb.length - 42)).toBe(0x07064b50); // its locator
    const dir = mkdtempSync(join(tmp, 'y-'));
    execFileSync('unzip', ['-q', file, '-d', dir]);
    expect(readFileSync(join(dir, 'Ünïcødé/日本語 ファイル.txt'), 'utf8')).toBe('こんにちは世界\n');
    expect(readFileSync(join(dir, 'other/third-link.txt'), 'utf8')).toBe('one inode, three names\n');
    expect(compareExtracted(dir, join(FIX, cat.source!), 'deep')).toBe(2);
    const py = execFileSync('python3', ['-c', 'import zipfile,sys;z=zipfile.ZipFile(sys.argv[1]);print(z.testzip())', file], { encoding: 'utf8' });
    expect(py.trim()).toBe('None');
  });

  it('a tampered chunk fails the ZIP stream instead of producing a bad archive', async () => {
    const fetcher = new FakeFetcher();
    const { s } = await openCatalog(fetcher);
    fetcher.tamper = (m, _a, d) => {
      if (m === 'chunk') d[d.length - 1] ^= 1;
      return d;
    };
    await expect(readAll(s.extractZip(['big/medium-700KiB.bin']))).rejects.toThrow(IntegrityError);
  });

  it('symlinks are skipped by default, or become text notes; never links', async () => {
    const { s } = await openCatalog();
    const { file, skipped } = await zipTo(s, ['links'], 'nolinks.zip');
    expect(skipped.sort()).toEqual(['links/abs', 'links/dangling', 'links/dirlink', 'links/rel']);
    expect(execFileSync('unzip', ['-l', file], { encoding: 'utf8' })).toMatch(/links\//);
    const n = await zipTo(s, ['links'], 'notes.zip', { symlinks: 'note' });
    expect(n.skipped).toEqual([]);
    const dir = mkdtempSync(join(tmp, 'n-'));
    execFileSync('unzip', ['-q', n.file, '-d', dir]);
    expect(readFileSync(join(dir, 'links/abs.symlink.txt'), 'utf8')).toMatch(/Target: \/etc\/hostname/);
    expect(lstatSync(join(dir, 'links/abs.symlink.txt')).isFile()).toBe(true);
    const types = execFileSync('python3', ['-c', 'import zipfile,sys;print(sorted({i.external_attr>>28 for i in zipfile.ZipFile(sys.argv[1]).infolist()}))', n.file], { encoding: 'utf8' });
    expect(types.trim()).toBe('[4, 8]'); // directories and regular files only (0xA would be a symlink)
  });
});
