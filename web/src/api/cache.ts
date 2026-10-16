// Stale-while-revalidate for list views.
//
//   const hosts = useResource(() => `hosts:${repo.current}`, (signal) => api.hosts.list(signal), { refreshOn: ['host'] })
//   hosts.data / hosts.error / hosts.loading (first load) / hosts.refreshing (background) / hosts.refresh()
//
// A cached value is shown immediately when the view opens again; it is refetched in the background
// when older than `ttl`. Identical requests in flight are shared. Live events of the listed types and
// a stream reconnect ('resync') trigger a refresh (debounced). invalidate(prefix) drops entries after
// a mutation so the next read fetches.
import { usePoll } from '@/lib/poll'
import { onBeforeUnmount, shallowRef, watch, type ShallowRef } from 'vue'
import { bus, type LiveEventType } from './events'
import { isAbort } from './client'

interface Entry<T> { data?: T; at: number; inflight?: Promise<T>; controller?: AbortController }
const store = new Map<string, Entry<unknown>>()
const listeners = new Map<string, Set<() => void>>()

export function invalidate(prefix = '') {
  for (const k of [...store.keys()]) {
    if (k.startsWith(prefix)) {
      store.get(k)!.at = 0
      listeners.get(k)?.forEach((fn) => fn())
    }
  }
}

/** Drop everything (sign-out, repository switch). */
export function clearCache() { store.clear() }

export function cached<T>(key: string): T | undefined { return store.get(key)?.data as T | undefined }

/** Fetch through the cache; shares an in-flight request for the same key. */
export function fetchCached<T>(key: string, fetcher: (signal: AbortSignal) => Promise<T>, ttl = 15_000, force = false): Promise<T> {
  let e = store.get(key) as Entry<T> | undefined
  if (!e) store.set(key, (e = { at: 0 }))
  if (e.inflight) return e.inflight
  if (!force && e.data !== undefined && Date.now() - e.at < ttl) return Promise.resolve(e.data)
  const controller = new AbortController()
  const entry = e
  entry.controller = controller
  entry.inflight = fetcher(controller.signal).then(
    (d) => { entry.data = d; entry.at = Date.now(); entry.inflight = undefined; return d },
    (err) => { entry.inflight = undefined; throw err },
  )
  return entry.inflight
}

export interface Resource<T> {
  data: ShallowRef<T | undefined>
  error: ShallowRef<Error | null>
  /** True only while there is nothing to show yet. */
  loading: ShallowRef<boolean>
  /** True while a background refresh runs (show a subtle indicator, keep the data). */
  refreshing: ShallowRef<boolean>
  updatedAt: ShallowRef<number>
  refresh: () => Promise<void>
}

export interface ResourceOptions {
  ttl?: number
  refreshOn?: LiveEventType[]
  /** Poll interval in ms while mounted (0 = none). Prefer refreshOn. */
  poll?: number
  /** Return false to skip fetching (e.g. no repository selected yet). */
  enabled?: () => boolean
}

export function useResource<T>(key: () => string, fetcher: (signal: AbortSignal) => Promise<T>, o: ResourceOptions = {}): Resource<T> {
  const ttl = o.ttl ?? 15_000
  const data = shallowRef<T | undefined>()
  const error = shallowRef<Error | null>(null)
  const loading = shallowRef(false)
  const refreshing = shallowRef(false)
  const updatedAt = shallowRef(0)
  let current = ''
  let alive = true

  async function load(force: boolean) {
    if (o.enabled && !o.enabled()) return
    const k = key()
    current = k
    const c = cached<T>(k)
    if (c !== undefined) { data.value = c; updatedAt.value = store.get(k)!.at }
    loading.value = data.value === undefined
    refreshing.value = !loading.value
    try {
      const d = await fetchCached(k, fetcher, ttl, force)
      if (!alive || current !== k) return
      data.value = d
      error.value = null
      updatedAt.value = store.get(k)?.at ?? Date.now()
    } catch (e) {
      if (!alive || current !== k || isAbort(e)) return
      error.value = e instanceof Error ? e : new Error(String(e))
    } finally {
      if (alive && current === k) { loading.value = false; refreshing.value = false }
    }
  }

  let debounce: ReturnType<typeof setTimeout> | null = null
  const soon = () => {
    if (debounce) clearTimeout(debounce)
    debounce = setTimeout(() => { debounce = null; void load(true) }, 250)
  }
  const offs: Array<() => void> = []
  for (const t of o.refreshOn ?? []) offs.push(bus.onVisible(t, soon)) // not while hidden: a refetch would renew the session
  offs.push(bus.onVisible('resync', soon))
  const pollTimer = o.poll ? usePoll(() => void load(true), o.poll) : null

  const onInvalidate = () => void load(true)
  watch(key, (k, old) => {
    if (old) listeners.get(old)?.delete(onInvalidate)
    if (!listeners.has(k)) listeners.set(k, new Set())
    listeners.get(k)!.add(onInvalidate)
    if (old !== undefined && k !== old) data.value = cached<T>(k)
    void load(false)
  }, { immediate: true })

  onBeforeUnmount(() => {
    alive = false
    offs.forEach((f) => f())
    listeners.get(current)?.delete(onInvalidate)
    pollTimer?.()
    if (debounce) clearTimeout(debounce)
  })

  return { data, error, loading, refreshing, updatedAt, refresh: () => load(true) }
}
