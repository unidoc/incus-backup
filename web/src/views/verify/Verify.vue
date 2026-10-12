<script setup lang="ts">
// Verification across the repository: which snapshots were never verified, which are stale (older
// than their policy's re-verify age), which failed and which chunks are quarantined; start a
// server-side verification (POST /verify: reads and checks every stored chunk object, no key needed)
// or a host-side deep verification (job deep_verify: the host decrypts and recomputes every chunk id
// with its key); the history of verification and scrub runs.
import { computed, reactive, ref, shallowRef } from 'vue'
import { RouterLink } from 'vue-router'
import StatTile from '@/components/StatTile.vue'
import DataTable from '@/components/DataTable.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import StatusPill from '@/components/StatusPill.vue'
import Spinner from '@/components/Spinner.vue'
import Tooltip from '@/components/Tooltip.vue'
import FormField from '@/components/form/FormField.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import PIcon from '@/components/protect/PIcon.vue'
import VerifyBadge from '@/components/protect/VerifyBadge.vue'
import JobPanel from '@/components/protect/JobPanel.vue'
import { useSnapRows } from '@/components/protect/useSnapRows'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { protectApi, type Job } from '@/api/sections/protect'
import { repos, repoNone, currentRepo } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { verifySummary, kindLabel, type SnapRow } from '@/lib/protect/snapshots'
import { describeSchedule, formatDuration, formatNumber, formatRelative } from '@/lib/format'
import { noun } from '@/lib/protect/text'
import type { ColumnDef } from '@/lib/table'
import type { CorruptChunk, RunRow, VerifyResponse } from '@/api/types'

const on = () => !!repos.current && !repoNone.value
const { rows, maint, refresh: refreshSnaps } = useSnapRows()
const runs = useResource(() => `runs:${repos.current}:200`, (s) => api.maintenance.runs(200, s), { enabled: on, refreshOn: ['run'] })
const corrupt = useResource(() => `corrupt:${repos.current}`, (s) => api.maintenance.corrupt(false, s), { enabled: on, refreshOn: ['run'] })
const healed = useResource(() => `corrupt-healed:${repos.current}`, (s) => api.maintenance.corrupt(true, s), { enabled: on, refreshOn: ['run'] })
const hosts = useResource(() => `hosts:${repos.current}`, (s) => api.hosts.list(s), { enabled: on })
const objects = useResource(() => `objects:${repos.current}`, (s) => api.objects.list(s), { enabled: on })

const all = computed(() => rows.value ?? [])
const sum = computed(() => verifySummary(all.value))
const attention = computed(() => all.value.filter((r) => r.verifyClass !== 'ok').sort((a, b) => order[a.verifyClass] - order[b.verifyClass] || (a.created < b.created ? 1 : -1)))
const order = { failed: 0, stale: 1, never: 2, ok: 3 } as const
const vruns = computed(() => list(runs.data.value).filter((r) => r.kind === 'verify' || r.kind === 'scrub'))
const lastRun = computed(() => vruns.value[0] ?? null)
const coverage = computed(() => (sum.value.total ? sum.value.ok / sum.value.total : null))
const quarantined = computed(() => list(corrupt.data.value).filter((c) => c.state !== 'healed'))

const attCols: ColumnDef<SnapRow>[] = [
  { key: 'verify', label: 'State', value: (r) => r.verifyClass, width: '130px' },
  { key: 'object', label: 'Object', value: (r) => `${r.project}/${r.object}`, required: true },
  { key: 'kind', label: 'Kind', format: (r) => kindLabel(r.kind), width: '110px' },
  { key: 'host', label: 'Host', width: '100px' },
  { key: 'created', label: 'Created', value: (r) => r.created, width: '120px' },
  { key: 'verified', label: 'Last verified', value: (r) => r.verifiedAt ?? '', width: '130px' },
  { key: 'snapshot_id', label: 'Snapshot', mono: true, hidden: true },
]
const qCols: ColumnDef<CorruptChunk>[] = [
  { key: 'chunk_id', label: 'Chunk', mono: true, required: true },
  { key: 'state', label: 'State', width: '110px' },
  { key: 'detected_at', label: 'Detected', width: '120px' },
  { key: 'detail', label: 'Problem' },
  { key: 'affected', label: 'Snapshots affected', value: (r) => (r.affected ?? []).length, align: 'right', width: '150px' },
]
const runCols: ColumnDef<RunRow>[] = [
  { key: 'kind', label: 'Run', width: '90px' },
  { key: 'status', label: 'Result', width: '110px' },
  { key: 'started', label: 'Started', width: '130px' },
  { key: 'took', label: 'Took', value: (r) => (r.finished ? Date.parse(r.finished) - Date.parse(r.started) : null), align: 'right', width: '90px' },
  { key: 'summary', label: 'Summary', value: (r) => runSummary(r), sortable: false },
]
function runSummary(r: RunRow): string {
  if (r.error) return r.error
  try {
    const s = JSON.parse(r.stats || '{}') as { snapshots?: number; chunks?: number; skipped?: number; quarantined?: number; problems?: unknown[] | null }
    const parts = []
    if (s.snapshots !== undefined) parts.push(`${formatNumber(s.snapshots)} ${noun(s.snapshots, 'snapshot')}`)
    if (s.chunks !== undefined) parts.push(`${formatNumber(s.chunks)} ${noun(s.chunks, 'chunk')}`)
    if (s.skipped) parts.push(`${formatNumber(s.skipped)} skipped`)
    if (s.quarantined) parts.push(`${s.quarantined} quarantined`)
    if (s.problems?.length) parts.push(`${s.problems.length} ${noun(s.problems.length, 'problem')}`)
    return parts.join(', ')
  } catch { return '' }
}

// ---- server-side verification
const canRun = computed(() => hasRole('operator'))
const vreq = reactive({ mode: 'new' as 'new' | 'stale' | 'all', staleDays: 30, noQuarantine: false })
const verifying = ref(false)
const vres = shallowRef<VerifyResponse | null>(null)
const verr = ref('')
async function runVerify() {
  verifying.value = true
  vres.value = null
  verr.value = ''
  try {
    vres.value = await api.maintenance.verify({ mode: vreq.mode, stale_days: vreq.mode === 'stale' ? vreq.staleDays : undefined, no_quarantine: vreq.noQuarantine || undefined })
    if (vres.value.ok) toast.success('Verification finished', `${formatNumber(vres.value.stats.snapshots)} ${noun(vres.value.stats.snapshots, 'snapshot')} and ${formatNumber(vres.value.stats.chunks)} ${noun(vres.value.stats.chunks, 'chunk')} checked; nothing wrong.`)
    else toast.warn('Verification found problems', vres.value.error)
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    verr.value = (e as { status?: number }).status === 503 ? `${msg} Verification records its result in the repository, so it needs the repository online.` : msg
  } finally {
    verifying.value = false
    invalidate(`runs:${repos.current}`); invalidate(`corrupt:${repos.current}`); invalidate(`snapshots:${repos.current}`); invalidate(`snapshot-stats:${repos.current}`)
    void refreshSnaps(); void runs.refresh(); void corrupt.refresh()
  }
}

// ---- host deep verification (job)
const dreq = reactive({ host: '', last: 1, object: '' })
const backupHosts = computed(() => list(hosts.data.value).filter((h) => h.scope === 'backup' && !h.disabled))
const hostObjects = computed(() => list(objects.data.value).filter((o) => o.host === dreq.host && o.kind !== 'instance'))
const deepJob = shallowRef<Job | null>(null)
const deepErr = ref('')
async function runDeep() {
  deepErr.value = ''
  const o = hostObjects.value.find((x) => `${x.project}/${x.name}` === dreq.object)
  try {
    deepJob.value = await protectApi.jobs.create({ type: 'deep_verify', host: dreq.host, params: { last: dreq.last, project: o?.project, object: o?.name } })
    toast.success('Deep verification queued', `${dreq.host} starts it within seconds when it is online.`)
  } catch (e) {
    deepErr.value = e instanceof Error ? e.message : String(e)
  }
}
const affectedLink = (a: string) => { const [g, s] = a.split('/'); return { name: 'snapshot', params: { group: g, snap: s } } }
</script>

<template>
  <div data-testid="verify">
    <header class="page-head">
      <div>
        <h1>Verification</h1>
        <p class="lede">Proof that what is stored can still be read. The server checks every chunk object it holds; a host can go further and decrypt every chunk with its key. A snapshot that failed is never used as a base for new backups.</p>
      </div>
      <div class="actions"><RouterLink class="btn" :to="{ name: 'restore-tests' }"><PIcon name="badge-check" :size="15" />Restore tests</RouterLink></div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database">Verification starts once a repository exists.</EmptyState>
    <template v-else>
      <div class="tiles">
        <StatTile label="Verified recently" :value="coverage === null ? '–' : `${Math.round(coverage * 100)} %`" :hint="`${formatNumber(sum.ok)} of ${formatNumber(sum.total)} ${noun(sum.total, 'snapshot')}`" :status="coverage === null ? undefined : coverage >= 0.95 ? 'ok' : coverage >= 0.5 ? 'warn' : 'critical'" :loading="rows === undefined" />
        <StatTile label="Failed" :value="formatNumber(sum.failed)" :status="sum.failed ? 'critical' : 'ok'" :loading="rows === undefined" :to="{ name: 'snapshots', query: { verify: 'failed' } }" />
        <StatTile label="Stale" :value="formatNumber(sum.stale)" :status="sum.stale ? 'warn' : undefined" hint="older than the re-verify age" :loading="rows === undefined" :to="{ name: 'snapshots', query: { verify: 'stale' } }" />
        <StatTile label="Never verified" :value="formatNumber(sum.never)" :status="sum.never ? 'warn' : undefined" :loading="rows === undefined" :to="{ name: 'snapshots', query: { verify: 'never' } }" />
        <StatTile label="Quarantined chunks" :value="formatNumber(quarantined.length)" :status="quarantined.length ? 'critical' : 'ok'" :loading="corrupt.loading.value" />
        <StatTile label="Last verification" :value="lastRun ? formatRelative(lastRun.started) : 'never'" :status="lastRun ? (lastRun.status === 'ok' ? 'ok' : 'critical') : 'warn'" :hint="maint.data.value ? `schedule: ${describeSchedule(maint.data.value.verify)}` : ''" />
      </div>

      <div class="grid">
        <section class="panel">
          <header class="panel-head"><PIcon name="shield-check" /><h2>Verify on the server</h2></header>
          <div class="panel-body stack">
            <p class="muted small">Reads the chunk objects, index and manifest of the selected snapshots from the datastore and checks their integrity (no key needed). Corrupt chunks are moved to quarantine so the next backup re-uploads them.</p>
            <div class="modes" role="radiogroup" aria-label="What to verify">
              <label :class="{ on: vreq.mode === 'new' }"><input v-model="vreq.mode" type="radio" value="new" /> <span><strong>New snapshots</strong><small>Never verified ({{ formatNumber(sum.never) }}). Quick.</small></span></label>
              <label :class="{ on: vreq.mode === 'stale' }"><input v-model="vreq.mode" type="radio" value="stale" /> <span><strong>Stale and new</strong><small>Not verified within <input v-model.number="vreq.staleDays" type="number" min="1" max="3650" class="input days" aria-label="Days" /> days.</small></span></label>
              <label :class="{ on: vreq.mode === 'all' }"><input v-model="vreq.mode" type="radio" value="all" /> <span><strong>Everything</strong><small>Every snapshot and a full chunk scrub. Reads the whole repository: can take hours.</small></span></label>
            </div>
            <SwitchInput v-model="vreq.noQuarantine" label="Only report, do not quarantine corrupt chunks" />
            <Callout v-if="verr" status="critical" title="Could not verify">{{ verr }}</Callout>
            <Callout v-if="vres && vres.ok" status="ok" title="Nothing wrong" data-testid="verify-ok">{{ formatNumber(vres.stats.snapshots) }} {{ noun(vres.stats.snapshots, 'snapshot') }} and {{ formatNumber(vres.stats.chunks) }} {{ noun(vres.stats.chunks, 'chunk') }} checked{{ vres.stats.skipped ? `, ${formatNumber(vres.stats.skipped)} not selected by this mode` : '' }}.</Callout>
            <Callout v-else-if="vres" status="critical" title="Verification found problems" data-testid="verify-problems">
              {{ vres.error }}
              <ul class="probs"><li v-for="(p, i) in (vres.stats.problems ?? []).slice(0, 10)" :key="i"><strong>{{ p.kind }}</strong> {{ p.object }}: {{ p.detail }}</li></ul>
            </Callout>
            <div class="row">
              <Tooltip v-if="!canRun" text="Starting a verification needs the operator role."><button type="button" class="btn btn-primary" disabled><PIcon name="play" :size="14" />Verify now</button></Tooltip>
              <button v-else type="button" class="btn btn-primary" :disabled="verifying" data-testid="verify-run" @click="runVerify"><Spinner v-if="verifying" :size="13" /><PIcon v-else name="play" :size="14" />{{ verifying ? 'Verifying…' : 'Verify now' }}</button>
              <span v-if="verifying" class="muted small">The run continues on the server if you leave; it appears in the history below.</span>
            </div>
            <p v-if="currentRepo?.mode && currentRepo.mode !== 'online'" class="muted small">The repository is {{ currentRepo.mode }}: verification records its result in the repository and needs it online.</p>
          </div>
        </section>

        <section class="panel">
          <header class="panel-head"><PIcon name="key" /><h2>Deep verification on a host</h2></header>
          <div class="panel-body stack">
            <p class="muted small">The host downloads every chunk of the last snapshots, decrypts it with its key and recomputes its id: this also catches damage the server cannot see (it has no key). It runs as a job and never changes anything.</p>
            <div class="form-grid dv">
              <FormField label="Host" v-slot="f">
                <select :id="f.id" v-model="dreq.host" class="select" data-testid="deep-host"><option value="" disabled>Choose…</option><option v-for="h in backupHosts" :key="h.name" :value="h.name">{{ h.name }}</option></select>
              </FormField>
              <FormField label="Object" optional hint="Empty: every object of the host" v-slot="f">
                <select :id="f.id" v-model="dreq.object" class="select" :disabled="!dreq.host"><option value="">Every object</option><option v-for="o in hostObjects" :key="o.group_id" :value="`${o.project}/${o.name}`">{{ o.project }}/{{ o.name }}</option></select>
              </FormField>
              <FormField label="Last snapshots per object" v-slot="f">
                <input :id="f.id" v-model.number="dreq.last" type="number" min="1" max="1000" class="input" />
              </FormField>
            </div>
            <Callout v-if="deepErr" status="critical" title="Could not start the job">{{ deepErr }}</Callout>
            <div class="row">
              <Tooltip v-if="!canRun" text="Starting a host job needs the operator role."><button type="button" class="btn" disabled><PIcon name="play" :size="14" />Start deep verification</button></Tooltip>
              <button v-else type="button" class="btn" :disabled="!dreq.host || dreq.last < 1" data-testid="deep-run" @click="runDeep"><PIcon name="play" :size="14" />Start deep verification</button>
            </div>
          </div>
        </section>
      </div>
      <JobPanel v-if="deepJob" :job-id="deepJob.id" compact class="gap" />

      <section class="block">
        <h2 class="bh">Needs attention <span class="muted">({{ formatNumber(attention.length) }})</span></h2>
        <DataTable :rows="rows === undefined ? undefined : attention" :columns="attCols" row-key="key" storage-key="verify-attention" csv-name="verification-attention" :loading="rows === undefined" :initial-sort="null" max-height="420px" data-testid="attention-table">
          <template #cell-verify="{ row }"><VerifyBadge :state="row.verifyClass" :at="row.verifiedAt" /></template>
          <template #cell-object="{ row }"><RouterLink :to="{ name: 'snapshot', params: { group: row.group_id, snap: row.snapshot_id } }"><span class="muted">{{ row.project }}/</span>{{ row.object }}</RouterLink></template>
          <template #cell-created="{ row }"><RelativeTime :time="row.created" /></template>
          <template #cell-verified="{ row }"><RelativeTime :time="row.verifiedAt" never="never" /></template>
          <template #empty><EmptyState title="Every snapshot verified recently" icon="circle-check" /></template>
        </DataTable>
      </section>

      <section class="block">
        <h2 class="bh">Quarantined chunks <span class="muted">({{ formatNumber(quarantined.length) }})</span></h2>
        <p class="muted small bp">A chunk object that failed its check is moved aside, not deleted: the next backup of an affected object uploads a good copy (healing it), and snapshots that need it stay marked failed until then.</p>
        <DataTable :rows="corrupt.data.value === undefined ? undefined : quarantined" :columns="qCols" row-key="chunk_id" storage-key="verify-quarantine" csv-name="quarantined-chunks" :loading="corrupt.loading.value" :error="corrupt.error.value" :retry="corrupt.refresh" data-testid="quarantine-table">
          <template #cell-chunk_id="{ row }"><span class="mono" :title="row.chunk_id">{{ row.chunk_id.slice(0, 20) }}…</span></template>
          <template #cell-state="{ row }"><StatusPill :state="row.state === 'quarantined' ? 'corrupt' : row.state" :label="row.state" size="sm" /></template>
          <template #cell-detected_at="{ row }"><RelativeTime :time="row.detected_at" /></template>
          <template #cell-affected="{ row }">
            <span class="aff"><RouterLink v-for="a in (row.affected ?? []).slice(0, 3)" :key="a" :to="affectedLink(a)" class="mono">{{ a.split('/')[1] }}</RouterLink><span v-if="(row.affected ?? []).length > 3" class="muted">+{{ (row.affected ?? []).length - 3 }}</span></span>
          </template>
          <template #empty><EmptyState title="No quarantined chunks" icon="circle-check">Every chunk read so far passed its check.</EmptyState></template>
        </DataTable>
        <p v-if="list(healed.data.value).filter((c) => c.state === 'healed').length" class="muted small bp">{{ list(healed.data.value).filter((c) => c.state === 'healed').length }} earlier quarantined chunks were healed by later backups.</p>
      </section>

      <section class="block">
        <h2 class="bh">History</h2>
        <DataTable :rows="runs.data.value === undefined ? undefined : vruns" :columns="runCols" row-key="started" storage-key="verify-runs" csv-name="verification-runs" :loading="runs.loading.value" :error="runs.error.value" :retry="runs.refresh" max-height="420px" data-testid="verify-history">
          <template #cell-status="{ row }"><StatusPill :state="row.status" size="sm" /></template>
          <template #cell-started="{ row }"><RelativeTime :time="row.started" /></template>
          <template #cell-took="{ row }"><span v-if="row.finished" class="num">{{ formatDuration(Date.parse(row.finished) - Date.parse(row.started)) }}</span><span v-else class="muted">running</span></template>
          <template #empty><EmptyState title="No verification has run yet" icon="shield-check">Start one above, or set a verification schedule under Repositories.</EmptyState></template>
        </DataTable>
      </section>
    </template>
  </div>
</template>

<style scoped>
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: var(--s-3); margin-bottom: var(--s-4); }
.grid { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: var(--s-4); align-items: start; margin-bottom: var(--s-4); }
@media (max-width: 1000px) { .grid { grid-template-columns: minmax(0, 1fr); } }
.small { font-size: var(--fs-sm); }
.modes { display: grid; gap: var(--s-2); }
.modes label { display: flex; gap: var(--s-2); align-items: flex-start; padding: var(--s-2) var(--s-3); border: 1px solid var(--border); border-radius: var(--r); cursor: pointer; }
.modes label.on { border-color: var(--accent); background: var(--accent-soft); }
.modes span { display: flex; flex-direction: column; gap: 2px; }
.modes small { color: var(--text-muted); font-size: var(--fs-sm); }
.days { width: 72px; height: 24px; display: inline-block; padding: 0 6px; }
.probs { margin: var(--s-2) 0 0; padding-left: 18px; }
.dv { grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); }
.gap { margin-bottom: var(--s-4); }
.block { margin-bottom: var(--s-6); }
.bh { font-size: var(--fs-md); margin-bottom: var(--s-2); }
.bp { margin-bottom: var(--s-2); max-width: 90ch; }
.mono { font-family: var(--font-mono); font-size: var(--fs-sm); }
.aff { display: inline-flex; gap: 6px; flex-wrap: wrap; justify-content: flex-end; }
</style>
