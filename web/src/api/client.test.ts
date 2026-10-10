import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, buildUrl, onUnauthorized, request, setCsrf, setRepositoryProvider } from './client'

function respond(status: number, body?: unknown, raw?: string) {
  return vi.fn(async () => new Response(raw ?? (body === undefined ? null : JSON.stringify(body)), { status, headers: { 'Content-Type': 'application/json' } }))
}

afterEach(() => { vi.unstubAllGlobals(); setCsrf(''); setRepositoryProvider(() => ''); onUnauthorized(() => {}) })

describe('api client', () => {
  it('builds URLs with query and the selected repository', () => {
    setRepositoryProvider(() => 'custody')
    expect(buildUrl('/hosts', undefined, true)).toBe('/api/v1/hosts?repository=custody')
    expect(buildUrl('/runs', { limit: 5, x: undefined, y: '' }, true)).toBe('/api/v1/runs?limit=5&repository=custody')
    expect(buildUrl('/runs', { repository: 'other' }, true)).toBe('/api/v1/runs?repository=other')
    expect(buildUrl('/overview')).toBe('/api/v1/overview')
  })

  it('sends the CSRF header only on state-changing requests', async () => {
    const f = respond(204)
    vi.stubGlobal('fetch', f)
    setCsrf('tok123')
    await request('GET', '/x')
    await request('POST', '/x', { body: { a: 1 } })
    const h0 = (f.mock.calls[0] as unknown as [string, RequestInit])[1].headers as Record<string, string>
    const h1 = (f.mock.calls[1] as unknown as [string, RequestInit])[1].headers as Record<string, string>
    expect(h0['X-CSRF-Token']).toBeUndefined()
    expect(h1['X-CSRF-Token']).toBe('tok123')
    expect(h1['Content-Type']).toBe('application/json')
  })

  it('normalises {error} bodies, non-JSON errors and network failures', async () => {
    vi.stubGlobal('fetch', respond(409, { error: 'snapshot is protected' }))
    await expect(request('DELETE', '/snapshots/a/b')).rejects.toMatchObject({ status: 409, message: 'snapshot is protected' })
    vi.stubGlobal('fetch', respond(502, undefined, '<html>bad gateway</html>'))
    await expect(request('GET', '/x')).rejects.toMatchObject({ status: 502 })
    vi.stubGlobal('fetch', respond(503, undefined, ''))
    await expect(request('GET', '/x')).rejects.toThrow(/maintenance/)
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    const e = await request('GET', '/x').catch((x) => x)
    expect(e).toBeInstanceOf(ApiError)
    expect((e as ApiError).isNetwork).toBe(true)
  })

  it('calls the unauthorized handler on 401 unless the route opts out', async () => {
    const seen: number[] = []
    onUnauthorized((e) => seen.push(e.status))
    vi.stubGlobal('fetch', respond(401, { error: 'not signed in' }))
    await expect(request('GET', '/overview')).rejects.toBeInstanceOf(ApiError)
    await expect(request('POST', '/session', { noAuthRedirect: true })).rejects.toBeInstanceOf(ApiError)
    expect(seen).toEqual([401])
  })

  it('passes aborts through untouched', async () => {
    vi.stubGlobal('fetch', vi.fn(async (_u: string, init: RequestInit) => {
      if (init.signal?.aborted) throw new DOMException('aborted', 'AbortError')
      return new Response('{}')
    }))
    const c = new AbortController()
    c.abort()
    await expect(request('GET', '/x', { signal: c.signal })).rejects.toMatchObject({ name: 'AbortError' })
  })

  it('parses JSON and treats 204 as undefined', async () => {
    vi.stubGlobal('fetch', respond(200, { ok: true }))
    expect(await request('GET', '/ping')).toEqual({ ok: true })
    vi.stubGlobal('fetch', respond(204))
    expect(await request('DELETE', '/x')).toBeUndefined()
  })
})
