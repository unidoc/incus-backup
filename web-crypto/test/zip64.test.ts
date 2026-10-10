// The ZIP writer at real ZIP64 scale: an entry larger than 4 GiB, entries whose local headers lie
// beyond 4 GiB, and more than 65535 entries. Checked by Info-ZIP unzip and Python's zipfile.
import { execFileSync } from 'node:child_process';
import { createWriteStream, mkdtempSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, describe, expect, it } from 'vitest';
import { zipStream, type ZipEntry } from '../src/index.js';

const tmp = mkdtempSync(join(tmpdir(), 'ibzip64-'));
afterAll(() => rmSync(tmp, { recursive: true, force: true }));

async function write(gen: AsyncGenerator<Uint8Array>, file: string): Promise<number> {
  const out = createWriteStream(file);
  let n = 0;
  for await (const p of gen) {
    n += p.length;
    if (!out.write(p)) await new Promise<void>((r) => out.once('drain', () => r()));
  }
  await new Promise<void>((r, j) => out.end((e?: Error | null) => (e ? j(e) : r())));
  return n;
}

const py = (file: string, code: string) => execFileSync('python3', ['-c', code, file], { encoding: 'utf8', maxBuffer: 64 << 20 }).trim();

describe('ZIP64 at scale', () => {
  it('an entry over 4 GiB and entries beyond the 4 GiB offset', async () => {
    const big = 4 * 1024 ** 3 + 12345; // > 0xFFFFFFFF
    const piece = new Uint8Array(8 << 20);
    piece.fill(0x5a);
    async function* entries(): AsyncGenerator<ZipEntry> {
      yield { name: 'small-before.txt', type: 'file', mode: 0o644, mtime: 1700000000, size: 5, data: async function* () { yield new TextEncoder().encode('hello'); } };
      yield {
        name: 'huge.bin',
        type: 'file',
        mode: 0o600,
        mtime: 1700000000,
        size: big,
        data: async function* () {
          let left = big;
          while (left > 0) {
            const n = Math.min(left, piece.length);
            yield n === piece.length ? piece : piece.subarray(0, n);
            left -= n;
          }
        },
      };
      yield { name: 'after/dir', type: 'dir', mode: 0o755, mtime: 1700000000 };
      yield { name: 'after/ünïcode.txt', type: 'file', mode: 0o644, mtime: 1700000000, size: 3, data: async function* () { yield Uint8Array.of(1, 2, 3); } };
      yield { name: 'after/link', type: 'symlink', mode: 0o777, mtime: 1700000000, link: '../small-before.txt' };
    }
    const file = join(tmp, 'big.zip');
    const n = await write(zipStream(entries(), { allowSymlinks: true }), file);
    expect(statSync(file).size).toBe(n);
    expect(n).toBeGreaterThan(big);
    expect(execFileSync('unzip', ['-t', file], { encoding: 'utf8', maxBuffer: 64 << 20 })).toMatch(/No errors detected in compressed data/);
    const listing = py(file, 'import zipfile,sys;z=zipfile.ZipFile(sys.argv[1]);print("|".join(f"{i.filename}:{i.file_size}:{i.header_offset>0xFFFFFFFF}:{oct(i.external_attr>>16)}" for i in z.infolist()))');
    expect(listing.split('|')).toEqual([
      'small-before.txt:5:False:0o100644',
      `huge.bin:${big}:False:0o100600`,
      'after/dir/:0:True:0o40755',
      'after/ünïcode.txt:3:True:0o100644',
      'after/link:19:True:0o120777',
    ]);
    expect(py(file, 'import zipfile,sys;z=zipfile.ZipFile(sys.argv[1]);print(z.read("after/ünïcode.txt").hex(), z.read("after/link").decode())')).toBe('010203 ../small-before.txt');
  }, 600_000);

  it('more than 65535 entries', async () => {
    const count = 70000;
    async function* entries(): AsyncGenerator<ZipEntry> {
      for (let i = 0; i < count; i++) {
        const d = new TextEncoder().encode(`entry ${i}\n`);
        yield { name: `many/e${i}.txt`, type: 'file', mode: 0o644, mtime: 1700000000, size: d.length, data: async function* () { yield d; } };
      }
    }
    const file = join(tmp, 'many.zip');
    await write(zipStream(entries()), file);
    expect(execFileSync('unzip', ['-tq', file], { encoding: 'utf8' })).toMatch(/No errors detected/);
    expect(py(file, 'import zipfile,sys;z=zipfile.ZipFile(sys.argv[1]);n=z.namelist();print(len(n), z.read("many/e69999.txt").decode().strip())')).toBe('70000 entry 69999');
  }, 300_000);

  it('rejects content that does not match the announced size', async () => {
    async function* entries(): AsyncGenerator<ZipEntry> {
      yield { name: 'x', type: 'file', mode: 0o644, mtime: 0, size: 10, data: async function* () { yield new Uint8Array(3); } };
    }
    const drain = async () => {
      for await (const _ of zipStream(entries()));
    };
    await expect(drain()).rejects.toThrow(/delivered 3 bytes, expected 10/);
  });
});
