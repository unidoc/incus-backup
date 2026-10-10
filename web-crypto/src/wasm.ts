// Loading the Go WebAssembly module into the current JavaScript realm (Node, a page, or a worker).

/** The raw functions cmd/ibwasm installs on globalThis.ibwasm. Each returns {ok} or {err}. */
export type RawApi = Record<string, (...args: unknown[]) => { ok?: unknown; err?: string }>;

export interface WasmLocation {
  /** URL (or file URL) of ibwasm.wasm; default: ../wasm/ibwasm.wasm next to this module. */
  wasmUrl?: string | URL;
  /** URL of wasm_exec.js; default: ../wasm/wasm_exec.js next to this module. */
  wasmExecUrl?: string | URL;
  /** The module bytes, when the caller already has them. */
  wasmBytes?: Uint8Array | ArrayBuffer;
}

let loading: Promise<RawApi> | undefined;

const isNode = !!(globalThis as { process?: { versions?: { node?: string } } }).process?.versions?.node;

async function moduleBytes(loc: WasmLocation): Promise<WebAssembly.Module> {
  if (loc.wasmBytes) return WebAssembly.compile(loc.wasmBytes as BufferSource);
  const url = new URL(loc.wasmUrl ?? '../wasm/ibwasm.wasm', import.meta.url);
  if (url.protocol === 'file:' && isNode) {
    const fs = await import(/* @vite-ignore */ 'node:fs/promises' as string);
    const { fileURLToPath } = await import(/* @vite-ignore */ 'node:url' as string);
    return WebAssembly.compile((await fs.readFile(fileURLToPath(url))) as BufferSource);
  }
  const res = await fetch(url);
  if (!res.ok) throw new Error(`loading ${url}: HTTP ${res.status}`);
  if (typeof WebAssembly.compileStreaming === 'function' && res.headers.get('content-type')?.startsWith('application/wasm')) {
    return WebAssembly.compileStreaming(res);
  }
  return WebAssembly.compile(await res.arrayBuffer());
}

/** Loads the module once per realm and returns its API. */
export function loadWasm(loc: WasmLocation = {}): Promise<RawApi> {
  if (!loading) {
    loading = (async () => {
      const g = globalThis as unknown as { Go?: new () => GoRuntime; ibwasm?: RawApi; __ibwasmReady?: () => void };
      if (!g.Go) {
        await import(/* @vite-ignore */ new URL(loc.wasmExecUrl ?? '../wasm/wasm_exec.js', import.meta.url).href);
      }
      if (!g.Go) throw new Error('wasm_exec.js did not define Go');
      const mod = await moduleBytes(loc);
      const go = new g.Go();
      const inst = await WebAssembly.instantiate(mod, go.importObject);
      const ready = new Promise<void>((resolve) => {
        g.__ibwasmReady = resolve;
      });
      const exited = go.run(inst).then(() => {
        throw new Error('the Go runtime exited');
      });
      await Promise.race([ready, exited]);
      if (!g.ibwasm) throw new Error('ibwasm did not register');
      return g.ibwasm;
    })();
    loading.catch(() => {
      loading = undefined;
    });
  }
  return loading;
}

interface GoRuntime {
  importObject: WebAssembly.Imports;
  run(instance: WebAssembly.Instance): Promise<void>;
}
