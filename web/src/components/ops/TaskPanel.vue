<script setup lang="ts">
// One server task, live: state, progress, message, the log (followed while it runs) and cancel.
// <TaskPanel :id="taskId" @done="(t) => ..." />. Polls GET /tasks/{id}?after=<seq> every second while
// the task is active and on every SSE "task" event; stops when it ended.
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import { opsApi, type TaskDetail, type TaskLogLine } from '@/api/sections/ops'
import { bus } from '@/api/events'
import { isAbort } from '@/api/client'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { cancelRole, isActive, levelStatus, mergeLog, startedByLabel, taskKindLabel, taskResultSummary } from '@/lib/tasks'
import { formatDuration } from '@/lib/format'

const props = withDefaults(defineProps<{ id: string; logHeight?: string; showResult?: boolean }>(), { logHeight: '240px', showResult: true })
const emit = defineEmits<{ done: [TaskDetail]; update: [TaskDetail] }>()

const t = ref<TaskDetail | null>(null)
const log = ref<TaskLogLine[]>([])
const error = ref('')
const cancelling = ref(false)
const follow = ref(true)
const box = ref<HTMLElement | null>(null)
let after = 0
let timer: ReturnType<typeof setTimeout> | null = null
let ctrl: AbortController | null = null
let alive = true
let emittedDone = false

async function poll() {
  if (!alive) return
  if (timer) { clearTimeout(timer); timer = null }
  ctrl?.abort()
  ctrl = new AbortController()
  try {
    const d = await opsApi.tasks.get(props.id, after, ctrl.signal)
    if (!alive) return
    t.value = d
    error.value = ''
    log.value = mergeLog(log.value, d.log ?? [])
    after = Math.max(after, d.next_seq)
    emit('update', d)
    if (follow.value) void nextTick(() => { if (box.value) box.value.scrollTop = box.value.scrollHeight })
    if (!isActive(d)) {
      if (!emittedDone) { emittedDone = true; emit('done', d) }
      return
    }
  } catch (e) {
    if (isAbort(e)) return
    error.value = errorText(e)
  }
  if (alive) timer = setTimeout(poll, 1000)
}

watch(() => props.id, () => { t.value = null; log.value = []; after = 0; emittedDone = false; void poll() }, { immediate: true })
const off = bus.onVisible('task', (d) => { if ((d as { id?: string })?.id === props.id && t.value && isActive(t.value)) void poll() })
onBeforeUnmount(() => { alive = false; off(); ctrl?.abort(); if (timer) clearTimeout(timer) })

function onScroll() {
  const el = box.value
  if (el) follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 24
}

const canCancel = computed(() => !!t.value && isActive(t.value) && hasRole(cancelRole(t.value.kind)))
async function cancel() {
  if (!t.value) return
  cancelling.value = true
  try {
    await opsApi.tasks.cancel(t.value.id)
    toast.info('Cancel requested', 'The task stops at its next check and ends as cancelled.')
    void poll()
  } catch (e) {
    toast.error(e, 'Could not cancel the task')
  } finally {
    cancelling.value = false
  }
}
const summary = computed(() => (t.value ? taskResultSummary(t.value.kind, t.value.result) : ''))
// a verification that found problems succeeded as a task, but its result is a finding
const finding = computed(() => (t.value?.result as { ok?: boolean } | null)?.ok === false)
const pct = computed(() => Math.max(0, Math.min(100, t.value?.progress ?? 0)))
const time = (s: string) => new Date(s).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
</script>

<template>
  <div class="tp" :data-testid="`task-panel-${id}`">
    <Callout v-if="error && !t" status="critical" title="Could not load the task" :retry="poll">{{ error }}</Callout>
    <div v-else-if="!t" class="muted small"><Spinner :size="12" /> Loading the task…</div>
    <template v-else>
      <div class="head">
        <StatusPill :state="t.state" size="sm" />
        <b>{{ taskKindLabel(t.kind) }}</b>
        <span v-if="t.repository" class="muted small">{{ t.repository }}</span>
        <span class="muted small">· {{ startedByLabel(t) }} · <RelativeTime :time="t.created" /> · {{ formatDuration(t.duration_ms) }}</span>
        <button v-if="isActive(t)" type="button" class="btn btn-sm cancel" :disabled="!canCancel || cancelling || t.cancel_requested" :title="!hasRole(cancelRole(t.kind)) ? `Needs the ${cancelRole(t.kind)} role` : ''" data-testid="task-cancel" @click="cancel">
          <Spinner v-if="cancelling" :size="12" /><Icon v-else name="square" :size="12" />{{ t.cancel_requested ? 'Cancelling…' : 'Cancel' }}
        </button>
      </div>
      <div v-if="isActive(t)" class="prog">
        <div class="bar" role="progressbar" :aria-valuenow="pct" aria-valuemin="0" aria-valuemax="100" :aria-label="`${taskKindLabel(t.kind)} progress`"><div class="fill" :style="{ width: `${pct}%` }" /></div>
        <span class="num small">{{ pct }}%</span>
      </div>
      <p v-if="t.message && isActive(t)" class="msg small">{{ t.message }}</p>
      <Callout v-if="t.state === 'failed'" status="critical" title="The task failed">{{ t.error }}</Callout>
      <Callout v-else-if="t.state === 'cancelled'" status="neutral" title="Cancelled">{{ t.error || 'The task was cancelled.' }} Nothing after that point was done; the repository is consistent.</Callout>
      <p v-else-if="showResult && t.state === 'succeeded' && summary" class="small res" :class="{ warn: finding }"><Icon :name="finding ? 'triangle-alert' : 'circle-check'" :size="13" /> {{ summary }}</p>
      <div class="loghead">
        <span class="small muted">Log · {{ t.log_lines }} {{ t.log_lines === 1 ? 'line' : 'lines' }}<template v-if="t.log_dropped"> ({{ t.log_dropped }} dropped)</template></span>
        <span v-if="isActive(t) && !follow" class="small"><button type="button" class="link" @click="follow = true; poll()">Follow</button></span>
      </div>
      <div ref="box" class="log mono" :style="{ maxHeight: logHeight }" tabindex="0" aria-label="Task log" @scroll="onScroll">
        <div v-if="!log.length" class="muted">{{ isActive(t) ? 'Waiting for output…' : 'No log lines.' }}</div>
        <div v-for="l in log" :key="l.seq" class="ln" :class="`l-${levelStatus(l.level)}`"><span class="ts">{{ time(l.at) }}</span><span class="lv">{{ l.level }}</span><span class="tx">{{ l.line }}</span></div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.tp { display: flex; flex-direction: column; gap: 8px; min-width: 0; }
.head { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 8px; }
.cancel { margin-left: auto; }
.prog { display: flex; align-items: center; gap: 8px; }
.bar { flex: 1; height: 6px; border-radius: 3px; background: var(--surface-3); overflow: hidden; }
.fill { height: 100%; background: var(--accent); border-radius: inherit; transition: width var(--t) var(--ease); }
.msg { color: var(--text-2); overflow-wrap: anywhere; }
.res { display: flex; gap: 6px; align-items: center; color: var(--ok-text); }
.res.warn { color: var(--warn-text); }
.loghead { display: flex; justify-content: space-between; }
.link { border: 0; background: none; color: var(--accent-text); cursor: pointer; font: inherit; padding: 0; }
.log { overflow: auto; background: var(--surface-2); border: 1px solid var(--border); border-radius: var(--r); padding: 6px 8px; font-size: 12px; line-height: 1.5; }
.ln { display: grid; grid-template-columns: auto 3.6em 1fr; gap: 8px; white-space: pre-wrap; overflow-wrap: anywhere; }
.ts { color: var(--text-faint); }
.lv { color: var(--text-muted); text-transform: uppercase; font-size: 11px; }
.l-warn .lv, .l-warn .tx { color: var(--warn-text); }
.l-critical .lv, .l-critical .tx { color: var(--critical-text); }
.muted { color: var(--text-muted); }
.small { font-size: var(--fs-sm); }
@media (max-width: 600px) { .ln { grid-template-columns: auto 1fr; } .lv { display: none; } }
</style>
