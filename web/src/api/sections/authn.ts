// Passkeys / security keys (WebAuthn) and single sign-on (OIDC): wire types (exact Go JSON names, from
// internal/server/auth/{webauthn,oidc,policy}.go and internal/server/mgmt/routes_authn.go) and one
// function per route. docs/authentication.md describes the flows.
import { ApiError, http, request, seg } from '../client'
import type { RFC3339, Role, LoginResponse } from '../types'
import type { ServerOptions } from '@/lib/webauthn'

type Sig = AbortSignal | undefined

/** auth.CeremonyStart: what a begin route answers. */
export interface CeremonyStart { ceremony: string; options: ServerOptions }

/** auth.WebAuthnKey */
export interface WebAuthnKey {
  id: string
  name: string
  created_at: RFC3339
  last_used_at?: RFC3339
  sign_count: number
  transports: string[] | null
  attachment: string
  user_verified: boolean
  backup_eligible: boolean
  backup_state: boolean
  discoverable?: boolean
  clone_warning: boolean
}

/** The 401 body of POST /session when a second factor is needed. */
export interface SecondFactorChallenge {
  error: string
  totp_required: true
  second_factor?: { totp: boolean; webauthn: boolean }
  ticket?: string
}

/** GET /auth/oidc/providers (public). */
export interface SignInOptions { providers: { name: string; label: string }[]; local_login: boolean; passkeys: boolean }

export interface RoleMapping { group: string; role: Role }
export interface ScopeMapping { group: string; repository: string; role: Role }

/** auth.OIDCProvider (never the client secret). */
export interface OIDCProvider {
  id: string
  name: string
  display_name: string
  issuer: string
  client_id: string
  has_secret: boolean
  scopes: string[]
  allowed_domains: string[]
  groups_claim: string
  default_role: Role | 'none'
  role_mappings: RoleMapping[]
  scope_mappings: ScopeMapping[]
  auto_create: boolean
  enabled: boolean
  created_at: RFC3339
  updated_at: RFC3339
}

/** auth.OIDCProviderInput: client_secret is write-only ('' keeps the stored one). */
export interface OIDCProviderInput {
  name: string
  display_name: string
  issuer: string
  client_id: string
  client_secret?: string
  clear_secret?: boolean
  scopes: string[]
  allowed_domains: string[]
  groups_claim: string
  default_role: Role | 'none'
  role_mappings: RoleMapping[]
  scope_mappings: ScopeMapping[]
  auto_create: boolean
  enabled: boolean
}

/** auth.DiscoveryReport */
export interface DiscoveryReport {
  ok: boolean
  error?: string
  issuer?: string
  authorization_endpoint?: string
  token_endpoint?: string
  jwks_uri?: string
  jwks_keys: number
  jwks_error?: string
  code_challenge_methods_supported?: string[]
  scopes_supported?: string[]
  claims_supported?: string[]
  redirect_uri: string
  warnings: string[]
}

/** auth.Identity */
export interface Identity { provider: string; subject: string; email: string; pending: boolean; created_at: RFC3339; last_login_at?: RFC3339 }

export type Require2FA = 'none' | 'admin' | 'all'

/** GET/PUT /auth/settings (mgmt.docAuthSettings) */
export interface AuthSettings {
  require_2fa: Require2FA
  local_login: boolean
  webauthn_rp_id: string
  webauthn_origins: string[]
  base_url: string
  webauthn_rp_id_effective: string
  webauthn_origins_effective: string[]
  webauthn_error?: string
  sso_admins: string[]
  oidc_redirect_base: string
}
export interface AuthSettingsUpdate { require_2fa?: Require2FA; local_login?: boolean; webauthn_rp_id?: string; webauthn_origins?: string[] }

/** A 401 from a route that re-checks the password means "wrong password", not "signed out" (see endpoints.ts). */
async function passwordChecked<T>(call: () => Promise<T>): Promise<T> {
  try {
    return await call()
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      await http.get('/session')
      throw new ApiError(401, 'The password is wrong.', e.body, e.method, e.path)
    }
    throw e
  }
}

export const authn = {
  signInOptions: (signal?: Sig) => http.get<SignInOptions>('/auth/oidc/providers', { signal, noAuthRedirect: true }),
  /** The browser navigates here (a top-level GET): the server redirects to the identity provider. */
  ssoURL: (provider: string, next?: string) => `/api/v1/auth/oidc/${seg(provider)}/login${next && next !== '/' ? `?next=${encodeURIComponent(next)}` : ''}`,
  /** Passkey sign-in (no ticket) or the security-key second factor (ticket from POST /session). */
  beginLogin: (ticket?: string) => http.post<CeremonyStart>('/session/webauthn/begin', ticket ? { ticket } : {}, { noAuthRedirect: true }),
  finishLogin: (ceremony: string, credential: Record<string, unknown>) => http.post<LoginResponse>('/session/webauthn/finish', { ceremony, credential }, { noAuthRedirect: true }),

  keys: (signal?: Sig) => http.get<WebAuthnKey[]>('/me/webauthn', { signal }),
  beginRegister: (password?: string) => passwordChecked(() => http.post<CeremonyStart>('/me/webauthn/register/begin', password ? { password } : {}, { noAuthRedirect: true })),
  finishRegister: (ceremony: string, name: string, credential: Record<string, unknown>) => http.post<WebAuthnKey>('/me/webauthn/register/finish', { ceremony, name, credential }),
  renameKey: (id: string, name: string) => request<void>('PATCH', `/me/webauthn/${seg(id)}`, { body: { name } }),
  removeKey: (id: string, password?: string) => passwordChecked(() => http.del<void>(`/me/webauthn/${seg(id)}`, password ? { password } : {}, { noAuthRedirect: true })),

  resetUserKeys: (name: string) => http.del<{ removed: number }>(`/users/${seg(name)}/webauthn`),
  identities: (name: string, signal?: Sig) => http.get<Identity[]>(`/users/${seg(name)}/identities`, { signal }),
  link: (name: string, provider: string, email: string) => http.post<void>(`/users/${seg(name)}/identities`, { provider, email }),
  unlink: (name: string, provider: string) => http.del<void>(`/users/${seg(name)}/identities/${seg(provider)}`),

  providers: (signal?: Sig) => http.get<OIDCProvider[]>('/oidc/providers', { signal }),
  createProvider: (p: OIDCProviderInput) => http.post<OIDCProvider>('/oidc/providers', p),
  updateProvider: (name: string, p: OIDCProviderInput) => http.put<OIDCProvider>(`/oidc/providers/${seg(name)}`, p),
  deleteProvider: (name: string) => http.del<void>(`/oidc/providers/${seg(name)}`),
  testProvider: (name: string) => http.post<DiscoveryReport>(`/oidc/providers/${seg(name)}/test`),

  settings: (signal?: Sig) => http.get<AuthSettings>('/auth/settings', { signal }),
  saveSettings: (u: AuthSettingsUpdate) => http.put<AuthSettings>('/auth/settings', u),
}

/** The explanation for /login?sso_error=<code> (the server sends a fixed word, never IdP text). */
export function ssoErrorText(code: string): string {
  switch (code) {
    case 'state': return 'The single sign-on attempt expired, was already used or was started in another browser. Start again from this page.'
    case 'denied': return 'The identity provider did not sign you in (cancelled or refused there).'
    case 'unverified': return 'The identity provider did not confirm your e-mail address (email_verified is not true). Ask your identity administrator to verify it.'
    case 'domain': return 'Your e-mail domain is not allowed to sign in here. Ask an administrator of this server.'
    case 'no_account': return 'You signed in at the identity provider, but no account here is linked to you and accounts are not created automatically. Ask an administrator to link your account.'
    case 'exists': return 'A local user with your name already exists and is not linked to your identity provider account. Ask an administrator to link it.'
    case 'disabled': return 'Your account is disabled. Ask an administrator.'
    case 'locked': return 'Your account is locked for a few minutes after failed sign-ins. Wait, then try again.'
    case 'provider': return 'That sign-in method is not available (unknown or disabled provider, or the identity provider could not be reached).'
    case 'config': return 'Single sign-on is not set up completely: the console address (base_url) is missing. Ask an administrator.'
  }
  return 'Single sign-on failed. Try again; if it keeps failing, an administrator finds the reason in the audit log.'
}
