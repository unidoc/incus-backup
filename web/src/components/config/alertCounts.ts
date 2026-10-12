// Counts of open alerts for the sidebar badge, kept current by the live 'alert' events (debounced),
// a stream reconnect, and a one-minute poll as a safety net. Shared by every NavBadge.
import { reactive, watch } from 'vue'
import { bus } from '@/api/events'
import { cfg, list, type Alert } from '@/api/sections/config'
import { session } from '@/stores/session'

export const alertCounts = reactive({
  /** Firing (not acknowledged, not silenced) critical and warning alerts: what needs a human. */
  critical: 0,
  warning: 0,
  acknowledged: 0,
  silenced: 0,
  loaded: false,
})

export function countAlerts(l: Pick<Alert, 'state' | 'severity'>[]) {
  const c = { critical: 0, warning: 0, acknowledged: 0, silenced: 0 }
  for (const a of l) {
    if (a.state === 'firing' && a.severity === 'critical') c.critical++
    else if (a.state === 'firing' && a.severity === 'warning') c.warning++
    else if (a.state === 'acknowledged') c.acknowledged++
    else if (a.state === 'silenced') c.silenced++
  }
  return c
}

let started = false
let timer: ReturnType<typeof setTimeout> | null = null

async function refresh() {
  if (session.status !== 'authenticated') return
  try {
    const l = list(await cfg.alerts.list({ active: 1, limit: 1000 }))
    Object.assign(alertCounts, countAlerts(l), { loaded: true })
  } catch { /* the badge is a hint; the alert page shows errors */ }
}

/** Refetch soon (several events in a burst cause one request). */
export function refreshAlertCounts(delay = 400) {
  if (timer) clearTimeout(timer)
  timer = setTimeout(() => { timer = null; void refresh() }, delay)
}

export function startAlertCounts() {
  if (started) return
  started = true
  bus.onVisible('alert', () => refreshAlertCounts())
  bus.onVisible('resync', () => refreshAlertCounts())
  setInterval(() => { if (document.visibilityState === 'visible') void refresh() }, 60_000)
  watch(() => session.status, (s) => {
    if (s === 'authenticated') void refresh()
    else Object.assign(alertCounts, { critical: 0, warning: 0, acknowledged: 0, silenced: 0, loaded: false })
  }, { immediate: true })
}
