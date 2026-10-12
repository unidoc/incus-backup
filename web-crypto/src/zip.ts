// A streaming ZIP writer: STORE only (no compression), UTF-8 names, data descriptors (the CRC is
// computed while the content streams), Unix modes and mtimes, and ZIP64 records whenever a size, an
// offset or the entry count needs them. Memory use is one central-directory record per entry plus
// whatever piece is being written.
//
// Names come from guest filesystems, so they are hostile input: every entry name goes through
// ZipNamer (the safe mapping documented in docs/sealed-delivery.md, "Browser ZIP names"), modes
// lose setuid/setgid/sticky, symlink entries are refused unless the caller explicitly allows them,
// and entry count, entry size, total size and name length are capped (ZIP_LIMITS).
import { crc32 } from './crc32.js';
import { IbError } from './errors.js';

export interface ZipEntry {
  /** Path inside the archive, '/' separated, no leading slash; directories without the trailing '/'. */
  name: string;
  type: 'dir' | 'file' | 'symlink';
  /** Unix permission bits (only the 0o777 part is stored). */
  mode: number;
  /** Seconds since the epoch. */
  mtime: number;
  /** Exact content size for files (checked). */
  size?: number;
  /** File content. */
  data?: () => AsyncIterable<Uint8Array>;
  /** Symlink target. */
  link?: string;
}

/** Caps of one archive (a browser download): beyond them, use a host job (fs_extract). */
export interface ZipLimits {
  /** Entries (each costs a central-directory record in memory until the end). */
  maxEntries: number;
  /** Bytes of one file. */
  maxEntryBytes: number;
  /** Content bytes of the whole archive. */
  maxTotalBytes: number;
  /** UTF-8 bytes of one stored path. */
  maxNameBytes: number;
}

export const ZIP_LIMITS: Readonly<ZipLimits> = Object.freeze({
  maxEntries: 200_000,
  maxEntryBytes: 64 * 1024 ** 3,
  maxTotalBytes: 256 * 1024 ** 3,
  maxNameBytes: 4096,
});

/** A name that was stored differently from the original (reported, never silent). */
export interface ZipRename {
  original: string;
  stored: string;
  reasons: string[];
}

export interface ZipStreamOptions {
  /** Write ZIP64 records for every entry even when not needed (tests). */
  forceZip64?: boolean;
  /** Store symlink entries (type 'symlink'). Default false: they are refused with an error. */
  allowSymlinks?: boolean;
  limits?: Partial<ZipLimits>;
  /** Called for every entry whose stored name differs from its original name. */
  onRename?: (r: ZipRename) => void;
  /** Called for an entry that is left out (its stored name would be too long). */
  onSkip?: (name: string, reason: string) => void;
}

const COMPONENT_MAX = 255; // bytes: the common file-name limit of extracting filesystems
const enc0 = new TextEncoder();
const utf8len = (s: string) => enc0.encode(s).length;

function fnv(s: string): string {
  let h = 0x811c9dc5;
  for (const b of enc0.encode(s)) {
    h ^= b;
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h.toString(16).padStart(8, '0');
}

function isBidi(cp: number): boolean {
  return (cp >= 0x202a && cp <= 0x202e) || (cp >= 0x2066 && cp <= 0x2069) || cp === 0x200e || cp === 0x200f || cp === 0x061c;
}

/**
 * The safe form of one path component (see docs/sealed-delivery.md):
 *  - "" -> "_", "." -> "_", ".." -> "__";
 *  - control characters (C0, DEL, C1), backslash, bidirectional controls and lone surrogates -> "_";
 *  - the first component "X:..." (a drive letter on Windows) -> "X_...";
 *  - longer than 255 UTF-8 bytes -> cut to 200 bytes + "~" + 8 hex digits of a hash of the original
 *    (+ the original extension when it is short).
 */
export function safeComponent(c: string, first = false): { name: string; reasons: string[] } {
  const reasons = new Set<string>();
  if (c === '') return { name: '_', reasons: ['empty name'] };
  if (c === '.' || c === '..') return { name: c === '.' ? '_' : '__', reasons: ['dot name'] };
  let out = '';
  for (let i = 0; i < c.length; i++) {
    const u = c.charCodeAt(i);
    if (u >= 0xd800 && u <= 0xdbff && i + 1 < c.length) {
      const v = c.charCodeAt(i + 1);
      if (v >= 0xdc00 && v <= 0xdfff) {
        out += c[i]! + c[i + 1]!;
        i++;
        continue;
      }
    }
    if (u >= 0xd800 && u <= 0xdfff) {
      reasons.add('invalid unicode');
      out += '_';
    } else if (u < 0x20 || u === 0x7f || (u >= 0x80 && u <= 0x9f)) {
      reasons.add('control character');
      out += '_';
    } else if (u === 0x5c) {
      reasons.add('backslash');
      out += '_';
    } else if (isBidi(u)) {
      reasons.add('bidirectional control');
      out += '_';
    } else out += c[i]!;
  }
  if (first && /^[A-Za-z]:/.test(out)) {
    reasons.add('drive letter');
    out = out[0] + '_' + out.slice(2);
  }
  if (utf8len(out) > COMPONENT_MAX) {
    reasons.add('name too long');
    const dot = out.lastIndexOf('.');
    const ext = dot > 0 && utf8len(out.slice(dot)) <= 16 ? out.slice(dot) : '';
    let cut = '';
    for (const ch of out) {
      if (utf8len(cut + ch) > 200) break;
      cut += ch;
    }
    out = `${cut}~${fnv(c)}${ext}`;
  }
  return { name: out, reasons: [...reasons] };
}

/** How extracting filesystems compare names: case-insensitive, Unicode-normalised, trailing dots and spaces ignored (Windows). */
function fold(path: string): string {
  return path
    .split('/')
    .map((c) => c.normalize('NFC').toLowerCase().replace(/[. ]+$/, ''))
    .join('/');
}

/**
 * Maps original archive paths to safe, unique stored paths. Parents are mapped before children
 * (a renamed directory renames everything below it), and two names that an extracting filesystem
 * would treat as the same (case, Unicode form, trailing dots/spaces) never both appear: the later
 * one gets "~2", "~3", ... before its extension.
 */
export class ZipNamer {
  private map = new Map<string, string>();
  private used = new Set<string>();

  name(original: string, onRename?: (r: ZipRename) => void): string {
    const reasons = new Set<string>();
    let path = original;
    if (path.startsWith('/')) {
      reasons.add('absolute path');
      path = path.replace(/^\/+/, '');
    }
    const parts = path.split('/');
    let orig = '';
    let stored = '';
    for (let i = 0; i < parts.length; i++) {
      const p = parts[i]!;
      if (p === '' && i < parts.length - 1) {
        reasons.add('empty name');
        continue; // "a//b": the empty component vanishes
      }
      orig = orig ? `${orig}/${p}` : p;
      const known = this.map.get(orig);
      if (known !== undefined) {
        stored = known;
        continue;
      }
      const sc = safeComponent(p, stored === '');
      for (const r of sc.reasons) reasons.add(r);
      let cand = stored ? `${stored}/${sc.name}` : sc.name;
      if (this.used.has(fold(cand))) {
        reasons.add('name collision');
        const dot = sc.name.lastIndexOf('.');
        const [base, ext] = dot > 0 && i === parts.length - 1 ? [sc.name.slice(0, dot), sc.name.slice(dot)] : [sc.name, ''];
        for (let n = 2; ; n++) {
          const alt = `${base}~${n}${ext}`;
          cand = stored ? `${stored}/${alt}` : alt;
          if (!this.used.has(fold(cand))) break;
        }
      }
      this.used.add(fold(cand));
      this.map.set(orig, cand);
      stored = cand;
    }
    if (stored === '') {
      reasons.add('empty name');
      stored = this.name('_');
    }
    if (reasons.size && onRename) onRename({ original, stored, reasons: [...reasons] });
    return stored;
  }
}

const MAX32 = 0xffffffff;
const MAX16 = 0xffff;
const enc = new TextEncoder();

class Buf {
  private b: Uint8Array;
  private v: DataView;
  private p = 0;
  constructor(n: number) {
    this.b = new Uint8Array(n);
    this.v = new DataView(this.b.buffer);
  }
  u16(x: number): this {
    this.v.setUint16(this.p, x, true);
    this.p += 2;
    return this;
  }
  u32(x: number): this {
    this.v.setUint32(this.p, x >>> 0, true);
    this.p += 4;
    return this;
  }
  u64(x: number): this {
    this.v.setUint32(this.p, x % 0x100000000, true);
    this.v.setUint32(this.p + 4, Math.floor(x / 0x100000000), true);
    this.p += 8;
    return this;
  }
  bytes(x: Uint8Array): this {
    this.b.set(x, this.p);
    this.p += x.length;
    return this;
  }
  done(): Uint8Array {
    if (this.p !== this.b.length) throw new Error(`zip: record length ${this.p} != ${this.b.length}`);
    return this.b;
  }
}

function dosTime(sec: number): { time: number; date: number } {
  const d = new Date(sec * 1000);
  let y = d.getUTCFullYear();
  if (!Number.isFinite(y) || y < 1980) return { time: 0, date: (0 << 9) | (1 << 5) | 1 };
  if (y > 2107) y = 2107;
  return {
    time: (d.getUTCHours() << 11) | (d.getUTCMinutes() << 5) | (d.getUTCSeconds() >> 1),
    date: ((y - 1980) << 9) | ((d.getUTCMonth() + 1) << 5) | d.getUTCDate(),
  };
}

function utExtra(mtime: number): Uint8Array {
  const t = Math.min(Math.max(Math.floor(mtime), 0), MAX32);
  return new Buf(9).u16(0x5455).u16(5).bytes(Uint8Array.of(1)).u32(t).done();
}

function concat(parts: Uint8Array[]): Uint8Array {
  let n = 0;
  for (const p of parts) n += p.length;
  const out = new Uint8Array(n);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

/** Streams a ZIP archive of the entries (names made safe, see ZipNamer; caps from ZIP_LIMITS). */
export async function* zipStream(entries: AsyncIterable<ZipEntry>, opts: ZipStreamOptions = {}): AsyncGenerator<Uint8Array> {
  const force = !!opts.forceZip64;
  const lim: ZipLimits = { ...ZIP_LIMITS, ...opts.limits };
  const namer = new ZipNamer();
  const central: Uint8Array[] = [];
  let cdSize = 0;
  let offset = 0;
  let count = 0;
  let content = 0;
  for await (const e of entries) {
    if (e.type === 'symlink' && !opts.allowSymlinks) throw new IbError(`zip: ${e.name} is a symbolic link; links are not stored in archives (they can redirect extraction)`);
    if (e.type !== 'dir' && e.type !== 'file' && e.type !== 'symlink') throw new IbError(`zip: ${e.name}: unsupported entry type`);
    if (count + 1 > lim.maxEntries) throw new IbError(`zip: more than ${lim.maxEntries} entries; choose fewer files or extract them on the host (fs_extract)`);
    if (e.type === 'file') {
      const sz = e.size ?? 0;
      if (sz > lim.maxEntryBytes) throw new IbError(`zip: ${e.name} is larger than ${lim.maxEntryBytes} bytes; download it on its own or extract it on the host`);
      if (content + sz > lim.maxTotalBytes) throw new IbError(`zip: the archive would exceed ${lim.maxTotalBytes} bytes; choose fewer files or extract them on the host (fs_extract)`);
      content += sz;
    }
    const storedName = namer.name(e.name, opts.onRename);
    if (enc.encode(storedName).length > Math.min(lim.maxNameBytes, MAX16 - 1)) {
      opts.onSkip?.(e.name, `the path is longer than ${lim.maxNameBytes} bytes`);
      continue;
    }
    const name = enc.encode(e.type === 'dir' ? storedName + '/' : storedName);
    const { time, date } = dosTime(e.mtime);
    const ut = utExtra(e.mtime);
    const typeBits = e.type === 'dir' ? 0o040000 : e.type === 'symlink' ? 0o120000 : 0o100000;
    // permission bits only: setuid, setgid and sticky never travel into an archive
    const external = (((typeBits | (e.mode & 0o777)) << 16) | (e.type === 'dir' ? 0x10 : 0)) >>> 0;
    const localOffset = offset;
    let crc = 0;
    let size = 0;
    let zip64: boolean;
    let flags: number;
    if (e.type === 'file') {
      const expect = e.size ?? 0;
      zip64 = force || expect >= MAX32;
      flags = 0x0808; // UTF-8 names, sizes and CRC in the data descriptor
      const extra = zip64 ? concat([new Buf(20).u16(0x0001).u16(16).u64(0).u64(0).done(), ut]) : ut;
      const lh = new Buf(30 + name.length + extra.length)
        .u32(0x04034b50)
        .u16(zip64 ? 45 : 20)
        .u16(flags)
        .u16(0)
        .u16(time)
        .u16(date)
        .u32(0)
        .u32(zip64 ? MAX32 : 0)
        .u32(zip64 ? MAX32 : 0)
        .u16(name.length)
        .u16(extra.length)
        .bytes(name)
        .bytes(extra)
        .done();
      offset += lh.length;
      yield lh;
      if (e.data) {
        for await (const piece of e.data()) {
          crc = crc32(piece, crc);
          size += piece.length;
          offset += piece.length;
          yield piece;
        }
      }
      if (size !== expect) throw new IbError(`zip: ${e.name} delivered ${size} bytes, expected ${expect}`);
      const dd = zip64
        ? new Buf(24).u32(0x08074b50).u32(crc).u64(size).u64(size).done()
        : new Buf(16).u32(0x08074b50).u32(crc).u32(size).u32(size).done();
      offset += dd.length;
      yield dd;
    } else {
      const body = e.type === 'symlink' ? enc.encode(e.link ?? '') : new Uint8Array(0);
      crc = crc32(body);
      size = body.length;
      zip64 = force;
      flags = 0x0800;
      const extra = zip64 ? concat([new Buf(20).u16(0x0001).u16(16).u64(size).u64(size).done(), ut]) : ut;
      const lh = new Buf(30 + name.length + extra.length)
        .u32(0x04034b50)
        .u16(zip64 ? 45 : e.type === 'dir' ? 20 : 10)
        .u16(flags)
        .u16(0)
        .u16(time)
        .u16(date)
        .u32(crc)
        .u32(zip64 ? MAX32 : size)
        .u32(zip64 ? MAX32 : size)
        .u16(name.length)
        .u16(extra.length)
        .bytes(name)
        .bytes(extra)
        .done();
      offset += lh.length + body.length;
      yield lh;
      if (body.length) yield body;
    }
    // central directory record
    const bigSize = zip64 || size >= MAX32;
    const bigOff = force || localOffset >= MAX32;
    const z: number[] = [];
    if (bigSize) z.push(size, size);
    if (bigOff) z.push(localOffset);
    const z64 = z.length ? (() => {
      const b = new Buf(4 + 8 * z.length).u16(0x0001).u16(8 * z.length);
      for (const v of z) b.u64(v);
      return b.done();
    })() : new Uint8Array(0);
    const cextra = concat([z64, ut]);
    const needed = bigSize || bigOff ? 45 : 20;
    const ch = new Buf(46 + name.length + cextra.length)
      .u32(0x02014b50)
      .u16((3 << 8) | 45)
      .u16(needed)
      .u16(flags)
      .u16(0)
      .u16(time)
      .u16(date)
      .u32(crc)
      .u32(bigSize ? MAX32 : size)
      .u32(bigSize ? MAX32 : size)
      .u16(name.length)
      .u16(cextra.length)
      .u16(0)
      .u16(0)
      .u16(0)
      .u32(external)
      .u32(bigOff ? MAX32 : localOffset)
      .bytes(name)
      .bytes(cextra)
      .done();
    central.push(ch);
    cdSize += ch.length;
    count++;
  }
  const cdOffset = offset;
  for (const c of central) yield c;
  central.length = 0;
  const end64 = force || count >= MAX16 || cdSize >= MAX32 || cdOffset >= MAX32;
  if (end64) {
    const eocd64Offset = cdOffset + cdSize;
    yield new Buf(56)
      .u32(0x06064b50)
      .u64(44)
      .u16((3 << 8) | 45)
      .u16(45)
      .u32(0)
      .u32(0)
      .u64(count)
      .u64(count)
      .u64(cdSize)
      .u64(cdOffset)
      .done();
    yield new Buf(20).u32(0x07064b50).u32(0).u64(eocd64Offset).u32(1).done();
  }
  yield new Buf(22)
    .u32(0x06054b50)
    .u16(0)
    .u16(0)
    .u16(end64 ? MAX16 : count)
    .u16(end64 ? MAX16 : count)
    .u32(end64 ? MAX32 : cdSize)
    .u32(end64 ? MAX32 : cdOffset)
    .u16(0)
    .done();
}
