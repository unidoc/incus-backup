// What each role can do (docs/api.md "Roles"; enforced by internal/server/mgmt/perms.go).
import type { Role } from '@/api/types'

export const ROLE_INFO: { role: Role; label: string; can: string[] }[] = [
  { role: 'readonly', label: 'Read-only', can: ['See hosts, objects, snapshots, policies, alerts, capacity and certificates', 'No credentials, users, audit log or server settings'] },
  { role: 'restore', label: 'Restore', can: ['Everything read-only can', 'Browse snapshots and download the encrypted data to decrypt in the browser', 'Start restores'] },
  { role: 'operator', label: 'Operator', can: ['Everything restore can', 'Run verify, sync and backup-now jobs', 'Edit policies, assignments, schedules, notes and protection', 'Acknowledge and silence alerts; see channels, rules and the delivery log', 'Enable and disable hosts, probe repositories'] },
  { role: 'admin', label: 'Administrator', can: ['Everything operator can', 'Repositories, credentials, enrolment and onboarding', 'Prune, GC, reconcile, forget snapshots, maintenance modes', 'Users, every API token, the audit log, server settings', 'Notification channels and rules, certificates and CA rotation'] },
]

/** A random password of four groups of five characters (about 100 bits), easy to type. */
export function generatePassword(): string {
  const alphabet = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789'
  const out: string[] = []
  const buf = new Uint32Array(20)
  crypto.getRandomValues(buf)
  for (let i = 0; i < 20; i++) out.push(alphabet[buf[i] % alphabet.length])
  return [0, 5, 10, 15].map((i) => out.slice(i, i + 5).join('')).join('-')
}
