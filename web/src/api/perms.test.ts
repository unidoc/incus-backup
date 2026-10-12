// @vitest-environment node
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { ROUTE_ROLES, PLANNED_ROUTE_ROLES, atLeast, can, roleFor } from './perms'

const permsGo = readFileSync(fileURLToPath(new URL('../../../internal/server/mgmt/perms.go', import.meta.url)), 'utf8')
const roleOf: Record<string, string> = { rd: 'readonly', rs: 'restore', op: 'operator', ad: 'admin' }

function parsePermsGo(): Record<string, string> {
  const out: Record<string, string> = {}
  const re = /"([A-Z]+) \/api\/v1([^"]*)":\s*(rd|rs|op|ad)\b/g
  for (const m of permsGo.matchAll(re)) out[`${m[1]} ${m[2]}`] = roleOf[m[3]]
  return out
}

describe('perms mirror', () => {
  const server = parsePermsGo()

  it('parses a plausible number of routes from perms.go', () => {
    expect(Object.keys(server).length).toBeGreaterThan(60)
  })

  it('has exactly the routes and roles of internal/server/mgmt/perms.go', () => {
    expect(ROUTE_ROLES).toEqual(server)
  })

  it('lists no planned route that the server already registers (move it to ROUTE_ROLES)', () => {
    const shipped = Object.keys(PLANNED_ROUTE_ROLES).filter((r) => r in server)
    expect(shipped).toEqual([])
  })

  it('orders roles and fails closed for unknown routes', () => {
    expect(atLeast('admin', 'operator')).toBe(true)
    expect(atLeast('restore', 'operator')).toBe(false)
    expect(atLeast(undefined, 'readonly')).toBe(false)
    expect(can('operator', 'PUT /policies/{name}')).toBe(true)
    expect(can('operator', 'DELETE /policies/{name}')).toBe(false)
    expect(roleFor('GET /nonexistent')).toBe('admin')
  })
})
