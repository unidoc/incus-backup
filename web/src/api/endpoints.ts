// Typed functions for every management API route (docs/api.md). Views call these, never fetch().
// Routes marked R act on the repository chosen in the top bar (the client adds ?repository=).
// Planned routes (alerts, jobs, restore tests, certificates, notifications) are in `draft`: their
// shapes follow docs/api.md and may change when the backend lands; a 404 from them means "not yet".
import { ApiError, http, seg, request, passwordChecked as pwChecked } from './client'
import type * as T from './types'

type Sig = AbortSignal | undefined

/**
 * Routes that answer 401 for a WRONG PASSWORD (not only for "not signed in"): POST /me/password and
 * DELETE /me/totp. They must not trigger the sign-in redirect; on a 401 we probe GET /session (which
 * does redirect when the session really ended) and otherwise rethrow as "password wrong".
 */
async function passwordChecked<T>(call: () => Promise<T>): Promise<T> {
  try {
    return await call()
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      await http.get('/session') // throws (and redirects) if the session is gone
      throw new ApiError(401, 'The password is wrong.', e.body, e.method, e.path)
    }
    throw e
  }
}
const R = (signal?: Sig) => ({ repo: true, signal })

export const api = {
  ping: (signal?: Sig) => http.get<T.PingResponse>('/ping', { signal }),

  // ---- session and people
  setup: {
    status: (signal?: Sig) => http.get<T.SetupStatus>('/setup', { signal, noAuthRedirect: true }),
    complete: (b: T.SetupRequest) => http.post<T.User>('/setup', b, { noAuthRedirect: true }),
  },
  session: {
    login: (b: T.LoginRequest) => http.post<T.LoginResponse>('/session', b, { noAuthRedirect: true }),
    get: (signal?: Sig) => http.get<T.SessionResponse>('/session', { signal, noAuthRedirect: true }),
    logout: () => http.del<void>('/session', undefined, { noAuthRedirect: true }),
  },
  users: {
    list: (signal?: Sig) => http.get<T.User[]>('/users', { signal }),
    create: (b: T.CreateUserRequest) => http.post<T.User>('/users', b),
    update: (name: string, b: T.UpdateUserRequest) => http.put<void>(`/users/${seg(name)}`, b),
    remove: (name: string) => http.del<void>(`/users/${seg(name)}`),
    resetPassword: (name: string, b: T.ResetPasswordRequest) => http.post<void>(`/users/${seg(name)}/password`, b),
    removeTOTP: (name: string) => http.del<void>(`/users/${seg(name)}/totp`),
  },
  me: {
    changePassword: (b: T.ChangePasswordRequest) => passwordChecked(() => http.post<void>('/me/password', b, { noAuthRedirect: true })),
    beginTOTP: (password?: string) => pwChecked(() => http.post<T.TOTPEnrolment>('/me/totp', password ? { password } : {}, { noAuthRedirect: true })),
    confirmTOTP: (b: T.ConfirmTOTPRequest) => pwChecked(() => http.post<T.ConfirmTOTPResponse>('/me/totp/confirm', b, { noAuthRedirect: true })),
    disableTOTP: (b: T.DisableTOTPRequest) => passwordChecked(() => http.del<void>('/me/totp', b, { noAuthRedirect: true })),
    sessions: (signal?: Sig) => http.get<T.SessionInfo[]>('/me/sessions', { signal }),
    revokeSession: (id: string) => http.del<void>(`/me/sessions/${seg(id)}`),
    tokens: (signal?: Sig) => http.get<T.TokenInfo[]>('/me/tokens', { signal }),
    createToken: (b: T.CreateTokenRequest) => http.post<T.CreateTokenResponse>('/me/tokens', b),
    revokeToken: (id: string) => http.del<void>(`/me/tokens/${seg(id)}`),
  },
  audit: (q: { actor?: string; action?: string; before?: number; limit?: number } = {}, signal?: Sig) =>
    http.get<T.AuditEntry[]>('/audit', { query: q, signal }),

  // ---- dashboard and repositories
  overview: (signal?: Sig) => http.get<T.Overview>('/overview', { signal }),
  status: (signal?: Sig) => http.get<T.Status>('/status', R(signal)),
  capacity: (signal?: Sig) => http.get<T.CapacityView>('/capacity', R(signal)),
  capacitySeries: (days = 30, signal?: Sig) => http.get<T.CapacityPoint[]>('/capacity/series', { ...R(signal), query: { days } }),
  repositories: {
    list: (signal?: Sig) => http.get<T.RepoView[]>('/repositories', { signal }),
    create: (b: T.CreateRepoRequest) => http.post<T.RepoView>('/repositories', b),
    attach: (b: T.CreateRepoRequest) => http.post<T.RepoView>('/repositories/attach', b),
    probe: (name: string) => http.post<T.ProbeResult[]>(`/repositories/${seg(name)}/probe`),
    setMode: (name: string, b: T.SetModeRequest) => http.put<void>(`/repositories/${seg(name)}/mode`, b),
  },
  credentials: {
    list: (signal?: Sig) => http.get<T.Credential[] | null>('/credentials', { signal }),
    create: (b: T.CredentialRequest) => http.post<T.Credential>('/credentials', b),
    rotate: (name: string, b: T.CredentialRequest) => http.put<void>(`/credentials/${seg(name)}`, b),
    remove: (name: string) => http.del<void>(`/credentials/${seg(name)}`),
  },

  // ---- hosts, onboarding, policies (R)
  hosts: {
    list: (signal?: Sig) => http.get<T.HostRow[] | null>('/hosts', R(signal)),
    setEnabled: (name: string, scope: string, enabled: boolean) =>
      http.post<void>(`/hosts/${seg(name)}/${seg(scope)}/${enabled ? 'enable' : 'disable'}`, undefined, R()),
  },
  onboarding: (b: T.OnboardingRequest) => http.post<T.OnboardingResult>('/onboarding', b, R()),
  enrolmentToken: (b: T.EnrolmentTokenRequest) => http.post<T.EnrolmentTokenResponse>('/tokens', b, R()),
  policies: {
    list: (signal?: Sig) => http.get<T.Policy[] | null>('/policies', R(signal)),
    put: (name: string, p: T.Policy) => http.put<void>(`/policies/${seg(name)}`, p, R()),
    remove: (name: string) => http.del<void>(`/policies/${seg(name)}`, undefined, R()),
  },
  assignments: {
    list: (signal?: Sig) => http.get<T.AssignmentRow[] | null>('/assignments', R(signal)),
    put: (a: T.AssignmentRow) => http.put<void>('/assignments', a, R()),
    remove: (a: T.AssignmentRow) => http.del<void>('/assignments', a, R()),
  },

  // ---- objects and snapshots (R)
  objects: {
    list: (signal?: Sig) => http.get<T.ObjectRow[] | null>('/objects', R(signal)),
    setRetention: (group: string, b: T.SetRetentionRequest) => http.put<void>(`/objects/${seg(group)}/retention`, b, R()),
    setNotes: (group: string, b: T.SetNotesRequest) => http.put<void>(`/objects/${seg(group)}/notes`, b, R()),
    transfer: (group: string, b: T.TransferRequest) => http.post<void>(`/objects/${seg(group)}/transfer`, b, R()),
  },
  snapshots: {
    list: (group?: string, signal?: Sig) => http.get<T.SnapshotRef[] | null>('/snapshots', { ...R(signal), query: { group } }),
    setProtected: (group: string, snap: string, b: T.SetProtectedRequest) => http.put<void>(`/snapshots/${seg(group)}/${seg(snap)}/protected`, b, R()),
    setNotes: (group: string, snap: string, b: T.SetNotesRequest) => http.put<void>(`/snapshots/${seg(group)}/${seg(snap)}/notes`, b, R()),
    forget: (group: string, snap: string) => http.del<void>(`/snapshots/${seg(group)}/${seg(snap)}`, undefined, R()),
    verifications: (group: string, snap: string, signal?: Sig) =>
      http.get<T.SnapshotVerification[] | null>(`/snapshots/${seg(group)}/${seg(snap)}/verifications`, R(signal)),
    /** Raw ENCRYPTED bytes; decrypt in the browser (the key never leaves the page). */
    index: (group: string, snap: string, signal?: Sig) => request<ArrayBuffer>('GET', `/snapshots/${seg(group)}/${seg(snap)}/index`, { ...R(signal), raw: true }),
    manifest: (group: string, snap: string, signal?: Sig) => request<ArrayBuffer>('GET', `/snapshots/${seg(group)}/${seg(snap)}/manifest`, { ...R(signal), raw: true }),
    chunk: (id: string, signal?: Sig) => request<ArrayBuffer>('GET', `/chunks/${seg(id)}`, { ...R(signal), raw: true }),
  },

  // ---- maintenance and verification (R)
  maintenance: {
    get: (signal?: Sig) => http.get<T.MaintenanceView>('/maintenance', R(signal)),
    put: (b: T.MaintenanceUpdate) => http.put<void>('/maintenance', b, R()),
    runs: (limit = 50, signal?: Sig) => http.get<T.RunRow[] | null>('/runs', { ...R(signal), query: { limit } }),
    prune: (dryRun: boolean) => http.post<T.PruneResult>('/prune', undefined, { ...R(), query: { dry_run: dryRun ? 1 : undefined, wait: dryRun ? undefined : 1 } }),
    gc: (dryRun: boolean, graceHours?: number) => http.post<T.GCStats>('/gc', undefined, { ...R(), query: { dry_run: dryRun ? 1 : undefined, grace_hours: graceHours, wait: 1 } }),
    verify: (b: T.VerifyRequest) => http.post<T.VerifyResponse>('/verify', b, { ...R(), query: { wait: 1 } }),
    reconcile: () => http.post<T.ReconcileResult>('/reconcile', undefined, { ...R(), query: { wait: 1 } }),
    corrupt: (healed = false, signal?: Sig) => http.get<T.CorruptChunk[] | null>('/corrupt', { ...R(signal), query: { healed: healed ? 1 : undefined } }),
    clearEscrow: () => http.del<void>('/escrow', undefined, R()),
  },

  // ---- sync (R)
  sync: {
    list: (signal?: Sig) => http.get<T.SyncJob[] | null>('/sync', R(signal)),
    create: (b: T.SyncCreateRequest) => http.post<void>('/sync', b, R()),
    get: (name: string, signal?: Sig) => http.get<T.SyncStatus>(`/sync/${seg(name)}`, R(signal)),
    update: (name: string, b: T.SyncJobOptions) => http.put<void>(`/sync/${seg(name)}`, b, R()),
    remove: (name: string) => http.del<void>(`/sync/${seg(name)}`, undefined, R()),
    run: (name: string) => http.post<T.SyncRunResponse>(`/sync/${seg(name)}/run`, undefined, { ...R(), query: { wait: 1 } }),
  },

  // ---- settings (admin)
  settings: {
    get: (signal?: Sig) => http.get<T.SettingsMap>('/settings', { signal }),
    put: (kv: T.SettingsMap) => http.put<void>('/settings', kv),
  },

  /** Planned routes (docs/api.md "Planned routes"). Shapes are drafts. */
  draft: {
    channels: {
      list: (signal?: Sig) => http.get<T.DraftNotificationChannel[]>('/notifications/channels', { signal }),
      create: (b: Partial<T.DraftNotificationChannel>) => http.post<T.DraftNotificationChannel>('/notifications/channels', b),
      update: (id: string, b: Partial<T.DraftNotificationChannel>) => http.put<void>(`/notifications/channels/${seg(id)}`, b),
      remove: (id: string) => http.del<void>(`/notifications/channels/${seg(id)}`),
      test: (id: string) => http.post<T.DraftChannelTestResult>(`/notifications/channels/${seg(id)}/test`),
    },
    rules: {
      list: (signal?: Sig) => http.get<T.DraftNotificationRule[]>('/notifications/rules', { signal }),
      create: (b: Partial<T.DraftNotificationRule>) => http.post<T.DraftNotificationRule>('/notifications/rules', b),
      update: (id: string, b: Partial<T.DraftNotificationRule>) => http.put<void>(`/notifications/rules/${seg(id)}`, b),
      remove: (id: string) => http.del<void>(`/notifications/rules/${seg(id)}`),
    },
    deliveries: (signal?: Sig) => http.get<T.DraftDelivery[]>('/notifications/deliveries', { signal }),
    alerts: {
      list: (q: { state?: string; severity?: string } = {}, signal?: Sig) => http.get<T.DraftAlert[]>('/alerts', { query: q, signal }),
      get: (id: string, signal?: Sig) => http.get<T.DraftAlert>(`/alerts/${seg(id)}`, { signal }),
      ack: (id: string) => http.post<void>(`/alerts/${seg(id)}/ack`),
    },
    silences: {
      list: (signal?: Sig) => http.get<T.DraftSilence[]>('/silences', { signal }),
      create: (b: Partial<T.DraftSilence>) => http.post<T.DraftSilence>('/silences', b),
      remove: (id: string) => http.del<void>(`/silences/${seg(id)}`),
    },
    jobs: {
      list: (q: { state?: string; host?: string } = {}, signal?: Sig) => http.get<T.DraftJob[]>('/jobs', { query: q, signal }),
      create: (b: T.DraftJobRequest) => http.post<T.DraftJob>('/jobs', b),
      get: (id: string, signal?: Sig) => http.get<T.DraftJob>(`/jobs/${seg(id)}`, { signal }),
      cancel: (id: string) => http.post<void>(`/jobs/${seg(id)}/cancel`),
      delivery: (id: string, signal?: Sig) => request<ArrayBuffer>('GET', `/jobs/${seg(id)}/delivery`, { signal, raw: true }),
    },
    restoreTests: {
      list: (signal?: Sig) => http.get<T.DraftRestoreTest[]>('/restore-tests', { signal }),
      run: (b: T.DraftRestoreTestRequest) => http.post<T.DraftRestoreTest>('/restore-tests/run', b),
    },
    certificates: {
      get: (signal?: Sig) => http.get<T.DraftCertificateInfo>('/certificates', { signal }),
      renewHost: (name: string) => http.post<void>(`/certificates/hosts/${seg(name)}/renew-now`),
    },
  },
}

/** Normalise the `T[] | null` lists the server returns for empty results. */
export const list = <X>(v: X[] | null | undefined): X[] => v ?? []
