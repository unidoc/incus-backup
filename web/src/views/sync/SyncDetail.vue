<script setup lang="ts">
// One sync job: state and lag, run now (with live progress from the run history), the run history as
// a chart and a table, the PBS-class options, schedule and pause, and removal.
import { computed, reactive, ref, watch, onBeforeUnmount } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import StatTile from '@/components/StatTile.vue'
import StatusPill from '@/components/StatusPill.vue'
import KeyValue from '@/components/KeyValue.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import Skeleton from '@/components/Skeleton.vue'
import EmptyState from '@/components/EmptyState.vue'
import DataTable from '@/components/DataTable.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import BarChart from '@/components/charts/BarChart.vue'
import FormField from '@/components/form/FormField.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import TagInput from '@/components/form/TagInput.vue'
import ScheduleField from '@/components/storage/ScheduleField.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { storageApi, type SyncRunRecord } from '@/api/sections/storage'
import { repos } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { formatRelative, describeSchedule, formatBytes, formatDuration, formatNumber, parseSchedule, plural, formatDateTime } from '@/lib/format'
import { formatRateLimit, parseRate, scheduleIntervalMs, syncHealth } from '@/lib/storage'
import type { ColumnDef } from '@/lib/table'
import type { Status } from '@/lib/status'
import type { SyncResult, SyncRunFailure } from '@/api/types'

const props = defineProps<{ name: string }>()
const router = useRouter()
const st = useResource(() => `sync-job:${repos.current}:${props.name}`, (s) => api.sync.get(props.name, s), { refreshOn: ['run'] })
const runs = useResource(() => `sync-runs:${repos.current}:${props.name}`, (s) => storageApi.sync.runs(props.name, 60, s), { refreshOn: ['run'] })
const job = computed(() => st.data.value?.job)
const health = computed(() => {
  const s = st.data.value
  if (!s) return null
  if (running.value || list(runs.data.value)[0]?.status === 'running') return { status: 'info' as Status, label: 'Running' }
  const p = parseSchedule(s.job.schedule)
  return syncHealth(s.job, s.last_run_status, s.lag_ns / 1e6, p instanceof Error ? 0 : scheduleIntervalMs(p))
})
const notFound = computed(() => (st.error.value as { status?: number } | null)?.status === 404)

// ---- run now, with live state from the history
const running = ref(false)
const started = ref(0)
const now = ref(Date.now())
const lastResult = ref<{ ok: boolean; error?: string; r: SyncResult } | null>(null)
let poll: ReturnType<typeof setInterval> | null = null
const liveRun = computed(() => list(runs.data.value).find((r) => r.status === 'running'))
watch(() => running.value || !!liveRun.value, (on) => {
  if (on && !poll) poll = setInterval(() => { now.value = Date.now(); void runs.refresh() }, 2000)
  if (!on && poll) { clearInterval(poll); poll = null }
}, { immediate: true })
onBeforeUnmount(() => { if (poll) clearInterval(poll) })
async function runNow() {
  running.value = true
  started.value = Date.now()
  lastResult.value = null
  setTimeout(() => void runs.refresh(), 600)
  try {
    const r = await api.sync.run(props.name)
    if ('ok' in r && (r as SyncRunFailure).ok === false) {
      const f = r as SyncRunFailure
      lastResult.value = { ok: false, error: f.error, r: f.result }
      toast.warn(`Sync ${props.name} finished with failures`, f.error)
    } else {
      lastResult.value = { ok: true, r: r as SyncResult }
      toast.success(`Sync ${props.name} finished`, `${plural((r as SyncResult).synced, 'snapshot')} synced.`)
    }
  } catch (e) { toast.error(e, `Sync ${props.name} failed`); lastResult.value = null } finally {
    running.value = false
    invalidate(`sync-status:${repos.current}`)
    void st.refresh()
    void runs.refresh()
  }
}

// ---- chart: bytes transferred per run, coloured by outcome
const chartRuns = computed(() => list(runs.data.value).filter((r) => r.finished_at).slice(0, 30).reverse())
const chartLabels = computed(() => chartRuns.value.map((r) => formatDateTime(r.started_at).slice(5)))
const chartValues = computed(() => chartRuns.value.map((r) => r.bytes_transferred))
const chartStatus = computed(() => chartRuns.value.map((r) => (r.status === 'failed' ? 'critical' : r.status === 'partial' ? 'warn' : null) as Status | null))
const runCols: ColumnDef<SyncRunRecord>[] = [
  { key: 'started_at', label: 'Started', required: true },
  { key: 'status', label: 'Result' },
  { key: 'duration', label: 'Duration', align: 'right', value: (r) => (r.finished_at ? Date.parse(r.finished_at) - Date.parse(r.started_at) : now.value - Date.parse(r.started_at)), format: (r) => formatDuration((r.finished_at ? Date.parse(r.finished_at) : now.value) - Date.parse(r.started_at)) },
  { key: 'snapshots_synced', label: 'Synced', align: 'right' },
  { key: 'snapshots_pending', label: 'Pending', align: 'right' },
  { key: 'snapshots_skipped', label: 'Skipped', align: 'right' },
  { key: 'snapshots_repaired', label: 'Repaired', align: 'right', hidden: true },
  { key: 'bytes_transferred', label: 'Transferred', align: 'right', format: (r) => formatBytes(r.bytes_transferred) },
  { key: 'chunks', label: 'Chunks sent / reused', align: 'right', value: (r) => r.chunks_transferred, format: (r) => `${formatNumber(r.chunks_transferred)} / ${formatNumber(r.chunks_reused)}` },
  { key: 'error', label: 'Error' },
]

// ---- options
const o = reactive({ verified_only: false, resync_corrupt: false, transfer_last: 0, rate: '', groups: [] as string[] })
const sch = reactive({ schedule: '', enabled: true })
const schedOk = ref(true)
function loadOpts() {
  const j = job.value
  if (!j) return
  Object.assign(o, { verified_only: j.verified_only, resync_corrupt: j.resync_corrupt, transfer_last: j.transfer_last, rate: j.rate_limit_bps ? String(j.rate_limit_bps) : '', groups: [...(j.group_filter ?? [])] })
  Object.assign(sch, { schedule: j.schedule, enabled: j.enabled })
}
watch(job, loadOpts, { immediate: true })
const rate = computed(() => parseRate(o.rate))
const optsDirty = computed(() => {
  const j = job.value
  if (!j) return false
  return o.verified_only !== j.verified_only || o.resync_corrupt !== j.resync_corrupt || o.transfer_last !== j.transfer_last || (!(rate.value instanceof Error) && rate.value !== j.rate_limit_bps) || JSON.stringify(o.groups) !== JSON.stringify(j.group_filter ?? [])
})
const schedDirty = computed(() => !!job.value && (sch.schedule.trim() !== job.value.schedule || sch.enabled !== job.value.enabled))
const saving = ref(false)
const saveError = ref('')
async function saveOpts() {
  if (rate.value instanceof Error || o.transfer_last < 0) return
  saving.value = true
  saveError.value = ''
  try {
    if (optsDirty.value) await api.sync.update(props.name, { verified_only: o.verified_only, resync_corrupt: o.resync_corrupt, transfer_last: o.transfer_last, rate_limit_bps: rate.value, groups: o.groups })
    if (schedDirty.value) await storageApi.sync.setSchedule(props.name, { schedule: sch.schedule.trim(), enabled: sch.enabled })
    toast.success('Sync job saved', `${props.name}: ${sch.enabled ? describeSchedule(sch.schedule).toLowerCase() : 'paused'}.`)
    invalidate(`sync:${repos.current}`)
    await st.refresh()
  } catch (e) { saveError.value = errorText(e) } finally { saving.value = false }
}
async function remove() {
  if (!(await confirm({ title: `Remove sync job ${props.name}?`, danger: true, typeToConfirm: props.name, confirmLabel: 'Remove job',
    message: `The job and this server's certificate for ${job.value?.remote} are deleted. Snapshots already copied stay on both servers. Disable this server's identity on the remote too (its Hosts list) if you do not re-add the job.` }))) return
  try {
    await api.sync.remove(props.name)
    toast.success(`Sync job ${props.name} removed`)
    invalidate(`sync:${repos.current}`)
    void router.push({ name: 'sync' })
  } catch (e) { toast.error(e, 'Could not remove the sync job') }
}
const validGroup = (v: string) => (/^[0-9a-f]{32}$/.test(v) ? '' : 'A group id is 32 hex characters.')
const offline = computed(() => repos.list.find((r) => r.name === repos.current)?.mode === 'offline')
</script>

<template>
  <div class="sd">
    <div v-if="st.loading.value && !st.data.value" class="panel pad"><Skeleton :lines="6" height="16px" /></div>
    <template v-else-if="notFound">
      <header class="page-head"><div><h1 class="h1">{{ name }}</h1></div></header>
      <EmptyState title="No such sync job" icon="refresh-cw">Repository {{ repos.current }} has no sync job called “{{ name }}”.
        <template #actions><RouterLink :to="{ name: 'sync' }" class="btn btn-sm">All sync jobs</RouterLink></template></EmptyState>
    </template>
    <Callout v-else-if="st.error.value && !st.data.value" status="critical" title="Could not load the sync job" :retry="st.refresh">{{ st.error.value.message }}</Callout>
    <template v-else-if="st.data.value && job">
      <header class="page-head">
        <div>
          <h1 class="h1">{{ name }} <span class="dir" :class="job.direction"><Icon :name="job.direction === 'push' ? 'arrow-up' : 'arrow-down'" :size="12" />{{ job.direction }}</span></h1>
          <p class="lede">{{ job.direction === 'push' ? `Pushes ${repos.current} to` : `Pulls into ${repos.current} from` }} <span class="mono">{{ job.remote }}</span> (repository {{ job.remote_repository }}) · {{ job.enabled ? describeSchedule(job.schedule) : 'paused' }}</p>
        </div>
        <div class="actions">
          <StatusPill v-if="health" :status="health.status" :label="health.label" data-testid="sync-health" />
          <button v-if="hasRole('operator')" type="button" class="btn btn-primary" :disabled="running || !!liveRun || offline" data-testid="sync-run" @click="runNow"><Spinner v-if="running || liveRun" :size="13" /><Icon v-else name="play" />Run now</button>
          <button v-if="hasRole('admin')" type="button" class="btn btn-danger" data-testid="sync-remove" @click="remove"><Icon name="trash-2" />Remove</button>
        </div>
      </header>

      <Callout v-if="running || liveRun" status="info" title="Synchronising" class="mb" data-testid="sync-running">
        Running for {{ formatDuration(now - (liveRun ? Date.parse(liveRun.started_at) : started)) }}. The server continues if you leave this page; snapshots appear on the destination as each one commits.
      </Callout>
      <Callout v-if="lastResult" :status="lastResult.ok ? 'ok' : 'warn'" :title="lastResult.ok ? 'Run finished' : 'Run finished with failures'" class="mb" data-testid="sync-result">
        {{ plural(lastResult.r.synced, 'snapshot') }} synced of {{ formatNumber(lastResult.r.pending) }} pending; {{ formatNumber(lastResult.r.skipped_unverified) }} skipped as unverified, {{ formatNumber(lastResult.r.skipped_corrupt) }} as corrupt; {{ formatNumber(lastResult.r.repaired) }} repaired; {{ formatBytes(lastResult.r.bytes_transferred) }} in {{ formatNumber(lastResult.r.chunks_transferred) }} chunks ({{ formatNumber(lastResult.r.chunks_reused) }} already there).
        <template v-if="lastResult.error"> {{ lastResult.error }}</template>
        <ul v-if="lastResult.r.errors?.length" class="errs"><li v-for="(e, i) in lastResult.r.errors.slice(0, 5)" :key="i" class="mono">{{ e }}</li></ul>
      </Callout>
      <Callout v-if="st.data.value.last_run_status === 'failed' && !running" status="critical" title="The last run failed" class="mb">{{ st.data.value.last_error }}{{ /[.!?]$/.test(st.data.value.last_error) ? '' : '.' }}
        <template v-if="/host disabled/.test(st.data.value.last_error)"> The remote disabled this server's identity: enable it on the remote's Hosts list.</template>
        <template v-else-if="/connect|dial|refused|timeout/i.test(st.data.value.last_error)"> Check that the remote is running and reachable on {{ job.remote }}.</template>
      </Callout>

      <section class="tiles">
        <StatTile label="Lag" :value="st.data.value.lag_ns ? formatDuration(st.data.value.lag_ns / 1e6, { units: 1 }) : 'none'" hint="age of the oldest snapshot not yet copied" :status="health?.label === 'Lagging' ? 'warn' : undefined" />
        <StatTile label="Pending" :value="formatNumber(st.data.value.snapshots_pending)" hint="snapshots not on the destination yet" />
        <StatTile label="Last run" :value="st.data.value.last_run_status || 'never'" :status="st.data.value.last_run_status === 'failed' ? 'critical' : st.data.value.last_run_status === 'partial' ? 'warn' : undefined">
          <template #hint><RelativeTime :time="st.data.value.last_run_at" /></template>
        </StatTile>
        <StatTile label="Last success" :value="formatRelative(job.last_success)" :hint="job.last_success ? formatDateTime(job.last_success) : 'no run completed yet'" :status="job.last_success ? undefined : 'warn'" />
        <StatTile label="Last run moved" :value="formatBytes(st.data.value.bytes_transferred)" :hint="`${formatNumber(st.data.value.snapshots_synced)} snapshots, ${formatNumber(st.data.value.chunks_reused)} chunks reused`" />
        <StatTile label="Skipped / repaired" :value="`${st.data.value.snapshots_skipped} / ${st.data.value.snapshots_repaired}`" :hint="st.data.value.failed ? `${st.data.value.failed} snapshots failed` : 'last run'" :status="st.data.value.failed ? 'warn' : undefined" />
      </section>

      <div class="grid">
        <section class="panel">
          <div class="panel-head"><h2>Run history</h2><span class="sub">bytes transferred per run; failed and partial runs marked</span></div>
          <div class="panel-body">
            <BarChart v-if="chartRuns.length" :labels="chartLabels" :values="chartValues" :statuses="chartStatus" name="Transferred" :y-format="(v) => formatBytes(v)" :height="170" title="Transferred per run" />
            <EmptyState v-else title="No finished run yet" icon="history" compact>Run the job now or wait for its schedule.</EmptyState>
            <div v-if="chartRuns.length" class="strip" aria-label="Outcome of each run, oldest first">
              <span class="muted small">Outcome</span>
              <span v-for="r in chartRuns" :key="r.id" class="dot" :class="`d-${r.status}`" :title="`${formatDateTime(r.started_at)} · ${r.status}${r.error ? ' · ' + r.error : ''}`" />
              <span class="muted small">{{ chartRuns.filter((r) => r.status !== 'ok').length }} of {{ chartRuns.length }} not ok</span>
            </div>
          </div>
        </section>
        <section class="panel">
          <div class="panel-head"><h2>Settings</h2><span class="sub">{{ hasRole('operator') ? 'saved for the next run' : 'read-only for your role' }}</span></div>
          <form class="panel-body form-grid" data-testid="sync-options" @submit.prevent="saveOpts">
            <FormField v-slot="fl" label="Schedule">
              <ScheduleField :id="fl.id" v-model="sch.schedule" clock="server" :count="3" dense :disabled="!hasRole('operator')" :after="job.last_success" @valid="(v) => (schedOk = v)" />
            </FormField>
            <SwitchInput v-model="sch.enabled" :label="sch.enabled ? 'Enabled: runs on its schedule' : 'Paused: runs only when started by hand'" :disabled="!hasRole('operator')" />
            <SwitchInput v-model="o.verified_only" label="Verified only" :disabled="!hasRole('operator')" />
            <SwitchInput v-model="o.resync_corrupt" label="Resync corrupt (repair failed copies from a healthy source)" :disabled="!hasRole('operator')" />
            <div class="grid2">
              <FormField v-slot="fl" label="Transfer last" hint="Newest N per object; 0 = all">
                <input :id="fl.id" v-model.number="o.transfer_last" type="number" min="0" class="input num" :disabled="!hasRole('operator')" />
              </FormField>
              <FormField v-slot="fl" label="Rate limit" :error="rate instanceof Error ? rate.message : ''" :hint="rate instanceof Error ? '' : formatRateLimit(rate)">
                <input :id="fl.id" v-model="o.rate" class="input mono" placeholder="unlimited" :disabled="!hasRole('operator')" data-testid="sync-rate" />
              </FormField>
            </div>
            <FormField v-slot="fl" label="Only these objects (group ids)" optional hint="Empty: every object.">
              <TagInput :id="fl.id" v-model="o.groups" :validate="validGroup" />
            </FormField>
            <Callout v-if="saveError" status="critical" title="Could not save the sync job">{{ saveError }}</Callout>
            <div v-if="hasRole('operator')" class="form-actions">
              <button type="submit" class="btn btn-primary" :disabled="(!optsDirty && !schedDirty) || saving || !schedOk || rate instanceof Error" data-testid="sync-save"><Spinner v-if="saving" :size="13" />Save</button>
              <button type="button" class="btn" :disabled="(!optsDirty && !schedDirty) || saving" @click="loadOpts">Discard</button>
            </div>
          </form>
        </section>
      </div>

      <section class="panel">
        <div class="panel-head"><h2>Runs</h2><Spinner v-if="liveRun" :size="12" /></div>
        <DataTable :rows="runs.data.value === undefined ? undefined : list(runs.data.value)" :columns="runCols" row-key="id" storage-key="sync-runs" :csv-name="`sync-${name}-runs`" :loading="runs.loading.value" :error="runs.error.value" :retry="runs.refresh" dense max-height="420px">
          <template #cell-started_at="{ row }"><RelativeTime :time="row.started_at" /></template>
          <template #cell-status="{ row }"><StatusPill :state="row.status" size="sm" /></template>
          <template #cell-error="{ row }"><span class="truncate err" :title="row.error">{{ row.error || '–' }}</span></template>
          <template #empty><EmptyState title="No run yet" icon="history" compact>This job has not run yet.</EmptyState></template>
        </DataTable>
      </section>

      <section class="panel">
        <div class="panel-head"><h2>Peer</h2></div>
        <div class="panel-body">
          <KeyValue :columns="2" :items="[
            { key: 'remote', label: 'Remote agent address', value: job.remote, mono: true },
            { key: 'rrepo', label: 'Remote repository', value: job.remote_repository },
            { key: 'dir', label: 'Initiator', value: 'this server (both directions)' },
            { key: 'cred', label: 'Credential', value: `sync-${repos.current}-${name} (${job.direction === 'push' ? 'sync-write' : 'sync-read'} certificate at the remote)` },
            { key: 'id', label: 'Job id', value: job.id, mono: true },
            { key: 'groups', label: 'Objects', value: job.group_filter?.length ? plural(job.group_filter.length, 'object') : 'all' },
          ]" />
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.sd { display: flex; flex-direction: column; gap: var(--s-4); }
.sd .page-head { margin-bottom: 0; }
.pad { padding: var(--s-4); }
.mb { margin: 0; }
.h1 { display: flex; align-items: center; gap: 10px; }
.dir { display: inline-flex; align-items: center; gap: 4px; font-size: var(--fs-sm); font-weight: var(--fw-medium); padding: 1px 8px; border-radius: var(--r-pill); border: 1px solid var(--border-strong); }
.dir.push { color: var(--series-1); border-color: color-mix(in srgb, var(--series-1) 40%, transparent); }
.dir.pull { color: var(--series-3); border-color: color-mix(in srgb, var(--series-3) 40%, transparent); }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: var(--s-3); }
.grid { display: grid; grid-template-columns: 1.3fr 1fr; gap: var(--s-4); align-items: start; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: var(--s-3); }
.strip { display: flex; gap: 4px; align-items: center; flex-wrap: wrap; margin-top: 10px; }
.strip .small { margin: 0 6px; }
.dot { width: 12px; height: 12px; border-radius: 3px; background: var(--ok); }
.d-failed { background: var(--critical); } .d-partial { background: var(--warn); }
.errs { margin: 6px 0 0; padding-left: 18px; font-size: 12px; overflow-wrap: anywhere; }
.err { display: inline-block; max-width: 360px; vertical-align: middle; color: var(--critical-text); }
.panel > :deep(.dt) { border: 0; border-radius: 0 0 var(--r-lg) var(--r-lg); }
.actions { align-items: center; }
@media (max-width: 1000px) { .grid { grid-template-columns: 1fr; } }
</style>
