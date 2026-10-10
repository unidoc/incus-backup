// The channel editor's model: one flat form for every kind, turned into the strict per-kind config the
// server decodes (unknown fields are refused) plus the secret. Pure functions, unit-tested.
import type { Channel, ChannelInput, ChannelKind, ChannelSecret, GotifyConfig, NtfyConfig, SMTPConfig, WebhookConfig } from '@/api/sections/config'

export interface ChannelForm {
  name: string
  kind: ChannelKind
  enabled: boolean
  rate_per_hour: number
  timeout_seconds: number
  // smtp
  host: string; port: number; tls: SMTPConfig['tls']; auth: SMTPConfig['auth']; username: string; from: string; to: string[]; helo: string; tls_server_name: string
  // webhook
  format: WebhookConfig['format']; headers: string; signature_header: string
  // ntfy
  server: string; topic: string
  // gotify
  url: string
  // shared
  priorities: { info: string; warning: string; critical: string; resolved: string }
  tls_ca_pem: string
  /** keep: leave the stored secret; replace: send `secret` (replaces ALL of it); remove: send {} */
  secretMode: 'keep' | 'replace' | 'remove'
  secret: { password: string; token: string; url: string; hmac_secret: string; headers: string }
}

export function emptyForm(kind: ChannelKind = 'webhook'): ChannelForm {
  return {
    name: '', kind, enabled: true, rate_per_hour: 30, timeout_seconds: 15,
    host: '', port: 587, tls: 'starttls', auth: '', username: '', from: '', to: [], helo: '', tls_server_name: '',
    format: 'json', headers: '', signature_header: '',
    server: 'https://ntfy.sh', topic: '', url: '',
    priorities: { info: '', warning: '', critical: '', resolved: '' }, tls_ca_pem: '',
    secretMode: 'replace', secret: { password: '', token: '', url: '', hmac_secret: '', headers: '' },
  }
}

const str = (v: unknown) => (typeof v === 'string' ? v : '')
const num = (v: unknown, d: number) => (typeof v === 'number' ? v : d)

/** The form for an existing channel (secrets are never known: keep is the default). */
export function formFromChannel(c: Channel): ChannelForm {
  const f = emptyForm(c.kind)
  const x = (c.config ?? {}) as unknown as Record<string, unknown>
  Object.assign(f, { name: c.name, enabled: c.enabled, rate_per_hour: c.rate_per_hour, timeout_seconds: c.timeout_seconds, secretMode: c.has_secret ? 'keep' : 'replace' })
  f.tls_ca_pem = str(x.tls_ca_pem)
  const pr = (x.priorities ?? {}) as Record<string, number>
  f.priorities = { info: pr.info ? String(pr.info) : '', warning: pr.warning ? String(pr.warning) : '', critical: pr.critical ? String(pr.critical) : '', resolved: pr.resolved ? String(pr.resolved) : '' }
  if (c.kind === 'smtp') {
    Object.assign(f, { host: str(x.host), port: num(x.port, 587), tls: (str(x.tls) || 'starttls') as SMTPConfig['tls'], auth: str(x.auth) as SMTPConfig['auth'], username: str(x.username), from: str(x.from),
      to: Array.isArray(x.to) ? (x.to as string[]) : [], helo: str(x.helo), tls_server_name: str(x.tls_server_name) })
  } else if (c.kind === 'webhook') {
    f.format = (str(x.format) || 'json') as WebhookConfig['format']
    f.signature_header = str(x.signature_header)
    f.headers = headersToText((x.headers ?? {}) as Record<string, string>)
  } else if (c.kind === 'ntfy') {
    Object.assign(f, { server: str(x.server) || 'https://ntfy.sh', topic: str(x.topic), username: str(x.username) })
  } else {
    f.url = str(x.url)
  }
  return f
}

export function headersToText(h: Record<string, string>): string {
  return Object.entries(h).map(([k, v]) => `${k}: ${v}`).join('\n')
}

/** "Name: value" lines -> map; returns an error text for a malformed line. */
export function parseHeaders(text: string): { headers: Record<string, string>; error: string } {
  const headers: Record<string, string> = {}
  for (const [i, raw] of text.split('\n').entries()) {
    const line = raw.trim()
    if (!line) continue
    const at = line.indexOf(':')
    const k = at > 0 ? line.slice(0, at).trim() : ''
    if (!k || !/^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/.test(k)) return { headers, error: `Line ${i + 1}: write a header as "Name: value"` }
    headers[k] = line.slice(at + 1).trim()
  }
  return { headers, error: '' }
}

function priorities(f: ChannelForm): Record<string, number> | undefined {
  const out: Record<string, number> = {}
  for (const k of ['info', 'warning', 'critical', 'resolved'] as const) if (f.priorities[k] !== '') out[k] = Number(f.priorities[k])
  return Object.keys(out).length ? out : undefined
}

const EMAIL = /^[^\s@<>]+@[^\s@<>]+$/

/** Problems a user can fix before sending (the server validates again). Keyed by field. */
export function validateForm(f: ChannelForm, creating: boolean): Record<string, string> {
  const e: Record<string, string> = {}
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(f.name)) e.name = 'Letters, digits, dot, dash and underscore; up to 64 characters.'
  if (!(f.rate_per_hour >= 0)) e.rate_per_hour = 'Zero or more (0 = unlimited).'
  if (!(f.timeout_seconds >= 1 && f.timeout_seconds <= 300)) e.timeout_seconds = 'Between 1 and 300 seconds.'
  const sendingSecret = creating || f.secretMode === 'replace'
  if (f.kind === 'smtp') {
    if (!f.host || /[\s/]/.test(f.host)) e.host = 'The mail server name, e.g. smtp.example.com.'
    if (!(f.port >= 1 && f.port <= 65535)) e.port = 'A port between 1 and 65535 (587, 465 or 25).'
    if (f.username && f.tls === 'none') e.tls = 'A password is never sent over an unencrypted connection: use STARTTLS or TLS.'
    if (!EMAIL.test(f.from)) e.from = 'A sender address, e.g. backup@example.com.'
    if (!f.to.length) e.to = 'At least one recipient.'
    else if (f.to.some((t) => !EMAIL.test(t))) e.to = 'Every recipient must be an e-mail address.'
    if (f.username && sendingSecret && !f.secret.password) e['secret.password'] = 'A user name needs its password.'
    if (f.username && f.secretMode === 'remove') e['secret.password'] = 'A user name needs its password: keep or replace the secret.'
  } else if (f.kind === 'webhook') {
    if (sendingSecret && !/^https?:\/\/[^\s]+$/.test(f.secret.url)) e['secret.url'] = 'The full URL, e.g. https://hooks.example.com/incus-backup.'
    if (parseHeaders(f.headers).error) e.headers = parseHeaders(f.headers).error
    if (parseHeaders(f.secret.headers).error) e['secret.headers'] = parseHeaders(f.secret.headers).error
  } else if (f.kind === 'ntfy') {
    if (!/^https?:\/\/[^\s]+$/.test(f.server)) e.server = 'The ntfy server, e.g. https://ntfy.sh.'
    if (!/^[^\s/?#]{1,64}$/.test(f.topic)) e.topic = 'A topic name without spaces, slashes, ? or #.'
    if (f.username && sendingSecret && !f.secret.password) e['secret.password'] = 'A user name needs its password.'
  } else {
    if (!/^https?:\/\/[^\s]+$/.test(f.url)) e.url = 'The Gotify server, e.g. https://gotify.example.com.'
    if (sendingSecret && !f.secret.token) e['secret.token'] = 'The application token from Gotify.'
  }
  for (const k of ['info', 'warning', 'critical', 'resolved'] as const) {
    const v = f.priorities[k]
    if (v !== '' && !(Number.isInteger(Number(v)) && Number(v) >= 0 && Number(v) <= 10)) e[`priorities.${k}`] = '0 to 10'
  }
  return e
}

/** The request body. Only the chosen kind's fields; the secret only when it is replaced or removed. */
export function toInput(f: ChannelForm, creating: boolean): ChannelInput {
  const ca = f.tls_ca_pem.trim() || undefined
  let config: ChannelInput['config']
  const secret: ChannelSecret = {}
  if (f.kind === 'smtp') {
    const c: SMTPConfig = { host: f.host.trim(), port: Number(f.port), tls: f.tls, auth: f.auth, username: f.username.trim(), from: f.from.trim(), to: f.to }
    if (f.helo.trim()) c.helo = f.helo.trim()
    if (f.tls_server_name.trim()) c.tls_server_name = f.tls_server_name.trim()
    if (ca) c.tls_ca_pem = ca
    config = c
    if (f.secret.password) secret.password = f.secret.password
  } else if (f.kind === 'webhook') {
    const c: WebhookConfig = { format: f.format }
    const h = parseHeaders(f.headers).headers
    if (Object.keys(h).length) c.headers = h
    if (f.signature_header.trim()) c.signature_header = f.signature_header.trim()
    if (ca) c.tls_ca_pem = ca
    config = c
    if (f.secret.url) secret.url = f.secret.url.trim()
    if (f.secret.hmac_secret) secret.hmac_secret = f.secret.hmac_secret
    const sh = parseHeaders(f.secret.headers).headers
    if (Object.keys(sh).length) secret.headers = sh
  } else if (f.kind === 'ntfy') {
    const c: NtfyConfig = { server: f.server.trim(), topic: f.topic.trim() }
    if (f.username.trim()) c.username = f.username.trim()
    const p = priorities(f)
    if (p) c.priorities = p
    if (ca) c.tls_ca_pem = ca
    config = c
    if (f.secret.token) secret.token = f.secret.token
    if (f.secret.password) secret.password = f.secret.password
  } else {
    const c: GotifyConfig = { url: f.url.trim() }
    const p = priorities(f)
    if (p) c.priorities = p
    if (ca) c.tls_ca_pem = ca
    config = c
    if (f.secret.token) secret.token = f.secret.token
  }
  const out: ChannelInput = { name: f.name.trim(), config, enabled: f.enabled, rate_per_hour: Number(f.rate_per_hour), timeout_seconds: Number(f.timeout_seconds) }
  if (creating) {
    out.kind = f.kind
    if (Object.keys(secret).length) out.secret = secret
  } else if (f.secretMode === 'replace') {
    out.secret = secret
  } else if (f.secretMode === 'remove') {
    out.secret = {}
  }
  return out
}

/** Can the kind work without any secret? (Then "remove the secret" is offered.) */
export function secretOptional(f: ChannelForm): boolean {
  return (f.kind === 'smtp' && !f.username) || (f.kind === 'ntfy' && !f.username)
}
