<script setup lang="ts">
// One host: is it online, is everything on it protected, what ran, what is queued, is its
// certificate healthy. Tabs keep each question one click away (?tab= survives reloads and links).
// The repository comes from the top bar; a link from another repository's host carries ?repository=.
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Tabs from '@/components/Tabs.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import StatTile from '@/components/StatTile.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import DurationText from '@/components/DurationText.vue'
import KeyValue from '@/components/KeyValue.vue'
import CopyField from '@/components/CopyField.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import Tooltip from '@/components/Tooltip.vue'
import Sparkline from '@/components/charts/Sparkline.vue'
import BarChart from '@/components/charts/BarChart.vue'
import KindIcon from '@/components/infra/KindIcon.vue'
import FreshnessBadge from '@/components/infra/FreshnessBadge.vue'
import FilterChips from '@/components/infra/FilterChips.vue'
import RowMenu, { type MenuItem } from '@/components/infra/RowMenu.vue'
import BackupNowDialog from '@/components/infra/BackupNowDialog.vue'
import RestoreTestDialog from '@/components/infra/RestoreTestDialog.vue'
import { useHostRows } from './hostRows'
import { useResource, invalidate } from '@/api/cache'
import { infraApi, type BackupRun, type Job, type ObjectStat } from '@/api/sections/infra'
import { list } from '@/api/endpoints'
import { confirm } from '@/composables/useConfirm'
import { toast } from '@/stores/toast'
import { hasRole } from '@/stores/session'
import { repos, selectRepo } from '@/stores/repos'
import { activeJob, certStatus, freshnessRank, freshnessText, jobTarget, jobTypeText, runDurationMs, runsPerDay } from '@/lib/infra'
import { formatBytes, formatRelative } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'
import type { Status } from '@/lib/status'
import type { IconName } from '@/components/icons'

const props = defineProps<{ name: string }>()
const route = useRoute()
const router = useRouter()

// a link from another repository's host selects that repository first
watch(() => route.query.repository, (r) => {
  if (typeof r === 'string' && r && r !== repos.current && repos.list.some((x) => x.name === r)) selectRepo(r)
}, { immediate: true })
const scope = computed(() => (typeof route.query.scope === 'string' && route.query.scope) || 'backup')
const repo = computed(() => repos.current)
const isBackup = computed(() => scope.value === 'backup')
const canOperate = computed(() => hasRole('operator'))
const isAdmin = computed(() => hasRole('admin'))

const { rows, refresh: refreshRows, ids, certs } = useHostRows()
const host = computed(() => rows.value?.find((r) => r.repository === repo.value && r.name === props.name && r.scope === scope.value) ?? null)
const otherIdentities = computed(() => (rows.value ?? []).filter((r) => r.name === props.name && !(r.repository === repo.value && r.scope === scope.value)))
const offline = computed(() => host.value?.state === 'offline')

// ---- tabs (?tab=)
const tabIds = ['objects', 'runs', 'certificate', 'jobs', 'activity', 'danger'] as const
type TabId = typeof tabIds[number]
const tab = computed<TabId>({
  get: () => (tabIds as readonly string[]).includes(String(route.query.tab)) ? (route.query.tab as TabId) : (isBackup.value ? 'objects' : 'certificate'),
  set: (t) => { router.replace({ query: { ...route.query, tab: t } }) },
})

// ---- data
const on = (t: TabId) => () => !!repo.value && (tab.value === t || t === 'objects')
const stats = useResource(() => `objects-stats:${repo.value}:${props.name}`, (s) => infraApi.objectStats(props.name, s), { enabled: () => !!repo.value && isBackup.value, refreshOn: ['run'] })
const runs = useResource(() => `backup-runs:${repo.value}:${props.name}`, (s) => infraApi.backupRuns({ host: props.name, limit: 200 }, s), { enabled: () => !!repo.value && isBackup.value, refreshOn: ['run'] })
const jobs = useResource(() => `jobs:${repo.value}:${props.name}`, (s) => infraApi.jobs({ host: props.name, repository: repo.value, limit: 100 }, s), { enabled: () => !!repo.value, refreshOn: ['job'], poll: 15_000 })
const alerts = useResource(() => `alerts:host:${props.name}`, (s) => infraApi.alerts({ host: props.name, limit: 50 }, s), { enabled: on('activity'), refreshOn: ['alert'] })
const audit = useResource(() => 'audit:recent', (s) => infraApi.audit({ limit: 500 }, s), { enabled: () => on('activity')() && isAdmin.value })

// alerts and the audit log are only fetched when the Activity tab opens
watch(tab, (t) => { if (t === 'activity') { void alerts.refresh(); if (isAdmin.value) void audit.refresh() } })
const objects = computed(() => stats.data.value ?? [])
const dataObjects = computed(() => objects.value.filter((o) => o.kind !== 'instance'))
const runList = ref<BackupRun[] | null>(null)
watch(() => runs.data.value, (d) => { runList.value = d ?? null })
const moreBusy = ref(false)
const noMore = ref(false)
async function loadMoreRuns() {
  const cur = runList.value ?? []
  if (!cur.length) return
  moreBusy.value = true
  try {
    const next = await infraApi.backupRuns({ host: props.name, limit: 200, before: cur[cur.length - 1].started })
    runList.value = [...cur, ...next]
    noMore.value = next.length < 200
  } catch (e) { toast.error(e, 'Could not load older runs') } finally { moreBusy.value = false }
}

// ---- tiles
const runs14 = computed(() => {
  const since = Date.now() - 14 * 86400_000
  const r = (runList.value ?? []).filter((x) => Date.parse(x.started) >= since)
  return { ok: r.filter((x) => x.status === 'ok').length, partial: r.filter((x) => x.status === 'partial').length, failed: r.filter((x) => x.status === 'failed').length, total: r.length }
})
const runChart = computed(() => runsPerDay(runList.value ?? [], 30))
const certRow = computed(() => host.value?.cert ?? null)
const tabs = computed(() => [
  ...(isBackup.value ? [{ id: 'objects', label: 'Objects', count: stats.data.value ? objects.value.length : undefined }, { id: 'runs', label: 'Runs', count: runList.value ? runList.value.length : undefined }] : []),
  { id: 'certificate', label: 'Certificate' },
  { id: 'jobs', label: 'Jobs', count: jobs.data.value ? list(jobs.data.value).filter((j) => activeJob(j.state)).length || undefined : undefined },
  { id: 'activity', label: 'Activity' },
  ...(canOperate.value ? [{ id: 'danger', label: 'Danger zone' }] : []),
])

// ---- objects tab
const fview = ref('all')
const fcount = (f: string) => objects.value.filter((o) => o.freshness === f).length
const fchips = computed(() => [
  { value: 'all', label: 'All', count: objects.value.length },
  { value: 'stale', label: 'Overdue', count: fcount('stale'), status: 'warn' as const },
  { value: 'never', label: 'Never', count: fcount('never'), status: 'critical' as const },
  { value: 'ok', label: 'Current', count: fcount('ok') },
  { value: 'unscheduled', label: 'No schedule', count: fcount('unscheduled') },
  { value: 'reference', label: 'Instance records', count: fcount('reference') },
])
// worst first (overdue, never, no schedule, current, instance records); the table keeps this order until a header is clicked
const shownObjects = computed(() => (stats.data.value ? objects.value.filter((o) => fview.value === 'all' || o.freshness === fview.value).sort((a, b) => (freshnessRank[a.freshness] ?? 9) - (freshnessRank[b.freshness] ?? 9) || a.name.localeCompare(b.name)) : undefined))
const objCols: ColumnDef<ObjectStat>[] = [
  { key: 'name', label: 'Object', required: true },
  { key: 'project', label: 'Project' },
  { key: 'kind', label: 'Kind', hidden: true },
  { key: 'policy', label: 'Policy', value: (o) => o.retention_policy || o.declared_policy },
  { key: 'freshness', label: 'Freshness', value: (o) => freshnessText[o.freshness] ?? o.freshness },
  { key: 'latest', label: 'Last snapshot', value: (o) => o.latest?.created ?? '' },
  { key: 'size', label: 'Size', align: 'right', value: (o) => o.latest?.logical_bytes ?? null },
  { key: 'trend', label: 'Size trend', sortable: false, noExport: true, value: () => '' },
  { key: 'snapshots', label: 'Snapshots', align: 'right' },
  { key: 'protected', label: 'Protected', align: 'right' },
  { key: 'acts', label: 'Actions', sortable: false, noExport: true, align: 'right', required: true, value: () => '' },
]

// ---- runs tab
const runCols: ColumnDef<BackupRun>[] = [
  { key: 'started', label: 'Started', required: true },
  { key: 'status', label: 'Status' },
  { key: 'duration', label: 'Duration', align: 'right', value: (r) => runDurationMs(r) },
  { key: 'objects', label: 'Objects', align: 'right', value: (r) => r.objects_total, format: (r) => `${r.objects_ok} of ${r.objects_total}` },
  { key: 'objects_failed', label: 'Failed', align: 'right' },
  { key: 'logical_bytes', label: 'Read', align: 'right', format: (r) => formatBytes(r.logical_bytes) },
  { key: 'uploaded_bytes', label: 'Uploaded', align: 'right', format: (r) => formatBytes(r.uploaded_bytes) },
  { key: 'error', label: 'Error' },
]

// ---- jobs tab
const jobCols: ColumnDef<Job>[] = [
  { key: 'type', label: 'Job', required: true, value: (j) => jobTypeText[j.type] ?? j.type },
  { key: 'target', label: 'Target', value: (j) => jobTarget(j.params) },
  { key: 'state', label: 'State' },
  { key: 'created_at', label: 'Queued' },
  { key: 'finished_at', label: 'Finished', value: (j) => j.finished_at ?? '' },
  { key: 'created_by', label: 'By' },
  { key: 'message', label: 'Message' },
  { key: 'acts', label: '', sortable: false, noExport: true, align: 'right', value: () => '' },
]
async function cancelJob(j: Job) {
  if (!(await confirm({ title: `Cancel this ${jobTypeText[j.type] ?? j.type} job?`, message: j.state === 'running' ? 'The host stops the job at its next progress report.' : 'The job leaves the queue.', confirmLabel: 'Cancel job' }))) return
  try { await infraApi.cancelJob(j.id); toast.success('Job cancelled'); invalidate('jobs'); jobs.refresh() } catch (e) { toast.error(e, 'Could not cancel the job') }
}

// ---- activity
interface Act { at: string; icon: IconName; status: Status; text: string; detail?: string; to?: unknown }
const activity = computed<Act[]>(() => {
  const out: Act[] = []
  for (const r of runList.value ?? []) out.push({ at: r.started, icon: 'history', status: r.status === 'ok' ? 'ok' : r.status === 'partial' ? 'warn' : r.status === 'failed' ? 'critical' : 'info', text: `Backup run ${r.status}: ${r.objects_ok} of ${r.objects_total} objects`, detail: r.error || undefined })
  for (const j of list(jobs.data.value)) {
    out.push({ at: j.created_at, icon: 'activity', status: 'info', text: `${jobTypeText[j.type] ?? j.type} queued by ${j.created_by}`, detail: jobTarget(j.params), to: { name: 'job', params: { id: j.id } } })
    if (j.finished_at) out.push({ at: j.finished_at, icon: 'activity', status: j.state === 'succeeded' ? 'ok' : j.state === 'failed' ? 'critical' : 'neutral', text: `${jobTypeText[j.type] ?? j.type} ${j.state}`, detail: j.message || undefined, to: { name: 'job', params: { id: j.id } } })
  }
  for (const a of list(alerts.data.value)) {
    out.push({ at: a.first_seen, icon: 'bell', status: a.severity === 'critical' ? 'critical' : 'warn', text: `Alert: ${a.title}`, detail: a.detail })
    if (a.resolved_at) out.push({ at: a.resolved_at, icon: 'bell', status: 'ok', text: `Resolved: ${a.title}` })
  }
  const needle = `/hosts/${encodeURIComponent(props.name)}/`
  for (const e of list(audit.data.value)) if (e.target.includes(needle)) out.push({ at: e.at, icon: 'scroll-text', status: e.ok ? 'neutral' : 'critical', text: `${e.actor}: ${e.action}${e.ok ? '' : ' (refused)'}`, detail: e.target })
  return out.sort((a, b) => Date.parse(b.at) - Date.parse(a.at)).slice(0, 150)
})

// ---- actions
const bnOpen = ref(false)
const bnObject = ref<ObjectStat | null>(null)
const rtOpen = ref(false)
const rtObject = ref<ObjectStat | null>(null)
function backupNow(o: ObjectStat | null = null) { bnObject.value = o; bnOpen.value = true }
function restoreTest(o: ObjectStat | null = null) { rtObject.value = o; rtOpen.value = true }
function afterQueue() { jobs.refresh() }

async function setEnabled(enabled: boolean) {
  const h = host.value
  if (!h) return
  if (!enabled && !(await confirm({
    title: `Disable ${h.name}?`, typeToConfirm: h.name, confirmLabel: 'Disable host',
    message: 'Every request the host makes is refused from now on: backups, restores, jobs and certificate renewal. Nothing is deleted; enable it again to restore access.',
  }))) return
  try {
    await infraApi.setHostEnabled(h.repository, h.name, h.scope, enabled)
    toast.success(`${h.name} ${enabled ? 'enabled' : 'disabled'}`, enabled ? 'The host can connect again.' : 'Its requests are refused until it is enabled.')
    invalidate('infra-hosts'); invalidate('overview'); invalidate('certificates')
    await refreshRows()
  } catch (e) { toast.error(e, `Could not ${enabled ? 'enable' : 'disable'} ${h.name}`) }
}
const reEnrol = () => router.push({ name: 'host-onboard', query: { host: props.name, scope: scope.value, replace: '1' } })
const menu = computed<MenuItem[]>(() => {
  const items: MenuItem[] = []
  if (isAdmin.value) items.push({ label: 'Re-enrol or replace', icon: 'key-round', run: reEnrol })
  if (host.value) items.push(host.value.disabled
    ? { label: 'Enable host', icon: 'lock-open', run: () => setEnabled(true), disabled: canOperate.value ? false : 'Needs the operator role' }
    : { label: 'Disable host', icon: 'lock', run: () => setEnabled(false), danger: true, disabled: canOperate.value ? false : 'Needs the operator role' })
  items.push({ label: 'Open jobs', icon: 'activity', run: () => router.push({ name: 'jobs', query: { host: props.name } }) })
  return items
})
const opReason = computed(() => (!canOperate.value ? 'Needs the operator role' : host.value?.disabled ? 'The host is disabled' : ''))
const loading = computed(() => !(repos.loaded && !repos.list.length) && !rows.value && !ids.error.value)
</script>

<template>
  <div class="host-page">
    <template v-if="loading">
      <Skeleton :lines="3" height="22px" />
    </template>

    <Callout v-else-if="ids.error.value && !rows" status="critical" title="Could not load the host" :retry="refreshRows">{{ ids.error.value.message }}</Callout>
    <template v-else-if="!host">
    <header class="page-head"><div><h1>{{ name }}</h1></div></header>
    <div class="panel">
      <EmptyState :title="`No ${scope} identity named ${name} in ${repo || 'this repository'}`" icon="server">
        <template v-if="otherIdentities.length">It exists elsewhere:
          <span v-for="o in otherIdentities" :key="o.key"> <RouterLink :to="{ name: 'host', params: { name }, query: { repository: o.repository, ...(o.scope !== 'backup' ? { scope: o.scope } : {}) } }">{{ o.repository }} ({{ o.scope }})</RouterLink></span>
        </template>
        <template v-else>The host may have been removed, or it never enrolled. Check the name or add it as a new host.</template>
        <template #actions><RouterLink :to="{ name: 'hosts' }" class="btn btn-sm">All hosts</RouterLink><RouterLink v-if="isAdmin" :to="{ name: 'host-onboard', query: { host: name } }" class="btn btn-sm btn-primary">Add {{ name }}</RouterLink></template>
      </EmptyState>
    </div>
    </template>

    <template v-else>
      <header class="page-head">
        <div class="title">
          <h1><Icon :name="isBackup ? 'server' : 'refresh-cw'" :size="20" class="hi" />{{ host.name }}<StatusPill :state="host.state" class="hp" data-testid="host-state" /><span class="scope">{{ host.scope }}</span></h1>
          <p class="lede small">
            Repository <b>{{ host.repository }}</b><template v-if="host.last_addr"> · <span class="mono">{{ host.last_addr }}</span></template>
            · last seen <RelativeTime :time="host.last_seen" /> · enrolled <RelativeTime :time="host.enrolled" />
          </p>
        </div>
        <div class="actions">
          <button v-if="isBackup" type="button" class="btn" :disabled="!!opReason" :title="opReason || undefined" data-testid="host-restore-test" @click="restoreTest()"><Icon name="shield-check" :size="14" />Run restore test</button>
          <button v-if="isBackup" type="button" class="btn btn-primary" :disabled="!!opReason" :title="opReason || undefined" data-testid="host-backup-now" @click="backupNow()"><Icon name="play" :size="14" />Back up now</button>
          <RowMenu :items="menu" label="More host actions" button="More" />
        </div>
      </header>

      <Callout v-if="host.disabled" status="warn" title="This host is disabled">Every request it makes is refused: no backups, restores, jobs or certificate renewal.
        <template v-if="canOperate" #actions><button type="button" class="btn btn-sm" @click="setEnabled(true)">Enable</button></template>
      </Callout>
      <Callout v-else-if="offline && isBackup" status="critical" title="The host is offline">Not seen for more than 6 hours (last <RelativeTime :time="host.last_seen" />). Check that the incus-backup daemon runs on it and can reach this server.</Callout>

      <div v-if="isBackup" class="tiles">
        <StatTile label="Protection" :value="host.protection === 'protected' ? 'Protected' : host.protection === 'empty' ? 'No objects' : `${host.stale + host.never} at risk`" :status="host.protectionStatus === 'ok' ? 'ok' : host.protectionStatus === 'neutral' ? undefined : host.protectionStatus"
          :hint="host.protection === 'protected' || host.protection === 'empty' ? `${host.objects - host.instances} data objects` : host.protectionLabel" />
        <StatTile label="Objects" :value="host.objects" :hint="`${host.instances} instances · ${host.disks} disks · ${host.filesystems} fs · ${host.volumes} volumes`" />
        <StatTile label="Last backup" :value="formatRelative(host.last_backup)" :status="host.last_run_status === 'failed' ? 'critical' : host.last_run_status === 'partial' ? 'warn' : undefined"
          :hint="host.last_run_status ? `last run ${host.last_run_status}` : 'no run reported yet'" />
        <StatTile label="Runs, 14 days" :value="runs14.total" :loading="runs.loading.value" :status="runs14.failed ? 'critical' : runs14.partial ? 'warn' : undefined" :hint="`${runs14.ok} ok · ${runs14.partial} partial · ${runs14.failed} failed`" />
        <StatTile label="Certificate" :value="certRow ? `${certRow.days_left} d` : '–'" :status="certRow && certRow.state !== 'ok' ? certStatus(certRow.state) : undefined" :hint="certRow ? (certRow.state === 'ok' ? 'valid, renews by itself' : certRow.state) : certs.loading.value ? 'loading' : 'unknown'" />
      </div>

      <Tabs v-model="tab" :tabs="tabs" label="Host sections" data-testid="host-tabs" />

      <!-- Objects -->
      <section v-if="tab === 'objects'" class="tabpane" data-testid="tab-objects">
        <div class="bar"><FilterChips v-model="fview" :options="fchips" label="Freshness" /></div>
        <DataTable :rows="shownObjects" :columns="objCols" row-key="group_id" :loading="stats.loading.value" :error="stats.error.value" :retry="stats.refresh" storage-key="host-objects" :csv-name="`${name}-objects`" >
          <template #cell-name="{ row }"><span class="on"><KindIcon :kind="row.kind" :name="row.name" /><RouterLink :to="{ name: 'object', params: { group: row.group_id } }" class="strong">{{ row.name }}</RouterLink></span></template>
          <template #cell-policy="{ row }"><span>{{ row.retention_policy || row.declared_policy || '–' }}</span><Tooltip v-if="row.retention_policy && row.retention_policy !== row.declared_policy" :text="`Retention set by an operator; the host declares ${row.declared_policy || 'no policy'}.`"><Icon name="pencil" :size="12" class="muted ov" /></Tooltip></template>
          <template #cell-freshness="{ row }"><FreshnessBadge :freshness="row.freshness" :due="row.due" :overdue-at="row.overdue_at" :schedule="row.schedule" /></template>
          <template #cell-latest="{ row }"><RelativeTime :time="row.latest?.created" /></template>
          <template #cell-size="{ row }"><ByteSize v-if="row.latest && row.kind !== 'instance'" :bytes="row.latest.logical_bytes" /><span v-else class="muted">–</span></template>
          <template #cell-trend="{ row }"><Sparkline v-if="row.history.length > 1 && row.kind !== 'instance'" :values="row.history.map((h) => h.logical_bytes)" :label="`Size of the last ${row.history.length} snapshots`" /><span v-else class="muted small">–</span></template>
          <template #cell-protected="{ row }"><span v-if="row.protected" class="prot"><Icon name="lock" :size="12" />{{ row.protected }}</span><span v-else class="muted">0</span></template>
          <template #cell-acts="{ row }">
            <span v-if="row.kind !== 'instance' && canOperate" class="acts">
              <button type="button" class="btn btn-sm btn-ghost btn-icon" :disabled="host.disabled" :aria-label="`Back up ${row.name} now`" title="Back up now" @click="backupNow(row)"><Icon name="play" :size="13" /></button>
              <button type="button" class="btn btn-sm btn-ghost btn-icon" :disabled="host.disabled || !row.snapshots" :aria-label="`Restore test of ${row.name}`" title="Run restore test" @click="restoreTest(row)"><Icon name="shield-check" :size="13" /></button>
            </span>
          </template>
          <template #empty><EmptyState title="No objects reported yet" icon="box">The host reports its instances and volumes on its first run. Back up now to start one.</EmptyState></template>
        </DataTable>
      </section>

      <!-- Runs -->
      <section v-if="tab === 'runs'" class="tabpane" data-testid="tab-runs">
        <div class="panel">
          <div class="panel-head"><h2>Backup runs per day, 30 days</h2><span class="sub">bar colour: worst result of the day</span></div>
          <div class="panel-body"><BarChart :labels="runChart.labels" :values="runChart.values" :statuses="runChart.statuses" name="Runs" :height="140" /></div>
        </div>
        <DataTable :rows="runList ?? undefined" :columns="runCols" row-key="id" :loading="runs.loading.value" :error="runs.error.value" :retry="runs.refresh" storage-key="host-runs" :csv-name="`${name}-runs`" dense>
          <template #cell-started="{ row }"><AbsoluteTime :time="row.started" /></template>
          <template #cell-status="{ row }"><StatusPill :state="row.status" size="sm" /></template>
          <template #cell-duration="{ row }"><DurationText v-if="runDurationMs(row) !== null" :ms="runDurationMs(row)!" /><span v-else class="muted">running</span></template>
          <template #cell-objects_failed="{ row }"><span :class="{ critv: row.objects_failed }">{{ row.objects_failed }}</span></template>
          <template #cell-error="{ row }"><span class="truncate err" :title="row.error">{{ row.error || '–' }}</span></template>
          <template #empty><EmptyState title="No backup run yet" icon="history">The host's daemon reports a run each time its schedule fires.</EmptyState></template>
        </DataTable>
        <div v-if="(runList?.length ?? 0) >= 200 && !noMore" class="more"><button type="button" class="btn btn-sm" :disabled="moreBusy" @click="loadMoreRuns">Load older runs</button></div>
      </section>

      <!-- Certificate -->
      <section v-if="tab === 'certificate'" class="tabpane" data-testid="tab-certificate">
        <Callout v-if="certs.error.value" status="critical" title="Could not load certificate states" :retry="certs.refresh">{{ certs.error.value.message }}</Callout>
        <div v-else-if="!certRow" class="panel"><EmptyState title="No certificate record" icon="badge-check" compact>{{ certs.loading.value ? 'Loading…' : 'The server has no certificate status for this identity.' }}</EmptyState></div>
        <template v-else>
          <Callout v-if="certRow.untrusted" status="critical" title="Issued by a CA this server no longer trusts">The host must enrol again with a new token.<template v-if="isAdmin" #actions><button class="btn btn-sm" type="button" @click="reEnrol">Re-enrol</button></template></Callout>
          <Callout v-else-if="certRow.state === 'expired'" status="critical" title="The certificate has expired">If it expired less than 30 days ago, run <code>incus-backup cert renew</code> on the host. Otherwise enrol it again.</Callout>
          <Callout v-else-if="certRow.state === 'expiring' && certRow.days_left < 45" status="warn" title="Renewal is failing">Hosts renew 45 days before expiry; this one has {{ certRow.days_left }} days left. On the host, run <code>incus-backup cert status</code> to see the error, then <code>incus-backup cert renew</code>.</Callout>
          <Callout v-else-if="certRow.state === 'expiring'" status="info" title="Renewal is due soon">The host renews by itself when 45 days are left ({{ certRow.days_left }} days now).</Callout>
          <div class="panel panel-body">
            <KeyValue :columns="2" :items="[
              { key: 'state', label: 'State' }, { key: 'not_after', label: 'Expires' }, { key: 'not_before', label: 'Issued' },
              { key: 'renewed', label: 'Last renewed' }, { key: 'other', label: 'Other valid certificates', value: certRow.other_valid ? `${certRow.other_valid} (a renewal is not confirmed yet)` : 'none' },
              { key: 'rotation', label: 'CA rotation', value: certRow.on_old_ca ? 'still on the old CA: renews at its next daily check' : 'up to date' },
              { key: 'fp', label: 'Fingerprint (SHA-256)' },
            ]">
              <template #value-state><StatusPill :status="certStatus(certRow.state)" :label="`${certRow.state}, ${certRow.days_left} days left`" /></template>
              <template #value-not_after><AbsoluteTime :time="certRow.not_after" /></template>
              <template #value-not_before><AbsoluteTime :time="certRow.not_before" /></template>
              <template #value-renewed><RelativeTime :time="certRow.last_renewed" never="never (enrolment certificate)" /></template>
              <template #value-fp><CopyField :value="certRow.fingerprint" /></template>
            </KeyValue>
            <p class="muted small note">Certificates are valid for 2 years. The host renews 45 days before expiry and keeps the old certificate valid until the new one is confirmed. See docs/certificates.md.</p>
          </div>
        </template>
        <div v-if="otherIdentities.length" class="panel">
          <div class="panel-head"><h2>Other identities named {{ name }}</h2></div>
          <ul class="ids"><li v-for="o in otherIdentities" :key="o.key"><RouterLink :to="{ name: 'host', params: { name }, query: { repository: o.repository, ...(o.scope !== 'backup' ? { scope: o.scope } : {}), tab: 'certificate' } }">{{ o.repository }} · {{ o.scope }}</RouterLink><StatusPill :state="o.state" size="sm" variant="dot" /><span v-if="o.cert" class="muted small">certificate {{ o.cert.state }}, {{ o.cert.days_left }} d</span></li></ul>
        </div>
      </section>

      <!-- Jobs -->
      <section v-if="tab === 'jobs'" class="tabpane" data-testid="tab-jobs">
        <DataTable :rows="jobs.data.value === undefined ? undefined : list(jobs.data.value)" :columns="jobCols" row-key="id" :loading="jobs.loading.value" :error="jobs.error.value" :retry="jobs.refresh" storage-key="host-jobs" :csv-name="`${name}-jobs`" dense>
          <template #toolbar>
            <button v-if="isBackup" type="button" class="btn btn-sm" :disabled="!!opReason" :title="opReason || undefined" @click="backupNow()"><Icon name="play" :size="13" />Back up now</button>
          </template>
          <template #cell-type="{ row }"><RouterLink :to="{ name: 'job', params: { id: row.id } }" class="strong">{{ jobTypeText[row.type] ?? row.type }}</RouterLink></template>
          <template #cell-state="{ row }"><StatusPill :state="row.state" size="sm" :label="row.state === 'running' && row.progress ? `running ${row.progress} %` : row.cancel_requested && activeJob(row.state) ? 'cancelling' : row.state" /></template>
          <template #cell-created_at="{ row }"><RelativeTime :time="row.created_at" /></template>
          <template #cell-finished_at="{ row }"><RelativeTime :time="row.finished_at" never="–" /></template>
          <template #cell-message="{ row }"><span class="truncate err" :title="row.message">{{ row.message || '–' }}</span></template>
          <template #cell-acts="{ row }"><button v-if="activeJob(row.state) && canOperate && !row.cancel_requested" type="button" class="btn btn-sm btn-ghost" @click="cancelJob(row)">Cancel</button></template>
          <template #empty><EmptyState title="No jobs for this host" icon="activity">Jobs are work the server asks the host to do: a backup now, a restore test, a restore.</EmptyState></template>
        </DataTable>
      </section>

      <!-- Activity -->
      <section v-if="tab === 'activity'" class="tabpane panel" data-testid="tab-activity">
        <div class="panel-head"><h2>Activity</h2><span class="sub">backup runs, jobs, alerts<template v-if="isAdmin"> and administrator actions</template>, newest first</span></div>
        <Callout v-if="alerts.error.value" status="warn" title="Alerts are not shown" class="m">{{ alerts.error.value.message }}</Callout>
        <ol v-if="activity.length" class="acts-list">
          <li v-for="(a, i) in activity" :key="i" :class="`s-${a.status}`">
            <Icon :name="a.icon" :size="14" class="ai" />
            <div class="at">
              <component :is="a.to ? RouterLink : 'span'" :to="a.to" class="atx">{{ a.text }}</component>
              <span v-if="a.detail" class="muted small truncate" :title="a.detail">{{ a.detail }}</span>
            </div>
            <RelativeTime :time="a.at" class="small muted" />
          </li>
        </ol>
        <EmptyState v-else title="Nothing happened yet" icon="history" compact>{{ runs.loading.value || jobs.loading.value ? 'Loading…' : 'Runs, jobs and alerts of this host appear here.' }}</EmptyState>
      </section>

      <!-- Danger zone -->
      <section v-if="tab === 'danger' && canOperate" class="tabpane panel danger" data-testid="tab-danger">
        <div class="dz">
          <div>
            <h3>{{ host.disabled ? 'Enable this host' : 'Disable this host' }}</h3>
            <p class="muted small">{{ host.disabled ? 'The host can connect again with its existing certificate.' : 'Refuses every request of the host (backups, restores, jobs, certificate renewal). Its backups stay; nothing is deleted. Use this when a host is compromised or decommissioned.' }}</p>
          </div>
          <button v-if="host.disabled" type="button" class="btn" data-testid="danger-enable" @click="setEnabled(true)"><Icon name="lock-open" :size="14" />Enable host</button>
          <button v-else type="button" class="btn btn-danger" data-testid="danger-disable" @click="setEnabled(false)"><Icon name="lock" :size="14" />Disable host</button>
        </div>
        <div class="dz">
          <div>
            <h3>Re-enrol or replace this host</h3>
            <p class="muted small">Issues a new one-time token for <b>{{ host.name }}</b>. When it is used, every earlier certificate of this identity is revoked: use it after a reinstall or when the host's credentials may be exposed. Objects stay with the host name.</p>
          </div>
          <button type="button" class="btn" :disabled="!isAdmin" :title="isAdmin ? undefined : 'Needs the admin role'" @click="reEnrol"><Icon name="key-round" :size="14" />Re-enrol</button>
        </div>
      </section>

      <BackupNowDialog v-if="isBackup" v-model:open="bnOpen" :repository="host.repository" :host="host.name" :objects="objects" :object="bnObject" :offline="offline" @queued="afterQueue" />
      <RestoreTestDialog v-if="isBackup" v-model:open="rtOpen" :repository="host.repository" :host="host.name" :objects="dataObjects" :object="rtObject" :offline="offline" @queued="afterQueue" />
    </template>
  </div>
</template>

<style scoped>
.host-page { display: flex; flex-direction: column; gap: 14px; }
.page-head { margin-bottom: 0; }
.title h1 { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.hi { color: var(--text-muted); }
.hp { font-size: var(--fs-sm); }
.scope { font-size: var(--fs-xs); font-weight: var(--fw-medium); padding: 2px 7px; border-radius: var(--r-sm); background: var(--surface-2); border: 1px solid var(--border); color: var(--text-2); }
.mono { font-family: var(--font-mono); }
.tiles { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; }
.tilev { font-size: var(--fs-lg); font-weight: var(--fw-semibold); color: var(--text); }
.tabpane { display: flex; flex-direction: column; gap: 12px; }
.bar { display: flex; gap: 8px; flex-wrap: wrap; }
.on { display: inline-flex; align-items: center; gap: 7px; min-width: 0; }
.strong { font-weight: var(--fw-medium); color: var(--text); }
.ov { margin-left: 4px; }
.prot { display: inline-flex; gap: 4px; align-items: center; color: var(--text-2); }
.acts { display: inline-flex; gap: 2px; }
.critv { color: var(--critical-text); font-weight: var(--fw-semibold); }
.err { display: inline-block; max-width: 340px; vertical-align: middle; }
.more { display: flex; justify-content: center; }
.note { margin-top: 12px; }
.ids { list-style: none; margin: 0; padding: 8px 16px; }
.ids li { display: flex; align-items: center; gap: 10px; min-height: 32px; }
.acts-list { list-style: none; margin: 0; padding: 4px 0; }
.acts-list li { display: grid; grid-template-columns: 20px minmax(0, 1fr) auto; gap: 10px; align-items: start; padding: 8px 16px; border-bottom: 1px solid var(--border); }
.acts-list li:last-child { border-bottom: 0; }
.ai { margin-top: 3px; color: var(--text-muted); }
.s-ok .ai { color: var(--ok); } .s-warn .ai { color: var(--warn); } .s-critical .ai { color: var(--critical); } .s-info .ai { color: var(--info); }
.at { display: flex; flex-direction: column; min-width: 0; }
.atx { color: var(--text); }
.m { margin: 10px 16px 0; }
.danger { border-color: var(--critical-border); }
.dz { display: flex; gap: 20px; align-items: center; justify-content: space-between; padding: 16px; border-bottom: 1px solid var(--border); }
.dz:last-child { border-bottom: 0; }
.dz h3 { margin-bottom: 4px; }
.dz p { max-width: 70ch; }
@media (max-width: 1100px) { .tiles { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (max-width: 600px) { .tiles { grid-template-columns: repeat(2, minmax(0, 1fr)); } .dz { flex-direction: column; align-items: flex-start; } .err { max-width: 180px; } }
</style>
