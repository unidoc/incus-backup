// The alert types of the notification engine (docs/notifications.md "Alert types"): what each one
// means, what to do about it, and the thresholds its condition takes (the params of
// PUT /notifications/conditions/{type}, with units and the server's defaults).

export interface ParamDef {
  key: string
  label: string
  /** Unit shown next to the number field. */
  unit: string
  /** Server default (migrations/20261008000000_initial_schema.sql). */
  def: number | string
  help: string
  /** A severity choice instead of a number. */
  severity?: boolean
  step?: number
}

export interface AlertTypeDef {
  type: string
  label: string
  group: string
  /** When it fires, in one or two sentences. */
  fires: string
  /** What to do at 3 a.m. */
  todo: string[]
  params: ParamDef[]
  /** Emitted by other parts of the server (no evaluated condition). */
  event?: boolean
}

export const ALERT_GROUPS = ['Backups', 'Hosts', 'Data integrity', 'Replication', 'Storage', 'Security and access', 'Jobs'] as const

export const ALERT_TYPES: AlertTypeDef[] = [
  { type: 'backup_missed', label: 'Backup missed', group: 'Backups',
    fires: 'An object whose policy has a schedule has no committed snapshot for interval factor × the schedule interval + grace. With the defaults a daily schedule alerts after 37 h.',
    todo: ['Open the host and check that its daemon runs and reaches the server.', 'Look at the last backup run of the host for errors.', 'Start a backup now from the object page to confirm the fix.'],
    params: [
      { key: 'interval_factor', label: 'Interval factor', unit: '× interval', def: 1.5, step: 0.1, help: 'How many schedule intervals may pass without a snapshot before the warning.' },
      { key: 'grace_minutes', label: 'Grace', unit: 'min', def: 60, help: 'Added on top, so a slow backup does not alert.' },
      { key: 'critical_factor', label: 'Critical after', unit: '× interval', def: 3, step: 0.1, help: 'Becomes critical after this many intervals (plus grace).' },
      { key: 'stale_object_days', label: 'Retired after', unit: 'days', def: 7, help: 'Objects the host has not reported for this long (deleted instances) stop alerting.' },
    ] },
  { type: 'backup_failed', label: 'Backup failed', group: 'Backups',
    fires: 'The latest finished backup run of a host failed (critical by default) or was partial (warning).',
    todo: ['Open the run on the host page and read the error of the failed objects.', 'A partial run means some objects were backed up: fix the failing ones and run again.'],
    params: [{ key: 'failed_severity', label: 'Severity of a failed run', unit: '', def: 'critical', severity: true, help: 'A partial run is always a warning.' }] },
  { type: 'restore_test_failed', label: 'Restore test failed', group: 'Backups',
    fires: 'The latest restore test of an object failed: the backup could not be restored and checked.',
    todo: ['Open the restore test and read its log.', 'Verify the snapshot; a verification failure means data damage.', 'Run the restore test again after the fix.'],
    params: [] },
  { type: 'host_offline', label: 'Host offline', group: 'Hosts',
    fires: 'A backup host has not contacted the server for a while. The daemon contacts its server every minute.',
    todo: ['Check that the host is up and the incus-backup daemon runs (rc-service incus-backup status).', 'Check the network path and the firewall to the agent port.', 'Disable the host on its page if it was retired on purpose.'],
    params: [
      { key: 'after_minutes', label: 'Warning after', unit: 'min', def: 120, help: 'Minutes without contact before the warning.' },
      { key: 'critical_after_minutes', label: 'Critical after', unit: 'min', def: 1440, help: 'Minutes without contact before it becomes critical.' },
    ] },
  { type: 'corruption', label: 'Corruption', group: 'Data integrity',
    fires: 'Snapshots of an object failed verification, or chunks are quarantined. Always critical.',
    todo: ['Open Verification and look at the affected snapshots.', 'Take a fresh backup of the object: the next backup re-uploads damaged chunks.', 'Do not prune: older snapshots may be the only good copy.'],
    params: [] },
  { type: 'maintenance_failed', label: 'Maintenance failed', group: 'Data integrity',
    fires: 'The latest run of a maintenance job (gc, verify, scrub, retention, reconcile, heal) failed.',
    todo: ['Open the repository page and read the error of the last run.', 'Run the job again once the cause is fixed.'],
    params: [] },
  { type: 'sync_failed', label: 'Sync failed', group: 'Replication',
    fires: 'The latest run of an enabled sync job failed or was partial.',
    todo: ['Open the sync job and read the error of the last run.', 'Check that the peer server is reachable and its certificate valid.'],
    params: [] },
  { type: 'sync_lag', label: 'Sync lagging', group: 'Replication',
    fires: 'A scheduled sync job has not succeeded for a while: the off-site copy falls behind.',
    todo: ['Open the sync job: is it failing, or not running at all?', 'Run it now from its page.'],
    params: [{ key: 'lag_hours', label: 'Lag allowed', unit: 'h', def: 48, help: 'Hours without a successful run before the warning.' }] },
  { type: 'capacity_warning', label: 'Capacity', group: 'Storage',
    fires: 'A filesystem datastore is nearly full, or its 30-day growth trend fills it soon. Object storage gives no free-space figure and never alerts.',
    todo: ['Open Capacity for the forecast.', 'Prune with the retention preview, then run GC to free space.', 'Grow the filesystem or add a repository.'],
    params: [
      { key: 'used_percent', label: 'Used above', unit: '%', def: 90, help: 'Warning when the filesystem is this full.' },
      { key: 'full_within_days', label: 'Full within', unit: 'days', def: 14, help: 'Warning when the trend fills it within this many days.' },
      { key: 'critical_within_days', label: 'Critical within', unit: 'days', def: 3, help: 'Critical when the trend fills it within this many days, or at 0 bytes free.' },
    ] },
  { type: 'repository_unreachable', label: 'Repository unreachable', group: 'Storage',
    fires: 'Consecutive probes of an S3 repository failed. Always critical: backups to it fail.',
    todo: ['Run the probe on the repository page: it says which check fails.', 'Check the endpoint, the credential and the bucket policy.'],
    params: [
      { key: 'probe_minutes', label: 'Probe every', unit: 'min', def: 10, help: 'How often the server probes each S3 repository.' },
      { key: 'failures', label: 'Failed probes', unit: 'in a row', def: 2, help: 'Consecutive failures before the alert.' },
    ] },
  { type: 'update_available', label: 'Update available', group: 'Security and access',
    fires: 'The opt-in release check (Settings: update_check on) found a newer incus-backup release. Informational; it resolves after the upgrade.',
    todo: ['Read the release notes of the new version.', 'Upgrade the server package (apk upgrade incus-backup-server), then the hosts.'],
    params: [] },
  { type: 'cert_expiry', label: 'Certificate expiry', group: 'Security and access',
    fires: 'A certificate (CA, server, sync peer) expires soon or cannot be read. Hosts renew their own certificates 45 days before expiry.',
    todo: ['Open Certificates: the table shows which certificate and how long it has left.', 'A host certificate that does not renew means the host does not reach the server: check the host.'],
    params: [
      { key: 'warn_days', label: 'Warning within', unit: 'days', def: 21, help: 'Days before expiry for the warning.' },
      { key: 'critical_days', label: 'Critical within', unit: 'days', def: 7, help: 'Days before expiry for critical; expired is always critical.' },
    ] },
  { type: 'login_security', label: 'Sign-in security', group: 'Security and access', event: true,
    fires: 'Emitted by the sign-in code: repeated failed sign-ins, a locked account.',
    todo: ['Open the audit log, filter by the action "login" and look at the addresses.', 'Lock or disable the account if the attempts are not the owner.'],
    params: [] },
  { type: 'job_failed', label: 'Host job failed', group: 'Jobs', event: true,
    fires: 'Emitted when a host job (restore, file restore, verify, restore test) fails.',
    todo: ['Open the job and read its log.', 'Run it again once the cause is fixed.'],
    params: [] },
]

const byType = new Map(ALERT_TYPES.map((t) => [t.type, t]))

/** The definition of a type; unknown types (future events) get a generic one. */
export function alertType(type: string): AlertTypeDef {
  return byType.get(type) ?? { type, label: type.replace(/_/g, ' '), group: 'Other', fires: 'Emitted by another part of the server.', todo: ['Read the detail of the alert.'], params: [], event: true }
}

export const SEVERITIES = ['info', 'warning', 'critical'] as const
const SEV_RANK: Record<string, number> = { info: 0, warning: 1, critical: 2 }
export function sevRank(s: string): number { return SEV_RANK[s] ?? -1 }

/** Order for the alert list: active before resolved, then critical first, then newest. */
export function alertOrder(a: { state: string; severity: string; last_change: string }, b: { state: string; severity: string; last_change: string }): number {
  const act = (x: string) => (x === 'resolved' ? 1 : 0)
  return act(a.state) - act(b.state) || sevRank(b.severity) - sevRank(a.severity) || (b.last_change < a.last_change ? -1 : b.last_change > a.last_change ? 1 : 0)
}

/** Words for the alert states, for pills (the StatusPill colour comes from statusOfAlert). */
export function statusOfAlert(state: string, severity: string): 'critical' | 'warn' | 'info' | 'neutral' | 'ok' {
  if (state === 'resolved') return 'ok'
  if (state === 'silenced') return 'neutral'
  if (state === 'acknowledged') return 'info'
  return severity === 'critical' ? 'critical' : severity === 'warning' ? 'warn' : 'info'
}

/** "2 h 5 min" style age from milliseconds. */
export function shortAge(ms: number): string {
  if (!(ms >= 0)) return ''
  const m = Math.floor(ms / 60000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return m % 60 && h < 10 ? `${h} h ${m % 60} min` : `${h} h`
  return `${Math.floor(h / 24)} d`
}
