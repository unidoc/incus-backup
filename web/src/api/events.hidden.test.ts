import { afterEach, describe, expect, it, vi } from 'vitest'
import { startLive, stopLive } from './events'

class FakeES {
  static made = 0
  static last: FakeES | null = null
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  constructor(public url: string) { FakeES.made++; FakeES.last = this }
  addEventListener() {}
  close() {}
}

afterEach(() => { stopLive(); vi.useRealTimers(); vi.restoreAllMocks() })

describe('event stream reconnect in a hidden tab', () => {
  it('does not reconnect (a session-renewing request) while the tab is hidden, and does once it is visible', () => {
    vi.useFakeTimers()
    let state: DocumentVisibilityState = 'visible'
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
    FakeES.made = 0
    startLive({ factory: (u) => new FakeES(u) as unknown as EventSource })
    FakeES.last!.onopen?.()
    state = 'hidden' // the user switches tabs, then the server restarts / a proxy drops the stream
    FakeES.last!.onerror?.()
    vi.advanceTimersByTime(60_000)
    const hidden = FakeES.made
    state = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    vi.advanceTimersByTime(0)
    expect(`connects while hidden=${hidden} | after visible=${FakeES.made}`).toBe('connects while hidden=1 | after visible=2')
  })

  it('stopLive cancels a reconnect that is waiting for the tab to become visible', () => {
    vi.useFakeTimers()
    let state: DocumentVisibilityState = 'visible'
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
    FakeES.made = 0
    startLive({ factory: (u) => new FakeES(u) as unknown as EventSource })
    state = 'hidden'
    FakeES.last!.onerror?.()
    vi.advanceTimersByTime(60_000)
    stopLive()
    state = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    expect(FakeES.made).toBe(1)
  })
})
