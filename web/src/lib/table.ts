// Table logic used by DataTable.vue: sorting, filtering, CSV and the virtual-scroll window.
// Pure functions so they are unit-tested (table.test.ts) and reusable by section code.

export interface ColumnDef<T> {
  key: string
  label: string
  /** Value used for sorting, filtering and CSV (default: row[key]). */
  value?: (row: T) => unknown
  /** Text shown in the cell when there is no #cell-<key> slot (default: String(value)). */
  format?: (row: T) => string
  sortable?: boolean
  align?: 'left' | 'right' | 'center'
  /** CSS width ("120px", "20%"). */
  width?: string
  /** Hidden by default (the user can show it with the column chooser). */
  hidden?: boolean
  /** Never hideable (identity column). */
  required?: boolean
  mono?: boolean
  /** Excluded from the CSV export. */
  noExport?: boolean
}

export type SortDir = 'asc' | 'desc'
export interface SortState { key: string; dir: SortDir }

export function cellValue<T>(row: T, col: ColumnDef<T>): unknown {
  return col.value ? col.value(row) : (row as Record<string, unknown>)[col.key]
}

const collator = new Intl.Collator('en', { numeric: true, sensitivity: 'base' })

/** Compare for sorting: null/undefined/'' last in both directions, numbers numerically, dates by time, strings naturally. */
export function compareValues(a: unknown, b: unknown): number {
  const ea = a === null || a === undefined || a === ''
  const eb = b === null || b === undefined || b === ''
  if (ea || eb) return ea === eb ? 0 : ea ? 1 : -1
  if (typeof a === 'number' && typeof b === 'number') return a - b
  if (typeof a === 'boolean' && typeof b === 'boolean') return a === b ? 0 : a ? -1 : 1
  if (a instanceof Date && b instanceof Date) return a.getTime() - b.getTime()
  return collator.compare(String(a), String(b))
}

export function sortRows<T>(rows: readonly T[], cols: ColumnDef<T>[], sort: SortState | null): T[] {
  const out = rows.slice()
  if (!sort) return out
  const col = cols.find((c) => c.key === sort.key)
  if (!col) return out
  const mul = sort.dir === 'asc' ? 1 : -1
  // stable: index tiebreak
  return out
    .map((r, i) => ({ r, i, v: cellValue(r, col) }))
    .sort((x, y) => {
      const ex = x.v === null || x.v === undefined || x.v === ''
      const ey = y.v === null || y.v === undefined || y.v === ''
      if (ex || ey) return ex === ey ? x.i - y.i : ex ? 1 : -1 // empties last regardless of direction
      return compareValues(x.v, y.v) * mul || x.i - y.i
    })
    .map((x) => x.r)
}

/** Next sort state when a header is clicked: asc -> desc -> none. */
export function nextSort(cur: SortState | null, key: string): SortState | null {
  if (!cur || cur.key !== key) return { key, dir: 'asc' }
  if (cur.dir === 'asc') return { key, dir: 'desc' }
  return null
}

function text(v: unknown): string {
  if (v === null || v === undefined) return ''
  if (v instanceof Date) return v.toISOString()
  if (Array.isArray(v)) return v.map(text).join(' ')
  return String(v)
}

/**
 * Filter by a query: whitespace-separated terms, all must match (case-insensitive substring of any
 * visible column). `col:term` restricts a term to the column with that key or label; `-term` excludes.
 */
export function filterRows<T>(rows: readonly T[], cols: ColumnDef<T>[], query: string): T[] {
  const terms = query.trim().toLowerCase().split(/\s+/).filter(Boolean)
  if (!terms.length) return rows.slice()
  const parsed = terms.map((t) => {
    const neg = t.startsWith('-') && t.length > 1
    const body = neg ? t.slice(1) : t
    const m = /^([a-z0-9_]+):(.+)$/.exec(body)
    const col = m ? cols.find((c) => c.key.toLowerCase() === m[1] || c.label.toLowerCase() === m[1]) : undefined
    return { neg, col, term: col && m ? m[2] : body }
  })
  return rows.filter((r) => parsed.every(({ neg, col, term }) => {
    const hit = col
      ? text(cellValue(r, col)).toLowerCase().includes(term) || (col.format ? col.format(r).toLowerCase().includes(term) : false)
      : cols.some((c) => text(cellValue(r, c)).toLowerCase().includes(term) || (c.format ? c.format(r).toLowerCase().includes(term) : false))
    return neg ? !hit : hit
  }))
}

/** RFC 4180 CSV with a header row; formulas are neutralised (a leading = + - @ gets a quote prefix). */
export function toCSV<T>(rows: readonly T[], cols: ColumnDef<T>[]): string {
  const esc = (v: unknown) => {
    let s = text(v)
    if (/^[=+\-@\t\r]/.test(s) && !/^-?\d+(\.\d+)?$/.test(s)) s = "'" + s
    return /[",\r\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s
  }
  const use = cols.filter((c) => !c.noExport)
  const lines = [use.map((c) => esc(c.label)).join(',')]
  for (const r of rows) lines.push(use.map((c) => esc(cellValue(r, c))).join(','))
  return lines.join('\r\n') + '\r\n'
}

export interface Window { start: number; end: number; padTop: number; padBottom: number }

/** Rows to render for a scroll position (fixed row height), with overscan on both sides. */
export function virtualWindow(scrollTop: number, viewport: number, rowHeight: number, total: number, overscan = 8): Window {
  if (total <= 0 || rowHeight <= 0) return { start: 0, end: 0, padTop: 0, padBottom: 0 }
  const first = Math.floor(Math.max(0, scrollTop) / rowHeight)
  const visible = Math.ceil(Math.max(viewport, rowHeight) / rowHeight)
  const start = Math.max(0, first - overscan)
  const end = Math.min(total, first + visible + overscan)
  return { start, end, padTop: start * rowHeight, padBottom: (total - end) * rowHeight }
}
