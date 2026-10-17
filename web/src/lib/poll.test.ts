import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { usePoll, visibleTimeout } from './poll'

let state: DocumentVisibilityState
const setVisible = (v: DocumentVisibilityState) => { state = v; document.dispatchEvent(new Event('visibilitychange')) }

beforeEach(() => {
  vi.useFakeTimers()
  state = 'visible'
  vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
})
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

describe('usePoll', () => {
  it('ticks while visible', () => {
    const fn = vi.fn()
    const stop = usePoll(fn, 1000)
    vi.advanceTimersByTime(3000)
    expect(fn).toHaveBeenCalledTimes(3)
    stop()
  })

  it('hidden: no tick; visible again: exactly one catch-up', () => {
    const fn = vi.fn()
    const stop = usePoll(fn, 1000)
    state = 'hidden'
    vi.advanceTimersByTime(10_000)
    expect(fn).not.toHaveBeenCalled()
    setVisible('visible')
    expect(fn).toHaveBeenCalledTimes(1)
    setVisible('visible')
    expect(fn).toHaveBeenCalledTimes(1) // nothing more was missed
    stop()
  })

  it('no catch-up when no tick was missed, and stop() clears everything', () => {
    const fn = vi.fn()
    const stop = usePoll(fn, 1000)
    setVisible('hidden')
    setVisible('visible')
    expect(fn).not.toHaveBeenCalled()
    state = 'hidden'
    vi.advanceTimersByTime(1000)
    stop()
    setVisible('visible')
    vi.advanceTimersByTime(5000)
    expect(fn).not.toHaveBeenCalled()
  })
})

describe('visibleTimeout', () => {
  it('runs after the delay while visible', () => {
    const fn = vi.fn()
    visibleTimeout(fn, 1000)
    vi.advanceTimersByTime(1000)
    expect(fn).toHaveBeenCalledTimes(1)
  })

  it('hidden: waits, then runs once when visible', () => {
    const fn = vi.fn()
    visibleTimeout(fn, 1000)
    state = 'hidden'
    vi.advanceTimersByTime(60_000)
    expect(fn).not.toHaveBeenCalled()
    setVisible('visible')
    setVisible('visible')
    expect(fn).toHaveBeenCalledTimes(1)
  })

  it('cancel() prevents the call, hidden or not', () => {
    const fn = vi.fn()
    const c1 = visibleTimeout(fn, 1000)
    c1()
    vi.advanceTimersByTime(2000)
    const c2 = visibleTimeout(fn, 1000)
    state = 'hidden'
    vi.advanceTimersByTime(1000)
    c2()
    setVisible('visible')
    expect(fn).not.toHaveBeenCalled()
  })
})
