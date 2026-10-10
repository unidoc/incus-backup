// @vitest-environment node
// The operations role table against the Go route registrations it mirrors.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { OPS_ROUTE_ROLES, canOps } from './ops'

const read = (p: string) => readFileSync(fileURLToPath(new URL(p, import.meta.url)), 'utf8')
const roleOf: Record<string, string> = { rd: 'readonly', rs: 'restore', op: 'operator', ad: 'admin' }

function goInitPerms(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const f of ['routes_tasks.go', 'routes_system.go', 'routes_lifecycle.go']) {
    for (const m of read(`../../../../internal/server/mgmt/${f}`).matchAll(/perms\["([A-Z]+) \/api\/v1([^"]*)"\]\s*=\s*(rd|rs|op|ad)\b/g)) out[`${m[1]} ${m[2]}`] = roleOf[m[3]]
  }
  return out
}

describe('ops section', () => {
  it('has exactly the routes and roles the server registers', () => {
    expect(OPS_ROUTE_ROLES).toEqual(goInitPerms())
  })
  it('checks roles', () => {
    expect(canOps('operator', 'POST /tasks/{id}/cancel')).toBe(true)
    expect(canOps('readonly', 'POST /tasks/{id}/cancel')).toBe(false)
    expect(canOps('operator', 'GET /system/logs')).toBe(false)
    expect(canOps('admin', 'DELETE /repositories/{name}')).toBe(true)
  })
})
