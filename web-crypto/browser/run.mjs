// Runs the library in headless Chromium (Playwright) against a local static server that serves
// dist/, wasm/, the test page and the fixture repository at the HttpFetcher paths. Exits non-zero
// on any failed check. Needs: npm run build, the fixture (npm run fixture), a Playwright Chromium.
//
//   node browser/run.mjs [--bench] [--json out.json]
//   env: CHROMIUM_PATH (browser binary), CHROMIUM_LIBS (extra LD_LIBRARY_PATH for it)
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createReadStream, existsSync, mkdtempSync, readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { homedir, tmpdir } from 'node:os';
import { extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const root = fileURLToPath(new URL('..', import.meta.url));
const fix = join(root, '.cache/fixture');
const bench = process.argv.includes('--bench');
const jsonOut = process.argv.includes('--json') ? process.argv[process.argv.indexOf('--json') + 1] : '';

const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.html': 'text/html; charset=utf-8', '.wasm': 'application/wasm', '.json': 'application/json', '.map': 'application/json' };
const requests = [];
const server = createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  const path = decodeURIComponent(u.pathname);
  requests.push({ method: req.method, path, headers: req.headers });
  let file;
  if (path.startsWith('/api/v1/')) file = join(fix, path);
  else if (path.startsWith('/fixture/')) file = join(fix, path.slice('/fixture/'.length));
  else if (/^\/(dist|wasm|browser|bench)\//.test(path)) file = join(root, path);
  else if (path === '/') file = join(root, 'browser/index.html');
  if (!file || normalize(file) !== file || !existsSync(file) || !statSync(file).isFile()) {
    res.writeHead(404).end();
    return;
  }
  res.writeHead(200, { 'content-type': types[extname(file)] ?? 'application/octet-stream', 'content-length': statSync(file).size, 'cache-control': 'no-store' });
  createReadStream(file).pipe(res);
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const base = `http://127.0.0.1:${server.address().port}`;

function chromePath() {
  if (process.env.CHROMIUM_PATH) return process.env.CHROMIUM_PATH;
  const pw = join(homedir(), '.cache/ms-playwright');
  for (const d of readdirSync(pw).filter((d) => d.startsWith('chromium_headless_shell-')).sort().reverse()) {
    const p = join(pw, d, 'chrome-headless-shell-linux64/chrome-headless-shell');
    if (existsSync(p)) return p;
  }
  throw new Error('no Playwright chromium_headless_shell found (set CHROMIUM_PATH)');
}

const fx = JSON.parse(readFileSync(join(fix, 'fixture.json'), 'utf8'));
const sha = (b) => createHash('sha256').update(b).digest('hex');
const cat = fx.snapshots.find((s) => s.label === 'catalog');
const bigSha = sha(readFileSync(join(fix, cat.source, 'big/random-30MiB.bin')));
const unsealSha = sha(readFileSync(join(fix, fx.sealed.multi.plain)));
const failures = [];
const check = (what, ok, detail = '') => {
  if (!ok) failures.push(`${what} ${detail}`);
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}${ok ? '' : ' ' + detail}`);
};

function verify(label, r) {
  check(`${label}: bad key rejected with KeyError`, /^KeyError: .*checksum mismatch/.test(r.badKey), r.badKey);
  check(`${label}: domain id`, r.domainId === fx.domainId);
  check(`${label}: catalog format`, r.format === 'catalog' && r.fsFormat === '2');
  check(`${label}: walk = count`, r.walkCount === r.treeCount && r.treeCount > 400, `${r.walkCount} ${r.treeCount}`);
  check(`${label}: unicode listing`, JSON.stringify(r.unicode) === JSON.stringify(['emoji 🎉 party.txt', 'ß-straße', '日本語 ファイル.txt'].sort()), JSON.stringify(r.unicode));
  check(`${label}: 30 MiB file SHA-256`, r.bigSize === 30 << 20 && r.bigSha === bigSha && r.bigShaCatalog === bigSha);
  check(`${label}: range read`, r.rangeOk === true);
  check(`${label}: unseal`, r.unsealSha === unsealSha);
  check(`${label}: truncated sealed stream rejected`, /^IntegrityError/.test(r.unsealTruncated), r.unsealTruncated);
  check(`${label}: another job's delivery rejected`, /^IntegrityError/.test(r.unsealOtherJob), r.unsealOtherJob);
  check(`${label}: keypair (private half stays in the module)`, JSON.stringify(r.keypairLens) === '[32,"hidden"]', JSON.stringify(r.keypairLens));
  check(`${label}: tampered chunk rejected`, /^IntegrityError/.test(r.tampered), r.tampered);
  check(`${label}: wrong key rejected`, /^IntegrityError: manifest authentication failed/.test(r.wrongKey), r.wrongKey);
  check(`${label}: archive unsupported`, r.archive === 'archive format cannot be browsed');
  check(`${label}: zip name`, r.zipName === 'deep.zip');
  const dir = mkdtempSync(join(tmpdir(), 'ibbrowser-'));
  const zf = join(dir, 'sel.zip');
  writeFileSync(zf, Buffer.from(r.zipB64, 'base64'));
  let t = '';
  try {
    t = execFileSync('unzip', ['-t', zf], { encoding: 'utf8' });
    execFileSync('unzip', ['-q', zf, '-d', join(dir, 'x')]);
  } catch (e) {
    t = String(e);
  }
  check(`${label}: zip passes unzip -t`, /No errors detected/.test(t), t.slice(0, 300));
  const same = (p) => existsSync(join(dir, 'x', p)) && readFileSync(join(dir, 'x', p)).equals(readFileSync(join(fix, cat.source, p)));
  check(`${label}: zip content`, same('deep/a/b/c/d/e/f/leaf.txt') && same('Ünïcødé/日本語 ファイル.txt') && same('big/medium-700KiB.bin') && same('other/third-link.txt'));
}

// CHROMIUM_LIBS: extra library directories for the browser process (a container without the
// system libraries Chromium links against: unpack the Debian packages there, see README.md)
// (scripts/chromium-libs.py unpacks them into .cache/chromium-libs, which is picked up by itself)
const env = { ...process.env };
const unpacked = join(root, '.cache/chromium-libs/root');
const libs = [process.env.CHROMIUM_LIBS, ...(existsSync(unpacked) ? [join(unpacked, 'usr/lib/x86_64-linux-gnu'), join(unpacked, 'lib/x86_64-linux-gnu')] : []), process.env.LD_LIBRARY_PATH];
if (libs.some(Boolean)) env.LD_LIBRARY_PATH = libs.filter(Boolean).join(':');
const browser = await chromium.launch({ executablePath: chromePath(), headless: true, env });
const results = { browser: browser.version(), base };
try {
  const page = await browser.newPage();
  const logs = [];
  page.on('console', (m) => logs.push(m.text()));
  page.on('pageerror', (e) => logs.push('pageerror: ' + e.message));
  await page.goto(base + '/');
  await page.waitForFunction(() => window.ibready === true, null, { timeout: 30000 });

  const def = await page.evaluate(() => window.ibtest.functional('default'));
  check('page default core runs in Web Workers', def.transport === 'PoolTransport' || def.transport === 'WorkerTransport', def.transport);
  verify('worker pool (page default)', def);
  const one = await page.evaluate(() => window.ibtest.functional('worker', 1));
  check('single worker transport', one.transport === 'WorkerTransport', one.transport);
  verify('single worker', one);
  const direct = await page.evaluate(() => window.ibtest.functional('direct'));
  check('direct transport', direct.transport === 'DirectTransport', direct.transport);
  verify('page main thread', direct);
  const uw = await page.evaluate(() => window.ibtest.inUserWorker());
  check('user worker: library ran inside a Web Worker', uw.inWorker === true && uw.transport === 'DirectTransport', JSON.stringify([uw.inWorker, uw.transport]));
  verify('inside a user Web Worker', uw);
  const sd = await page.evaluate(() => window.ibtest.shutdown());
  check('shutdown(): the old workers are gone (old handles fail)', /terminated/.test(sd.oldKey) && /terminated/.test(sd.oldCore), JSON.stringify(sd));
  check('shutdown(): a fresh core opens the key again', sd.fresh === true && sd.reopened === true, JSON.stringify(sd));
  results.readFile30MiBms = { pool: def.bigMs, oneWorker: one.bigMs, mainThread: direct.bigMs, userWorker: uw.bigMs };

  if (bench) {
    results.bench = {
      pool: await page.evaluate(() => window.ibtest.bench('worker')),
      oneWorker: await page.evaluate(() => window.ibtest.bench('worker', 1)),
      mainThread: await page.evaluate(() => window.ibtest.bench('direct')),
    };
    results.hardwareConcurrency = await page.evaluate(() => navigator.hardwareConcurrency);
  }
  // nothing the page sent to the server carried key material
  const secrets = [fx.recoveryKey, fx.recoveryKey.replace(/-/g, ''), fx.recoveryKey.toLowerCase(), fx.masterHex];
  const leaked = requests.filter((r) => secrets.some((s) => JSON.stringify(r).includes(s)));
  check('no request carried key material', leaked.length === 0, JSON.stringify(leaked.slice(0, 2)));
  check('api requests are plain GETs', requests.filter((r) => r.path.startsWith('/api/')).every((r) => r.method === 'GET'));
  const keyLogs = logs.filter((l) => secrets.some((s) => l.includes(s)));
  check('no key material in console output', keyLogs.length === 0);
  results.consoleLines = logs.length;
  results.apiRequests = requests.filter((r) => r.path.startsWith('/api/')).length;
} finally {
  await browser.close();
  server.close();
}
console.log(JSON.stringify(results, null, 2));
if (jsonOut) writeFileSync(jsonOut, JSON.stringify(results, null, 2));
if (failures.length) {
  console.error(`${failures.length} browser check(s) failed`);
  process.exit(1);
}
console.log('all browser checks passed');
