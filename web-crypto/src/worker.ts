// Module Web Worker entry: hosts the Go WebAssembly module and answers Core calls. Keys never
// leave this worker; only handles, ciphertext and the plaintext the page asked for cross it.
import { loadWasm, type RawApi } from './wasm.js';

interface Scope {
  onmessage: ((ev: MessageEvent) => void) | null;
  postMessage(msg: unknown, transfer?: Transferable[]): void;
}
const scope = globalThis as unknown as Scope;

let api: Promise<RawApi> | undefined;
const queue: MessageEvent[] = [];

function transferables(v: unknown, out: Transferable[]): void {
  if (v instanceof Uint8Array || v instanceof Float64Array) {
    out.push(v.buffer as ArrayBuffer);
  } else if (v && typeof v === 'object') {
    for (const x of Object.values(v)) transferables(x, out);
  }
}

async function handle(ev: MessageEvent): Promise<void> {
  const m = ev.data as { id: number; name: string; args: unknown[] };
  try {
    const a = await (api ?? (api = loadWasm()));
    const f = a[m.name];
    if (typeof f !== 'function') throw new Error(`unknown wasm function ${m.name}`);
    const r = f(...m.args);
    if (r.err !== undefined) {
      scope.postMessage({ id: m.id, err: r.err, goErr: true });
      return;
    }
    const t: Transferable[] = [];
    transferables(r.ok, t);
    scope.postMessage({ id: m.id, ok: r.ok }, t);
  } catch (e) {
    scope.postMessage({ id: m.id, err: e instanceof Error ? e.message : String(e) });
  }
}

scope.onmessage = (ev: MessageEvent) => {
  const d = ev.data as { init?: { wasmUrl?: string; wasmExecUrl?: string } };
  if (d.init) {
    api = loadWasm({ wasmUrl: d.init.wasmUrl, wasmExecUrl: d.init.wasmExecUrl });
    api.catch(() => undefined);
    return;
  }
  queue.push(ev);
  if (queue.length === 1) void drain();
};

// calls are answered strictly in order (the module is single threaded anyway)
async function drain(): Promise<void> {
  while (queue.length > 0) {
    await handle(queue[0]!);
    queue.shift();
  }
}
