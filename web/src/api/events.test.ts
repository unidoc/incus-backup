import { describe, expect, it, vi } from 'vitest'
import { EventBus, backoff, bus, live, startLive, stopLive } from './events'

class FakeES {
  static last: FakeES | null = null
  listeners = new Map<string, (m: MessageEvent) => void>()
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  closed = false
  constructor(public url: string) { FakeES.last = this }
  addEventListener(t: string, fn: (m: MessageEvent) => void) { this.listeners.set(t, fn) }
  close() { this.closed = true }
  emit(t: string, data: unknown) { this.listeners.get(t)?.({ data: JSON.stringify(data) } as MessageEvent) }
}

describe('live events', () => {
  it('backs off exponentially with jitter, capped at 30 s', () => {
    expect(backoff(1, () => 0.5)).toBe(1000)
    expect(backoff(2, () => 0.5)).toBe(2000)
    expect(backoff(4, () => 0.5)).toBe(8000)
    expect(backoff(10, () => 0.5)).toBe(30000)
    expect(backoff(1, () => 0)).toBe(800)
    expect(backoff(1, () => 1)).toBe(1200)
  })

  it('routes typed events and isolates failing handlers', () => {
    const b = new EventBus()
    const got: unknown[] = []
    b.on('job', () => { throw new Error('boom') })
    const off = b.on('job', (d) => got.push(d))
    b.on('*', (d, e) => got.push(`*${e.type}`))
    vi.spyOn(console, 'error').mockImplementation(() => {})
    b.emit({ type: 'job', data: { id: 1 } })
    off()
    b.emit({ type: 'job', data: { id: 2 } })
    expect(got).toEqual([{ id: 1 }, '*job', '*job'])
  })

  it('onVisible holds events while the tab is hidden and delivers one when it is visible again', () => {
    let state: DocumentVisibilityState = 'hidden'
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
    const b = new EventBus()
    const got: unknown[] = []
    const off = b.onVisible('job', (d) => got.push(d))
    b.emit({ type: 'job', data: 1 })
    b.emit({ type: 'job', data: 2 })
    expect(got).toEqual([]) // hidden: no refetch, so no activity for the server
    state = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    expect(got).toEqual([2]) // one catch-up with the latest
    document.dispatchEvent(new Event('visibilitychange'))
    expect(got).toEqual([2])
    b.emit({ type: 'job', data: 3 })
    expect(got).toEqual([2, 3]) // visible: immediate
    off()
    state = 'hidden'
    b.emit({ type: 'job', data: 4 })
    state = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    expect(got).toEqual([2, 3]) // unsubscribed
    vi.restoreAllMocks()
  })

  it('parses the server envelope, reconnects with backoff and emits resync after a reconnect', async () => {
    vi.useFakeTimers()
    const seen: unknown[] = []
    const offA = bus.on('repository', (d) => seen.push(d))
    const offR = bus.on('resync', () => seen.push('resync'))
    startLive({ factory: (u) => new FakeES(u) as unknown as EventSource })
    const first = FakeES.last!
    expect(first.url).toBe('/api/v1/events')
    expect(live.state).toBe('connecting')
    first.onopen!()
    expect(live.state).toBe('live')
    first.emit('repository', { type: 'repository', data: { name: 'custody', mode: 'read-only' } })
    expect(seen).toEqual([{ name: 'custody', mode: 'read-only' }])
    first.onerror!()
    expect(first.closed).toBe(true)
    expect(live.state).toBe('reconnecting')
    await vi.advanceTimersByTimeAsync(1300)
    const second = FakeES.last!
    expect(second).not.toBe(first)
    second.onopen!()
    expect(seen).toContain('resync')
    expect(live.failures).toBe(0)
    stopLive()
    expect(live.state).toBe('idle')
    offA(); offR()
    vi.useRealTimers()
  })

  it('checks the session after repeated failures and stops when signed out', async () => {
    vi.useFakeTimers()
    const check = vi.fn(async () => false)
    startLive({ factory: (u) => new FakeES(u) as unknown as EventSource, checkSession: check })
    for (let i = 0; i < 3; i++) { FakeES.last!.onerror!(); await vi.advanceTimersByTimeAsync(40_000) }
    expect(check).toHaveBeenCalled()
    expect(live.state).toBe('idle')
    vi.useRealTimers()
  })
})
