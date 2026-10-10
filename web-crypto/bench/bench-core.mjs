// Throughput measurements shared by Node (bench/node.mjs) and the browser page (browser/page.js).
// `lib` is the package (dist/index.js), `fetcher` serves the fixture, `fx` is fixture.json.

const MB = 1e6;

function now() {
  return globalThis.performance.now();
}

async function drain(stream) {
  const r = stream.getReader();
  let n = 0;
  for (;;) {
    const x = await r.read();
    if (x.done) return n;
    n += x.value.length;
  }
}

/** Runs every measurement and returns {name: {mb, ms, mbps}}. */
export async function runBench(lib, core, fetcher, fx, opts = {}) {
  const out = {};
  const cat = fx.snapshots.find((s) => s.label === 'catalog');
  const key = await lib.openKey(fx.recoveryKey, { core });
  const kh = key.check();

  // 1. raw chunk decode (verify trailer + XChaCha20-Poly1305 + zstd + HMAC id check)
  const [man, idx] = await Promise.all([fetcher.manifest(cat.group, cat.snap), fetcher.index(cat.group, cat.snap)]);
  const info = await core.openSnapshot(kh, cat.group, cat.snap, man, idx);
  const blobs = [];
  for (let i = 0; i < info.payloadEntries; i++) {
    if (info.ids[i]) blobs.push([info.ids[i], await fetcher.chunk(info.ids[i])]);
  }
  const rounds = opts.rounds ?? 3;
  let bytes = 0;
  let t = now();
  // 8 decodes in flight: a worker pool spreads them, a single module runs them one by one
  const jobs = [];
  for (let r = 0; r < rounds; r++) jobs.push(...blobs);
  let next = 0;
  await Promise.all(
    Array.from({ length: 8 }, async () => {
      while (next < jobs.length) {
        const [id, b] = jobs[next++];
        const n = (await core.decodeChunk(kh, b, id)).length;
        bytes += n;
      }
    }),
  );
  let ms = now() - t;
  out.decodeChunk = { mb: bytes / MB, ms, mbps: bytes / MB / (ms / 1000) };

  // 2. SHA-256 inside the module (whole-file verification cost)
  const piece = await core.decodeChunk(kh, blobs[0][1], blobs[0][0]);
  const h = await core.hashNew();
  bytes = 0;
  t = now();
  for (let r = 0; r < 20; r++) {
    await core.hashUpdate(h, piece);
    bytes += piece.length;
  }
  await core.hashSum(h);
  ms = now() - t;
  out.sha256 = { mb: bytes / MB, ms, mbps: bytes / MB / (ms / 1000) };

  // 3. CRC-32 in JavaScript (ZIP)
  bytes = 0;
  t = now();
  let c = 0;
  for (let r = 0; r < 20; r++) {
    c = lib.crc32(piece, c);
    bytes += piece.length;
  }
  ms = now() - t;
  out.crc32 = { mb: bytes / MB, ms, mbps: bytes / MB / (ms / 1000) };

  // 4. readFile end to end (fetch from memory, decode, slice, SHA-256 verify), cold cache
  const reader = new lib.RepoReader(key, fetcher, { cacheBytes: 1 });
  const snap = await reader.openSnapshot(cat.group, cat.snap);
  t = now();
  bytes = await drain(snap.readFile('big/random-30MiB.bin'));
  ms = now() - t;
  out.readFile30MiB = { mb: bytes / MB, ms, mbps: bytes / MB / (ms / 1000) };

  // 5. ZIP of the big/ directory (readFile + CRC-32 + ZIP framing)
  t = now();
  bytes = await drain(snap.extractZip(['big']));
  ms = now() - t;
  out.zipBigDir = { mb: bytes / MB, ms, mbps: bytes / MB / (ms / 1000) };

  // 6. unseal (X25519 + HKDF + XChaCha20-Poly1305 frames)
  const sealedBytes = await opts.loadSealed();
  const seal = await lib.importSealKey(Uint8Array.from(fx.sealPrivateHex.match(/../g).map((x) => parseInt(x, 16))), { core });
  const request = { jobId: fx.sealJobId, type: fx.sealType, params: fx.sealParams };
  bytes = 0;
  t = now();
  for (let r = 0; r < 5; r++) bytes += await drain(lib.unsealStream(sealedBytes, { key, seal, request }));
  await seal.close();
  ms = now() - t;
  out.unseal = { mb: bytes / MB, ms, mbps: bytes / MB / (ms / 1000) };

  await snap.close();
  await key.close();
  for (const k of Object.keys(out)) {
    const v = out[k];
    out[k] = { mb: +v.mb.toFixed(1), ms: Math.round(v.ms), mbps: +v.mbps.toFixed(1) };
  }
  return out;
}
