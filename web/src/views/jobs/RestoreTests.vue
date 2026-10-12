<script setup lang="ts">
// Restore tests: "a backup that was never restored is not a backup". Per object: the latest test and
// whether it is within the policy's restore-test schedule (passed / failed / overdue / never), the
// history, failure details, and "Run now" (a restore_test job on the object's host). The confidence
// tile is the share of objects whose latest test passed within their schedule.
import { computed, reactive, ref, shallowRef } from 'vue'
import { RouterLink } from 'vue-router'
import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import StatusPill from '@/components/StatusPill.vue'
import StatTile from '@/components/StatTile.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import Callout from '@/components/Callout.vue'
import Modal from '@/components/Modal.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import Tooltip from '@/components/Tooltip.vue'
import Gauge from '@/components/Gauge.vue'
import PIcon from '@/components/protect/PIcon.vue'
import JobPanel from '@/components/protect/JobPanel.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { ApiError } from '@/api/client'
import { protectApi, type Job, type RestoreTest, type RestoreTestMethod } from '@/api/sections/protect'
import { repos, repoNone } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { defaultMethod, restoreConfidence, schedulePeriodMs, testHealth, type TestHealth } from '@/lib/protect/jobs'
import { kindLabel } from '@/lib/protect/snapshots'
import { describeSchedule, formatDuration, formatNumber, formatPercent } from '@/lib/format'
import { noun } from '@/lib/protect/text'
import type { ColumnDef } from '@/lib/table'
import type { ObjectRow } from '@/api/types'

const on = () => !!repos.current && !repoNone.value
const notFound = (e: unknown) => e instanceof ApiError && e.status === 404
const latest = useResource(() => `restore-tests-latest:${repos.current}`, async (s) => { try { return await protectApi.restoreTests.latest(s) } catch (e) { if (notFound(e)) return null; throw e } }, { enabled: on, refreshOn: ['job'] })
const history = useResource(() => `restore-tests:${repos.current}:all`, async (s) => { try { return await protectApi.restoreTests.list({ limit: 300 }, s) } catch (e) { if (notFound(e)) return null; throw e } }, { enabled: on, refreshOn: ['job'] })
const objects = useResource(() => `objects:${repos.current}`, (s) => api.objects.list(s), { enabled: on })
const policies = useResource(() => `policies:${repos.current}`, (s) => api.policies.list(s), { enabled: on, ttl: 60_000 })

interface Row { key: string; obj: ObjectRow | null; object: string; host: string; kind: string; policy: string; schedule: string; period: number | null; last: RestoreTest | undefined; health: TestHealth }
const rows = computed<Row[] | undefined>(() => {
  if (objects.data.value === undefined || latest.data.value === undefined) return undefined
  const pol = new Map(list(policies.data.value).map((p) => [p.name, p]))
  const byObj = new Map<string, RestoreTest>()
  for (const t of list(latest.data.value)) byObj.set(`${t.host}|${t.object}`, t)
  const out: Row[] = []
  const seen = new Set<string>()
  for (const o of list(objects.data.value)) {
    const object = `${o.project}/${o.name}`
    const k = `${o.host}|${object}`
    seen.add(k)
    const policy = o.retention_policy || o.declared_policy
    const schedule = pol.get(policy)?.restore_test_schedule ?? ''
    const period = schedulePeriodMs(schedule)
    const last = byObj.get(k)
    out.push({ key: k, obj: o, object, host: o.host, kind: o.kind, policy, schedule, period, last, health: testHealth(last, period) })
  }
  for (const [k, t] of byObj) if (!seen.has(k)) out.push({ key: k, obj: null, object: t.object, host: t.host, kind: '', policy: '', schedule: '', period: null, last: t, health: testHealth(t, null) })
  return out
})
const conf = computed(() => restoreConfidence((rows.value ?? []).map((r) => r.health)))
const failedRows = computed(() => (rows.value ?? []).filter((r) => r.health === 'failed'))
const healthLabel: Record<TestHealth, string> = { passed: 'passed', failed: 'failed', overdue: 'overdue', never: 'never tested', running: 'running', unscheduled: 'not scheduled' }
const healthState: Record<TestHealth, string> = { passed: 'passed', failed: 'failed', overdue: 'overdue', never: 'never', running: 'running', unscheduled: 'unscheduled' }

const cols: ColumnDef<Row>[] = [
  { key: 'health', label: 'Result', value: (r) => ['failed', 'overdue', 'never', 'running', 'passed', 'unscheduled'].indexOf(r.health), width: '130px' },
  { key: 'object', label: 'Object', required: true },
  { key: 'host', label: 'Host', width: '100px' },
  { key: 'kind', label: 'Kind', format: (r) => kindLabel(r.kind), width: '120px', hidden: true },
  { key: 'when', label: 'Last test', value: (r) => r.last?.finished ?? r.last?.started ?? '', width: '120px' },
  { key: 'method', label: 'Method', value: (r) => r.last?.method ?? '', width: '80px' },
  { key: 'schedule', label: 'Schedule', value: (r) => r.schedule, width: '150px' },
  { key: 'size', label: 'Checked', value: (r) => r.last?.bytes ?? null, align: 'right', width: '90px' },
  { key: 'dur', label: 'Took', value: (r) => r.last?.duration_ms ?? null, align: 'right', width: '80px' },
  { key: 'act', label: '', sortable: false, noExport: true, width: '110px' },
]

// ---- history of one object / details
const detail = shallowRef<Row | null>(null)
const objHistory = computed(() => list(history.data.value).filter((t) => detail.value && t.object === detail.value.object && t.host === detail.value.host))
const detailOf = (t: RestoreTest) => (t.details ? JSON.stringify(t.details, null, 2) : '')

// ---- run now
const canRun = computed(() => hasRole('operator'))
const run = reactive({ open: false, row: null as Row | null, mode: 'deep' as RestoreTestMethod, busy: false, err: '' })
const runJob = shallowRef<Job | null>(null)
function openRun(r: Row) { run.row = r; run.mode = defaultMethod(r.kind); run.err = ''; run.open = true }
async function startRun() {
  const r = run.row
  if (!r?.obj) return
  run.busy = true
  run.err = ''
  try {
    runJob.value = await protectApi.restoreTests.run({ host: r.host, project: r.obj.project, object: r.obj.name, mode: run.mode })
    run.open = false
    toast.success('Restore test queued', `${r.host} runs it within seconds when it is online.`)
    invalidate(`restore-tests`)
  } catch (e) {
    run.err = e instanceof Error ? e.message : String(e)
  } finally {
    run.busy = false
  }
}
const methods = computed(() => {
  const k = run.row?.kind
  const m: { id: RestoreTestMethod; label: string; help: string; ok: boolean }[] = [
    { id: 'deep', label: 'Deep', help: 'Downloads, decrypts and re-hashes every chunk of the snapshot. Proves the data is intact and readable with the key.', ok: true },
    { id: 'extract', label: 'Extract', help: 'Restores every file into a scratch directory on the host and checks each against its recorded SHA-256, then deletes it.', ok: k === 'filesystem' },
    { id: 'full', label: 'Full', help: 'Restores into a scratch Incus volume, reads it back and compares the hash, then deletes the volume. Proves an Incus restore works end to end.', ok: k === 'block' || k === 'filesystem' },
  ]
  return m
})
const confText = computed(() => (conf.value.score === null ? '–' : formatPercent(conf.value.score, 0)))
const refreshing = ref(false)
async function refreshAll() { refreshing.value = true; await Promise.all([latest.refresh(), history.refresh(), objects.refresh()]); refreshing.value = false }
</script>

<template>
  <div data-testid="restore-tests">
    <header class="page-head">
      <div>
        <h1>Restore tests</h1>
        <p class="lede">A backup that was never restored is not a backup. Hosts test a rotating share of their objects on each policy's restore-test schedule; an operator can test any object now.</p>
      </div>
      <div class="actions"><button type="button" class="btn" :disabled="refreshing" @click="refreshAll"><PIcon name="refresh-cw" :size="15" />Refresh</button></div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database" />
    <EmptyState v-else-if="latest.data.value === null" title="This server does not record restore tests yet" icon="badge-check">Upgrade the server; restore tests need the host-jobs API.</EmptyState>
    <template v-else>
      <div class="top">
        <section class="panel conf" :class="`l-${conf.level}`" data-testid="confidence">
          <Gauge :value="conf.score ?? 0" :max="1" :warn="0.95" :critical="0.5" invert :size="84" :center="confText" label="Restore confidence" />
          <div>
            <h2>Restore confidence</h2>
            <p v-if="conf.score === null" class="muted">No object has a restore-test schedule yet. Set one in its policy.</p>
            <p v-else>{{ formatNumber(conf.passed) }} of {{ formatNumber(conf.passed + conf.failed + conf.overdue + conf.never) }} scheduled {{ noun(conf.objects, 'object') }} passed a restore test within their schedule.</p>
            <p v-if="conf.failed" class="bad">{{ conf.failed }} {{ noun(conf.failed, 'object') }} failed the last test.</p>
          </div>
        </section>
        <div class="tiles">
          <StatTile label="Passed" :value="formatNumber(conf.passed)" status="ok" :loading="rows === undefined" />
          <StatTile label="Failed" :value="formatNumber(conf.failed)" :status="conf.failed ? 'critical' : 'ok'" :loading="rows === undefined" />
          <StatTile label="Overdue" :value="formatNumber(conf.overdue)" :status="conf.overdue ? 'warn' : undefined" hint="older than the schedule" :loading="rows === undefined" />
          <StatTile label="Never tested" :value="formatNumber(conf.never)" :status="conf.never ? 'warn' : undefined" :loading="rows === undefined" />
        </div>
      </div>

      <Callout v-for="r in failedRows.slice(0, 3)" :key="r.key" status="critical" :title="`${r.object} failed its restore test`" class="gap" data-testid="failed-test">
        {{ r.last?.method }} test<template v-if="r.last?.snapshot_id"> of <span class="mono">{{ r.last.snapshot_id }}</span></template> on {{ r.host }}, <RelativeTime :time="r.last?.finished ?? r.last?.started" />. {{ (r.last?.details as { error?: string } | null)?.error ?? 'See the details.' }}
        <template #actions><button type="button" class="btn btn-sm" @click="detail = r">Details</button></template>
      </Callout>

      <JobPanel v-if="runJob" :job-id="runJob.id" compact class="gap" @finished="refreshAll" />

      <DataTable :rows="rows" :columns="cols" row-key="key" storage-key="restore-tests" csv-name="restore-tests" :loading="rows === undefined" :error="latest.error.value ?? objects.error.value" :retry="refreshAll"
        :initial-sort="{ key: 'health', dir: 'asc' }" row-clickable data-testid="rt-table" @row-click="detail = $event">
        <template #cell-health="{ row }"><StatusPill :state="healthState[row.health]" :label="healthLabel[row.health]" size="sm" /></template>
        <template #cell-object="{ row }"><button type="button" class="linkish" @click.stop="detail = row">{{ row.object }}</button></template>
        <template #cell-when="{ row }"><RelativeTime :time="row.last?.finished ?? row.last?.started" never="never" /></template>
        <template #cell-schedule="{ row }"><span v-if="row.period" :title="describeSchedule(row.schedule)">{{ row.schedule }}</span><span v-else class="muted">none</span></template>
        <template #cell-size="{ row }"><ByteSize v-if="row.last" :bytes="row.last.bytes" /><span v-else class="muted">–</span></template>
        <template #cell-dur="{ row }"><span v-if="row.last" class="num">{{ formatDuration(row.last.duration_ms) }}</span><span v-else class="muted">–</span></template>
        <template #cell-act="{ row }">
          <Tooltip v-if="!canRun" text="Running a restore test needs the operator role."><button type="button" class="btn btn-sm" disabled>Run now</button></Tooltip>
          <button v-else-if="row.obj" type="button" class="btn btn-sm" :data-testid="`run-${row.object}`" @click.stop="openRun(row)"><PIcon name="play" :size="13" />Run now</button>
        </template>
        <template #empty><EmptyState title="No objects yet" icon="badge-check">Restore tests start once hosts back up objects.</EmptyState></template>
      </DataTable>
    </template>

    <Modal v-model:open="run.open" :title="`Test ${run.row?.object ?? ''} now`" description="A restore test never touches the original object." width="560px">
      <div class="stack">
        <div class="methods" role="radiogroup" aria-label="Method">
          <label v-for="m in methods" :key="m.id" :class="{ on: run.mode === m.id, off: !m.ok }"><input v-model="run.mode" type="radio" :value="m.id" :disabled="!m.ok" /> <span><strong>{{ m.label }}</strong><small>{{ m.help }}<template v-if="!m.ok"> Not for {{ kindLabel(run.row?.kind).toLowerCase() }} objects.</template></small></span></label>
        </div>
        <p class="muted small">Tests the latest snapshot on {{ run.row?.host }}. The host must be online; it runs the test as a job and reports the result here.</p>
        <Callout v-if="run.err" status="critical" title="Could not start the test">{{ run.err }}</Callout>
      </div>
      <template #footer>
        <button type="button" class="btn" @click="run.open = false">Cancel</button>
        <button type="button" class="btn btn-primary" :disabled="run.busy" data-testid="run-start" @click="startRun"><PIcon name="play" :size="14" />Start the test</button>
      </template>
    </Modal>

    <Modal :open="!!detail" :title="detail?.object ?? ''" :description="detail ? `${detail.host} · ${kindLabel(detail.kind)} · policy ${detail.policy || 'none'}` : ''" width="720px" drawer @update:open="(v: boolean) => { if (!v) detail = null }">
      <div v-if="detail" class="stack">
        <p v-if="detail.period">Schedule: {{ describeSchedule(detail.schedule) }}; every object should pass a test at least once per period.</p>
        <p v-else class="muted">This object's policy has no restore-test schedule; it is only tested on demand.</p>
        <p v-if="!objHistory.length" class="muted">No restore test recorded for this object.</p>
        <ul class="hist">
          <li v-for="t in objHistory" :key="t.id" :data-state="t.state">
            <div class="hh"><StatusPill :state="t.state" size="sm" /><strong>{{ t.method }}</strong> <span class="mono">{{ t.snapshot_id }}</span><span class="muted"><RelativeTime :time="t.finished ?? t.started" /> · {{ formatDuration(t.duration_ms) }} · <ByteSize :bytes="t.bytes" /><template v-if="t.files"> · {{ formatNumber(t.files) }} files</template></span>
              <RouterLink v-if="t.job_id" :to="{ name: 'job', params: { id: t.job_id } }" class="jl">job</RouterLink></div>
            <CodeBlock v-if="t.state === 'failed' && detailOf(t)" :code="detailOf(t)" />
          </li>
        </ul>
      </div>
      <template #footer>
        <button v-if="detail?.obj && canRun" type="button" class="btn btn-primary" @click="openRun(detail!); detail = null"><PIcon name="play" :size="14" />Run now</button>
        <button type="button" class="btn" @click="detail = null">Close</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.top { display: grid; grid-template-columns: minmax(280px, 1fr) minmax(0, 2fr); gap: var(--s-3); margin-bottom: var(--s-4); }
@media (max-width: 900px) { .top { grid-template-columns: minmax(0, 1fr); } }
.conf { display: flex; gap: var(--s-4); align-items: center; padding: var(--s-4); }
.conf h2 { font-size: var(--fs-md); margin-bottom: 4px; }
.conf p { font-size: var(--fs-sm); color: var(--text-2); }
.conf .bad { color: var(--critical-text); font-weight: var(--fw-medium); }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: var(--s-3); }
.gap { margin-bottom: var(--s-3); }
.mono { font-family: var(--font-mono); font-size: var(--fs-sm); }
.linkish { border: 0; background: none; padding: 0; font: inherit; color: var(--text); font-weight: var(--fw-medium); cursor: pointer; text-align: left; }
.linkish:hover { text-decoration: underline; }
.methods { display: grid; gap: var(--s-2); }
.methods label { display: flex; gap: var(--s-2); align-items: flex-start; padding: var(--s-2) var(--s-3); border: 1px solid var(--border); border-radius: var(--r); cursor: pointer; }
.methods label.on { border-color: var(--accent); background: var(--accent-soft); }
.methods label.off { opacity: .6; cursor: not-allowed; }
.methods span { display: flex; flex-direction: column; gap: 2px; }
.methods small { color: var(--text-muted); font-size: var(--fs-sm); }
.small { font-size: var(--fs-sm); }
.hist { list-style: none; margin: 0; padding: 0; display: grid; gap: var(--s-3); }
.hh { display: flex; flex-wrap: wrap; gap: var(--s-2); align-items: center; font-size: var(--fs-sm); margin-bottom: 4px; }
.jl { margin-left: auto; }
</style>
