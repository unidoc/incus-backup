import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiFetcher, RAW_LIMITS, explainCryptoError } from './crypto'

const G = '0123456789abcdef0123456789abcdef'
const S = '20261007120000-0000000a'
const C = 'ab'.repeat(32)

/** A response that streams `total` bytes in 1 MiB pieces and records how far it was read. */
function streaming(total: number, declare = false) {
  const st = { pulled: 0, cancelled: false }
  const body = new ReadableStream<Uint8Array>({
    pull(c) {
      if (st.pulled >= total) return c.close()
      const n = Math.min(1 << 20, total - st.pulled)
      st.pulled += n
      c.enqueue(new Uint8Array(n))
    },
    cancel() { st.cancelled = true },
  })
  const headers: Record<string, string> = { 'Content-Type': 'application/octet-stream' }
  if (declare) headers['Content-Length'] = String(total)
  return { res: new Response(body, { status: 200, headers }), st }
}

afterEach(() => { vi.unstubAllGlobals() })

describe('raw repository reads are capped (W5)', () => {
  it('caps follow the format: 64 MiB chunk + framing, 256 MiB index, 16 MiB manifest', () => {
    expect(RAW_LIMITS.chunk).toBeGreaterThan(64 << 20)
    expect(RAW_LIMITS.chunk).toBeLessThan((64 << 20) + 65536)
    expect(RAW_LIMITS.index).toBe(256 << 20)
    expect(RAW_LIMITS.manifest).toBe(16 << 20)
  })

  it('stops reading an oversized manifest mid-stream and explains it', async () => {
    const { res, st } = streaming(64 << 20) // 64 MiB "manifest", no Content-Length
    const seen: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (u: string) => { seen.push(u); return res }))
    const err = await new ApiFetcher().manifest(G, S).catch((e) => e)
    expect(String(err.message)).toMatch(/at most 16777216 are accepted/)
    expect(st.cancelled).toBe(true)
    expect(st.pulled).toBeLessThanOrEqual((16 << 20) + (2 << 20)) // the cap plus what the stream machinery pre-pulls, not 64 MiB
    expect(seen[0]).toMatch(new RegExp(`/api/v1/snapshots/${G}/${S}/manifest`))
    expect(explainCryptoError(err).title).toBe('The server sent an oversized object')
  })

  it('refuses a declared oversized chunk before reading it', async () => {
    const { res, st } = streaming(RAW_LIMITS.chunk + 1, true)
    vi.stubGlobal('fetch', vi.fn(async () => res))
    await expect(new ApiFetcher().chunk(C)).rejects.toThrow(/are accepted/)
    expect(st.pulled).toBeLessThanOrEqual(1 << 20) // at most the stream's own pre-pull, never the body
    expect(st.cancelled).toBe(true)
  })

  it('a chunk within the cap arrives whole', async () => {
    const { res } = streaming(3 << 20)
    vi.stubGlobal('fetch', vi.fn(async () => res))
    expect((await new ApiFetcher().chunk(C)).byteLength).toBe(3 << 20)
  })
})
