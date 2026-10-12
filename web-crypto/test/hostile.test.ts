// Hostile names and modes from a guest filesystem must not escape into the user's machine through a
// browser-made ZIP. The 'hostile' fixture snapshot is a real catalog of names a guest tenant can
// create on Linux (internal/webfixture buildHostile); the unit tests cover what Linux cannot hold.
import { execFileSync } from 'node:child_process';
import { lstatSync, mkdtempSync, readdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { IbError, openKey, RepoReader, safeComponent, ZipNamer, zipStream, type CatalogSnapshot, type ZipEntry, type ZipRename } from '../src/index.js';
import { FakeFetcher, fixture, readAll, snapOf } from './helpers.js';

const f = fixture();
const tmp = mkdtempSync(join(tmpdir(), 'ibhostile-'));

async function hostile(): Promise<CatalogSnapshot> {
  const key = await openKey(f.recoveryKey);
  const h = snapOf('hostile');
  return (await new RepoReader(key, new FakeFetcher()).openSnapshot(h.group, h.snap)) as CatalogSnapshot;
}

const zipInfo = (file: string) =>
  JSON.parse(
    execFileSync('python3', ['-c', 'import zipfile,sys,json;print(json.dumps([[i.filename,i.external_attr>>16] for i in zipfile.ZipFile(sys.argv[1]).infolist()]))', file], {
      encoding: 'utf8',
    }),
  ) as [string, number][];

function walk(dir: string, out: string[] = []): string[] {
  for (const n of readdirSync(dir)) {
    const p = join(dir, n);
    out.push(p);
    if (lstatSync(p).isDirectory()) walk(p, out);
  }
  return out;
}

describe('ZIPs of a hostile guest tree', () => {
  it('neutralises names, drops symlinks and special mode bits, lists every rename, and extracts inside the target only', async () => {
    const s = await hostile();
    const renames: ZipRename[] = [];
    const skipped: string[] = [];
    const bytes = await readAll(s.extractZip([], { onRename: (r) => renames.push(r), onSkip: (p) => skipped.push(p) }));
    const file = join(tmp, 'hostile.zip');
    writeFileSync(file, bytes);
    expect(execFileSync('unzip', ['-t', file], { encoding: 'utf8' })).toMatch(/No errors detected/);
    const info = zipInfo(file);
    const names = info.map(([n]) => n);
    for (const [n, attr] of info) {
      expect(n, n).not.toMatch(/[\\\x00-\x1f\x7f‪-‮⁦-⁩]/);
      expect(n.startsWith('/'), n).toBe(false);
      expect(n.split('/').some((c) => c === '..' || c === '.'), n).toBe(false);
      expect(attr & 0o7000, `${n} keeps setuid/setgid/sticky`).toBe(0);
      expect(attr >> 12, `${n} is a symlink`).not.toBe(0o12);
    }
    expect(names).toContain('C_/win.ini');
    expect(names.find((n) => n.endsWith('AppData_evil.bat'))).toBe('.._.._AppData_evil.bat');
    expect(names).toContain('ctrl__[31mred.txt');
    expect(names).toContain('invoice_txt.exe');
    // case collision: both files survive, the second under a new name
    expect(names).toContain('README.TXT');
    expect(names).toContain('Readme~2.txt');
    expect(names).toContain('SSH/authorized_keys');
    expect(skipped).toContain('ssh'); // the symlink to /root/.ssh is not in the archive
    expect(names.some((n) => n === 'ssh' || n === 'ssh/')).toBe(false);
    expect(info.find(([n]) => n === 'modes/suid.sh')![1] & 0o7777).toBe(0o755);
    // every rename is reported and listed inside the archive
    const listed = readFileSync(join(tmp, 'hostile.zip')).includes(Buffer.from('RENAMED-ENTRIES.txt'));
    expect(listed).toBe(true);
    expect(renames.map((r) => r.reasons.join(',')).join(' ')).toMatch(/backslash/);
    expect(renames.some((r) => r.reasons.includes('control character'))).toBe(true);
    expect(renames.some((r) => r.reasons.includes('bidirectional control'))).toBe(true);
    expect(renames.some((r) => r.reasons.includes('drive letter'))).toBe(true);
    expect(renames.some((r) => r.reasons.includes('name collision'))).toBe(true);
    // a real extractor writes inside the target directory only
    const dir = mkdtempSync(join(tmp, 'x-'));
    execFileSync('unzip', ['-q', file, '-d', dir]);
    const root = realpathSync(dir);
    for (const p of walk(dir)) expect(realpathSync(p).startsWith(root + '/'), p).toBe(true);
    expect(readFileSync(join(dir, 'RENAMED-ENTRIES.txt'), 'utf8')).toMatch(/README~2|Readme~2/);
    expect(readFileSync(join(dir, 'SSH/authorized_keys'), 'utf8')).toMatch(/attacker/);
    // python's zipfile (Windows-style sanitising extractor) agrees nothing escapes either
    const py = execFileSync('python3', ['-c', 'import zipfile,sys,os;d=sys.argv[2];zipfile.ZipFile(sys.argv[1]).extractall(d);print(all(os.path.realpath(os.path.join(r,n)).startswith(os.path.realpath(d)) for r,ds,fs in os.walk(d) for n in ds+fs))', file, mkdtempSync(join(tmp, 'p-'))], { encoding: 'utf8' });
    expect(py.trim()).toBe('True');
  });

  it('symlinks as notes collide with nothing', async () => {
    const s = await hostile();
    const bytes = await readAll(s.extractZip(['ssh', 'SSH'], { symlinks: 'note' }));
    const file = join(tmp, 'notes.zip');
    writeFileSync(file, bytes);
    const names = zipInfo(file).map(([n]) => n);
    expect(names).toContain('ssh.symlink.txt');
    expect(names).toContain('SSH/authorized_keys');
  });

  it('caps refuse a selection before the first byte, with a message that names the way out', async () => {
    const s = await hostile();
    const r = s.extractZip([], { limits: { maxEntries: 3 } }).getReader();
    await expect(r.read()).rejects.toThrow(/at most 3.*Extract files on the host/);
    const r2 = s.extractZip([], { limits: { maxTotalBytes: 10 } }).getReader();
    await expect(r2.read()).rejects.toThrow(/at most 10/);
    // a caller cannot raise the caps above the defaults
    const r3 = s.extractZip([], { limits: { maxEntries: 10 ** 12 } });
    expect((await readAll(r3)).length).toBeGreaterThan(0);
  });
});

describe('safe ZIP name mapping (names Linux cannot hold, from a hostile catalog or caller)', () => {
  it('maps every unsafe component', () => {
    expect(safeComponent('')).toEqual({ name: '_', reasons: ['empty name'] });
    expect(safeComponent('.').name).toBe('_');
    expect(safeComponent('..').name).toBe('__');
    expect(safeComponent('a\x00b').name).toBe('a_b');
    expect(safeComponent('a\\..\\b').name).toBe('a_.._b');
    expect(safeComponent('\x85next\x9b').name).toBe('_next_');
    expect(safeComponent('C:evil', true).name).toBe('C_evil');
    expect(safeComponent('C:evil', false).name).toBe('C:evil');
    expect(safeComponent('a\ud800b').reasons).toContain('invalid unicode');
    expect(safeComponent('emoji 🎉').name).toBe('emoji 🎉'); // valid surrogate pairs stay
    const long = safeComponent('x'.repeat(300) + '.pdf');
    expect(Buffer.byteLength(long.name)).toBeLessThanOrEqual(255);
    expect(long.name).toMatch(/^x{200}~[0-9a-f]{8}\.pdf$/);
    expect(safeComponent('ü'.repeat(200)).name.length).toBeLessThan(200); // cut on a character boundary
  });

  it('maps whole paths: absolute, empty components, dot-dot, collisions by case, Unicode form and trailing dots', () => {
    const n = new ZipNamer();
    const rs: ZipRename[] = [];
    const on = (r: ZipRename) => rs.push(r);
    expect(n.name('/etc/passwd', on)).toBe('etc/passwd');
    expect(n.name('../../x', on)).toBe('__/__/x');
    expect(n.name('a//b', on)).toBe('a/b');
    expect(n.name('Dir', on)).toBe('Dir');
    expect(n.name('dir/x', on)).toBe('dir~2/x'); // "dir" folds to "Dir": a new directory name
    expect(n.name('dir/y', on)).toBe('dir~2/y'); // children follow their renamed directory
    expect(n.name('Dir/z', on)).toBe('Dir/z');
    expect(n.name('café.txt', on)).toBe('café.txt');
    expect(n.name('café.txt', on)).toBe('café~2.txt'); // NFD of the same name
    expect(n.name('evil.bat', on)).toBe('evil.bat');
    expect(n.name('evil.bat.', on)).toBe('evil.bat~2.'); // Windows drops the trailing dot
    expect(rs.find((r) => r.original === '/etc/passwd')!.reasons).toContain('absolute path');
    expect(rs.find((r) => r.original === '../../x')!.reasons).toContain('dot name');
  });

  it('zipStream applies the mapping, masks modes, refuses symlinks and enforces caps', async () => {
    async function* entries(list: ZipEntry[]) {
      for (const e of list) yield e;
    }
    const file = (name: string, mode = 0o4755, size = 1): ZipEntry => ({ name, type: 'file', mode, mtime: 1700000000, size, data: async function* () { yield new Uint8Array(size); } });
    const out = join(tmp, 'direct.zip');
    const skipped: string[] = [];
    writeFileSync(out, await readAll(toStream(zipStream(entries([file('../../evil'), file('ok/' + 'd/'.repeat(3000) + 'deep')]), { onSkip: (n) => skipped.push(n) }))));
    const info = zipInfo(out);
    expect(info).toEqual([['__/__/evil', 0o100755]]);
    expect(skipped.length).toBe(1);
    const z = (list: ZipEntry[], o: Parameters<typeof zipStream>[1] = {}) => readAll(toStream(zipStream(entries(list), o)));
    await expect(z([{ name: 'l', type: 'symlink', mode: 0o777, mtime: 0, link: '/etc' }])).rejects.toThrow(/symbolic link/);
    await expect(z([file('a'), file('b')], { limits: { maxEntries: 1 } })).rejects.toThrow(IbError);
    await expect(z([file('a', 0o644, 100)], { limits: { maxEntryBytes: 99 } })).rejects.toThrow(/larger than 99/);
    await expect(z([file('a', 0o644, 60), file('b', 0o644, 60)], { limits: { maxTotalBytes: 100 } })).rejects.toThrow(/exceed 100/);
  });
});

function toStream(g: AsyncGenerator<Uint8Array>): ReadableStream<Uint8Array> {
  return new ReadableStream({
    async pull(c) {
      try {
        const x = await g.next();
        if (x.done) c.close();
        else c.enqueue(x.value);
      } catch (e) {
        c.error(e);
      }
    },
  });
}
