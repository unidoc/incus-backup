// Retention, as the server applies it: a TypeScript port of internal/retention.Plan (PBS semantics),
// used to preview "what this policy keeps" before saving. retention.test.ts checks it against vectors
// computed by the Go code (retention.vectors.json; internal/retention/webvectors_test.go keeps them honest).
//
//  * Rules run in the order last, hourly, daily, weekly, monthly, yearly; each keeps the newest snapshot
//    of each of the N most recent distinct periods.
//  * Rules are cumulative: a period an earlier rule already kept does not use up a later rule's slot.
//  * When a rule keeps the newest snapshot of a period, the older snapshots of that period it walks
//    past are removed for good: later rules skip them (PBS PruneMark::Remove). See docs/retention.md.
//  * keep-last counts snapshots, not instants: two snapshots with the same time are two.
//  * Periods are calendar periods in the policy's time zone (daily = local day, weekly = ISO week).
//  * Protected snapshots are never removed and use no slot; the newest snapshot is always kept;
//    no rule at all keeps everything.

export interface Rules { last: number; hourly: number; daily: number; weekly: number; monthly: number; yearly: number }
export interface RSnapshot { id: string; time: number /* ms */; protected?: boolean }
export interface Decision { id: string; time: number; protected: boolean; keep: boolean; reason: string }

export const RULE_ORDER = ['last', 'hourly', 'daily', 'weekly', 'monthly', 'yearly'] as const
export type RuleName = (typeof RULE_ORDER)[number]

export function rulesOf(p: { keep_last: number; keep_hourly: number; keep_daily: number; keep_weekly: number; keep_monthly: number; keep_yearly: number }): Rules {
  return { last: p.keep_last || 0, hourly: p.keep_hourly || 0, daily: p.keep_daily || 0, weekly: p.keep_weekly || 0, monthly: p.keep_monthly || 0, yearly: p.keep_yearly || 0 }
}

export const anyRule = (r: Rules) => RULE_ORDER.some((k) => r[k] > 0)

const fmtCache = new Map<string, Intl.DateTimeFormat>()
function fmt(tz: string): Intl.DateTimeFormat {
  let f = fmtCache.get(tz)
  if (!f) {
    f = new Intl.DateTimeFormat('en-CA', { timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', hourCycle: 'h23' })
    fmtCache.set(tz, f)
  }
  return f
}

/** Calendar parts of an instant in a zone. */
export function zoned(ms: number, tz: string): { y: number; m: number; d: number; h: number } {
  const o: Record<string, string> = {}
  for (const p of fmt(tz).formatToParts(new Date(ms))) o[p.type] = p.value
  return { y: +o.year, m: +o.month, d: +o.day, h: +o.hour % 24 }
}

/** ISO 8601 week-numbering year and week of a calendar date (as Go's time.ISOWeek). */
export function isoWeek(y: number, m: number, d: number): [number, number] {
  const t = new Date(Date.UTC(y, m - 1, d))
  const dow = t.getUTCDay() || 7 // Mon=1..Sun=7
  t.setUTCDate(t.getUTCDate() + 4 - dow) // the Thursday of this week decides the year
  const wy = t.getUTCFullYear()
  const jan1 = Date.UTC(wy, 0, 1)
  return [wy, Math.floor((t.getTime() - jan1) / 86_400_000 / 7) + 1]
}

function periodKey(rule: RuleName, ms: number, tz: string): string {
  if (rule === 'last') return String(ms) // unused by plan(): keep-last is keyed by snapshot id there
  const z = zoned(ms, tz)
  switch (rule) {
    case 'hourly': return `${z.y}-${z.m}-${z.d}-${z.h}`
    case 'daily': return `${z.y}-${z.m}-${z.d}`
    case 'weekly': { const [wy, w] = isoWeek(z.y, z.m, z.d); return `${wy}-W${w}` }
    case 'monthly': return `${z.y}-${z.m}`
    case 'yearly': return `${z.y}`
  }
}

/** A decision for every snapshot, newest first (internal/retention.Plan). `tz` is the policy's zone. */
export function plan(snaps: readonly RSnapshot[], r: Rules, tz = 'UTC'): Decision[] {
  const s = [...snaps].sort((a, b) => (a.time !== b.time ? b.time - a.time : a.id < b.id ? 1 : a.id > b.id ? -1 : 0))
  const out: Decision[] = s.map((x) => ({ id: x.id, time: x.time, protected: !!x.protected, keep: false, reason: '' }))
  const any = anyRule(r)
  s.forEach((x, i) => {
    if (x.protected) { out[i].keep = true; out[i].reason = 'protected' } else if (!any) { out[i].keep = true; out[i].reason = 'retention disabled' }
  })
  if (!s.length || !any) return out
  const mark = (i: number, why: string) => {
    if (s[i].protected) return
    out[i].reason = out[i].reason ? `${out[i].reason},${why}` : why
    out[i].keep = true
  }
  const kept = new Array<boolean>(s.length).fill(false)
  // older snapshots of a period a rule already kept: every later rule skips them
  const removed = new Array<boolean>(s.length).fill(false)
  const keys = new Map<RuleName, string[]>()
  const keyOf = (rule: RuleName, i: number) => {
    let k = keys.get(rule)
    // keep-last: every snapshot is its own period (PBS keys it by the snapshot name)
    if (!k) { k = s.map((x) => (rule === 'last' ? `\u0000${x.id}` : periodKey(rule, x.time, tz))); keys.set(rule, k) }
    return k[i]
  }
  for (const rule of RULE_ORDER) {
    const n = r[rule]
    if (n <= 0) continue
    const already = new Set<string>()
    for (let i = 0; i < s.length; i++) if (kept[i]) already.add(keyOf(rule, i))
    const used = new Set<string>()
    const newly: number[] = []
    for (let i = 0; i < s.length; i++) {
      if (s[i].protected || kept[i] || removed[i]) continue
      const k = keyOf(rule, i)
      if (already.has(k)) continue
      if (used.has(k)) { removed[i] = true; continue }
      if (used.size >= n) break
      used.add(k)
      newly.push(i)
    }
    for (const i of newly) { mark(i, rule); kept[i] = true }
  }
  for (let i = 0; i < s.length; i++) {
    if (!s[i].protected) { if (!out[i].keep) mark(i, 'newest'); break }
  }
  return out
}

/** "7 daily, 4 weekly and 12 monthly" (or "the last 3" first); "" when no rule is set. */
export function describeRules(r: Rules): string {
  const parts: string[] = []
  if (r.last) parts.push(`the last ${r.last}`)
  for (const k of ['hourly', 'daily', 'weekly', 'monthly', 'yearly'] as const) if (r[k]) parts.push(`${r[k]} ${k}`)
  if (!parts.length) return ''
  return parts.length === 1 ? parts[0] : `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`
}

/** One sentence for cards and lists: "Keeps 7 daily, 4 weekly and 12 monthly." */
export function retentionSummary(r: Rules): string {
  const d = describeRules(r)
  return d ? `Keeps ${d}.` : 'Keeps everything (no retention rule).'
}

export interface Simulation {
  /** Hypothetical snapshot times (ms), newest first, with the decision. */
  decisions: Decision[]
  kept: number
  /** Age in ms of the oldest kept snapshot (how far back you can restore). */
  reach: number
  /** Kept snapshots by the rule that kept them first. */
  byRule: Record<string, number>
}

/**
 * What a policy keeps after running for `days` days with one snapshot every `intervalMs`, the newest
 * taken at `now`. A preview only: real snapshots come when hosts run, and retention uses the
 * server's commit time.
 */
export function simulate(r: Rules, tz: string, intervalMs: number, now: number, days: number): Simulation {
  const snaps: RSnapshot[] = []
  const n = Math.min(20_000, Math.floor((days * 86_400_000) / Math.max(60_000, intervalMs)) + 1)
  for (let i = 0; i < n; i++) snaps.push({ id: `s${String(n - i).padStart(6, '0')}`, time: now - i * intervalMs })
  const decisions = plan(snaps, r, tz)
  const keptD = decisions.filter((d) => d.keep)
  const byRule: Record<string, number> = {}
  for (const d of keptD) {
    const first = d.reason.split(',')[0]
    byRule[first] = (byRule[first] ?? 0) + 1
  }
  return { decisions, kept: keptD.length, reach: keptD.length ? now - keptD[keptD.length - 1].time : 0, byRule }
}
