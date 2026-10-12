// Vite plugin: ships the browser decryption library (web-crypto: Go -> WebAssembly + its ES modules)
// next to the console, unbundled, with its own relative layout (worker.js imports ./wasm.js, wasm.js
// loads ../wasm/ibwasm.wasm). The files land under assets/crypto-<hash>/ so the server caches them as
// immutable (the hash covers every file), and the page learns the base URL through __IB_CRYPTO_BASE__.
//
//   assets/crypto-<hash>/dist/*.js        the library (index.js, worker.js, ...)
//   assets/crypto-<hash>/wasm/ibwasm.wasm the module (served as application/wasm)
//   assets/crypto-<hash>/wasm/wasm_exec.js  Go's loader (same Go release as the module)
//
// `vite build` fails when the library was not built: run `just web` (or `npm run crypto`).
// The dev server serves the same paths from the source directories.
import { createHash } from 'node:crypto'
import { existsSync, readdirSync, readFileSync, realpathSync } from 'node:fs'
import { join, resolve } from 'node:path'
import type { Plugin } from 'vite'

const MIME: Record<string, string> = { '.js': 'text/javascript; charset=utf-8', '.wasm': 'application/wasm', '.map': 'application/json' }

function libraryRoot(webDir: string): string {
  const linked = join(webDir, 'node_modules', '@incus-backup', 'web-crypto')
  return existsSync(linked) ? realpathSync(linked) : resolve(webDir, '..', 'web-crypto')
}

/** The files to ship: published path (relative to the base) -> absolute source path. */
export function cryptoFiles(webDir: string): { files: Map<string, string>; missing: string[]; root: string } {
  const root = libraryRoot(webDir)
  const files = new Map<string, string>()
  const missing: string[] = []
  const dist = join(root, 'dist')
  for (const need of ['dist/index.js', 'dist/worker.js', 'wasm/ibwasm.wasm', 'wasm/wasm_exec.js']) {
    if (!existsSync(join(root, need))) missing.push(need)
  }
  if (existsSync(dist)) {
    for (const f of readdirSync(dist).sort()) if (f.endsWith('.js')) files.set(`dist/${f}`, join(dist, f))
  }
  for (const f of ['ibwasm.wasm', 'wasm_exec.js']) if (existsSync(join(root, 'wasm', f))) files.set(`wasm/${f}`, join(root, 'wasm', f))
  return { files, missing, root }
}

export function hashFiles(files: Map<string, string>): string {
  const h = createHash('sha256')
  for (const [p, src] of files) {
    h.update(p + '\0')
    h.update(readFileSync(src))
    h.update('\0')
  }
  return h.digest('hex').slice(0, 12)
}

export function webCrypto(webDir: string): Plugin {
  let base = '/assets/crypto-missing/'
  let files = new Map<string, string>()
  let missing: string[] = []
  let root = ''
  let command: 'build' | 'serve' = 'serve'
  return {
    name: 'ib-web-crypto',
    config(_c, env) {
      command = env.command
      ;({ files, missing, root } = cryptoFiles(webDir))
      if (missing.length === 0) base = `/assets/crypto-${hashFiles(files)}/`
      return { define: { __IB_CRYPTO_BASE__: JSON.stringify(base) } }
    },
    buildStart() {
      if (command === 'build' && missing.length > 0 && !process.env.VITEST) {
        this.error(`web-crypto is not built (missing ${missing.join(', ')} in ${root}). Run \`just web\` or \`npm run crypto\` in web/ first.`)
      }
    },
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = (req.url ?? '').split('?')[0]
        if (!url.startsWith(base)) return next()
        const src = files.get(url.slice(base.length))
        if (!src) return next()
        const ext = src.slice(src.lastIndexOf('.'))
        res.setHeader('Content-Type', MIME[ext] ?? 'application/octet-stream')
        res.end(readFileSync(src))
      })
    },
    generateBundle() {
      if (missing.length > 0) return
      for (const [p, src] of files) {
        this.emitFile({ type: 'asset', fileName: `${base.slice(1)}${p}`, source: readFileSync(src) })
      }
    },
  }
}
