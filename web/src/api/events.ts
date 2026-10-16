// Live updates: one EventSource on /api/v1/events for the whole app, a typed event bus on top.
//
// The server sends NAMED events (`event: job`), each `data:` line is the envelope {type, data}.
// EventSource reconnects by itself but gives no status and no backoff control, so on an error we
// close it and reconnect ourselves with exponential backoff and jitter. After a few failures we ask
// GET /session first: an expired session shows up there as a 401 (EventSource cannot see status codes),
// which the API client turns into the sign-in redirect.
//
// After every reconnect the bus emits 'resync': views refetch, because events sent while we were
// away are lost (the server does not replay).
import { reactive } from 'vue'
import { API_BASE } from './client'

export const EVENT_TYPES = ['job', 'alert', 'run', 'repository', 'host', 'task'] as const
export type LiveEventType = (typeof EVENT_TYPES)[number]
export interface LiveEvent<T = unknown> { type: LiveEventType; data: T }

type Handler = (data: unknown, ev: LiveEvent) => void
type BusKey = LiveEventType | 'resync' | '*'

/** Most events one onVisible() subscription keeps while the tab is hidden. */
const HIDDEN_QUEUE_MAX = 50
function safeKey(ev: LiveEvent): string | null {
  try { return JSON.stringify([ev.type, ev.data]) } catch { return null }
}

/** A tiny typed event bus. on() returns the unsubscribe function. */
export class EventBus {
  private handlers = new Map<BusKey, Set<Handler>>()
  on(type: BusKey, fn: Handler): () => void {
    let set = this.handlers.get(type)
    if (!set) this.handlers.set(type, (set = new Set()))
    set.add(fn)
    return () => set!.delete(fn)
  }
  /**
   * on(), but handlers run only while the tab is visible. Events that arrive in a hidden tab are queued
   * (per subscription, at most HIDDEN_QUEUE_MAX, oldest dropped, an event identical to one already queued
   * is not queued twice) and replayed in order when the tab becomes visible again, so handlers that filter
   * by id still see their own event even if another one arrived after it.
   * Refetching on an event is a request like any other, and the server counts it as activity: a hidden
   * tab must not keep its session alive (docs/authentication.md, "What counts as activity").
   */
  onVisible(type: BusKey, fn: Handler): () => void {
    let pending: [unknown, LiveEvent][] = []
    const run = (d: unknown, ev: LiveEvent) => {
      if (typeof document === 'undefined' || document.visibilityState === 'visible') { fn(d, ev); return }
      const key = safeKey(ev)
      if (key !== null && pending.some(([, q]) => safeKey(q) === key)) return
      pending.push([d, ev])
      if (pending.length > HIDDEN_QUEUE_MAX) pending.shift()
    }
    const onVis = () => {
      if (document.visibilityState !== 'visible' || !pending.length) return
      const batch = pending
      pending = []
      for (const [d, ev] of batch) {
        try { fn(d, ev) } catch (e) { console.error('event handler failed', e) }
      }
    }
    if (typeof document !== 'undefined') document.addEventListener('visibilitychange', onVis)
    const off = this.on(type, run)
    return () => {
      off()
      pending = []
      if (typeof document !== 'undefined') document.removeEventListener('visibilitychange', onVis)
    }
  }
  emit(ev: LiveEvent | { type: 'resync'; data: null }) {
    for (const key of [ev.type, '*'] as BusKey[]) {
      for (const fn of this.handlers.get(key) ?? []) {
        try { fn(ev.data, ev as LiveEvent) } catch (e) { console.error('event handler failed', e) }
      }
    }
  }
}

export const bus = new EventBus()

export type LiveState = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'offline'
export const live = reactive({
  state: 'idle' as LiveState,
  /** When the stream last delivered something (event or open). */
  lastEventAt: 0,
  /** Consecutive failures since the last successful open. */
  failures: 0,
  /** Milliseconds until the next attempt (when reconnecting). */
  retryIn: 0,
})

/** Delay before reconnect attempt n (1-based): 1 s, 2 s, 4 s ... capped at 30 s, ±20 % jitter. */
export function backoff(n: number, rand: () => number = Math.random): number {
  const base = Math.min(30_000, 1000 * 2 ** Math.max(0, n - 1))
  return Math.round(base * (0.8 + 0.4 * rand()))
}

export interface LiveOptions {
  /** Called before reconnecting after >= 3 failures; resolve false to stop (signed out). */
  checkSession?: () => Promise<boolean>
  factory?: (url: string) => EventSource
}

// The server ends the stream with `event: signed_out` when the session or token behind it stopped
// being valid (logout elsewhere, revocation, a disabled account). Reconnecting would only get 401s:
// the stream stops and the registered handler sends the user to sign-in.
let signedOutHandler: () => void = () => {}
export function onSignedOut(fn: () => void) { signedOutHandler = fn }

let es: EventSource | null = null
let timer: ReturnType<typeof setTimeout> | null = null
let everOpened = false
let stopped = true
let opts: LiveOptions = {}

export function startLive(o: LiveOptions = {}) {
  opts = o
  stopped = false
  if (es) return
  connect()
}

export function stopLive() {
  stopped = true
  if (timer) clearTimeout(timer)
  timer = null
  es?.close()
  es = null
  everOpened = false
  live.state = 'idle'
  live.failures = 0
}

function connect() {
  if (stopped) return
  live.state = live.failures ? 'reconnecting' : 'connecting'
  const make = opts.factory ?? ((u: string) => new EventSource(u, { withCredentials: true }))
  const src = make(API_BASE + '/events')
  es = src
  src.onopen = () => {
    const wasReconnect = everOpened
    everOpened = true
    live.state = 'live'
    live.failures = 0
    live.retryIn = 0
    live.lastEventAt = Date.now()
    if (wasReconnect) bus.emit({ type: 'resync', data: null })
  }
  for (const t of EVENT_TYPES) {
    src.addEventListener(t, (m: MessageEvent) => {
      live.lastEventAt = Date.now()
      let env: LiveEvent
      try {
        const parsed = JSON.parse(m.data)
        env = parsed && typeof parsed === 'object' && 'type' in parsed ? parsed : { type: t, data: parsed }
      } catch {
        return
      }
      bus.emit(env)
    })
  }
  src.addEventListener('signed_out', () => {
    if (es !== src) return
    stopLive()
    signedOutHandler()
  })
  src.onerror = () => {
    src.close()
    if (es === src) es = null
    if (stopped) return
    live.failures++
    live.state = live.failures >= 3 ? 'offline' : 'reconnecting'
    const delay = backoff(live.failures)
    live.retryIn = delay
    timer = setTimeout(async () => {
      timer = null
      if (live.failures >= 3 && opts.checkSession) {
        const ok = await opts.checkSession().catch(() => true) // network error: keep trying
        if (!ok) return stopLive()
      }
      connect()
    }, delay)
  }
}

/** Reconnect immediately (the user clicked the connection indicator). */
export function reconnectNow() {
  if (stopped) return
  if (timer) clearTimeout(timer)
  timer = null
  es?.close()
  es = null
  connect()
}
