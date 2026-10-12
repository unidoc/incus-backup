// Who is signed in, the CSRF token, first-run setup state and the idle-session warning.
//
// The server ends a session after 30 min without API activity (12 h absolute). Successful API calls
// renew it, so the store tracks the time of the last successful call: at 25 min it shows a warning
// ("stay signed in" makes one GET /session, which renews), at 30 min it checks and, if the session is
// gone, the 401 handler sends the user to the sign-in page with the page they were on kept in ?next=.
import { reactive, computed, watch } from 'vue'
import { api } from '@/api/endpoints'
import { ApiError, onActivity, setCsrf } from '@/api/client'
import { atLeast } from '@/api/perms'
import type { LoginRequest, PrincipalKind, Role, SessionResponse, User } from '@/api/types'
import { clearCache } from '@/api/cache'
import { repos } from '@/stores/repos'
import type { SessionAccess } from '@/api/sections/gapcore'

export const IDLE_TIMEOUT_MS = 30 * 60_000
export const IDLE_WARN_MS = 25 * 60_000

export type SessionStatus = 'unknown' | 'anonymous' | 'authenticated'

export const session = reactive({
  status: 'unknown' as SessionStatus,
  user: null as User | null,
  /** 'user' for a person; 'token'/'local' when the API is used by other principals. */
  kind: 'user' as PrincipalKind,
  name: '',
  role: '' as Role | '',
  setupNeeded: false,
  /** The last sign-out was caused by an expired session (the login page says so). */
  expired: false,
  lastActivity: Date.now(),
  /** Absolute end of the session (only known right after sign-in). */
  expiresAt: '' as string,
  idleWarning: false,
  /** Per-repository roles (docs/permissions.md); null: the role applies to every repository. */
  access: null as SessionAccess | null,
  /** How the session was opened: password, passkey or oidc (docs/authentication.md). */
  method: '' as string,
  /** The sign-in policy requires a second factor that is not set up: only enrolment is possible. */
  enrol2fa: false,
})

// With per-repository roles, `role` is what the user may do on the selected repository (or globally,
// whichever is higher), so every can()/hasRole() check in the views follows the repository selector.
// The server decides per request anyway; this only keeps the console from offering what would be refused.
function effectiveRole(): Role | '' {
  const a = session.access
  if (!a) return session.role
  const ranks = ['readonly', 'restore', 'operator', 'admin']
  const g = a.global as string
  const r = (a.repositories ?? {})[repos.current] ?? ''
  const best = ranks.indexOf(r) > ranks.indexOf(g) ? r : g
  return (ranks.includes(best) ? best : '') as Role | ''
}
watch(() => [repos.current, session.access] as const, () => { if (session.access) session.role = effectiveRole() })

export const isAdmin = computed(() => session.role === 'admin')
export function hasRole(r: Role) { return atLeast(session.role, r) }

onActivity(() => {
  session.lastActivity = Date.now()
  if (session.idleWarning) session.idleWarning = false
})

function applySession(s: SessionResponse) {
  if ('csrf' in s) {
    session.user = s.user
    session.kind = 'user'
    session.name = s.user.name
    session.role = s.user.role
    session.access = (s as { access?: SessionAccess }).access ?? null
    if (session.access) session.role = effectiveRole()
    setCsrf(s.csrf)
    if (s.expires_at) session.expiresAt = s.expires_at
    const x = s as { method?: string; enrol_2fa_required?: boolean }
    session.method = x.method ?? ''
    session.enrol2fa = !!x.enrol_2fa_required
  } else {
    session.user = null
    session.kind = s.kind
    session.name = s.user.name
    session.role = s.user.role
    session.access = (s as { access?: SessionAccess | null }).access ?? null
    if (session.access) session.role = effectiveRole()
  }
  session.status = 'authenticated'
  session.expired = false
  session.lastActivity = Date.now()
}

function clear() {
  session.user = null
  session.name = ''
  session.role = ''
  session.access = null
  session.status = 'anonymous'
  session.idleWarning = false
  session.expiresAt = ''
  session.method = ''
  session.enrol2fa = false
  setCsrf('')
  clearCache()
}

let initPromise: Promise<void> | null = null

/** Resolve the current session once (router guard awaits it). */
export function initSession(force = false): Promise<void> {
  if (initPromise && !force) return initPromise
  initPromise = (async () => {
    try {
      applySession(await api.session.get())
      session.setupNeeded = false
      return
    } catch (e) {
      if (!(e instanceof ApiError) || e.status !== 401) {
        // Server unreachable or broken: stay anonymous; the sign-in page shows the error when used.
        clear()
        return
      }
    }
    clear()
    try {
      session.setupNeeded = (await api.setup.status()).needed
    } catch {
      session.setupNeeded = false
    }
  })()
  return initPromise
}

export type LoginOutcome = { ok: true } | { ok: false; totpRequired: true; message: string; methods: { totp: boolean; webauthn: boolean }; ticket: string }

/** Sign in. Throws ApiError for wrong credentials/lockout; returns totpRequired when a code is needed. */
export async function login(req: LoginRequest): Promise<LoginOutcome> {
  try {
    const s = await api.session.login(req)
    applySession(s)
    session.setupNeeded = false
    initPromise = Promise.resolve()
    return { ok: true }
  } catch (e) {
    const b = e instanceof ApiError ? (e.body as { totp_required?: boolean; second_factor?: { totp: boolean; webauthn: boolean }; ticket?: string } | null) : null
    if (e instanceof ApiError && e.status === 401 && b?.totp_required) {
      // second_factor: which second factors the user has (a server without it knows only TOTP)
      return { ok: false, totpRequired: true, message: e.message, methods: b.second_factor ?? { totp: true, webauthn: false }, ticket: b.ticket ?? '' }
    }
    throw e
  }
}

/** Take over a session that another sign-in route opened (passkey, security key). */
export function adoptSession(s: SessionResponse) {
  applySession(s)
  session.setupNeeded = false
  initPromise = Promise.resolve()
}

export async function logout() {
  try { await api.session.logout() } catch { /* signing out of a dead session is still signing out */ }
  clear()
  session.expired = false
  initPromise = Promise.resolve()
}

/** Called by the API client on a 401 from an authenticated route. Returns true when this was a live session. */
export function sessionLost(): boolean {
  const wasIn = session.status === 'authenticated'
  clear()
  session.expired = wasIn
  initPromise = Promise.resolve()
  return wasIn
}

/** Keep the session alive (the idle warning's button). */
export async function stayActive() {
  const s = await api.session.get()
  applySession(s)
  session.idleWarning = false
}

/** Re-read the signed-in user (after changing the password or 2FA). */
export async function refreshUser() {
  applySession(await api.session.get())
}

let idleTimer: ReturnType<typeof setInterval> | null = null
/** Starts the idle watcher; `onExpired` runs when the idle timeout passed and the server agrees. */
export function startIdleWatch(onExpired: () => void, now: () => number = Date.now) {
  if (idleTimer) clearInterval(idleTimer)
  idleTimer = setInterval(async () => {
    if (session.status !== 'authenticated' || session.kind !== 'user') return
    const idle = now() - session.lastActivity
    if (idle >= IDLE_TIMEOUT_MS) {
      try { await api.session.get() } catch (e) {
        if (e instanceof ApiError && e.status === 401) { sessionLost(); onExpired() }
      }
    } else if (idle >= IDLE_WARN_MS) {
      session.idleWarning = true
    }
  }, 15_000)
}
