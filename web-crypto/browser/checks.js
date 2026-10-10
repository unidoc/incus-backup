// Functional checks run inside a real browser: in the page (the library's own worker pool or the
// page's main thread) and inside a user Web Worker. Plain JavaScript over dist/index.js.
const hex = (b) => [...new Uint8Array(b)].map((x) => x.toString(16).padStart(2, '0')).join('');

async function readAll(stream) {
  const r = stream.getReader();
  const parts = [];
  let n = 0;
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

function b64(u8) {
  let s = '';
  for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000));
  return btoa(s);
}

async function errName(p) {
  try {
    await p;
    return 'no error';
  } catch (e) {
    return `${e.name}: ${e.message}`;
  }
}

/** Runs the checks with `core`; expected values come from the Node runner. */
export async function functional(lib, core, expected) {
  const fx = await (await fetch('/fixture/fixture.json')).json();
  const cat = fx.snapshots.find((s) => s.label === 'catalog');
  const out = {};
  // Not the last character: its low bits are unused, so changing it can decode to the same key (no error).
  const typo = fx.recoveryKey.split('');
  const at = typo.length - 2;
  typo[at] = typo[at] === 'A' ? 'B' : 'A';
  out.badKey = await errName(lib.openKey(typo.join(''), { core }));
  const key = await lib.openKey(fx.recoveryKey, { core });
  out.domainId = key.domainId;
  const fetcher = new lib.HttpFetcher('');
  const reader = new lib.RepoReader(key, fetcher);
  const s = await reader.openSnapshot(cat.group, cat.snap);
  out.format = s.format;
  out.fsFormat = s.manifest.meta['fs.format'];
  out.rootNames = (await s.tree.list('')).map((e) => e.name);
  let n = 0;
  for await (const _ of s.tree.walk('')) n++;
  out.walkCount = n;
  out.treeCount = s.tree.count;
  out.unicode = (await s.tree.list('Ünïcødé')).map((e) => e.name).sort();
  const t0 = performance.now();
  const big = await readAll(s.readFile('big/random-30MiB.bin'));
  out.bigMs = Math.round(performance.now() - t0);
  out.bigSize = big.length;
  out.bigSha = hex(await crypto.subtle.digest('SHA-256', big));
  out.bigShaCatalog = (await s.tree.stat('big/random-30MiB.bin')).sha256;
  const range = await readAll(s.readFile('big/random-30MiB.bin', { start: 1000, end: 1010 }));
  out.rangeOk = range.every((x, i) => x === big[1000 + i]);
  const zip = await readAll(s.extractZip(['deep', 'Ünïcødé', 'hard', 'other', 'links', 'big/medium-700KiB.bin']));
  out.zipB64 = b64(zip);
  out.zipName = s.suggestedFilename(['deep'], true);
  // unseal a delivery produced by format.NewSealWriter (IBS2): the one-time key moves into the
  // module, the repository key authenticates it, the request binds it to its job and parameters
  const req = { jobId: fx.sealJobId, type: fx.sealType, params: fx.sealParams };
  const sealOf = () => lib.importSealKey(Uint8Array.from(fx.sealPrivateHex.match(/../g).map((x) => parseInt(x, 16))), { core });
  const seal = await sealOf();
  const sealedResp = await fetch('/fixture/' + fx.sealed.multi.file);
  const plain = await readAll(lib.unsealStream(sealedResp.body, { key, seal, request: req }));
  out.unsealSha = hex(await crypto.subtle.digest('SHA-256', plain));
  const sealedBytes = new Uint8Array(await (await fetch('/fixture/' + fx.sealed.multi.file)).arrayBuffer());
  out.unsealTruncated = await errName(readAll(lib.unsealStream(sealedBytes.subarray(0, sealedBytes.length - 3), { key, seal, request: req })));
  out.unsealOtherJob = await errName(readAll(lib.unsealStream(sealedBytes, { key, seal, request: { ...req, jobId: 'another-job' } })));
  await seal.close();
  const kp = await lib.generateSealKeypair({ core });
  out.keypairLens = [kp.publicKey.length, 'privateKey' in kp ? 'exposed' : 'hidden'];
  await kp.close();
  // tampering and a wrong key
  const tampering = {
    index: (g, sn, sig) => fetcher.index(g, sn, sig),
    manifest: (g, sn, sig) => fetcher.manifest(g, sn, sig),
    chunk: async (id, sig) => {
      const b = await fetcher.chunk(id, sig);
      b[b.length >> 1] ^= 1;
      return b;
    },
  };
  out.tampered = await errName(new lib.RepoReader(key, tampering).openSnapshot(cat.group, cat.snap));
  const wrong = await lib.openKey(fx.wrongRecoveryKey, { core });
  out.wrongKey = await errName(new lib.RepoReader(wrong, fetcher).openSnapshot(cat.group, cat.snap));
  const a = fx.snapshots.find((x) => x.label === 'archive');
  out.archive = (await reader.openSnapshot(a.group, a.snap)).unsupported;
  await s.close();
  await key.close();
  await wrong.close();
  out.expected = expected;
  return out;
}

export { readAll };
