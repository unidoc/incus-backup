// node bench/node.mjs  (after npm run build and npm run fixture)
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as lib from '../dist/index.js';
import { runBench } from './bench-core.mjs';

const fix = fileURLToPath(new URL('../.cache/fixture/', import.meta.url));
const fx = JSON.parse(readFileSync(join(fix, 'fixture.json'), 'utf8'));
const file = (...p) => new Uint8Array(readFileSync(join(fix, 'api/v1', ...p)));
const fetcher = {
  index: async (g, s) => file('snapshots', g, s, 'index'),
  manifest: async (g, s) => file('snapshots', g, s, 'manifest'),
  chunk: async (id) => file('chunks', id),
};
const core = lib.createCore({ mode: 'direct' });
const res = await runBench(lib, core, fetcher, fx, { loadSealed: async () => new Uint8Array(readFileSync(join(fix, fx.sealed.multi.file))) });
console.log(JSON.stringify({ runtime: `node ${process.version}`, ...res }, null, 2));
