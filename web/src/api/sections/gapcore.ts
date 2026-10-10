// Repository access (per-repository roles), traffic control and the OpenAPI document.
// Sources: internal/server/auth/scopes.go, internal/server/mgmt/scope.go, routes_traffic.go, openapi.go;
// docs/permissions.md, docs/traffic.md, docs/api.md.
import { http, seg } from '../client'
import type { RFC3339, Role, TokenInfo } from '../types'

type Sig = AbortSignal | undefined

/** A global role may be "none": the person only has per-repository roles. */
export type GlobalRole = Role | 'none'

/** auth.RepoRole */
export interface RepoRole { repository: string; role: Role }

/** auth.UserAccess (GET/PUT /users/{name}/access). */
export interface UserAccess { global_role: GlobalRole; scoped: boolean; scopes: RepoRole[] | null }

/** auth.Access as GET /session reports it (absent: the role applies to every repository). */
export interface SessionAccess { global: GlobalRole; repositories: Record<string, Role> | null }

/** POST /me/tokens with an optional repository restriction. */
export interface ScopedTokenRequest { name: string; role?: Role; ttl_hours?: number; repositories?: string[] }
export interface ScopedTokenResponse { token: string; info: TokenInfo & { repositories: string[] | null }; note: string }

/** traffic.Rule */
export interface TrafficRule {
  id: string
  name: string
  enabled: boolean
  scope: 'host' | 'global'
  hosts: string
  direction: 'upload' | 'download'
  rate_bps: number
  burst_bytes: number
  schedule: string
  timezone: string
  comment: string
  created_at: RFC3339
  updated_at: RFC3339
}
export type TrafficRuleInput = Omit<TrafficRule, 'id' | 'created_at' | 'updated_at'>

/** traffic.HostRate */
export interface HostRate {
  repository: string
  host: string
  upload_bps: number
  download_bps: number
  upload_bytes_total: number
  download_bytes_total: number
  last_seen: RFC3339
}
/** traffic.ActiveRule */
export interface ActiveRule { name: string; scope: string; direction: string; rate_bps: number; active: boolean }
/** GET /traffic/current */
export interface TrafficCurrent { at: RFC3339; hosts: HostRate[] | null; rules: ActiveRule[] | null }

export const gap = {
  access: {
    get: (user: string, signal?: Sig) => http.get<UserAccess>(`/users/${seg(user)}/access`, { signal }),
    set: (user: string, scoped: boolean, scopes: RepoRole[]) => http.put<UserAccess>(`/users/${seg(user)}/access`, { scoped, scopes }),
    setGlobalRole: (user: string, role: GlobalRole) => http.put<void>(`/users/${seg(user)}`, { role }),
  },
  createToken: (b: ScopedTokenRequest) => http.post<ScopedTokenResponse>('/me/tokens', b),
  traffic: {
    rules: (signal?: Sig) => http.get<TrafficRule[] | null>('/traffic/rules', { signal }),
    create: (r: TrafficRuleInput) => http.post<TrafficRule>('/traffic/rules', r),
    update: (name: string, r: TrafficRuleInput) => http.put<TrafficRule>(`/traffic/rules/${seg(name)}`, r),
    remove: (name: string) => http.del<void>(`/traffic/rules/${seg(name)}`),
    current: (signal?: Sig) => http.get<TrafficCurrent>('/traffic/current', { signal }),
  },
  openapi: (signal?: Sig) => http.get<OpenAPIDoc>('/openapi.json', { signal }),
}

// ---- OpenAPI (the subset the explorer reads) ---------------------------------------------------

export interface OSchema {
  $ref?: string
  type?: string | string[]
  format?: string
  description?: string
  properties?: Record<string, OSchema>
  required?: string[]
  items?: OSchema
  additionalProperties?: OSchema | boolean
  anyOf?: OSchema[]
  allOf?: OSchema[]
  contentEncoding?: string
}
export interface OParam { name: string; in: 'path' | 'query'; required?: boolean; description?: string; schema?: OSchema }
export interface OOperation {
  operationId: string
  summary: string
  description?: string
  tags: string[]
  'x-role': Role
  'x-repository-binding': string
  parameters?: OParam[]
  requestBody?: { content: Record<string, { schema: OSchema }> }
  responses: Record<string, { description: string; content?: Record<string, { schema: OSchema }> }>
  security?: unknown[]
}
export interface OpenAPIDoc {
  openapi: string
  info: { title: string; version: string; description: string }
  paths: Record<string, Record<string, OOperation>>
  components: { schemas: Record<string, OSchema> }
}

/** One operation, flattened for lists. */
export interface Endpoint { method: string; path: string; op: OOperation }

export function endpoints(doc: OpenAPIDoc): Endpoint[] {
  const out: Endpoint[] = []
  for (const [path, ops] of Object.entries(doc.paths)) {
    for (const [method, op] of Object.entries(ops)) out.push({ method: method.toUpperCase(), path, op })
  }
  const order = ['GET', 'POST', 'PUT', 'DELETE']
  return out.sort((a, b) => (a.path !== b.path ? (a.path < b.path ? -1 : 1) : order.indexOf(a.method) - order.indexOf(b.method)))
}

/** Resolve a local $ref ("#/components/schemas/X"). */
export function deref(doc: OpenAPIDoc, s: OSchema | undefined): OSchema | undefined {
  let cur = s
  for (let i = 0; cur?.$ref && i < 20; i++) cur = doc.components.schemas[cur.$ref.replace('#/components/schemas/', '')]
  return cur
}

/** A short type label: "string", "integer[]", "jobs.Job", "string | null". */
export function typeLabel(s: OSchema | undefined): string {
  if (!s) return 'any'
  if (s.$ref) return s.$ref.replace('#/components/schemas/', '')
  if (s.anyOf) return s.anyOf.map(typeLabel).join(' | ')
  if (s.allOf?.length === 1) return typeLabel(s.allOf[0])
  if (Array.isArray(s.type)) return s.type.map((t) => (t === 'array' ? `${typeLabel(s.items)}[]` : t)).join(' | ')
  if (s.type === 'array') return `${typeLabel(s.items)}[]`
  if (s.type === 'object' && s.additionalProperties && typeof s.additionalProperties === 'object' && !s.properties) return `map<string, ${typeLabel(s.additionalProperties)}>`
  if (s.format) return `${s.type} (${s.format})`
  return s.type ?? 'any'
}

/** A JSON example built from a schema (for request bodies). Depth-limited; arrays get one element. */
export function example(doc: OpenAPIDoc, s: OSchema | undefined, depth = 0): unknown {
  const r = deref(doc, s)
  if (!r || depth > 4) return null
  if (r.anyOf) return example(doc, r.anyOf.find((x) => x.type !== 'null'), depth)
  if (r.allOf?.length) return example(doc, r.allOf[0], depth)
  const t = Array.isArray(r.type) ? r.type.find((x) => x !== 'null') : r.type
  switch (t) {
    case 'string': return r.format === 'date-time' ? '2026-10-08T12:00:00Z' : ''
    case 'integer': case 'number': return 0
    case 'boolean': return false
    case 'array': return [example(doc, r.items, depth + 1)]
    case 'object': {
      if (!r.properties) return {}
      const o: Record<string, unknown> = {}
      for (const [k, v] of Object.entries(r.properties)) o[k] = example(doc, v, depth + 1)
      return o
    }
  }
  return null
}

/** curl for an operation. Bearer token placeholder; ?repository= when the route takes one. */
export function curlFor(origin: string, method: string, path: string, query: Record<string, string>, body: string): string {
  const qs = new URLSearchParams(Object.entries(query).filter(([, v]) => v !== '')).toString()
  const parts = [`curl -sS -X ${method}`, `-H 'Authorization: Bearer $IB_TOKEN'`]
  if (body.trim()) parts.push(`-H 'Content-Type: application/json'`, `--data '${body.trim().replace(/'/g, "'\\''")}'`)
  parts.push(`'${origin}${path}${qs ? '?' + qs : ''}'`)
  return parts.join(' \\\n  ')
}

/** bytes/s for people: "12.5 MiB/s". */
export function rateText(bps: number): string {
  if (!bps) return '0 B/s'
  const u = ['B/s', 'KiB/s', 'MiB/s', 'GiB/s']
  let v = bps, i = 0
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v >= 100 || i === 0 ? Math.round(v) : +v.toFixed(1)} ${u[i]}`
}

/** Parse "10 MiB/s", "800k", "1.5M", "2 MB/s" (decimal) or plain bytes into bytes per second; NaN when unreadable. */
export function parseRate(s: string): number {
  const m = s.trim().match(/^(\d+(?:\.\d+)?)\s*([kmgKMG]?)(i?)([bB]?)(?:\/s)?$/)
  if (!m) return NaN
  const n = parseFloat(m[1])
  const unit = m[2].toLowerCase()
  const base = m[3] === 'i' || (unit && !m[4]) ? 1024 : 1000
  const pow = { '': 0, k: 1, m: 2, g: 3 }[unit] ?? 0
  return Math.round(n * base ** pow)
}

/** The same schedule grammar as internal/server/traffic.ParseSchedule (for instant form feedback). */
export function scheduleError(spec: string): string {
  const s = spec.trim().toLowerCase()
  if (!s || s === 'always') return ''
  const day = '(mon|tue|wed|thu|fri|sat|sun)'
  const days = new RegExp(`^(\\*|daily|all|${day}(-${day})?(,${day}(-${day})?)*)$`)
  const clock = /^([01]\d|2[0-3]):[0-5]\d-(([01]\d|2[0-3]):[0-5]\d|24:00)$/
  for (const part of s.split(';').map((p) => p.trim()).filter(Boolean)) {
    const f = part.split(/\s+/)
    if (f.length > 2) return `“${part}”: write DAYS HH:MM-HH:MM`
    for (const x of f) {
      if (x.includes(':')) {
        if (!clock.test(x)) return `“${x}”: times are HH:MM-HH:MM (24:00 allowed at the end)`
        const [a, b] = x.split('-')
        if (a === b) return `“${x}”: the window is empty`
      } else if (!days.test(x)) return `“${x}”: days are mon..sun, ranges like mon-fri, lists like sat,sun`
    }
  }
  return ''
}
