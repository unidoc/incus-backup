// One vocabulary for state colours: ok / warn / critical / info / neutral. Map every server state
// string through statusOf() so the same word always gets the same colour and icon.
import type { IconName } from '@/components/icons'

export type Status = 'ok' | 'warn' | 'critical' | 'info' | 'neutral'

const MAP: Record<string, Status> = {
  ok: 'ok', success: 'ok', succeeded: 'ok', online: 'ok', healthy: 'ok', verified: 'ok', protected: 'ok', active: 'ok', enabled: 'ok', valid: 'ok', live: 'ok', passed: 'ok', done: 'ok',
  warn: 'warn', warning: 'warn', partial: 'warn', deferred: 'warn', stale: 'warn', expiring: 'warn', 'read-only': 'warn', degraded: 'warn', overdue: 'warn', lagging: 'warn', unverified: 'warn', pending: 'warn',
  critical: 'critical', error: 'critical', failed: 'critical', failure: 'critical', corrupt: 'critical', offline: 'critical', expired: 'critical', disabled: 'neutral', unreachable: 'critical', never: 'critical',
  info: 'info', running: 'info', queued: 'info', scheduled: 'info', new: 'info', syncing: 'info',
  neutral: 'neutral', unknown: 'neutral', manual: 'neutral', cancelled: 'neutral', skipped: 'neutral', idle: 'neutral', none: 'neutral', unscheduled: 'neutral',
}

export function statusOf(state: string | null | undefined): Status {
  if (!state) return 'neutral'
  return MAP[state.toLowerCase()] ?? 'neutral'
}

export const statusIcon: Record<Status, IconName> = {
  ok: 'circle-check',
  warn: 'triangle-alert',
  critical: 'circle-alert',
  info: 'info',
  neutral: 'circle-dashed',
}

/** Worst of several statuses (critical > warn > info > neutral > ok) for roll-ups. */
const RANK: Record<Status, number> = { ok: 0, neutral: 1, info: 2, warn: 3, critical: 4 }
export function worst(...s: Status[]): Status {
  return s.reduce<Status>((a, b) => (RANK[b] > RANK[a] ? b : a), 'ok')
}
