// Host jobs and restore tests: labels, parameter summaries, schedule periods and the "restore
// confidence" roll-up. Pure logic, unit-tested in jobs.test.ts.
import type { Job, RestoreTest } from '@/api/sections/protect'
import { parseSchedule } from '@/lib/format'

export const JOB_LABEL: Record<string, string> = {
  backup_now: 'Back up now',
  restore_test: 'Restore test',
  deep_verify: 'Deep verification',
  restore_instance: 'Restore instance',
  restore_volume: 'Restore volume',
  guest_files: 'Files from a VM disk',
  fs_extract: 'Files from a filesystem',
}

export const JOB_HELP: Record<string, string> = {
  backup_now: 'Backs up the selection now on its host. Does not move the policy schedule.',
  restore_test: 'Proves that a snapshot restores, without touching the original object.',
  deep_verify: 'The host downloads, decrypts and checks every chunk of the last snapshots with its own key.',
  restore_instance: 'Recreates an instance (configuration and volumes) in Incus. Never overwrites an existing instance.',
  restore_volume: 'Restores a stored disk or filesystem into a NEW custom volume. Never overwrites an existing volume.',
  guest_files: 'Boots a disposable recovery VM on the host, copies the requested paths out of the backed-up disk and delivers them sealed to this browser.',
  fs_extract: 'Extracts paths of a filesystem snapshot on the host and delivers them sealed to this browser.',
}

export const jobLabel = (t: string) => JOB_LABEL[t] ?? t

/** One line describing what a job acts on, from its canonical params (never prints key material). */
export function jobSubject(j: Pick<Job, 'type' | 'params'>): string {
  const p = (j.params ?? {}) as Record<string, unknown>
  const s = (k: string) => (typeof p[k] === 'string' && p[k] ? String(p[k]) : '')
  const at = s('snapshot') ? ` @ ${s('snapshot')}` : ''
  switch (j.type) {
    case 'backup_now':
      return [s('project') && `project ${s('project')}`, s('object'), s('policy') && `policy ${s('policy')}`].filter(Boolean).join(', ') || 'everything'
    case 'restore_test': return `${s('project')}/${s('object')}${at} (${s('mode') || 'deep'})`
    case 'deep_verify': {
      const last = Number(p.last ?? 1)
      const what = s('object') ? `${s('project')}/${s('object')}` : s('project') ? `project ${s('project')}` : 'every object'
      return `${what}, last ${last} snapshot${last === 1 ? '' : 's'}`
    }
    case 'restore_instance': {
      const dst = `${s('as_project') || s('project')}/${s('as_name') || s('instance')}`
      return `${s('project')}/${s('instance')}${at} → ${dst}`
    }
    case 'restore_volume': return `${s('project')}/${s('object')}${at} → ${s('as_project') || s('project')}/${s('as_name')} on ${s('pool')}`
    case 'guest_files':
    case 'fs_extract': {
      const paths = Array.isArray(p.paths) ? (p.paths as string[]) : []
      return `${s('project')}/${s('object')}${at}: ${paths.length === 1 ? paths[0] : `${paths.length} paths`}`
    }
  }
  return ''
}

/** Params for display: the recipient public key is not secret, but long and useless to read. */
export function displayParams(params: Record<string, unknown> | null | undefined): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(params ?? {})) {
    out[k] = k === 'recipient_public_key' && typeof v === 'string' ? `${v.slice(0, 8)}… (one-time public key of the browser that asked)` : v
  }
  return out
}

/** Length of the period within which a schedule wants every object tested (restore-test coverage). */
export function schedulePeriodMs(schedule: string | undefined | null): number | null {
  const p = parseSchedule(schedule ?? '')
  if (p instanceof Error) return null
  switch (p.kind) {
    case 'manual': return null
    case 'every': return p.everyMs ?? null
    case 'daily': return 86_400_000
    case 'weekly': return 7 * 86_400_000
    case 'monthly': return 31 * 86_400_000
  }
  return null
}

export type TestHealth = 'passed' | 'failed' | 'overdue' | 'never' | 'running' | 'unscheduled'

/** The state an operator needs per object: last result, and whether it is older than the schedule wants. */
export function testHealth(last: RestoreTest | undefined, periodMs: number | null, now = Date.now()): TestHealth {
  if (!last) return periodMs === null ? 'unscheduled' : 'never'
  if (last.state === 'running') return 'running'
  if (last.state === 'failed') return 'failed'
  const at = Date.parse(last.finished ?? last.started)
  // one period plus a day of slack (the host tests a rotating share once a day)
  if (periodMs !== null && now - at > periodMs + 86_400_000) return 'overdue'
  return 'passed'
}

export interface Confidence { objects: number; passed: number; failed: number; overdue: number; never: number; score: number | null; level: 'ok' | 'warn' | 'critical' | 'neutral' }

/** Share of objects whose latest restore test passed within its schedule. */
export function restoreConfidence(health: TestHealth[]): Confidence {
  const c = { objects: health.length, passed: 0, failed: 0, overdue: 0, never: 0 }
  for (const h of health) {
    if (h === 'passed' || h === 'running') c.passed++
    else if (h === 'failed') c.failed++
    else if (h === 'overdue') c.overdue++
    else if (h === 'never') c.never++
  }
  const scored = c.passed + c.failed + c.overdue + c.never
  const score = scored ? c.passed / scored : null
  const level = score === null ? 'neutral' : c.failed > 0 || score < 0.5 ? 'critical' : score < 0.95 ? 'warn' : 'ok'
  return { ...c, score, level }
}

/** Restore-test method the host would pick (docs/jobs.md "Method"), to preselect in "Run now". */
export function defaultMethod(kind: string | undefined): 'deep' | 'extract' | 'full' {
  return kind === 'filesystem' ? 'extract' : 'deep'
}

/** X25519 public key bytes -> the standard base64 jobspec.RecipientKey expects. */
export function keyToBase64(b: Uint8Array): string {
  let s = ''
  for (const x of b) s += String.fromCharCode(x)
  return btoa(s)
}

/** Splits a textarea of paths (one per line) into cleaned absolute guest paths or relative fs paths. */
export function parsePaths(text: string, absolute: boolean): { paths: string[]; error?: string } {
  const paths: string[] = []
  for (const raw of text.split('\n')) {
    const l = raw.trim()
    if (!l) continue
    if (l.split('/').includes('..')) return { paths, error: `"${l}": ".." is not allowed` }
    let p = l.replace(/\/+/g, '/')
    if (absolute && !p.startsWith('/')) p = '/' + p
    if (!absolute) p = p.replace(/^\/+/, '')
    if (p.length > 1) p = p.replace(/\/$/, '')
    if (absolute && p === '/') return { paths, error: '"/" is the whole disk: name the files or folders you need' }
    if (!absolute && p === '') return { paths, error: 'An empty path is not allowed (it would be the whole volume: name the folders)' }
    if (!paths.includes(p)) paths.push(p)
  }
  if (paths.length > 256) return { paths, error: `At most 256 paths per job (you entered ${paths.length})` }
  return { paths }
}
