import { describe, expect, it } from 'vitest'
import { live, onSignedOut, startLive, stopLive } from './events'

class FakeES {
  static last: FakeES | null = null
  listeners = new Map<string, (m: MessageEvent) => void>()
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  closed = false
  constructor(public url: string) { FakeES.last = this }
  addEventListener(t: string, fn: (m: MessageEvent) => void) { this.listeners.set(t, fn) }
  close() { this.closed = true }
  emit(t: string) { this.listeners.get(t)?.({ data: '{}' } as MessageEvent) }
}

describe('signed_out from the server', () => {
  it('stops the stream (no reconnect into 401s) and calls the sign-out handler once', () => {
    let calls = 0
    onSignedOut(() => { calls++ })
    startLive({ factory: (u) => new FakeES(u) as unknown as EventSource })
    const es = FakeES.last!
    es.onopen!()
    expect(live.state).toBe('live')
    es.emit('signed_out')
    expect(calls).toBe(1)
    expect(es.closed).toBe(true)
    expect(live.state).toBe('idle')
    // the closed stream's late error does not reconnect
    es.onerror?.()
    expect(FakeES.last).toBe(es)
    stopLive()
  })
})
