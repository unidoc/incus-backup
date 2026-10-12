<script setup lang="ts">
// Tasks: start verify, retention, GC and reconcile now (each with a preview or a confirmation and a
// result view), and the run history with live running state.
//
// Verify, retention apply, GC and reconcile run as server TASKS: the start answers at once with a task id
// and the card follows it live (progress, log, cancel) through TaskPanel; the result appears when it
// ends. Previews (retention plan, GC dry run) answer directly. Tasks already running on this repository
// (started elsewhere or by the schedule) are picked up when the tab opens. The history below is the
// maintenance-run table (GET /runs), every run linked to its task on the Tasks page.
import { computed, reactive, ref, onBeforeUnmount, watch } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import Modal from '@/components/Modal.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import KeyValue from '@/components/KeyValue.vue'
import Tooltip from '@/components/Tooltip.vue'
import PrunePlanTable from '@/components/storage/PrunePlanTable.vue'
import TaskPanel from '@/components/ops/TaskPanel.vue'
import { opsApi, type TaskAccepted, type TaskDetail } from '@/api/sections/ops'
import { list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import type { RepoApi } from '@/api/sections/storage'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { formatBytes, formatDuration, formatNumber, plural } from '@/lib/format'
import { isRunning, parseRunStats, runDurationMs, runKindLabel, runState, runSummary } from '@/lib/storage'
import type { ColumnDef } from '@/lib/table'
import type { GCStats, PruneResult, ReconcileResult, RepoView, RunRow, VerifyResponse } from '@/api/types'

const props = defineProps<{ repo: RepoView; api: RepoApi }>()
// a 1 s clock while something runs (elapsed time, live durations)
const now = ref(Date.now())
const writable = computed(() => props.repo.mode === 'online')
const offline = computed(() => props.repo.mode === 'offline')

// ---- runs, polled fast while something runs
const runs = useResource(() => `runs:${props.repo.name}:100`, (s) => props.api.runs(100, s), { refreshOn: ['run'] })
const rows = computed(() => list(runs.data.value).map((r, i) => ({ ...r, key: `${r.kind}-${r.started}-${i}` })))
const anyRunning = computed(() => rows.value.some((r) => isRunning(r)))
const m = useResource(() => `maintenance:${props.repo.name}`, (s) => props.api.maintenance(s))

type Kind = 'verify' | 'prune' | 'gc' | 'reconcile'
interface Task { busy: boolean; started: number; error: string }
const task = reactive<Record<Kind, Task>>({ verify: t0(), prune: t0(), gc: t0(), reconcile: t0() })
function t0(): Task { return { busy: false, started: 0, error: '' } }
const pending = computed(() => Object.values(task).some((t) => t.busy))
let poll: ReturnType<typeof setInterval> | null = null
let tick: ReturnType<typeof setInterval> | null = null
watch(() => pending.value || anyRunning.value, (on) => {
  if (on && !poll) { poll = setInterval(() => void runs.refresh(), 3000); tick = setInterval(() => { now.value = Date.now() }, 1000) }
  if (!on && poll) { clearInterval(poll); poll = null; if (tick) clearInterval(tick); tick = null; now.value = Date.now() }
}, { immediate: true })
onBeforeUnmount(() => { if (poll) clearInterval(poll); if (tick) clearInterval(tick) })

async function track<T>(k: Kind, fn: () => Promise<T>): Promise<T | undefined> {
  task[k] = { busy: true, started: Date.now(), error: '' }
  void runs.refresh()
  try {
    return await fn()
  } catch (e) {
    task[k].error = errorText(e)
    toast.error(e, `${({ verify: 'Verify', prune: 'Retention', gc: 'GC', reconcile: 'Reconcile' })[k]} failed`)
    return undefined
  } finally {
    task[k].busy = false
    invalidate(`runs:${props.repo.name}`)
    invalidate('overview')
    invalidate(`corrupt:${props.repo.name}`)
    void runs.refresh()
  }
}
const elapsed = (k: Kind) => formatDuration(now.value - task[k].started)

// ---- live tasks per card (the task id, and whether it still runs)
const live = reactive<Record<Kind, string>>({ verify: '', prune: '', gc: '', reconcile: '' })
const liveActive = reactive<Record<Kind, boolean>>({ verify: false, prune: false, gc: false, reconcile: false })
const KIND_CARD: Record<string, Kind> = { verify: 'verify', scrub: 'verify', retention: 'prune', gc: 'gc', reconcile: 'reconcile' }
const LABEL: Record<Kind, string> = { verify: 'Verify', prune: 'Retention', gc: 'Garbage collection', reconcile: 'Reconcile' }
const blocked = (k: Kind) => task[k].busy || liveActive[k]
async function startTask(k: Kind, fn: () => Promise<TaskAccepted>) {
  task[k] = { busy: true, started: Date.now(), error: '' }
  try {
    const r = await fn()
    live[k] = r.task.id
    liveActive[k] = true
    toast.info(`${LABEL[k]} started`, 'It runs on the server; follow it here or on the Tasks page. Leaving the page does not stop it.')
  } catch (e) {
    task[k].error = errorText(e)
    toast.error(e, `Could not start ${LABEL[k].toLowerCase()}`)
  } finally {
    task[k].busy = false
    invalidate(`runs:${props.repo.name}`)
    invalidate('tasks:')
  }
}
function onUpdate(k: Kind, d: TaskDetail) { liveActive[k] = d.state === 'queued' || d.state === 'running' }
function onDone(k: Kind, d: TaskDetail) {
  liveActive[k] = false
  invalidate(`runs:${props.repo.name}`)
  invalidate('overview')
  invalidate(`corrupt:${props.repo.name}`)
  invalidate('tasks:')
  void runs.refresh()
  if (d.state !== 'succeeded') {
    if (d.state === 'failed') toast.error(d.error, `${LABEL[k]} failed`)
    return
  }
  const r = d.result as Record<string, unknown> | null
  if (k === 'verify' && r) {
    verifyResult.value = r as unknown as VerifyResponse
    if (verifyResult.value.ok) toast.success('Verify finished: no problems', `${plural(verifyResult.value.stats.snapshots, 'snapshot')}, ${plural(verifyResult.value.stats.chunks, 'chunk')} checked.`)
    else toast.warn('Verify found problems', verifyResult.value.error ?? 'See the Integrity tab.')
  } else if (k === 'prune' && r) {
    pruneApplied.value = r as unknown as PruneResult
    prunePreview.value = null
    toast.success('Retention applied', `${plural(pruneApplied.value.removed, 'snapshot')} removed, ${formatNumber(pruneApplied.value.kept)} kept.`)
  } else if (k === 'gc' && r) {
    gcDone.value = r as unknown as GCStats
    gcDry.value = null
    toast.success('GC finished', `${plural(gcDone.value.removed, 'chunk')} removed, ${formatBytes(gcDone.value.bytes_reclaimed)} freed.`)
  } else if (k === 'reconcile' && r) {
    recResult.value = r as unknown as ReconcileResult
    toast.success('Reconcile finished', `${plural(recResult.value.inserted, 'record')} added, ${formatNumber(recResult.value.removed)} removed.`)
  }
}
// pick up what already runs on this repository (scheduled, or started elsewhere)
void opsApi.tasks.list({ state: 'active', repository: props.repo.name, limit: 20 }).then((l) => {
  for (const t of l ?? []) {
    const k = KIND_CARD[t.kind]
    if (k && !live[k]) { live[k] = t.id; liveActive[k] = true }
  }
}).catch(() => { /* the history still shows running runs */ })

// ---- verify
const v = reactive({ mode: 'new' as 'all' | 'new' | 'stale', staleDays: 0, noQuarantine: false })
const verifyResult = ref<VerifyResponse | null>(null)
async function runVerify() {
  if (v.mode === 'all' && !(await confirm({ title: 'Verify everything?', message: `Reads every chunk object of ${props.repo.name} once${props.repo.backend === 's3' ? ' (S3 GET requests and egress are billed)' : ''}. On a large repository this takes long; the server keeps going if you close the page.`, confirmLabel: 'Start full verify' }))) return
  verifyResult.value = null
  await startTask('verify', () => opsApi.start.verify(props.repo.name, { mode: v.mode, stale_days: v.mode === 'stale' && v.staleDays > 0 ? v.staleDays : undefined, no_quarantine: v.noQuarantine || undefined }))
}

// ---- retention: preview, then apply
const prunePreview = ref<PruneResult | null>(null)
const pruneApplied = ref<PruneResult | null>(null)
const previewAt = ref(0)
async function previewPrune() {
  pruneApplied.value = null
  const r = await track('prune', () => props.api.prune(true))
  if (r) { prunePreview.value = r; previewAt.value = Date.now() }
}
async function applyPrune() {
  const p = prunePreview.value
  if (!p) return
  if (!(await confirm({ title: `Remove ${plural(p.removed, 'snapshot')} from ${props.repo.name}?`, danger: true, typeToConfirm: props.repo.name, confirmLabel: 'Apply retention',
    message: `Keeps ${formatNumber(p.kept)} (${formatNumber(p.protected)} protected) in ${plural(p.groups, 'object')}. The plan is computed again when applied; anything that changed since the preview is decided anew. Their chunks are freed by the next GC.` }))) return
  pruneApplied.value = null
  await startTask('prune', () => opsApi.start.prune(props.repo.name))
}

// ---- GC: dry run, then run
const g = reactive({ grace: 0 })
watch(() => m.data.value, (x) => { if (x && !g.grace) g.grace = x.gc_grace_hours }, { immediate: true })
const graceError = computed(() => (g.grace < 12 ? 'At least 12 hours.' : ''))
const gcDry = ref<GCStats | null>(null)
const gcDone = ref<GCStats | null>(null)
async function dryGC() {
  gcDone.value = null
  const r = await track('gc', () => props.api.gc(true, g.grace))
  if (r) gcDry.value = r
}
async function runGC() {
  const d = gcDry.value
  if (!(await confirm({ title: `Run garbage collection on ${props.repo.name}?`, danger: !!d?.removed, typeToConfirm: d?.removed ? props.repo.name : undefined, confirmLabel: 'Run GC',
    message: d ? `The dry run would delete ${plural(d.removed, 'chunk')} (${formatBytes(d.bytes_reclaimed)}) that no snapshot references and that are older than ${g.grace} h. Chunks of running backups are pinned and stay.` : 'Deletes unreferenced chunks older than the grace period.' }))) return
  gcDone.value = null
  await startTask('gc', () => opsApi.start.gc(props.repo.name, false, g.grace))
}

// ---- reconcile
const recResult = ref<ReconcileResult | null>(null)
async function runReconcile() {
  if (!(await confirm({ title: `Reconcile ${props.repo.name}?`, confirmLabel: 'Reconcile', message: 'Lists the whole datastore and repairs the database: snapshots that exist but are missing are added (with protection, notes and verification state), records of snapshots that no longer exist are removed. Nothing in the datastore changes.' }))) return
  recResult.value = null
  await startTask('reconcile', () => opsApi.start.reconcile(props.repo.name))
}

// ---- history
const kinds = computed(() => [...new Set(rows.value.map((r) => r.kind))])
const kindFilter = ref('')
const shown = computed(() => rows.value.filter((r) => !kindFilter.value || r.kind === kindFilter.value))
const strips = computed(() => kinds.value.map((k) => ({ kind: k, runs: rows.value.filter((r) => r.kind === k).slice(0, 14).reverse() })))
type Row = (typeof rows.value)[number]
const cols: ColumnDef<Row>[] = [
  { key: 'kind', label: 'Task', format: (r) => runKindLabel(r.kind), required: true },
  { key: 'status', label: 'Result', value: (r) => runState(r) },
  { key: 'started', label: 'Started' },
  { key: 'duration', label: 'Duration', align: 'right', value: (r) => runDurationMs(r, now.value), format: (r) => formatDuration(runDurationMs(r, now.value)) },
  { key: 'summary', label: 'Summary', value: (r) => runSummary(r), sortable: false },
]
const open = ref<Row | null>(null)
const openStats = computed(() => (open.value ? parseRunStats(open.value) : null))
const openOpen = computed({ get: () => !!open.value, set: (x: boolean) => { if (!x) open.value = null } })
const stateLabel = (s: string) => (s === 'warning' ? 'problems found' : s)
const dis = (need: 'operator' | 'admin', write = true) => !hasRole(need) || (write ? !writable.value : offline.value)
const why = (need: 'operator' | 'admin', write = true) => (!hasRole(need) ? `Needs the ${need} role` : write && !writable.value ? `The repository is ${props.repo.mode}` : !write && offline.value ? 'The repository is offline' : '')
</script>

<template>
  <div class="tasks">
    <Callout v-if="!writable" :status="offline ? 'critical' : 'warn'" class="mb" :title="`The repository is ${repo.mode}`">
      {{ offline ? 'No task can run until it is online again.' : 'Verify, retention, GC and reconcile write to the repository; dry runs and previews still work.' }}
    </Callout>

    <div class="cards">
      <!-- verify -->
      <section class="panel card" data-testid="task-verify">
        <div class="panel-head"><Icon name="shield-check" :size="16" /><h2>Verify</h2><span class="sub">operator</span></div>
        <div class="panel-body form-grid">
          <p class="txt">Checks chunk trailers, indexes and manifests without any key; each chunk is read once per run. Corrupt chunks are quarantined and the next backup of an affected object reads everything again and heals them.</p>
          <div class="grid2">
            <FormField v-slot="f" label="What to check">
              <SelectInput :id="f.id" v-model="v.mode" :options="[{ value: 'new', label: 'New snapshots (never verified)' }, { value: 'stale', label: 'New and stale snapshots' }, { value: 'all', label: 'Everything (full scrub)' }]" data-testid="verify-mode" />
            </FormField>
            <FormField v-if="v.mode === 'stale'" v-slot="f" label="Stale after (days)" hint="0 = each policy's re-verify age">
              <input :id="f.id" v-model.number="v.staleDays" type="number" min="0" class="input num" />
            </FormField>
          </div>
          <SwitchInput v-model="v.noQuarantine" label="Report only: do not quarantine corrupt chunks" />
          <div class="form-actions">
            <button :title="why('operator')" type="button" class="btn btn-primary" :disabled="blocked('verify') || dis('operator')" data-testid="verify-run" @click="runVerify"><Spinner v-if="task.verify.busy" :size="13" /><Icon v-else name="play" />Start verify</button>
          </div>
          <TaskPanel v-if="live.verify" :id="live.verify" :show-result="false" log-height="180px" @update="(d) => onUpdate('verify', d)" @done="(d) => onDone('verify', d)" />
          <div v-if="verifyResult" class="result" data-testid="verify-result">
            <StatusPill :status="verifyResult.ok ? 'ok' : 'critical'" :label="verifyResult.ok ? 'No problems' : `${verifyResult.stats.problems?.length ?? 0} problems`" />
            <KeyValue :items="[
              { key: 's', label: 'Snapshots checked', value: formatNumber(verifyResult.stats.snapshots) },
              { key: 'c', label: 'Chunks read', value: formatNumber(verifyResult.stats.chunks) },
              { key: 'k', label: 'Skipped (not selected)', value: formatNumber(verifyResult.stats.skipped) },
              { key: 'q', label: 'Quarantined', value: formatNumber(verifyResult.stats.quarantined) },
            ]" />
            <ul v-if="verifyResult.stats.problems?.length" class="probs"><li v-for="(p, i) in verifyResult.stats.problems.slice(0, 10)" :key="i"><b>{{ p.kind }}</b> <code class="mono">{{ p.object }}</code> <span class="muted">{{ p.detail }}</span></li></ul>
          </div>
        </div>
      </section>

      <!-- retention -->
      <section class="panel card" :class="{ wide: prunePreview || pruneApplied }" data-testid="task-prune">
        <div class="panel-head"><Icon name="history" :size="16" /><h2>Retention</h2><span class="sub">admin · preview first</span></div>
        <div class="panel-body form-grid">
          <p class="txt">Applies each object's retention policy. The preview lists every snapshot with keep or remove and the rule that keeps it; apply removes exactly what the plan says at that moment. Protected snapshots and the newest of each object always stay.</p>
          <div class="form-actions">
            <button :title="why('admin', false)" type="button" class="btn" :disabled="blocked('prune') || dis('admin', false)" data-testid="prune-preview" @click="previewPrune"><Spinner v-if="task.prune.busy && !prunePreview" :size="13" /><Icon v-else name="eye" />Preview</button>
            <button :title="why('admin') || (!prunePreview ? 'Preview first' : '')" type="button" class="btn btn-danger" :disabled="blocked('prune') || dis('admin') || !prunePreview" data-testid="prune-apply" @click="applyPrune"><Icon name="trash-2" />Apply{{ prunePreview ? ` (remove ${prunePreview.removed})` : '' }}</button>
            <span v-if="task.prune.busy && !prunePreview" class="run muted small">Computing the plan {{ elapsed('prune') }}…</span>
          </div>
          <TaskPanel v-if="live.prune" :id="live.prune" :show-result="false" log-height="180px" @update="(d) => onUpdate('prune', d)" @done="(d) => onDone('prune', d)" />
          <div v-if="prunePreview" class="result">
            <p class="small"><b>Preview</b> from <RelativeTime :time="previewAt" />: {{ plural(prunePreview.removed, 'snapshot') }} would be removed, {{ formatNumber(prunePreview.kept) }} kept ({{ formatNumber(prunePreview.protected) }} protected) across {{ plural(prunePreview.groups, 'object') }}.</p>
            <PrunePlanTable :plan="prunePreview.plan" dry-run />
          </div>
          <div v-if="pruneApplied" class="result" data-testid="prune-result">
            <StatusPill status="ok" :label="`${pruneApplied.removed} removed`" />
            <PrunePlanTable :plan="pruneApplied.plan" />
          </div>
        </div>
      </section>

      <!-- gc -->
      <section class="panel card" data-testid="task-gc">
        <div class="panel-head"><Icon name="trash-2" :size="16" /><h2>Garbage collection</h2><span class="sub">admin · dry run first</span></div>
        <div class="panel-body form-grid">
          <p class="txt">Deletes chunks no committed snapshot references, once they are older than the grace period. Chunks of running backups and syncs are pinned and never deleted.</p>
          <FormField v-slot="f" label="Grace (hours)" :error="graceError" :hint="m.data.value ? `Repository setting: ${m.data.value.gc_grace_hours} h` : ''">
            <input :id="f.id" v-model.number="g.grace" type="number" min="12" step="1" class="input num short" />
          </FormField>
          <div class="form-actions">
            <button :title="why('admin', false)" type="button" class="btn" :disabled="blocked('gc') || dis('admin', false) || !!graceError" data-testid="gc-dry" @click="dryGC"><Spinner v-if="task.gc.busy && !gcDry" :size="13" /><Icon v-else name="eye" />Dry run</button>
            <button :title="why('admin')" type="button" class="btn btn-danger" :disabled="blocked('gc') || dis('admin') || !!graceError" data-testid="gc-run" @click="runGC"><Icon name="trash-2" />Run GC</button>
            <span v-if="task.gc.busy" class="run muted small">Dry run {{ elapsed('gc') }}…</span>
          </div>
          <TaskPanel v-if="live.gc" :id="live.gc" :show-result="false" log-height="180px" @update="(d) => onUpdate('gc', d)" @done="(d) => onDone('gc', d)" />
          <div v-for="(x, i) in [gcDry, gcDone].filter(Boolean)" :key="i" class="result" :data-testid="x!.dry_run ? 'gc-dry-result' : 'gc-result'">
            <p class="small"><b>{{ x!.dry_run ? 'Dry run' : 'Done' }}:</b> {{ x!.dry_run ? 'would remove' : 'removed' }} {{ plural(x!.removed, 'chunk') }}, {{ formatBytes(x!.bytes_reclaimed) }}.</p>
            <KeyValue :columns="2" :items="[
              { key: 'scanned', label: 'Chunks scanned', value: formatNumber(x!.scanned) },
              { key: 'live', label: 'Referenced', value: formatNumber(x!.live) },
              { key: 'young', label: 'Kept: younger than grace', value: formatNumber(x!.kept_young) },
              { key: 'pinned', label: 'Kept: pinned by a session', value: formatNumber(x!.kept_pinned) },
              { key: 'q', label: 'Quarantine purged', value: formatNumber(x!.quarantine_purged) },
              { key: 'o', label: 'Orphans removed', value: formatNumber(x!.orphans_removed) },
            ]" />
          </div>
        </div>
      </section>

      <!-- reconcile -->
      <section class="panel card" data-testid="task-reconcile">
        <div class="panel-head"><Icon name="refresh-cw" :size="16" /><h2>Reconcile</h2><span class="sub">admin</span></div>
        <div class="panel-body form-grid">
          <p class="txt">Rebuilds the database records of this repository from the datastore, which is the source of truth. Run it after attaching a repository or restoring the database.</p>
          <div class="form-actions">
            <button :title="why('admin')" type="button" class="btn" :disabled="blocked('reconcile') || dis('admin')" data-testid="reconcile-run" @click="runReconcile"><Spinner v-if="task.reconcile.busy" :size="13" /><Icon v-else name="play" />Reconcile</button>
          </div>
          <TaskPanel v-if="live.reconcile" :id="live.reconcile" :show-result="false" log-height="180px" @update="(d) => onUpdate('reconcile', d)" @done="(d) => onDone('reconcile', d)" />
          <p v-if="recResult" class="result small"><StatusPill status="ok" label="Done" size="sm" /> {{ plural(recResult.inserted, 'record') }} added, {{ formatNumber(recResult.removed) }} removed.</p>
        </div>
      </section>
    </div>

    <section class="panel hist">
      <div class="panel-head"><h2>Run history</h2><RouterLink :to="{ name: 'tasks', query: { repository: repo.name } }" class="small all">All tasks</RouterLink><span class="sub">{{ anyRunning ? 'a task is running' : 'newest first' }}</span><Spinner v-if="anyRunning" :size="12" /></div>
      <div v-if="strips.length" class="strips panel-body">
        <div v-for="s in strips" :key="s.kind" class="strip">
          <button type="button" class="sk" :class="{ on: kindFilter === s.kind }" :aria-pressed="kindFilter === s.kind" @click="kindFilter = kindFilter === s.kind ? '' : s.kind">{{ runKindLabel(s.kind) }}</button>
          <span class="dots">
            <Tooltip v-for="(r, i) in s.runs" :key="i" :text="`${r.started.replace('T', ' ').slice(0, 16)} UTC · ${stateLabel(runState(r))} · ${runSummary(r)}`">
              <span class="dot" :class="`d-${runState(r)}`" />
            </Tooltip>
          </span>
        </div>
      </div>
      <DataTable :rows="runs.data.value === undefined ? undefined : shown" :columns="cols" row-key="key" storage-key="repo-runs" csv-name="maintenance-runs" :loading="runs.loading.value" :error="runs.error.value" :retry="runs.refresh"
        row-clickable dense max-height="520px" empty-title="No maintenance run yet" @row-click="(r) => (open = r)">
        <template #cell-status="{ row }"><StatusPill :state="runState(row) === 'warning' ? 'warn' : runState(row)" :label="stateLabel(runState(row))" size="sm" /></template>
        <template #cell-started="{ row }"><RelativeTime :time="row.started" /></template>
        <template #cell-summary="{ row }"><span class="truncate summ" :title="runSummary(row)">{{ runSummary(row) }}</span></template>
      </DataTable>
    </section>

    <Modal v-model:open="openOpen" :title="open ? runKindLabel(open.kind) : ''" :description="open ? `${repo.name} · ${open.started}` : ''" drawer width="720px">
      <div v-if="open" class="stack">
        <KeyValue :items="[
          { key: 'state', label: 'Result' },
          { key: 'started', label: 'Started' },
          { key: 'finished', label: 'Finished' },
          { key: 'duration', label: 'Duration', value: formatDuration(runDurationMs(open, now)) },
        ]">
          <template #value-state><StatusPill :state="runState(open) === 'warning' ? 'warn' : runState(open)" :label="stateLabel(runState(open))" size="sm" /></template>
          <template #value-started><AbsoluteTime :time="open.started" seconds /></template>
          <template #value-finished><AbsoluteTime :time="open.finished" seconds never="still running" /></template>
        </KeyValue>
        <Callout v-if="open.error" status="critical" title="Error">{{ open.error }}</Callout>
        <p>{{ runSummary(open) }}</p>
        <PrunePlanTable v-if="openStats?.kind === 'retention'" :plan="openStats.s.plan" :dry-run="openStats.s.dry_run" />
        <template v-if="(openStats?.kind === 'verify' || openStats?.kind === 'scrub') && openStats.s.problems?.length">
          <h3 class="h3">Problems</h3>
          <ul class="probs"><li v-for="(p, i) in openStats.s.problems" :key="i"><b>{{ p.kind }}</b> <code class="mono">{{ p.object }}</code> <span class="muted">{{ p.detail }}</span></li></ul>
        </template>
        <details class="raw"><summary>Raw statistics</summary><pre class="mono">{{ open.stats ? JSON.stringify(JSON.parse(open.stats), null, 2) : 'none' }}</pre></details>
      </div>
    </Modal>
  </div>
</template>

<style scoped>
.mb { margin-bottom: var(--s-4); }
.tasks { display: flex; flex-direction: column; gap: var(--s-4); }
.cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 440px), 1fr)); gap: var(--s-4); align-items: start; }
.card.wide { grid-column: 1 / -1; }
.card .panel-head .icon { color: var(--text-muted); }
.txt { color: var(--text-2); font-size: var(--fs-sm); }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: var(--s-3); }
.short { max-width: 120px; }
.run { display: inline-flex; gap: 6px; align-items: center; }
.result { display: flex; flex-direction: column; gap: 8px; padding-top: 10px; border-top: 1px solid var(--border); min-width: 0; }
.probs { margin: 0; padding-left: 18px; font-size: var(--fs-sm); display: flex; flex-direction: column; gap: 3px; overflow-wrap: anywhere; }
.strips { display: flex; flex-direction: column; gap: 6px; border-bottom: 1px solid var(--border); }
.strip { display: flex; align-items: center; gap: 10px; }
.sk { width: 170px; text-align: left; border: 0; background: none; color: var(--text-2); font: inherit; font-size: var(--fs-sm); cursor: pointer; padding: 2px 6px; border-radius: var(--r-sm); }
.sk:hover, .sk.on { background: var(--surface-3); color: var(--text); }
.dots { display: flex; gap: 4px; flex-wrap: wrap; }
.dot { display: inline-block; width: 12px; height: 12px; border-radius: 3px; background: var(--ok); }
.d-failed { background: var(--critical); } .d-warning, .d-partial { background: var(--warn); } .d-running { background: var(--info); animation: pulse 1.2s infinite; }
@keyframes pulse { 50% { opacity: .4; } }
@media (prefers-reduced-motion: reduce) { .d-running { animation: none; } }
.summ { display: inline-block; max-width: 520px; vertical-align: middle; }
.raw pre { max-height: 300px; overflow: auto; background: var(--surface-2); padding: 8px; border-radius: var(--r); font-size: 12px; }
.h3 { font-size: var(--fs-md); }
.all { margin-left: 8px; }
@media (max-width: 600px) { .grid2 { grid-template-columns: 1fr; } .sk { width: 120px; } }
</style>
