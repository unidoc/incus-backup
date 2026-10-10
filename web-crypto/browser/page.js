import * as lib from '/dist/index.js';
import { runBench } from '/bench/bench-core.mjs';
import { functional } from '/browser/checks.js';

const fixtureFetcher = () => new lib.HttpFetcher('');
const loadSealed = async () => {
  const fx = await (await fetch('/fixture/fixture.json')).json();
  return new Uint8Array(await (await fetch('/fixture/' + fx.sealed.multi.file)).arrayBuffer());
};

window.ibtest = {
  /** mode: 'worker' (the library's worker pool, the page default) or 'direct' (page main thread). */
  async functional(mode, workers) {
    const core = mode === 'default' ? lib.defaultCore() : lib.createCore({ mode, workers });
    const r = await functional(lib, core, null);
    r.transport = core.transport.constructor.name;
    return r;
  },
  /** The whole library running inside a user Web Worker (its own module instance there). */
  inUserWorker() {
    return new Promise((resolve, reject) => {
      const w = new Worker('/browser/user-worker.js', { type: 'module' });
      w.onmessage = (ev) => (ev.data.error ? reject(new Error(ev.data.error)) : resolve(ev.data));
      w.onerror = (ev) => reject(new Error(ev.message));
    });
  },
  /** W6: shutdown() ends the default core's workers (old handles die), and a fresh unlock works afterwards. */
  async shutdown() {
    const fx = await (await fetch('/fixture/fixture.json')).json();
    const before = lib.defaultCore();
    const key = await lib.openKey(fx.recoveryKey);
    const seal = await lib.generateSealKeypair();
    const workers = before.transport.workers ?? [before.transport];
    lib.shutdown();
    const out = { workers: workers.length };
    try { await key.groupId('p', 'filesystem', 'volume/x'); out.oldKey = 'still works'; } catch (e) { out.oldKey = e.message; }
    try { await before.sealNew(); out.oldCore = 'still works'; } catch (e) { out.oldCore = e.message; }
    const after = lib.defaultCore();
    out.fresh = after !== before;
    const k2 = await lib.openKey(fx.recoveryKey);
    out.reopened = k2.domainId === fx.domainId;
    await k2.close();
    void seal;
    return out;
  },
  async bench(mode, workers) {
    const core = lib.createCore({ mode, workers });
    const fx = await (await fetch('/fixture/fixture.json')).json();
    return runBench(lib, core, fixtureFetcher(), fx, { loadSealed });
  },
};
window.ibready = true;
