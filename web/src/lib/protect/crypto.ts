// The browser decryption library (web-crypto, Go -> WebAssembly) as the console uses it.
//
// The library is NOT bundled: the build ships it unchanged under assets/crypto-<hash>/ (see
// web/build/crypto.ts) and the page imports it from there at run time, so its worker, module and
// loader keep their own relative layout. Only types come from the package at compile time.
//
// Key handling (README "Never keep secrets anywhere but component memory"): the recovery key string
// goes straight into openKey() and is not kept; the derived sub-keys live inside the WebAssembly
// module (in a worker) and are wiped by key.close()/zeroize(). Nothing key-related goes into a URL,
// a header, storage or a store.
import type * as WC from '@incus-backup/web-crypto'
import { ApiError, request, seg } from '@/api/client'

declare const __IB_CRYPTO_BASE__: string

export type CryptoLib = typeof WC
export type { EntryInfo, CatalogSnapshot, OpenedSnapshot, KeyHandle, FetchProgress, Fetcher, Tree, SealKey, SealKeypair, DeliveryRequest } from '@incus-backup/web-crypto'

/** Where the build put the library (overridable for tests). */
export const cryptoBase = (): string => (typeof __IB_CRYPTO_BASE__ === 'string' ? __IB_CRYPTO_BASE__ : '/assets/crypto-missing/')

let lib: Promise<CryptoLib> | null = null

/** Loads the library once and points it at the shipped worker and module. */
export function loadCrypto(): Promise<CryptoLib> {
  if (!lib) {
    const base = cryptoBase()
    lib = (import(/* @vite-ignore */ `${base}dist/index.js`) as Promise<CryptoLib>).then((m) => {
      m.configure({ mode: 'auto', workerUrl: `${base}dist/worker.js`, wasmUrl: `${base}wasm/ibwasm.wasm`, wasmExecUrl: `${base}wasm/wasm_exec.js` })
      return m
    })
    lib.catch(() => { lib = null })
  }
  return lib
}

/**
 * Wipes every key, catalog, delivery key and stream inside the module, then terminates its Web
 * Workers so their memory goes back to the browser (best effort; safe to call any time; the next
 * use starts fresh workers).
 *
 * Residual limits (honest): Go cannot guarantee that no copy of a derived key survives in the
 * WebAssembly heap until the worker ends (the garbage collector may have moved it), which is why the
 * workers are terminated; JavaScript strings (the pasted recovery key) cannot be wiped at all, so the
 * page only drops its references; the browser itself (devtools, a crash dump, swap) is outside our reach.
 */
export async function wipeCrypto(): Promise<void> {
  if (!lib) return
  try {
    const m = await lib
    try { await m.zeroize() } catch { /* module not loaded or already gone */ }
    m.shutdown()
  } catch { /* the library never loaded */ }
}

const GROUP = /^[0-9a-f]{32}$/
const SNAP = /^[0-9]{14}-[0-9a-f]{8}$/
const CHUNK = /^[0-9a-f]{64}$/

/**
 * Size caps of raw repository objects, matching what the server accepts from hosts and what the
 * format allows: a chunk blob holds at most format.MaxChunkSize (64 MiB) of data plus its framing,
 * an index at most 256 MiB and a manifest at most 16 MiB (internal/server/httpapi). A larger answer
 * is cut off while it streams: a malicious or broken server cannot fill the tab's memory.
 */
export const RAW_LIMITS = { chunk: (64 << 20) + 4096, index: 256 << 20, manifest: 16 << 20 } as const

/**
 * Fetcher over the console's API client: the same routes HttpFetcher uses, but with ?repository= of
 * the selected repository (several repositories exist on most servers) and the client's 401 handling.
 * Only identifiers that passed a format check reach a URL; no header or body is ever added.
 */
export class ApiFetcher implements WC.Fetcher {
  calls = 0
  constructor(private readonly onCall?: (what: 'index' | 'manifest' | 'chunk') => void) {}
  private get(path: string, maxBytes: number, signal?: AbortSignal): Promise<ArrayBuffer> {
    return request<ArrayBuffer>('GET', path, { repo: true, raw: true, signal, maxBytes })
  }
  async index(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array> {
    if (!GROUP.test(group) || !SNAP.test(snap)) throw new Error('invalid snapshot reference')
    this.calls++; this.onCall?.('index')
    return new Uint8Array(await this.get(`/snapshots/${seg(group)}/${seg(snap)}/index`, RAW_LIMITS.index, signal))
  }
  async manifest(group: string, snap: string, signal?: AbortSignal): Promise<Uint8Array> {
    if (!GROUP.test(group) || !SNAP.test(snap)) throw new Error('invalid snapshot reference')
    this.calls++; this.onCall?.('manifest')
    return new Uint8Array(await this.get(`/snapshots/${seg(group)}/${seg(snap)}/manifest`, RAW_LIMITS.manifest, signal))
  }
  async chunk(id: string, signal?: AbortSignal): Promise<Uint8Array> {
    if (!CHUNK.test(id)) throw new Error('invalid chunk id')
    this.calls++; this.onCall?.('chunk')
    return new Uint8Array(await this.get(`/chunks/${seg(id)}`, RAW_LIMITS.chunk, signal))
  }
}

export type CryptoFailure = 'key-format' | 'wrong-key' | 'tampered' | 'missing' | 'forbidden' | 'maintenance' | 'network' | 'module' | 'aborted' | 'other'

export interface ExplainedError { kind: CryptoFailure; title: string; detail: string }

const nameOf = (e: unknown) => (e && typeof e === 'object' && 'name' in e ? String((e as { name: unknown }).name) : '')

/**
 * Turns any failure of key entry, snapshot opening or a download into words an operator can act on.
 * `keyMatchesRepo` says whether the key's domain id equals the repository's: a wrong key and
 * tampering look identical to the cryptography (both fail authentication), the domain id tells them apart.
 */
export function explainCryptoError(e: unknown, keyMatchesRepo: boolean | null = null): ExplainedError {
  const name = nameOf(e)
  const msg = e instanceof Error ? e.message : String(e)
  if (name === 'AbortError') return { kind: 'aborted', title: 'Cancelled', detail: 'The operation was cancelled. Nothing was saved.' }
  if (name === 'KeyError') {
    return { kind: 'key-format', title: 'This is not a valid recovery key', detail: `${msg}. A recovery key starts with IBR1- and is what \`incus-backup key recovery\` prints on the host. Check for a typo or a missing group.` }
  }
  if (name === 'IntegrityError') {
    if (/IBS1/.test(msg)) {
      return { kind: 'tampered', title: 'The host runs an older incus-backup', detail: 'The delivery uses the old sealed format, which this console no longer accepts because the server could have forged it. Update incus-backup on the host and run the job again. Nothing was saved.' }
    }
    if (/identity does not match/.test(msg)) {
      return { kind: 'tampered', title: 'This snapshot is not the object the server listed', detail: `${msg}. The server presented another object's data under this name. Nothing was shown. Report this: the server or its database was tampered with.` }
    }
    if (keyMatchesRepo === false) {
      return { kind: 'wrong-key', title: 'This key belongs to another repository', detail: 'The data does not open with this key. Use the recovery key of the hosts that back up into this repository.' }
    }
    if (/sha-?256|digest|does not match/i.test(msg) && !/manifest/i.test(msg)) {
      return { kind: 'tampered', title: 'Integrity check failed: the file was not saved', detail: `The decrypted content does not match the checksum recorded at backup time (${msg}). The stored data was altered or damaged. Run a verification of this snapshot and do not trust this copy.` }
    }
    return { kind: 'tampered', title: 'Tampering or damage detected', detail: `The repository data did not authenticate (${msg}). The key is from this repository, so the stored bytes were altered, swapped or damaged. Run a verification; nothing unverified was shown or saved.` }
  }
  if (e instanceof ApiError) {
    if (/are accepted\. The repository data is damaged/.test(e.message)) return { kind: 'tampered', title: 'The server sent an oversized object', detail: `${e.message} The download was stopped; nothing was kept.` }
    if (e.status === 404) return { kind: 'missing', title: 'Part of the snapshot is missing', detail: `${e.message} The snapshot may have been forgotten or pruned, or a chunk is missing from the datastore (run a verification).` }
    if (e.status === 403) return { kind: 'forbidden', title: 'Your role cannot read snapshot data', detail: 'Browsing needs the restore role. Ask an administrator.' }
    if (e.status === 503) return { kind: 'maintenance', title: 'The repository is offline', detail: `${e.message} Browsing works again once an administrator sets the repository online or read-only.` }
    if (e.status === 0) return { kind: 'network', title: 'The server could not be reached', detail: 'Check your connection and try again.' }
    return { kind: 'other', title: 'The server refused the request', detail: e.message }
  }
  if (/worker|wasm|WebAssembly|loading .*HTTP|Go runtime/i.test(msg)) {
    return { kind: 'module', title: 'The decryption module did not load', detail: `${msg}. Reload the page; if it persists, the server's UI build lacks the web-crypto files (run \`just web\` and rebuild the server).` }
  }
  return { kind: 'other', title: 'Something went wrong', detail: msg }
}

/** Extracts a recovery key from pasted text or a text file (a recovery sheet, an e-mail). */
export function findRecoveryKey(text: string): { key: string | null; hint?: string } {
  const t = text.replace(/\r/g, '')
  const trimmed = t.trim()
  // the key alone (any case, any separators): the module normalises it
  if (/^ibr1[-\sA-Z2-7]*$/i.test(trimmed)) return { key: trimmed.replace(/\s+/g, '') }
  // inside other text: upper-case base32 groups, each group ending at a word boundary
  const m = t.match(/IBR1(?:[-\s]+[A-Z2-7]{1,5}(?![A-Za-z0-9]))+/)
  if (m) return { key: m[0].replace(/\s+/g, '') }
  if (trimmed.startsWith('{')) {
    try {
      const j = JSON.parse(trimmed) as Record<string, unknown>
      if ('protection' in j || 'domain_id' in j || 'wrapped' in j || 'master' in j) {
        return { key: null, hint: 'This is the host\'s key file, not a recovery key. On the host, run `incus-backup key recovery` and paste what it prints (IBR1-...).' }
      }
    } catch { /* not JSON */ }
  }
  if (/IBE1/i.test(t)) return { key: null, hint: 'This is an escrow key (IBE1), not a recovery key. Open the escrow on the host with `incus-backup key recovery` first.' }
  return { key: null, hint: 'No recovery key found. A recovery key starts with IBR1- followed by groups of five letters and digits.' }
}
