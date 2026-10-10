// Policy assignment logic for the console: the assignment tree (host default -> project -> object)
// and a client-side mirror of the server's retention-policy resolution (service.ResolvePolicy +
// retentionPolicyFor) used to predict what deleting a policy or an assignment changes. The server's
// own answer (GET /policies/effective) is what the pages display; this mirror only previews.
import type { AssignmentRow } from '@/api/types'
import type { EffectivePolicy } from '@/api/sections/storage'

/** "instance/web/root" and "instance/web" -> "web"; "volume/data" -> "data" (service.ownerName). */
export function ownerName(name: string): string {
  if (name.startsWith('instance/')) {
    const n = name.slice('instance/'.length)
    return n.endsWith('/root') ? n.slice(0, -'/root'.length) : n
  }
  if (name.startsWith('volume/')) return name.slice('volume/'.length)
  return name
}

export type Source = 'object' | 'project' | 'host' | 'pinned' | 'none'

/** Which assignment of ONE host governs project/owner (object > project > host default). */
export function resolve(as: readonly AssignmentRow[], project: string, owner: string, pinned = ''): { policy: string; source: Source } {
  let best: AssignmentRow | null = null
  let rank = -1
  for (const a of as) {
    let r = -1
    if (a.project === '' && a.object === '') r = 0
    else if (a.project === project && a.object === '') r = 1
    else if (a.project === project && a.object === owner) r = 2
    if (r > rank) { best = a; rank = r }
  }
  if (best && rank >= 0) return { policy: best.policy, source: (['host', 'project', 'object'] as const)[rank] }
  if (pinned) return { policy: pinned, source: 'pinned' }
  return { policy: '', source: 'none' }
}

export const sourceLabel: Record<Source, string> = {
  object: 'object assignment', project: 'project assignment', host: 'host default', pinned: 'pinned at first backup', none: 'no policy',
}

export interface OwnerNode { owner: string; assigned: string; entries: EffectivePolicy[]; policy: string; source: Source }
export interface ProjectNode { project: string; assigned: string; owners: OwnerNode[] }
export interface HostNode { host: string; assigned: string; projects: ProjectNode[]; objects: number }

/** The assignment tree for the matrix editor: every host, its projects and objects, with assignments. */
export function assignmentTree(hosts: readonly string[], effective: readonly EffectivePolicy[], assignments: readonly AssignmentRow[]): HostNode[] {
  const names = new Set<string>([...hosts, ...effective.map((e) => e.host).filter(Boolean), ...assignments.map((a) => a.host)])
  const out: HostNode[] = []
  for (const host of [...names].sort((a, b) => a.localeCompare(b))) {
    const as = assignments.filter((a) => a.host === host)
    const find = (p: string, o: string) => as.find((a) => a.project === p && a.object === o)?.policy ?? ''
    const projects = new Map<string, Map<string, EffectivePolicy[]>>()
    for (const e of effective.filter((x) => x.host === host)) {
      const owner = e.owner || ownerName(e.name)
      if (!projects.has(e.project)) projects.set(e.project, new Map())
      const m = projects.get(e.project)!
      if (!m.has(owner)) m.set(owner, [])
      m.get(owner)!.push(e)
    }
    // assignments for projects/objects that have no object (yet)
    for (const a of as) {
      if (!a.project) continue
      if (!projects.has(a.project)) projects.set(a.project, new Map())
      if (a.object && !projects.get(a.project)!.has(a.object)) projects.get(a.project)!.set(a.object, [])
    }
    const pnodes: ProjectNode[] = [...projects.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([project, owners]) => ({
      project,
      assigned: find(project, ''),
      owners: [...owners.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([owner, entries]) => {
        const r = entries[0] ? { policy: entries[0].policy, source: entries[0].source } : resolve(as, project, owner)
        return { owner, assigned: find(project, owner), entries, policy: r.policy, source: r.source }
      }),
    }))
    out.push({ host, assigned: find('', ''), projects: pnodes, objects: effective.filter((x) => x.host === host).length })
  }
  return out
}

export interface DeleteImpact {
  assignments: number
  objects: number
  /** policy name ('' = none: keeps everything) -> objects that would fall back to it. */
  fallback: Record<string, number>
}

/**
 * What deleting a policy changes (its assignments go with it): the objects it governs now and what
 * governs them afterwards. An object whose pinned policy is the deleted one keeps everything.
 */
export function deleteImpact(name: string, effective: readonly EffectivePolicy[], assignments: readonly AssignmentRow[]): DeleteImpact {
  const remaining = assignments.filter((a) => a.policy !== name)
  const fallback: Record<string, number> = {}
  let objects = 0
  for (const e of effective) {
    if (e.policy !== name) continue
    objects++
    const r = resolve(remaining.filter((a) => a.host === e.host), e.project, e.owner || ownerName(e.name), e.pinned_policy)
    const p = r.policy === name ? '' : r.policy
    fallback[p] = (fallback[p] ?? 0) + 1
  }
  return { assignments: assignments.filter((a) => a.policy === name).length, objects, fallback }
}
