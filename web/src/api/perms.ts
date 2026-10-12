// The role each API route requires: a mirror of internal/server/mgmt/perms.go (the server's table is
// authoritative and enforced; this copy only decides what the UI shows or disables).
// perms.test.ts parses perms.go and fails when the two disagree, so this file cannot drift silently.
import type { Role } from './types'

export const ROLES: readonly Role[] = ['readonly', 'restore', 'operator', 'admin'] as const
const R: Role = 'readonly', S: Role = 'restore', O: Role = 'operator', A: Role = 'admin'

export function roleRank(r: Role | '' | undefined | null): number {
  return r ? ROLES.indexOf(r) : -1
}

/** True when `have` is at least `need`. */
export function atLeast(have: Role | '' | undefined | null, need: Role): boolean {
  return roleRank(have) >= roleRank(need)
}

/** "METHOD /path" (pattern as registered, without /api/v1) -> minimum role. */
export const ROUTE_ROLES: Record<string, Role> = {
  'GET /ping': R,
  'GET /events': R,
  'GET /status': R,
  'GET /capacity': R,
  'GET /capacity/series': R,
  'GET /overview': R,
  'PUT /repositories/{name}/mode': A,
  'GET /repositories': R,
  'POST /repositories': A,
  'POST /repositories/attach': A,
  'POST /repositories/{name}/probe': O,
  'GET /credentials': A,
  'POST /credentials': A,
  'PUT /credentials/{name}': A,
  'DELETE /credentials/{name}': A,
  'GET /hosts': R,
  'POST /hosts/{name}/{scope}/{action}': O,
  'POST /tokens': A,
  'GET /policies': R,
  'PUT /policies/{name}': O,
  'DELETE /policies/{name}': A,
  'GET /assignments': R,
  'PUT /assignments': O,
  'DELETE /assignments': O,
  'GET /objects': R,
  'PUT /objects/{group}/retention': O,
  'POST /objects/{group}/transfer': A,
  'PUT /objects/{group}/notes': O,
  'GET /snapshots': R,
  'PUT /snapshots/{group}/{snap}/protected': O,
  'PUT /snapshots/{group}/{snap}/notes': O,
  'GET /snapshots/{group}/{snap}/verifications': R,
  'DELETE /snapshots/{group}/{snap}': A,
  'GET /maintenance': R,
  'PUT /maintenance': A,
  'GET /runs': R,
  'POST /prune': A,
  'POST /gc': A,
  'POST /verify': O,
  'POST /reconcile': A,
  'GET /corrupt': R,
  'DELETE /escrow': A,
  'GET /sync': R,
  'POST /sync': A,
  'PUT /sync/{name}': O,
  'POST /sync/{name}/run': O,
  'GET /sync/{name}': R,
  'DELETE /sync/{name}': A,
  'POST /session': R,
  'GET /session': R,
  'DELETE /session': R,
  'GET /setup': R,
  'POST /setup': R,
  'GET /users': A,
  'POST /users': A,
  'PUT /users/{name}': A,
  'DELETE /users/{name}': A,
  'POST /users/{name}/password': A,
  'DELETE /users/{name}/totp': A,
  'POST /me/password': R,
  'POST /me/totp': R,
  'POST /me/totp/confirm': R,
  'DELETE /me/totp': R,
  'GET /me/sessions': R,
  'DELETE /me/sessions/{id}': R,
  'GET /me/tokens': R,
  'POST /me/tokens': R,
  'DELETE /me/tokens/{id}': R,
  'GET /audit': A,
  'GET /settings': A,
  'PUT /settings': A,
  'POST /onboarding': A,
  'GET /snapshots/{group}/{snap}/index': S,
  'GET /snapshots/{group}/{snap}/manifest': S,
  'GET /chunks/{id}': S,
  'GET /jobs': R, 'POST /jobs': S, 'GET /jobs/{id}': R, 'POST /jobs/{id}/cancel': O, 'GET /jobs/{id}/delivery': S, 'GET /restore-tests': R, 'POST /restore-tests/run': O, 'GET /certificates': R, 'PUT /certificates/sans': A, 'POST /certificates/ca/rotate': A, 'POST /certificates/ca/promote': A, 'GET /notifications/channels': O, 'POST /notifications/channels': A, 'PUT /notifications/channels/{id}': A, 'DELETE /notifications/channels/{id}': A, 'POST /notifications/channels/{id}/test': A, 'GET /notifications/rules': O, 'POST /notifications/rules': A, 'PUT /notifications/rules/{id}': A, 'DELETE /notifications/rules/{id}': A, 'GET /alerts': R, 'GET /alerts/{id}': R, 'POST /alerts/{id}/ack': O, 'GET /silences': R, 'POST /silences': O, 'DELETE /silences/{id}': O, 'GET /notifications/deliveries': O, 'POST /notifications/deliveries/{id}/retry': A, 'GET /notifications/settings': O, 'PUT /notifications/settings': A, 'PUT /notifications/conditions/{type}': A,
}

/**
 * Planned routes (docs/api.md "Planned routes"): not in perms.go yet. Roles follow the contract text;
 * perms.test.ts reports when one of these appears in perms.go so it can move up into ROUTE_ROLES.
 */
export const PLANNED_ROUTE_ROLES: Record<string, Role> = {
  'POST /certificates/hosts/{name}/renew-now': A,
}

/** Role needed for a route; unknown routes need admin (fail closed in the UI too). */
export function roleFor(route: string): Role {
  return ROUTE_ROLES[route] ?? PLANNED_ROUTE_ROLES[route] ?? A
}

/** Can a user with `role` call `route` ("PUT /policies/{name}")? */
export function can(role: Role | '' | undefined | null, route: string): boolean {
  return atLeast(role, roleFor(route))
}
