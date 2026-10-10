// The one fetch wrapper every API call goes through.
//
//  * JSON in and out; errors are normalised to ApiError ({error} from the server, or a plain-English
//    message for network failures and non-JSON answers).
//  * State-changing requests carry X-CSRF-Token (the value from POST/GET /session, kept in memory only).
//  * A 401 on any authenticated route calls the registered unauthorized handler (the session store
//    sends the user to the sign-in page and remembers where they were). Sign-in and setup calls are
//    exempt: a wrong password must not navigate away.
//  * `repo: true` appends ?repository=<selected> (the repository selector in the top bar).
//  * Pass an AbortSignal to cancel; isAbort(err) tells a cancellation from a failure.

export const API_BASE = '/api/v1'

export class ApiError extends Error {
  /** HTTP status; 0 = the server could not be reached. */
  readonly status: number
  /** The decoded JSON body of the error response, when there was one. */
  readonly body: unknown
  readonly method: string
  readonly path: string
  constructor(status: number, message: string, body: unknown, method: string, path: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
    this.method = method
    this.path = path
  }
  get isUnauthorized() { return this.status === 401 }
  get isForbidden() { return this.status === 403 }
  get isNotFound() { return this.status === 404 }
  get isConflict() { return this.status === 409 }
  get isMaintenance() { return this.status === 503 }
  get isNetwork() { return this.status === 0 }
}

export function isAbort(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'AbortError'
}

export type Query = Record<string, string | number | boolean | undefined | null>

export interface RequestOptions {
  body?: unknown
  query?: Query
  signal?: AbortSignal
  /** Append ?repository=<selected repository> (routes that act on one repository). */
  repo?: boolean | string
  /** Do not trigger the global sign-in redirect on 401 (sign-in, setup, session probe). */
  noAuthRedirect?: boolean
  /** Expect a binary body (raw encrypted snapshot material). */
  raw?: boolean
  /** With raw: refuse (and stop reading) a body larger than this many bytes. */
  maxBytes?: number
  headers?: Record<string, string>
}

// ---- process-wide state, set by the stores (kept here to avoid import cycles) ---------------
let csrfToken = ''
let repositoryProvider: () => string = () => ''
let unauthorizedHandler: (err: ApiError) => void = () => {}
let activityHandler: () => void = () => {}

export function setCsrf(token: string) { csrfToken = token }
export function getCsrf() { return csrfToken }
export function setRepositoryProvider(fn: () => string) { repositoryProvider = fn }
export function onUnauthorized(fn: (err: ApiError) => void) { unauthorizedHandler = fn }
/** Called after every successful request: the server renews the idle window on activity. */
export function onActivity(fn: () => void) { activityHandler = fn }

export function buildUrl(path: string, query?: Query, repo?: boolean | string): string {
  const params = new URLSearchParams()
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v === undefined || v === null || v === '') continue
    params.set(k, String(v))
  }
  if (repo) {
    const name = typeof repo === 'string' ? repo : repositoryProvider()
    if (name && !params.has('repository')) params.set('repository', name)
  }
  const qs = params.toString()
  return API_BASE + path + (qs ? (path.includes('?') ? '&' : '?') + qs : '')
}

const SAFE = new Set(['GET', 'HEAD', 'OPTIONS'])

/** Turn a non-OK response into an ApiError with the server's own words when it gave any. */
async function toError(res: Response, method: string, path: string): Promise<ApiError> {
  let body: unknown = null
  let message = ''
  const text = await res.text().catch(() => '')
  if (text) {
    try {
      body = JSON.parse(text)
      if (body && typeof body === 'object' && 'error' in body && typeof (body as { error: unknown }).error === 'string') {
        message = (body as { error: string }).error
      }
    } catch {
      message = text.length < 200 ? text.trim() : ''
    }
  }
  if (!message) message = fallbackMessage(res.status, res.statusText)
  return new ApiError(res.status, message, body, method, path)
}

export function fallbackMessage(status: number, statusText = ''): string {
  switch (status) {
    case 401: return 'You are not signed in, or your session ended. Sign in again.'
    case 403: return 'Your role does not allow this.'
    case 404: return 'Not found. It may have been removed.'
    case 409: return 'This conflicts with the current state. Reload and try again.'
    case 429: return 'Too many attempts. Wait a minute and try again.'
    case 502: case 504: return 'The server did not answer in time. Try again.'
    case 503: return 'The repository is in maintenance (read-only or offline).'
  }
  return `The server answered ${status}${statusText ? ' ' + statusText : ''}.`
}

export async function request<T = unknown>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
  const url = buildUrl(path, opts.query, opts.repo)
  const headers: Record<string, string> = { Accept: opts.raw ? 'application/octet-stream' : 'application/json', ...opts.headers }
  let body: BodyInit | undefined
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(opts.body)
  }
  if (!SAFE.has(method) && csrfToken) headers['X-CSRF-Token'] = csrfToken
  let res: Response
  try {
    res = await fetch(url, { method, headers, body, signal: opts.signal, credentials: 'same-origin', cache: 'no-store' })
  } catch (e) {
    if (isAbort(e)) throw e
    throw new ApiError(0, 'Could not reach the server. Check your connection; the page retries when you act again.', null, method, path)
  }
  if (!res.ok) {
    const err = await toError(res, method, path)
    if (err.status === 401 && !opts.noAuthRedirect) unauthorizedHandler(err)
    throw err
  }
  activityHandler()
  if (opts.raw) return (opts.maxBytes !== undefined ? await readCapped(res, opts.maxBytes, method, path) : await res.arrayBuffer()) as T
  if (res.status === 204) return undefined as T
  const text = await res.text()
  if (!text) return undefined as T
  try {
    return JSON.parse(text) as T
  } catch {
    throw new ApiError(res.status, 'The server sent an answer this page cannot read (not JSON).', text, method, path)
  }
}

/** Reads a body of at most max bytes; a larger one is cancelled mid-stream (a hostile server cannot fill the tab's memory). */
export async function readCapped(res: Response, max: number, method = 'GET', path = ''): Promise<ArrayBuffer> {
  const tooBig = (n: string) => new ApiError(res.status, `The server sent ${n} bytes for ${path}; at most ${max} are accepted. The repository data is damaged or the server misbehaves.`, null, method, path)
  const declared = Number(res.headers.get('Content-Length') ?? NaN)
  if (Number.isFinite(declared) && declared > max) { await res.body?.cancel().catch(() => undefined); throw tooBig(String(declared)) }
  if (!res.body) return new ArrayBuffer(0)
  const r = res.body.getReader()
  const parts: Uint8Array[] = []
  let n = 0
  for (;;) {
    const { done, value } = await r.read()
    if (done) break
    n += value.byteLength
    if (n > max) { await r.cancel().catch(() => undefined); throw tooBig(`more than ${max}`) }
    parts.push(value)
  }
  const out = new Uint8Array(n)
  let o = 0
  for (const p of parts) { out.set(p, o); o += p.byteLength }
  return out.buffer
}

export const http = {
  get: <T>(path: string, o?: RequestOptions) => request<T>('GET', path, o),
  post: <T>(path: string, body?: unknown, o?: RequestOptions) => request<T>('POST', path, { ...o, body }),
  put: <T>(path: string, body?: unknown, o?: RequestOptions) => request<T>('PUT', path, { ...o, body }),
  del: <T>(path: string, body?: unknown, o?: RequestOptions) => request<T>('DELETE', path, { ...o, body }),
}

/** Encode one path segment (names may contain dots, spaces, slashes are refused by the server anyway). */
export const seg = (s: string) => encodeURIComponent(s)
