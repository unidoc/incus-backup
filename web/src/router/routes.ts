// Every page of the console. Section agents: replace the placeholder view file a route points to; keep
// the route name, path and meta (the sidebar, breadcrumbs, command palette and role checks read them).
//
// meta:
//   title    page title (document title, breadcrumb, palette entry)
//   role     minimum role to open the page (the server enforces again on every call)
//   section  sidebar entry that is highlighted for this page (its route name)
//   parent   route name of the breadcrumb parent (when the path does not say it)
//   public   no sign-in needed
//   bare     rendered without the app shell (sign-in, setup)
//   apis     the API routes the page uses (shown by the placeholder; documentation for section agents)
//   planned  the backend routes do not exist yet (docs/api.md "Planned routes")
import type { RouteRecordRaw } from 'vue-router'
import type { Role } from '@/api/types'

declare module 'vue-router' {
  interface RouteMeta {
    title?: string
    role?: Role
    section?: string
    parent?: string
    public?: boolean
    bare?: boolean
    apis?: string[]
    planned?: boolean
    /** View file under src/views (for placeholders and docs). */
    file?: string
  }
}

// Route-level lazy loading: every view is its own chunk.
const pages = import.meta.glob('../views/**/*.vue')
const v = (file: string) => {
  const load = pages[`../views/${file}.vue`]
  if (!load) throw new Error(`routes.ts: no view file src/views/${file}.vue`)
  return load
}

export const routes: RouteRecordRaw[] = [
  // ---- sign-in, first run
  { path: '/login', name: 'login', component: v('auth/Login'), meta: { title: 'Sign in', public: true, bare: true, file: 'auth/Login.vue' } },
  { path: '/setup', name: 'setup', component: v('auth/Setup'), meta: { title: 'First-run setup', public: true, bare: true, file: 'auth/Setup.vue' } },
  { path: '/enrol-2fa', name: 'enrol-2fa', component: v('auth/Enrol2FA'), meta: { title: 'Set up two-factor authentication', bare: true, role: 'readonly', file: 'auth/Enrol2FA.vue', apis: ['GET /me/webauthn', 'POST /me/webauthn/register/begin', 'POST /me/webauthn/register/finish', 'POST /me/totp', 'POST /me/totp/confirm'] } },
  { path: '/change-password', name: 'change-password', component: v('auth/ChangePassword'), meta: { title: 'Choose a new password', bare: true, role: 'readonly', file: 'auth/ChangePassword.vue' } },

  // ---- overview
  { path: '/', name: 'dashboard', component: v('Dashboard'), meta: { title: 'Overview', role: 'readonly', file: 'Dashboard.vue', apis: ['GET /overview', 'GET /capacity/series'] } },

  // ---- infrastructure
  { path: '/hosts', name: 'hosts', component: v('hosts/HostList'), meta: { title: 'Hosts', role: 'readonly', file: 'hosts/HostList.vue', apis: ['GET /hosts', 'GET /overview', 'POST /hosts/{name}/{scope}/{action}'] } },
  { path: '/hosts/onboard', name: 'host-onboard', component: v('hosts/Onboarding'), meta: { title: 'Add a host', role: 'admin', section: 'hosts', parent: 'hosts', file: 'hosts/Onboarding.vue', apis: ['POST /onboarding', 'GET /policies'] } },
  { path: '/hosts/:name', name: 'host', component: v('hosts/HostDetail'), props: true, meta: { title: 'Host', role: 'readonly', section: 'hosts', parent: 'hosts', file: 'hosts/HostDetail.vue', apis: ['GET /hosts', 'GET /status', 'GET /objects', 'GET /assignments'] } },
  { path: '/objects', name: 'objects', component: v('backups/ObjectList'), meta: { title: 'Objects', role: 'readonly', file: 'backups/ObjectList.vue', apis: ['GET /objects', 'GET /policies'] } },
  { path: '/objects/:group', name: 'object', component: v('backups/ObjectDetail'), props: true, meta: { title: 'Object', role: 'readonly', section: 'objects', parent: 'objects', file: 'backups/ObjectDetail.vue', apis: ['GET /objects', 'GET /snapshots?group=', 'PUT /objects/{group}/retention', 'PUT /objects/{group}/notes', 'POST /objects/{group}/transfer'] } },

  // ---- protection
  { path: '/snapshots', name: 'snapshots', component: v('backups/SnapshotList'), meta: { title: 'Snapshots', role: 'readonly', file: 'backups/SnapshotList.vue', apis: ['GET /snapshots'] } },
  { path: '/snapshots/:group/:snap', name: 'snapshot', component: v('backups/SnapshotDetail'), props: true, meta: { title: 'Snapshot', role: 'readonly', section: 'snapshots', parent: 'snapshots', file: 'backups/SnapshotDetail.vue', apis: ['GET /snapshots?group=', 'GET /snapshots/{group}/{snap}/verifications', 'PUT /snapshots/{group}/{snap}/protected', 'PUT /snapshots/{group}/{snap}/notes', 'DELETE /snapshots/{group}/{snap}'] } },
  { path: '/snapshots/:group/:snap/browse', name: 'snapshot-browse', component: v('backups/Browser'), props: true, meta: { title: 'Browse files', role: 'restore', section: 'snapshots', parent: 'snapshot', file: 'backups/Browser.vue', apis: ['GET /snapshots/{group}/{snap}/manifest', 'GET /snapshots/{group}/{snap}/index', 'GET /chunks/{id}'] } },
  { path: '/restore', name: 'restore', component: v('restore/Restore'), meta: { title: 'Restore', role: 'restore', file: 'restore/Restore.vue', apis: ['GET /objects', 'GET /snapshots', 'POST /jobs'], planned: true } },
  { path: '/restore/tests', name: 'restore-tests', component: v('jobs/RestoreTests'), meta: { title: 'Restore tests', role: 'readonly', section: 'restore', parent: 'restore', file: 'jobs/RestoreTests.vue', apis: ['GET /restore-tests', 'POST /restore-tests/run'], planned: true } },
  { path: '/verify', name: 'verify', component: v('verify/Verify'), meta: { title: 'Verification', role: 'readonly', file: 'verify/Verify.vue', apis: ['GET /runs', 'GET /corrupt', 'POST /verify', 'GET /maintenance'] } },

  // ---- operations
  { path: '/jobs', name: 'jobs', component: v('jobs/JobList'), meta: { title: 'Jobs', role: 'readonly', file: 'jobs/JobList.vue', apis: ['GET /jobs', 'GET /runs'], planned: true } },
  { path: '/jobs/:id', name: 'job', component: v('jobs/JobDetail'), props: true, meta: { title: 'Job', role: 'readonly', section: 'jobs', parent: 'jobs', file: 'jobs/JobDetail.vue', apis: ['GET /jobs/{id}', 'POST /jobs/{id}/cancel'], planned: true } },
  { path: '/tasks', name: 'tasks', component: v('ops/Tasks'), meta: { title: 'Tasks', role: 'readonly', file: 'ops/Tasks.vue', apis: ['GET /tasks', 'GET /tasks/{id}', 'POST /tasks/{id}/cancel'] } },
  { path: '/sync', name: 'sync', component: v('sync/SyncList'), meta: { title: 'Sync', role: 'readonly', file: 'sync/SyncList.vue', apis: ['GET /sync', 'POST /sync', 'POST /sync/{name}/run'] } },
  { path: '/sync/:name', name: 'sync-job', component: v('sync/SyncDetail'), props: true, meta: { title: 'Sync job', role: 'readonly', section: 'sync', parent: 'sync', file: 'sync/SyncDetail.vue', apis: ['GET /sync/{name}', 'PUT /sync/{name}', 'DELETE /sync/{name}', 'POST /sync/{name}/run'] } },
  { path: '/alerts', name: 'alerts', component: v('alerts/AlertList'), meta: { title: 'Alerts', role: 'readonly', file: 'alerts/AlertList.vue', apis: ['GET /alerts', 'POST /alerts/{id}/ack'] } },
  { path: '/alerts/:id(\\d+)', name: 'alert', component: v('alerts/AlertDetail'), meta: { title: 'Alert', role: 'readonly', section: 'alerts', parent: 'alerts', file: 'alerts/AlertDetail.vue', apis: ['GET /alerts/{id}', 'POST /alerts/{id}/ack', 'POST /silences'] } },
  { path: '/traffic', name: 'traffic', component: v('ops/Traffic'), meta: { title: 'Traffic control', role: 'readonly', file: 'ops/Traffic.vue', apis: ['GET /traffic/rules', 'POST /traffic/rules', 'PUT /traffic/rules/{name}', 'DELETE /traffic/rules/{name}', 'GET /traffic/current'] } },
  { path: '/alerts/silences', name: 'alert-silences', component: v('alerts/Silences'), meta: { title: 'Silences', role: 'readonly', section: 'alerts', parent: 'alerts', file: 'alerts/Silences.vue', apis: ['GET /silences', 'POST /silences', 'DELETE /silences/{id}'] } },

  // ---- storage
  { path: '/repositories', name: 'repositories', component: v('repos/RepoList'), meta: { title: 'Repositories', role: 'readonly', file: 'repos/RepoList.vue', apis: ['GET /repositories', 'GET /overview'] } },
  { path: '/repositories/new', name: 'repository-new', component: v('repos/RepoCreate'), meta: { title: 'Add a repository', role: 'admin', section: 'repositories', parent: 'repositories', file: 'repos/RepoCreate.vue', apis: ['POST /repositories', 'POST /repositories/attach', 'GET /credentials'] } },
  { path: '/repositories/:name', name: 'repository', component: v('repos/RepoDetail'), props: true, meta: { title: 'Repository', role: 'readonly', section: 'repositories', parent: 'repositories', file: 'repos/RepoDetail.vue', apis: ['GET /repositories', 'GET /status', 'GET /maintenance', 'PUT /repositories/{name}/mode', 'POST /repositories/{name}/probe'] } },
  { path: '/capacity', name: 'capacity', component: v('storage/Capacity'), meta: { title: 'Capacity', role: 'readonly', file: 'storage/Capacity.vue', apis: ['GET /capacity', 'GET /capacity/series'] } },

  // ---- configuration
  { path: '/policies', name: 'policies', component: v('policies/PolicyList'), meta: { title: 'Policies', role: 'readonly', file: 'policies/PolicyList.vue', apis: ['GET /policies', 'GET /assignments'] } },
  { path: '/policies/new', name: 'policy-new', component: v('policies/PolicyEdit'), meta: { title: 'New policy', role: 'operator', section: 'policies', parent: 'policies', file: 'policies/PolicyEdit.vue', apis: ['PUT /policies/{name}'] } },
  { path: '/policies/:name', name: 'policy', component: v('policies/PolicyEdit'), props: true, meta: { title: 'Policy', role: 'readonly', section: 'policies', parent: 'policies', file: 'policies/PolicyEdit.vue', apis: ['GET /policies', 'PUT /policies/{name}', 'DELETE /policies/{name}', 'PUT /assignments'] } },
  { path: '/notifications', name: 'notifications', component: v('alerts/Channels'), meta: { title: 'Notifications', role: 'operator', file: 'alerts/Channels.vue', apis: ['GET /notifications/channels', 'POST /notifications/channels', 'POST /notifications/channels/{id}/test', 'GET /notifications/deliveries'] } },
  { path: '/notifications/rules', name: 'notification-rules', component: v('alerts/Rules'), meta: { title: 'Notification rules', role: 'operator', section: 'notifications', parent: 'notifications', file: 'alerts/Rules.vue', apis: ['GET /notifications/rules', 'POST /notifications/rules', 'POST /notifications/simulate'] } },
  { path: '/notifications/deliveries', name: 'notification-deliveries', component: v('alerts/Deliveries'), meta: { title: 'Delivery log', role: 'operator', section: 'notifications', parent: 'notifications', file: 'alerts/Deliveries.vue', apis: ['GET /notifications/deliveries', 'POST /notifications/deliveries/{id}/retry'] } },
  { path: '/notifications/settings', name: 'notification-settings', component: v('alerts/NotifySettings'), meta: { title: 'Alert conditions', role: 'operator', section: 'notifications', parent: 'notifications', file: 'alerts/NotifySettings.vue', apis: ['GET /notifications/settings', 'PUT /notifications/settings', 'PUT /notifications/conditions/{type}'] } },
  { path: '/settings/users', name: 'users', component: v('settings/Users'), meta: { title: 'Users', role: 'admin', file: 'settings/Users.vue', apis: ['GET /users', 'POST /users', 'PUT /users/{name}', 'DELETE /users/{name}', 'POST /users/{name}/password', 'DELETE /users/{name}/totp'] } },
  { path: '/settings/tokens', name: 'tokens', component: v('settings/Tokens'), meta: { title: 'API tokens', role: 'admin', section: 'users', parent: 'users', file: 'settings/Tokens.vue', apis: ['GET /api-tokens', 'DELETE /api-tokens/{id}'] } },
  { path: '/settings/authentication', name: 'authentication', component: v('settings/Authentication'), meta: { title: 'Authentication', role: 'admin', file: 'settings/Authentication.vue', apis: ['GET /auth/settings', 'PUT /auth/settings', 'GET /oidc/providers', 'POST /oidc/providers', 'PUT /oidc/providers/{name}', 'DELETE /oidc/providers/{name}', 'POST /oidc/providers/{name}/test'] } },
  { path: '/settings/certificates', name: 'certificates', component: v('settings/Certificates'), meta: { title: 'Certificates', role: 'readonly', file: 'settings/Certificates.vue', apis: ['GET /certificates', 'PUT /certificates/sans', 'POST /certificates/ca/rotate', 'POST /certificates/ca/promote'] } },
  { path: '/settings', name: 'settings', component: v('settings/Server'), meta: { title: 'Server settings', role: 'admin', file: 'settings/Server.vue', apis: ['GET /settings', 'PUT /settings', 'GET /about', 'GET /setup-checklist'] } },
  { path: '/settings/api', name: 'api-explorer', component: v('settings/ApiExplorer'), meta: { title: 'API', role: 'readonly', file: 'settings/ApiExplorer.vue', apis: ['GET /openapi.json'] } },
  { path: '/settings/system', name: 'system-status', component: v('ops/SystemStatus'), meta: { title: 'Server status', role: 'readonly', file: 'ops/SystemStatus.vue', apis: ['GET /system/status', 'GET /system/samples', 'POST /system/support-report', 'GET /system/update', 'POST /system/update/check'] } },
  { path: '/settings/logs', name: 'server-logs', component: v('ops/Logs'), meta: { title: 'Server log', role: 'admin', section: 'system-status', parent: 'system-status', file: 'ops/Logs.vue', apis: ['GET /system/logs'] } },
  { path: '/audit', name: 'audit', component: v('settings/Audit'), meta: { title: 'Audit log', role: 'admin', file: 'settings/Audit.vue', apis: ['GET /audit'] } },

  // ---- personal
  { path: '/profile', name: 'profile', component: v('Profile'), meta: { title: 'Profile', role: 'readonly', file: 'Profile.vue', apis: ['POST /me/password', 'POST /me/totp', 'POST /me/totp/confirm', 'DELETE /me/totp', 'GET /me/sessions', 'DELETE /me/sessions/{id}', 'GET /me/tokens', 'POST /me/tokens', 'DELETE /me/tokens/{id}'] } },

  // ---- hidden
  { path: '/_styleguide', name: 'styleguide', component: v('StyleGuide'), meta: { title: 'Style guide', role: 'readonly', file: 'StyleGuide.vue' } },
  { path: '/:pathMatch(.*)*', name: 'not-found', component: v('NotFound'), meta: { title: 'Page not found', role: 'readonly', file: 'NotFound.vue' } },
]
