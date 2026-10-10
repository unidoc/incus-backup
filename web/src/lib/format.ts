// Formatting helpers. Pure functions, unit-tested in format.test.ts. Use these everywhere instead of
// ad-hoc toFixed() so every number in the console reads the same way.

const IEC = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB']

/**
 * Bytes in binary units (what df, ZFS and Incus show): 1536 -> "1.5 KiB".
 * Three significant digits at most; integers below 1 KiB shown exactly.
 */
export function formatBytes(n: number | null | undefined, opts: { digits?: number; signed?: boolean } = {}): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '–'
  const sign = n < 0 ? '−' : opts.signed && n > 0 ? '+' : ''
  let v = Math.abs(n)
  if (v < 1024) return `${sign}${Math.round(v)} B`
  let i = 0
  while (v >= 1024 && i < IEC.length - 1) { v /= 1024; i++ }
  const digits = opts.digits ?? (v >= 100 ? 0 : v >= 10 ? 1 : 2)
  let s = v.toFixed(digits)
  if (s.includes('.')) s = s.replace(/\.?0+$/, '')
  if (Number(s) >= 1024 && i < IEC.length - 1) { s = '1'; i++ }
  return `${sign}${s} ${IEC[i]}`
}

/** Bytes per day etc.: "1.2 GiB/day". */
export function formatRate(bytesPerUnit: number | null | undefined, unit = 'day'): string {
  if (bytesPerUnit === null || bytesPerUnit === undefined || !Number.isFinite(bytesPerUnit)) return '–'
  return `${formatBytes(bytesPerUnit, { signed: true })}/${unit}`
}

/** 1234567 -> "1,234,567" (en-US grouping; the console copy is English). */
export function formatNumber(n: number | null | undefined, digits = 0): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '–'
  return n.toLocaleString('en-US', { maximumFractionDigits: digits, minimumFractionDigits: 0 })
}

/** 0.4567 -> "45.7 %" (ratio in, percent out). */
export function formatPercent(ratio: number | null | undefined, digits = 1): string {
  if (ratio === null || ratio === undefined || !Number.isFinite(ratio)) return '–'
  const p = ratio * 100
  const d = Math.abs(p) >= 100 || p === 0 ? 0 : digits
  return `${p.toFixed(d).replace(/\.0+$/, '')} %`
}

/** Dedup factor: 3.2 -> "3.2×". */
export function formatRatio(r: number | null | undefined): string {
  if (r === null || r === undefined || !Number.isFinite(r) || r <= 0) return '–'
  return `${r >= 10 ? r.toFixed(0) : r.toFixed(1)}×`
}

// ---- durations ---------------------------------------------------------------------------

const NS_PER_MS = 1e6

/** Go time.Duration (nanoseconds, as encoding/json writes it) to milliseconds. */
export const nsToMs = (ns: number) => ns / NS_PER_MS

/**
 * Milliseconds to a compact human duration with the two largest units: 0 -> "0 s", 950 -> "950 ms",
 * 61_000 -> "1 min 1 s", 3_600_000 -> "1 h", 90_061_000 -> "1 d 1 h".
 */
export function formatDuration(ms: number | null | undefined, opts: { units?: number } = {}): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return '–'
  const neg = ms < 0
  let v = Math.abs(ms)
  if (v < 1000) return `${neg ? '−' : ''}${Math.round(v)} ms`
  const parts: string[] = []
  const units: Array<[number, string]> = [[86_400_000, 'd'], [3_600_000, 'h'], [60_000, 'min'], [1000, 's']]
  for (const [size, label] of units) {
    if (v >= size) {
      const q = Math.floor(v / size)
      parts.push(`${q} ${label}`)
      v -= q * size
    } else if (parts.length) {
      parts.push('') // a skipped unit ends the run: "1 h" not "1 h 0 min 5 s"
    }
    if (parts.length >= (opts.units ?? 2)) break
  }
  return (neg ? '−' : '') + parts.filter(Boolean).join(' ')
}

/** Seconds variant. */
export const formatSeconds = (s: number | null | undefined) => formatDuration(s === null || s === undefined ? s : s * 1000)

// ---- time --------------------------------------------------------------------------------

export function toDate(t: string | number | Date | null | undefined): Date | null {
  if (t === null || t === undefined || t === '') return null
  const d = t instanceof Date ? t : new Date(t)
  if (Number.isNaN(d.getTime())) return null
  if (d.getUTCFullYear() <= 1) return null // Go's zero time "0001-01-01T00:00:00Z" means "never"
  return d
}

/**
 * "just now", "3 min ago", "in 2 h", "yesterday", "5 d ago"; beyond 30 days an absolute date.
 * `now` is injectable for tests and for the ticking RelativeTime component.
 */
export function formatRelative(t: string | number | Date | null | undefined, now: number = Date.now(), never = 'never'): string {
  const d = toDate(t)
  if (!d) return never
  const diff = d.getTime() - now
  const abs = Math.abs(diff)
  const future = diff > 0
  const wrap = (s: string) => (future ? `in ${s}` : `${s} ago`)
  if (abs < 45_000) return 'just now' // also absorbs small clock skew between browser and server
  if (abs < 3_600_000) return wrap(`${Math.max(1, Math.round(abs / 60_000))} min`)
  if (abs < 86_400_000) return wrap(`${Math.round(abs / 3_600_000)} h`)
  const days = Math.round(abs / 86_400_000)
  if (days === 1) return future ? 'tomorrow' : 'yesterday'
  if (days <= 30) return wrap(`${days} d`)
  return formatDate(d)
}

const pad = (n: number) => String(n).padStart(2, '0')

/** "2026-10-08" in the browser's time zone (ISO order: unambiguous for operators everywhere). */
export function formatDate(t: string | number | Date | null | undefined, tz?: string): string {
  const d = toDate(t)
  if (!d) return '–'
  const p = parts(d, tz)
  return `${p.year}-${p.month}-${p.day}`
}

/** "2026-10-08 14:03" (or with seconds). */
export function formatDateTime(t: string | number | Date | null | undefined, opts: { seconds?: boolean; tz?: string } = {}): string {
  const d = toDate(t)
  if (!d) return '–'
  const p = parts(d, opts.tz)
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}${opts.seconds ? ':' + p.second : ''}`
}

function parts(d: Date, tz?: string) {
  if (!tz) {
    return { year: String(d.getFullYear()), month: pad(d.getMonth() + 1), day: pad(d.getDate()), hour: pad(d.getHours()), minute: pad(d.getMinutes()), second: pad(d.getSeconds()) }
  }
  const f = new Intl.DateTimeFormat('en-CA', { timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' })
  const o: Record<string, string> = {}
  for (const x of f.formatToParts(d)) o[x.type] = x.value
  return { year: o.year, month: o.month, day: o.day, hour: o.hour, minute: o.minute, second: o.second }
}

/** The browser's IANA zone, e.g. "Atlantic/Reykjavik". */
export function localZone(): string {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'local time' } catch { return 'local time' }
}

// ---- schedules (internal/schedule) ---------------------------------------------------------

const DAYS: Record<string, string> = { sun: 'Sunday', mon: 'Monday', tue: 'Tuesday', wed: 'Wednesday', thu: 'Thursday', fri: 'Friday', sat: 'Saturday' }

export interface ParsedSchedule {
  kind: 'manual' | 'every' | 'daily' | 'weekly' | 'monthly'
  /** Interval in ms for kind 'every'. */
  everyMs?: number
  hour?: number
  minute?: number
  weekday?: string
}

/** Parse Go's time.ParseDuration subset used by "every …" (h, m, s, ms; decimals; sequences "1h30m"). */
export function parseGoDuration(s: string): number | null {
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gy
  const mult: Record<string, number> = { ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 }
  let total = 0
  let matched = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(s)) !== null) { total += parseFloat(m[1]) * mult[m[2]]; matched = re.lastIndex }
  if (matched === 0 || matched !== s.length) return s === '0' ? 0 : null
  return total
}

/**
 * Parse a schedule exactly like internal/schedule.Parse. Returns an Error with the server's wording
 * for invalid input (so a form can show it before saving).
 */
export function parseSchedule(input: string | null | undefined): ParsedSchedule | Error {
  const s = (input ?? '').trim().toLowerCase()
  switch (s) {
    case '': case 'manual': case 'never': return { kind: 'manual' }
    case 'hourly': return { kind: 'every', everyMs: 3_600_000 }
    case 'daily': return { kind: 'daily', hour: 3, minute: 0 }
    case 'weekly': return { kind: 'weekly', hour: 3, minute: 0, weekday: 'sun' }
    case 'monthly': return { kind: 'monthly', hour: 3, minute: 0 }
  }
  if (s.startsWith('every ')) {
    const d = parseGoDuration(s.slice(6).trim())
    if (d === null || d < 60_000 || d > 86_400_000) return new Error(`invalid interval "${s}" (use 1m..24h, e.g. "every 4h")`)
    return { kind: 'every', everyMs: d }
  }
  let fields = s.split(/\s+/)
  let weekday: string | undefined
  if (fields.length === 2) {
    if (!(fields[0] in DAYS)) return new Error(`invalid weekday "${fields[0]}"`)
    weekday = fields[0]
    fields = fields.slice(1)
  }
  if (fields.length !== 1) return new Error(`invalid schedule "${s}"`)
  const hm = fields[0].split(':')
  if (hm.length !== 2) return new Error(`invalid schedule "${s}" (want HH:MM)`)
  const h = /^[+-]?\d+$/.test(hm[0]) ? parseInt(hm[0], 10) : NaN
  const mi = /^[+-]?\d+$/.test(hm[1]) ? parseInt(hm[1], 10) : NaN
  if (Number.isNaN(h) || Number.isNaN(mi) || h < 0 || h > 23 || mi < 0 || mi > 59) return new Error(`invalid time in "${s}"`)
  return weekday ? { kind: 'weekly', hour: h, minute: mi, weekday } : { kind: 'daily', hour: h, minute: mi }
}

/**
 * Plain-English description of a schedule: "Every day at 03:00", "Every 4 hours", "Mondays at 02:30",
 * "Manual only". `tz` (the policy's time zone) is appended when given.
 */
export function describeSchedule(input: string | null | undefined, tz?: string): string {
  const p = parseSchedule(input)
  if (p instanceof Error) return `Invalid schedule: ${p.message}`
  const at = (h = 0, m = 0) => `${pad(h)}:${pad(m)}`
  const zone = tz ? ` (${tz})` : ''
  switch (p.kind) {
    case 'manual': return 'Manual only'
    case 'every': {
      const ms = p.everyMs!
      if (ms === 3_600_000) return 'Every hour, on the hour'
      if (ms % 3_600_000 === 0) return `Every ${ms / 3_600_000} hours from midnight`
      if (ms % 60_000 === 0 && ms < 3_600_000) return `Every ${ms / 60_000} minutes`
      return `Every ${formatDuration(ms)} from midnight`
    }
    case 'daily': return `Every day at ${at(p.hour, p.minute)}${zone}`
    case 'weekly': return `${DAYS[p.weekday!]}s at ${at(p.hour, p.minute)}${zone}`
    case 'monthly': return `1st of every month at ${at(p.hour, p.minute)}${zone}`
  }
}

/**
 * Next firing strictly after `from`, in the browser's time zone (mirrors schedule.Spec.Next).
 * Returns null for manual schedules.
 */
export function nextRun(input: string, from: Date = new Date()): Date | null {
  const p = parseSchedule(input)
  if (p instanceof Error || p.kind === 'manual') return null
  if (p.kind === 'every') {
    const day = new Date(from.getFullYear(), from.getMonth(), from.getDate())
    const n = Math.floor((from.getTime() - day.getTime()) / p.everyMs!) + 1
    const next = new Date(day.getTime() + n * p.everyMs!)
    if (next.getDate() !== day.getDate() && p.everyMs! < 86_400_000) return new Date(from.getFullYear(), from.getMonth(), from.getDate() + 1)
    return next
  }
  const wd = p.weekday ? Object.keys(DAYS).indexOf(p.weekday) : -1
  for (let i = 0; i < 400; i++) {
    const c = new Date(from.getFullYear(), from.getMonth(), from.getDate() + i, p.hour, p.minute)
    if (c > from && (wd < 0 || c.getDay() === wd) && (p.kind !== 'monthly' || c.getDate() === 1)) return c
  }
  return null
}

// ---- misc --------------------------------------------------------------------------------

/** "1 object" / "3 objects". */
export function plural(n: number, one: string, many = one + 's'): string {
  return `${formatNumber(n)} ${n === 1 ? one : many}`
}

/** Shorten a hex id for display: "0123456789abcdef…" -> "01234567". */
export const shortId = (id: string | null | undefined, n = 8) => (id ? id.slice(0, n) : '–')
