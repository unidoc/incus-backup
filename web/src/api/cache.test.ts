import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'
import { clearCache, fetchCached, invalidate, useResource } from './cache'

describe('cache', () => {
  it('shares in-flight requests and honours the ttl', async () => {
    clearCache()
    const f = vi.fn(async () => 42)
    const [a, b] = await Promise.all([fetchCached('k', f, 1000), fetchCached('k', f, 1000)])
    expect([a, b]).toEqual([42, 42])
    expect(f).toHaveBeenCalledTimes(1)
    await fetchCached('k', f, 1000)
    expect(f).toHaveBeenCalledTimes(1)
    await fetchCached('k', f, 1000, true)
    expect(f).toHaveBeenCalledTimes(2)
  })

  it('serves stale data immediately and revalidates in the background', async () => {
    clearCache()
    let n = 0
    const fetcher = vi.fn(async () => ++n)
    const C = defineComponent({
      setup() {
        const r = useResource(() => 'list', fetcher, { ttl: 0 })
        return () => h('span', `${r.data.value ?? '-'}|${r.loading.value}|${r.refreshing.value}`)
      },
    })
    const w1 = mount(C)
    expect(w1.text()).toBe('-|true|false') // nothing cached: loading
    await flushPromises()
    expect(w1.text()).toBe('1|false|false')
    w1.unmount()
    const w2 = mount(C)
    expect(w2.text()).toBe('1|false|true') // cached value shown at once, refreshing in background
    await flushPromises()
    expect(w2.text()).toBe('2|false|false')
    invalidate('li')
    await flushPromises()
    await nextTick()
    expect(w2.text()).toBe('3|false|false')
    w2.unmount()
  })

  it('keeps the last data and exposes the error when a refresh fails', async () => {
    clearCache()
    let fail = false
    const C = defineComponent({
      setup() {
        const r = useResource(() => 'e', async () => { if (fail) throw new Error('offline'); return 'ok' }, { ttl: 0 })
        return () => h('span', `${r.data.value}|${r.error.value?.message ?? ''}`)
      },
    })
    const w = mount(C)
    await flushPromises()
    fail = true
    invalidate('e')
    await flushPromises()
    expect(w.text()).toBe('ok|offline')
  })
})
