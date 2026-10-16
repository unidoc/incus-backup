import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { bus } from '@/api/events'
import { opsApi, type TaskDetail } from '@/api/sections/ops'
import TaskPanel from './TaskPanel.vue'

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

const running = { id: 't1', kind: 'verify', state: 'running', progress: 10, created: '2026-10-16T10:00:00Z', duration_ms: 1000, log: [], next_seq: 0 } as unknown as TaskDetail

describe('TaskPanel', () => {
  it('replays the events queued in a hidden tab as one poll, not one per event', async () => {
    vi.useFakeTimers()
    let state: DocumentVisibilityState = 'visible'
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
    const get = vi.spyOn(opsApi.tasks, 'get').mockResolvedValue(running)
    const w = mount(TaskPanel, { props: { id: 't1' } })
    await flushPromises()
    expect(get).toHaveBeenCalledTimes(1)
    state = 'hidden'
    for (let i = 0; i < 50; i++) bus.emit({ type: 'task', data: { id: 't1', progress: i } })
    state = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(300) // past the 250 ms debounce, before the 1 s poll timer
    expect(get).toHaveBeenCalledTimes(2) // 1 initial + exactly one for the 50 replayed events
    w.unmount()
  })
})
