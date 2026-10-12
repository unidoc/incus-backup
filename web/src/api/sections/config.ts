// "Alerts, notifications & administration" section: wire types (exact Go JSON names) and one function
// per route. Sources: internal/server/notify (alerts, channels, rules, silences, deliveries, settings),
// internal/server/service/certs.go (GET /certificates), internal/server/mgmt/routes_config.go (about,
// setup checklist, tick, simulate, the administrator's token overview).
import { http, seg } from '../client'
import type { RFC3339, Role, User, AuditEntry, TokenInfo, WebCertInfo } from '../types'
import { atLeast } from '../perms'

type Sig = AbortSignal | undefined

// ---- alerts -------------------------------------------------------------------------------------

/** Alert severities of the notification engine (not the `warn` of certificate levels). */
export type Severity = 'info' | 'warning' | 'critical'
export type AlertState = 'firing' | 'acknowledged' | 'silenced' | 'resolved'

/** notify.Alert */
export interface Alert {
  id: number
  key: string
  type: string
  severity: Severity
  title: string
  detail: string
  host: string
  repository: string
  object: string
  /** condition (evaluated every minute) or event (emitted by another part of the server). */
  source: 'condition' | 'event'
  state: AlertState
  /** Number of the current firing episode (a recurrence reopens the same alert). */
  episode: number
  /** Start of the current episode. */
  first_seen: RFC3339
  last_seen: RFC3339
  resolved_at: RFC3339 | null
  resolved_silently: boolean
  count: number
  flapping: boolean
  last_change: RFC3339
  acked_by: string
  acked_at: RFC3339 | null
  ack_comment: string
  silence_id: number | null
}

/** notify.AlertEvent: one timeline entry (fired, escalated, notified, acknowledged, silenced, flapping, stable, resolved, expired). */
export interface AlertEvent { at: RFC3339; kind: string; detail: string }

/** notify.AlertDetail (GET /alerts/{id}). */
export interface AlertDetail { alert: Alert; events: AlertEvent[] | null; deliveries: Delivery[] | null }

export type AlertQuery = { state?: string; severity?: Severity | ''; type?: string; host?: string; active?: 0 | 1; limit?: number }

/** notify.Silence */
export interface Silence {
  id: number
  type_glob: string
  key_glob: string
  host_glob: string
  repo_glob: string
  object_glob: string
  starts_at: RFC3339
  until: RFC3339
  reason: string
  author: string
  created: RFC3339
  active: boolean
}
export type SilenceInput = Pick<Silence, 'type_glob' | 'key_glob' | 'host_glob' | 'repo_glob' | 'object_glob' | 'until' | 'reason'>

// ---- channels -----------------------------------------------------------------------------------

export type ChannelKind = 'smtp' | 'webhook' | 'ntfy' | 'gotify'

export interface SMTPConfig { host: string; port: number; tls: 'starttls' | 'tls' | 'none'; auth: '' | 'plain' | 'login'; username: string; from: string; to: string[]; helo?: string; tls_ca_pem?: string; tls_server_name?: string }
export interface WebhookConfig { format: 'json' | 'slack' | 'discord'; headers?: Record<string, string>; signature_header?: string; tls_ca_pem?: string }
export interface NtfyConfig { server: string; topic: string; username?: string; priorities?: Record<string, number>; tls_ca_pem?: string }
export interface GotifyConfig { url: string; priorities?: Record<string, number>; tls_ca_pem?: string }
export type ChannelConfig = SMTPConfig | WebhookConfig | NtfyConfig | GotifyConfig

/** notify.ChannelSecret: accepted, never returned. Sending it REPLACES the whole stored secret; `{}` removes it. */
export interface ChannelSecret { password?: string; token?: string; url?: string; hmac_secret?: string; headers?: Record<string, string> }

/** notify.Channel */
export interface Channel {
  id: string
  name: string
  kind: ChannelKind
  config: ChannelConfig
  has_secret: boolean
  /** Where messages go, without credentials. */
  target: string
  enabled: boolean
  rate_per_hour: number
  timeout_seconds: number
  created: RFC3339
}

/** notify.ChannelInput */
export interface ChannelInput {
  name?: string
  kind?: ChannelKind
  config?: ChannelConfig
  secret?: ChannelSecret
  enabled?: boolean
  rate_per_hour?: number
  timeout_seconds?: number
}

/** POST /notifications/channels/{id}/test: always 200; `error` is the receiver's own answer. */
export interface ChannelTestResult { ok: boolean; error?: string; delivery?: Delivery | null }

// ---- rules --------------------------------------------------------------------------------------

/** notify.Rule (PUT decodes over the defaults: always send every field). */
export interface Rule {
  id: string
  name: string
  kind: 'alert' | 'digest'
  event_types: string[] | null
  min_severity: Severity
  host_glob: string
  repo_glob: string
  object_glob: string
  channel_ids: string[] | null
  repeat_minutes: number
  quiet_start: string
  quiet_end: string
  timezone: string
  critical_bypass: boolean
  send_resolved: boolean
  digest_at: string
  last_digest_at: RFC3339 | null
  enabled: boolean
  created: RFC3339
}
export type RuleInput = Omit<Rule, 'id' | 'created' | 'last_digest_at'>

export function defaultRule(): RuleInput {
  return { name: '', kind: 'alert', event_types: [], min_severity: 'warning', host_glob: '', repo_glob: '', object_glob: '', channel_ids: [], repeat_minutes: 1440,
    quiet_start: '', quiet_end: '', timezone: 'UTC', critical_bypass: true, send_resolved: true, digest_at: '08:00', enabled: true }
}

// ---- deliveries ---------------------------------------------------------------------------------

export type DeliveryState = 'pending' | 'sending' | 'sent' | 'dead'
export type DeliveryKind = 'firing' | 'reminder' | 'escalation' | 'resolved' | 'digest' | 'test'

/** notify.Delivery */
export interface Delivery {
  id: number
  channel_id: string
  channel_name: string
  rule_id: string
  alert_id: number | null
  kind: DeliveryKind
  severity: string
  subject: string
  body_text: string
  link: string
  state: DeliveryState
  attempts: number
  next_attempt_at: RFC3339
  last_error: string
  created: RFC3339
  sent_at: RFC3339 | null
}

// ---- engine settings and conditions --------------------------------------------------------------

/** notify.Settings (PUT replaces all fields). */
export interface NotifySettings {
  dedupe_minutes: number
  flap_window_minutes: number
  flap_threshold: number
  max_attempts: number
  backoff_base_seconds: number
  backoff_max_seconds: number
  event_expire_minutes: number
  alert_retention_days: number
  delivery_retention_days: number
}

/** notify.Condition: one alert type's switch, base severity and thresholds (params merged on PUT). */
export interface Condition { type: string; enabled: boolean; severity: Severity; params: Record<string, number | string> }

export interface NotifySettingsView { settings: NotifySettings; conditions: Condition[] | null }

/** notify.TickResult */
export interface TickResult { skipped: boolean; observed: number; fired: number; resolved: number; enqueued: number; delivered: number; failed: number; errors?: string[] }

export interface SimulateInput { type: string; severity: Severity; host?: string; repository?: string; object?: string; key?: string }
export interface SimulateRule { rule_id: string; rule_name: string; matched: boolean; reason: string; quiet: boolean; channels: { id: string; name: string; kind: ChannelKind; enabled: boolean }[] }
export interface SimulateResult { at: RFC3339; silenced_by: number[]; rules: SimulateRule[]; deliver: boolean }

// ---- certificates (service.CertReport) -------------------------------------------------------------

export type CertState = 'ok' | 'expiring' | 'expired' | 'untrusted'
export type CertLevel = 'ok' | 'warn' | 'critical'
export interface CertSummary { subject?: string; fingerprint: string; not_before: RFC3339; not_after: RFC3339; days_left: number; state: CertState; level: CertLevel }
export interface HostCertStatus extends CertSummary {
  repository: string
  name: string
  scope: string
  disabled: boolean
  last_renewed: RFC3339 | null
  last_seen: RFC3339 | null
  other_valid: number
  on_old_ca: boolean
  untrusted: boolean
}
export interface PeerCertStatus extends CertSummary { repository: string; job: string; error?: string }
export interface RotationStatus { next: CertSummary; pending: string[] | null }
export interface CertReport {
  generated: RFC3339
  ca: CertSummary
  rotation?: RotationStatus
  server: CertSummary
  server_sans: string[] | null
  want_sans: string[] | null
  hosts: HostCertStatus[] | null
  sync_peers: PeerCertStatus[] | null
  level: CertLevel
}
/** GET /certificates: `web` is missing or null when the server has no web listener (or does not report it). */
export interface CertificatesView { report: CertReport; web?: (Partial<WebCertInfo> & { mode: string }) | null }

// ---- server ---------------------------------------------------------------------------------------

/** mgmt.About (GET /about). */
export interface About { version: string; started: RFC3339; uptime_seconds: number; go_version: string; database_ok: boolean; database_error?: string; datastore_count: number; instance_name: string }

/** service.ChecklistItem; `route` is a route name of this console. */
export interface ChecklistItem { id: string; title: string; ok: boolean; detail: string; route: string }
export interface Checklist { items: ChecklistItem[]; done: number; total: number; complete: boolean }

/** auth.AdminTokenInfo (GET /api-tokens). */
export interface AdminToken extends TokenInfo { owner: string; owner_role: Role; owner_disabled: boolean; effective_role: Role | ''; expired: boolean }

/** PUT /users/{name} (pointer fields: only what is sent changes). */
export interface UserUpdate { role?: Role; display_name?: string; email?: string; disabled?: boolean }
/** POST /users */
export interface UserCreate { name: string; password: string; role: Role; display: string; email: string; must_change: boolean }

// ---- API ------------------------------------------------------------------------------------------

export const cfg = {
  alerts: {
    list: (q: AlertQuery = {}, signal?: Sig) => http.get<Alert[] | null>('/alerts', { query: q, signal }),
    get: (id: number, signal?: Sig) => http.get<AlertDetail>(`/alerts/${id}`, { signal }),
    ack: (id: number, comment: string) => http.post<Alert>(`/alerts/${id}/ack`, { comment }),
    unack: (id: number) => http.post<Alert>(`/alerts/${id}/ack`, { undo: true }),
  },
  silences: {
    list: (all: boolean, signal?: Sig) => http.get<Silence[] | null>('/silences', { query: { all: all ? 1 : undefined }, signal }),
    create: (b: SilenceInput) => http.post<Silence>('/silences', b),
    expire: (id: number) => http.del<void>(`/silences/${id}`),
  },
  channels: {
    list: (signal?: Sig) => http.get<Channel[] | null>('/notifications/channels', { signal }),
    create: (b: ChannelInput) => http.post<Channel>('/notifications/channels', b),
    update: (id: string, b: ChannelInput) => http.put<Channel>(`/notifications/channels/${seg(id)}`, b),
    remove: (id: string) => http.del<void>(`/notifications/channels/${seg(id)}`),
    test: (id: string) => http.post<ChannelTestResult>(`/notifications/channels/${seg(id)}/test`),
  },
  rules: {
    list: (signal?: Sig) => http.get<Rule[] | null>('/notifications/rules', { signal }),
    create: (b: RuleInput) => http.post<Rule>('/notifications/rules', b),
    update: (id: string, b: RuleInput) => http.put<Rule>(`/notifications/rules/${seg(id)}`, b),
    remove: (id: string) => http.del<void>(`/notifications/rules/${seg(id)}`),
    simulate: (b: SimulateInput) => http.post<SimulateResult>('/notifications/simulate', b),
  },
  deliveries: {
    list: (q: { channel?: string; state?: string; alert?: number; limit?: number } = {}, signal?: Sig) =>
      http.get<Delivery[] | null>('/notifications/deliveries', { query: q, signal }),
    retry: (id: number) => http.post<void>(`/notifications/deliveries/${id}/retry`),
  },
  notify: {
    get: (signal?: Sig) => http.get<NotifySettingsView>('/notifications/settings', { signal }),
    put: (s: NotifySettings) => http.put<void>('/notifications/settings', s),
    setCondition: (type: string, c: Omit<Condition, 'type'>) => http.put<void>(`/notifications/conditions/${seg(type)}`, c),
    tick: () => http.post<TickResult>('/notifications/tick'),
  },
  certificates: {
    get: (signal?: Sig) => http.get<CertificatesView>('/certificates', { signal }),
    setSANs: (sans: string[]) => http.put<void>('/certificates/sans', { sans }),
    rotate: () => http.post<{ new_ca_fingerprint: string }>('/certificates/ca/rotate'),
    promote: (force: boolean) => http.post<void>('/certificates/ca/promote', undefined, { query: { force: force ? 1 : undefined } }),
  },
  about: (signal?: Sig) => http.get<About>('/about', { signal }),
  checklist: (signal?: Sig) => http.get<Checklist>('/setup-checklist', { signal }),
  tokens: {
    list: (signal?: Sig) => http.get<AdminToken[] | null>('/api-tokens', { signal }),
    revoke: (id: string) => http.del<void>(`/api-tokens/${seg(id)}`),
  },
  users: {
    list: (signal?: Sig) => http.get<User[] | null>('/users', { signal }),
    create: (b: UserCreate) => http.post<User>('/users', b),
    update: (name: string, b: UserUpdate) => http.put<void>(`/users/${seg(name)}`, b),
    remove: (name: string) => http.del<void>(`/users/${seg(name)}`),
    resetPassword: (name: string, password: string, mustChange: boolean) => http.post<void>(`/users/${seg(name)}/password`, { password, must_change: mustChange }),
    removeTOTP: (name: string) => http.del<void>(`/users/${seg(name)}/totp`),
  },
  audit: (q: { actor?: string; action?: string; before?: number; limit?: number } = {}, signal?: Sig) =>
    http.get<AuditEntry[] | null>('/audit', { query: q, signal }),
}

/**
 * Roles of this section's routes as registered in internal/server/mgmt (perms.go plus the init() entries
 * of routes_config.go, which the perms.go mirror test does not parse). config.test.ts checks this table
 * against both Go files.
 */
export const CONFIG_ROUTE_ROLES: Record<string, Role> = {
  'GET /notifications/channels': 'operator', 'POST /notifications/channels': 'admin', 'PUT /notifications/channels/{id}': 'admin',
  'DELETE /notifications/channels/{id}': 'admin', 'POST /notifications/channels/{id}/test': 'admin',
  'GET /notifications/rules': 'operator', 'POST /notifications/rules': 'admin', 'PUT /notifications/rules/{id}': 'admin', 'DELETE /notifications/rules/{id}': 'admin',
  'GET /alerts': 'readonly', 'GET /alerts/{id}': 'readonly', 'POST /alerts/{id}/ack': 'operator',
  'GET /silences': 'readonly', 'POST /silences': 'operator', 'DELETE /silences/{id}': 'operator',
  'GET /notifications/deliveries': 'operator', 'POST /notifications/deliveries/{id}/retry': 'admin',
  'GET /notifications/settings': 'operator', 'PUT /notifications/settings': 'admin', 'PUT /notifications/conditions/{type}': 'admin',
  'GET /certificates': 'readonly', 'PUT /certificates/sans': 'admin', 'POST /certificates/ca/rotate': 'admin', 'POST /certificates/ca/promote': 'admin',
  'GET /about': 'readonly', 'GET /setup-checklist': 'admin', 'POST /notifications/tick': 'admin', 'POST /notifications/simulate': 'operator',
  'GET /api-tokens': 'admin', 'DELETE /api-tokens/{id}': 'admin',
}

/** Can `role` call one of this section's routes? Unknown routes need admin. */
export function canCfg(role: Role | '' | undefined | null, route: string): boolean {
  return atLeast(role, CONFIG_ROUTE_ROLES[route] ?? 'admin')
}

export const list = <X>(v: X[] | null | undefined): X[] => v ?? []
