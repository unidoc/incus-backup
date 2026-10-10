<script setup lang="ts">
// Server tasks: every long operation (GC, verify, retention, reconcile, sync runs, probes, repository
// destroys, object deletes), started by people or by the scheduler. Running tasks on top with progress;
// the history below with filters; a drawer with the live log and cancel. ?task=<id> opens one.
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import Modal from '@/components/Modal.vue'
import EmptyState from '@/components/EmptyState.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import TaskPanel from '@/components/ops/TaskPanel.vue'
import { useResource, invalidate } from '@/api/cache'
import { list } from '@/api/endpoints'
import { opsApi, type Task } from '@/api/sections/ops'
import { repos } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { toast } from '@/stores/toast'
import { cancelRole, isActive, startedByLabel, taskKindLabel, taskResultSummary, TASK_KINDS } from '@/lib/tasks'
import { formatDuration } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'

const route = useRoute()
const router = useRouter()
const f = ref({ state: '', kind: '', repository: typeof route.query.repository === 'string' ? route.query.repository : '', routine: false })

const active = useResource(() => 'tasks:active', (s) => opsApi.tasks.list({ state: 'active', limit: 100 }, s), { refreshOn: ['task'], ttl: 2_000, poll: 10_000 })
const hist = useResource(() => `tasks:list:${f.value.state}:${f.value.kind}:${f.value.repository}`,
  (s) => opsApi.tasks.list({ state: f.value.state || undefined, kind: f.value.kind || undefined, repository: f.value.repository || undefined, limit: 500 }, s),
  { refreshOn: ['task'], ttl: 5_000 })
const running = computed(() => list(active.data.value))
const rows = computed(() => {
  const l = hist.data.value === undefined ? undefined : list(hist.data.value)
  return l?.filter((t) => f.value.routine || f.value.kind === 'capacity' || t.kind !== 'capacity' || t.state === 'failed')
})
const routineHidden = computed(() => list(hist.data.value).filter((t) => t.kind === 'capacity' && t.state !== 'failed').length)

const cols: ColumnDef<Task>[] = [
  { key: 'kind', label: 'Task', format: (t) => taskKindLabel(t.kind), required: true },
  { key: 'repository', label: 'Repository', format: (t) => t.repository || '–' },
  { key: 'state', label: 'State' },
  { key: 'started_by', label: 'Started by', value: (t) => startedByLabel(t) },
  { key: 'created', label: 'Started' },
  { key: 'duration_ms', label: 'Duration', align: 'right', format: (t) => formatDuration(t.duration_ms) },
  { key: 'summary', label: 'Outcome', sortable: false, value: (t) => t.error || taskResultSummary(t.kind, t.result) || t.message },
]

const openId = ref<string>('')
watch(() => route.query.task, (q) => { openId.value = typeof q === 'string' ? q : '' }, { immediate: true })
const drawer = computed({
  get: () => !!openId.value,
  set: (x: boolean) => { if (!x) { openId.value = ''; void router.replace({ query: { ...route.query, task: undefined } }) } },
})
function open(t: Task) { void router.replace({ query: { ...route.query, task: t.id } }) }
const openTask = computed(() => [...running.value, ...list(hist.data.value)].find((t) => t.id === openId.value))

async function cancel(t: Task) {
  try {
    await opsApi.tasks.cancel(t.id)
    toast.info('Cancel requested', `${taskKindLabel(t.kind)} stops at its next check.`)
    invalidate('tasks:')
  } catch (e) {
    toast.error(e, 'Could not cancel the task')
  }
}
const stateOpts = [{ value: '', label: 'All states' }, { value: 'active', label: 'Queued or running' }, { value: 'succeeded', label: 'Succeeded' }, { value: 'failed', label: 'Failed' }, { value: 'cancelled', label: 'Cancelled' }]
const kindOpts = computed(() => [{ value: '', label: 'All kinds' }, ...Object.entries(TASK_KINDS).map(([value, label]) => ({ value, label }))])
const repoOpts = computed(() => [{ value: '', label: 'All repositories' }, ...repos.list.map((r) => ({ value: r.name, label: r.name }))])
</script>

<template>
  <div class="page">
    <header class="page-head">
      <div>
        <h1>Tasks</h1>
        <p class="lede">Every long operation of this server: garbage collection, verification, retention, reconcile, sync runs, probes and deletions, whether a person or the schedule started it. Each keeps its log for 30 days.</p>
      </div>
    </header>

    <section class="panel">
      <div class="panel-head"><Icon name="activity" :size="16" /><h2>Running now</h2><span class="sub">{{ running.length ? `${running.length} active` : 'nothing runs' }}</span></div>
      <div v-if="running.length" class="run-list">
        <div v-for="t in running" :key="t.id" class="run" data-testid="running-task">
          <div class="r1">
            <button type="button" class="name" @click="open(t)">{{ taskKindLabel(t.kind) }}</button>
            <span v-if="t.repository" class="muted">{{ t.repository }}</span>
            <span class="muted small">· {{ startedByLabel(t) }} · <RelativeTime :time="t.created" /></span>
            <span class="spacer" />
            <button type="button" class="btn btn-sm" @click="open(t)"><Icon name="scroll-text" :size="12" />Log</button>
            <button type="button" class="btn btn-sm" :disabled="!hasRole(cancelRole(t.kind)) || t.cancel_requested" :title="!hasRole(cancelRole(t.kind)) ? `Needs the ${cancelRole(t.kind)} role` : ''" @click="cancel(t)"><Icon name="square" :size="12" />{{ t.cancel_requested ? 'Cancelling…' : 'Cancel' }}</button>
          </div>
          <div class="r2">
            <div class="bar" role="progressbar" :aria-valuenow="t.progress" aria-valuemin="0" aria-valuemax="100"><div class="fill" :style="{ width: `${t.progress}%` }" /></div>
            <span class="num small">{{ t.progress }}%</span>
          </div>
          <p class="small muted msg">{{ t.message }}</p>
        </div>
      </div>
      <EmptyState v-else-if="!active.loading.value" compact icon="circle-check" title="No task is running">Scheduled maintenance and anything you start appear here with live progress.</EmptyState>
    </section>

    <section class="panel">
      <div class="panel-head"><h2>History</h2><span class="sub">newest first</span></div>
      <div class="filters panel-body">
        <SelectInput v-model="f.state" :options="stateOpts" size="sm" aria-label="State" data-testid="task-filter-state" />
        <SelectInput v-model="f.kind" :options="kindOpts" size="sm" aria-label="Kind" />
        <SelectInput v-model="f.repository" :options="repoOpts" size="sm" aria-label="Repository" />
        <SwitchInput v-model="f.routine" :label="`Show routine capacity samples${routineHidden && !f.routine ? ` (${routineHidden} hidden)` : ''}`" />
      </div>
      <DataTable :rows="rows" :columns="cols" row-key="id" storage-key="ops-tasks" csv-name="tasks" :loading="hist.loading.value" :error="hist.error.value" :retry="hist.refresh"
        row-clickable dense max-height="640px" empty-title="No task matches these filters" @row-click="open">
        <template #cell-state="{ row }"><StatusPill :state="row.state" size="sm" /></template>
        <template #cell-created="{ row }"><RelativeTime :time="row.created" /></template>
        <template #cell-summary="{ row }"><span class="truncate out" :class="{ err: row.error, warn: (row.result as { ok?: boolean } | null)?.ok === false }" :title="row.error || taskResultSummary(row.kind, row.result) || row.message">{{ row.error || taskResultSummary(row.kind, row.result) || row.message }}</span></template>
      </DataTable>
    </section>

    <Modal v-model:open="drawer" :title="openTask ? taskKindLabel(openTask.kind) : 'Task'" :description="openTask ? `${openTask.repository || 'server'} · ${openId}` : openId" drawer width="760px">
      <TaskPanel v-if="openId" :id="openId" log-height="calc(100vh - 300px)" @done="invalidate('tasks:')" />
      <details v-if="openTask?.result" class="raw"><summary>Result (JSON)</summary><pre class="mono">{{ JSON.stringify(openTask.result, null, 2) }}</pre></details>
      <details v-if="openTask && openTask.params && Object.keys(openTask.params as object).length" class="raw"><summary>Parameters</summary><pre class="mono">{{ JSON.stringify(openTask.params, null, 2) }}</pre></details>
    </Modal>
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: var(--s-4); }
.run-list { display: flex; flex-direction: column; }
.run { padding: 10px 16px; border-bottom: 1px solid var(--border); display: flex; flex-direction: column; gap: 6px; }
.run:last-child { border-bottom: 0; }
.r1 { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 8px; }
.name { border: 0; background: none; padding: 0; font: inherit; font-weight: var(--fw-semibold); color: var(--accent-text); cursor: pointer; }
.spacer { flex: 1; }
.r2 { display: flex; align-items: center; gap: 8px; }
.bar { flex: 1; height: 6px; border-radius: 3px; background: var(--surface-3); overflow: hidden; }
.fill { height: 100%; background: var(--accent); transition: width var(--t) var(--ease); }
.msg { overflow-wrap: anywhere; }
.muted { color: var(--text-muted); }
.small { font-size: var(--fs-sm); }
.filters { display: flex; flex-wrap: wrap; gap: 8px 12px; align-items: center; border-bottom: 1px solid var(--border); }
.filters :deep(select) { width: auto; min-width: 170px; max-width: 100%; }
.out { display: inline-block; max-width: 460px; vertical-align: middle; color: var(--text-2); }
.out.err { color: var(--critical-text); }
.out.warn { color: var(--warn-text); }
.raw { margin-top: 12px; }
.raw pre { max-height: 300px; overflow: auto; background: var(--surface-2); padding: 8px; border-radius: var(--r); font-size: 12px; }
</style>
