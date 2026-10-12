// An independent implementation of the IBS2 sealed-delivery format (docs/sealed-delivery.md) with
// Node's crypto (X25519, HKDF-SHA256, ChaCha20-Poly1305) and a hand-written HChaCha20, checked
// against the Go test vectors (internal/format/testdata/sealed_v2.json). It proves the document
// describes what the Go code does, byte for byte, without using any of the Go code.
import { createDecipheriv, createHash, createPrivateKey, createPublicKey, diffieHellman, hkdfSync } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

interface Vector {
  master_hex: string;
  ephemeral_private_hex: string;
  recipient_public_hex: string;
  job_id: string;
  request_digest_hex: string;
  plaintext: string;
  repeat: number;
  sealed_hex?: string;
  sealed_sha256: string;
}

const vectors = JSON.parse(readFileSync(fileURLToPath(new URL('../../internal/format/testdata/sealed_v2.json', import.meta.url)), 'utf8')) as Vector[];

const rotl = (x: number, n: number) => ((x << n) | (x >>> (32 - n))) >>> 0;
/** HChaCha20 (draft-irtf-cfrg-xchacha): the XChaCha20 sub-key. */
function hchacha20(key: Buffer, nonce16: Buffer): Buffer {
  const s = new Uint32Array(16);
  s[0] = 0x61707865; s[1] = 0x3320646e; s[2] = 0x79622d32; s[3] = 0x6b206574;
  for (let i = 0; i < 8; i++) s[4 + i] = key.readUInt32LE(4 * i);
  for (let i = 0; i < 4; i++) s[12 + i] = nonce16.readUInt32LE(4 * i);
  const qr = (a: number, b: number, c: number, d: number) => {
    s[a] = (s[a]! + s[b]!) >>> 0; s[d] = rotl(s[d]! ^ s[a]!, 16);
    s[c] = (s[c]! + s[d]!) >>> 0; s[b] = rotl(s[b]! ^ s[c]!, 12);
    s[a] = (s[a]! + s[b]!) >>> 0; s[d] = rotl(s[d]! ^ s[a]!, 8);
    s[c] = (s[c]! + s[d]!) >>> 0; s[b] = rotl(s[b]! ^ s[c]!, 7);
  };
  for (let i = 0; i < 10; i++) {
    qr(0, 4, 8, 12); qr(1, 5, 9, 13); qr(2, 6, 10, 14); qr(3, 7, 11, 15);
    qr(0, 5, 10, 15); qr(1, 6, 11, 12); qr(2, 7, 8, 13); qr(3, 4, 9, 14);
  }
  const out = Buffer.alloc(32);
  [0, 1, 2, 3, 12, 13, 14, 15].forEach((w, i) => out.writeUInt32LE(s[w]!, 4 * i));
  return out;
}

function xOpen(key: Buffer, nonce24: Buffer, ct: Buffer, aad: Buffer): Buffer {
  const sub = hchacha20(key, nonce24.subarray(0, 16));
  const n = Buffer.concat([Buffer.alloc(4), nonce24.subarray(16)]);
  const d = createDecipheriv('chacha20-poly1305', sub, n, { authTagLength: 16 });
  d.setAAD(aad, { plaintextLength: ct.length - 16 });
  d.setAuthTag(ct.subarray(ct.length - 16));
  return Buffer.concat([d.update(ct.subarray(0, ct.length - 16)), d.final()]);
}

const hkdf = (ikm: Buffer, salt: Buffer | string, info: Buffer | string) => Buffer.from(hkdfSync('sha256', ikm, salt, info, 32));
const x25519Priv = (raw: Buffer) => createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b656e04220420', 'hex'), raw]), format: 'der', type: 'pkcs8' });
const x25519Pub = (raw: Buffer) => createPublicKey({ key: Buffer.concat([Buffer.from('302a300506032b656e032100', 'hex'), raw]), format: 'der', type: 'spki' });

/** Opens an IBS2 stream from the ephemeral side (the vectors pin the ephemeral private key). */
function openIBS2(v: Vector, sealed: Buffer): Buffer {
  expect(sealed.subarray(0, 4).toString()).toBe('IBS2');
  const ephPub = sealed.subarray(4, 36);
  const ephPriv = Buffer.from(v.ephemeral_private_hex, 'hex');
  expect(createPublicKey(x25519Priv(ephPriv)).export({ format: 'der', type: 'spki' }).subarray(12).equals(ephPub)).toBe(true);
  const recipient = Buffer.from(v.recipient_public_hex, 'hex');
  const shared = diffieHellman({ privateKey: x25519Priv(ephPriv), publicKey: x25519Pub(recipient) });
  const delivery = hkdf(Buffer.from(v.master_hex, 'hex'), 'incus-backup/v1', 'delivery-auth');
  const job = Buffer.from(v.job_id);
  const len = Buffer.alloc(2);
  len.writeUInt16BE(job.length);
  const kauth = hkdf(delivery, 'incus-backup/v1', Buffer.concat([Buffer.from('incus-backup sealed delivery v2 auth'), len, job, Buffer.from(v.request_digest_hex, 'hex'), recipient, ephPub]));
  const kstream = hkdf(Buffer.concat([shared, kauth]), Buffer.concat([ephPub, recipient]), 'incus-backup sealed stream v2');
  const tr = createHash('sha256').update(sealed.subarray(0, 36));
  let off = 36;
  let n = 0n;
  const out: Buffer[] = [];
  for (;;) {
    const l = sealed.readUInt32BE(off);
    const body = sealed.subarray(off + 4, off + 4 + l);
    const nonce = Buffer.alloc(24);
    nonce.writeBigUInt64BE(n);
    try {
      const pt = xOpen(kstream, nonce, body, Buffer.from('IBS2\x00', 'latin1'));
      expect(pt.length).toBeGreaterThan(0);
      tr.update(sealed.subarray(off, off + 4 + l));
      out.push(pt);
    } catch {
      const t = xOpen(kstream, nonce, body, Buffer.from('IBS2\x01', 'latin1'));
      const total = out.reduce((a, p) => a + p.length, 0);
      expect(t.length).toBe(48);
      expect(t.readBigUInt64BE(0)).toBe(BigInt(total));
      expect(t.readBigUInt64BE(8)).toBe(n);
      expect(t.subarray(16).equals(tr.digest())).toBe(true);
      expect(off + 4 + l).toBe(sealed.length);
      return Buffer.concat(out);
    }
    off += 4 + l;
    n++;
  }
}

describe('IBS2 independent implementation', () => {
  it('decrypts and verifies every Go vector exactly as docs/sealed-delivery.md describes', () => {
    const full = vectors.filter((v) => v.sealed_hex);
    expect(full.length).toBeGreaterThanOrEqual(2);
    for (const v of full) {
      const sealed = Buffer.from(v.sealed_hex!, 'hex');
      expect(createHash('sha256').update(sealed).digest('hex')).toBe(v.sealed_sha256);
      expect(openIBS2(v, sealed).toString()).toBe(v.plaintext.repeat(v.repeat));
    }
  });

  it('the independent opener rejects an altered vector (negative control)', () => {
    const v = vectors.find((x) => x.sealed_hex && x.repeat > 0)!;
    const bad = Buffer.from(v.sealed_hex!, 'hex');
    bad[45] = bad[45]! ^ 1;
    expect(() => openIBS2(v, bad)).toThrow();
    expect(() => openIBS2({ ...v, job_id: v.job_id + 'x' }, Buffer.from(v.sealed_hex!, 'hex'))).toThrow();
    // and the hand-written HChaCha20 matches the draft's test vector (section 2.2.1)
    const k = Buffer.from('000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f', 'hex');
    const n = Buffer.from('000000090000004a0000000031415927', 'hex');
    expect(hchacha20(k, n).toString('hex')).toBe('82413b4227b27bfed30e42508a877d73a0f9e4d58a74a853c12ec41326d3ecdc');
  });
});
