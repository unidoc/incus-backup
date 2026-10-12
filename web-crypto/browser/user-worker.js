// A GUI-owned worker using the library directly (inside a worker the default Core is direct).
import * as lib from '/dist/index.js';
import { functional } from '/browser/checks.js';

try {
  const core = lib.defaultCore();
  const r = await functional(lib, core, null);
  r.transport = core.transport.constructor.name;
  r.inWorker = typeof window === 'undefined' && typeof self.importScripts === 'function';
  self.postMessage(r);
} catch (e) {
  self.postMessage({ error: `${e.name}: ${e.message}\n${e.stack}` });
}
