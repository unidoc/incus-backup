<script setup lang="ts">
// One host job, live: state, progress, bytes, message, attempts, the log (follow mode), the result
// and cancel. Used on the job page and right under every restore form after "Start".
import { computed, nextTick, ref, toRef, watch } from 'vue'
import { RouterLink } from 'vue-router'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import DurationText from '@/components/DurationText.vue'
import ByteSize from '@/components/ByteSize.vue'
import Callout from '@/components/Callout.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import Tooltip from '@/components/Tooltip.vue'
import Spinner from '@/components/Spinner.vue'
import PIcon from './PIcon.vue'
import { useJob } from './useJob'
import { terminal, type Job } from '@/api/sections/protect'
import { jobLabel, jobSubject } from '@/lib/protect/jobs'
import { hasRole } from '@/stores/session'
import { confirm } from '@/composables/useConfirm'
import { toast } from '@/stores/toast'
import { live } from '@/api/events'

const props = withDefaults(defineProps<{ jobId: string; compact?: boolean; showLink?: boolean }>(), { showLink: true })
const emit = defineEmits<{ finished: [Job]; update: [Job] }>()
const { job, error, loading, refresh, cancel } = useJob(toRef(props, 'jobId'))

const done = computed(() => !!job.value && terminal(job.value.state))
let reported = ''
watch(job, (j) => {
  if (!j) return
  emit('update', j)
  if (terminal(j.state) && reported !== j.id + j.state) { reported = j.id + j.state; emit('finished', j) }
})

const pct = computed(() => {
  const j = job.value
  if (!j) return 0
  if (j.state === 'succeeded') return 100
  if (j.bytes_total > 0) return Math.min(100, Math.round((j.bytes_done / j.bytes_total) * 100))
  return Math.max(0, Math.min(100, j.progress))
})
const indeterminate = computed(() => job.value?.state === 'running' && pct.value === 0)
const elapsed = computed(() => {
  const j = job.value
  if (!j?.started_at) return null
  const end = j.finished_at ? Date.parse(j.finished_at) : Date.now()
  return end - Date.parse(j.started_at)
})

const log = computed(() => job.value?.log ?? [])
const follow = ref(true)
const logEl = ref<HTMLElement | null>(null)
watch(() => log.value.length, async () => {
  if (!follow.value) return
  await nextTick()
  if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
})
function onLogScroll() {
  const el = logEl.value
  if (el) follow.value = el.scrollTop + el.clientHeight >= el.scrollHeight - 8
}

const canCancel = computed(() => hasRole('operator'))
const cancelling = ref(false)
async function doCancel() {
  const j = job.value
  if (!j) return
  const running = j.state === 'running'
  if (!(await confirm({ title: `Cancel this ${jobLabel(j.type).toLowerCase()} job?`, message: running ? `The host stops at its next progress report (within about 30 s). ${j.type.startsWith('restore_') && j.type !== 'restore_test' ? 'A partly restored object stays in Incus for inspection; delete it before retrying.' : 'Nothing is changed on the original object.'}` : 'The job has not started yet; it is removed from the queue.', confirmLabel: 'Cancel job', danger: true }))) return
  cancelling.value = true
  try {
    await cancel()
    toast.success(running ? 'Cancellation requested' : 'Job cancelled', running ? 'The host stops at its next progress report.' : undefined)
  } catch (e) {
    toast.error(e, 'Could not cancel the job')
  } finally {
    cancelling.value = false
  }
}

const resultText = computed(() => {
  const r = job.value?.result
  if (r === undefined || r === null) return ''
  try { return JSON.stringify(r, null, 2) } catch { return String(r) }
})
const stateHelp: Record<string, string> = {
  queued: 'Waiting for the host to pick it up. Hosts poll for work every few seconds; an offline host never picks it up (queued jobs expire after 24 h).',
  running: 'The host is working on it and reports progress at least every 30 s.',
  succeeded: 'The host finished the job.',
  failed: 'The host reported a failure, or stopped reporting and ran out of attempts. The message says why; the full error is in the host\'s job log.',
  cancelled: 'Cancelled by a person.',
  expired: 'No host picked it up within 24 h. Check that the host is online and its daemon runs.',
}
const levelClass = (l: string) => (l === 'error' ? 'lv-err' : l === 'warn' ? 'lv-warn' : '')
</script>

<template>
  <section class="jp panel" data-testid="job-panel" :data-state="job?.state ?? 'loading'">
    <header class="panel-head">
      <PIcon name="activity" />
      <h2>{{ job ? jobLabel(job.type) : 'Job' }}</h2>
      <StatusPill v-if="job" :state="job.state" />
      <span v-if="job?.cancel_requested && !done" class="sub">cancellation requested</span>
      <span class="spacer" />
      <span v-if="!done && live.state !== 'live'" class="sub" title="Live updates are not connected; the page polls every 2 s.">polling</span>
      <RouterLink v-if="showLink && job" class="btn btn-sm btn-ghost" :to="{ name: 'job', params: { id: job.id } }">Open job page<PIcon name="chevron-right" :size="14" /></RouterLink>
    </header>
    <div class="panel-body">
      <Callout v-if="error && !job" status="critical" title="Could not load the job" :retry="refresh">{{ error.message }}</Callout>
      <div v-else-if="loading && !job" class="row muted"><Spinner /> Loading the job…</div>
      <template v-else-if="job">
        <p class="subject">{{ jobSubject(job) }} <span class="muted">on host</span> <RouterLink :to="{ name: 'host', params: { name: job.host } }">{{ job.host }}</RouterLink></p>
        <p class="help muted">{{ stateHelp[job.state] ?? '' }}</p>
        <div class="bar" :class="[`s-${job.state}`, { ind: indeterminate }]" role="progressbar" :aria-valuenow="pct" aria-valuemin="0" aria-valuemax="100" :aria-label="`${jobLabel(job.type)} progress`">
          <div class="fill" :style="{ width: indeterminate ? '30%' : `${pct}%` }" />
        </div>
        <div class="facts">
          <span><strong class="num">{{ pct }} %</strong></span>
          <span v-if="job.bytes_total > 0"><ByteSize :bytes="job.bytes_done" /> of <ByteSize :bytes="job.bytes_total" /></span>
          <span v-if="elapsed !== null"><PIcon name="timer" :size="13" /> <DurationText :ms="elapsed" /></span>
          <span>attempt {{ job.attempt }} of {{ job.max_attempts }}</span>
          <span>created <RelativeTime :time="job.created_at" /> by {{ job.created_by }}</span>
        </div>
        <Callout v-if="job.message" :status="job.state === 'failed' || job.state === 'expired' ? 'critical' : job.state === 'succeeded' ? 'ok' : 'info'" :title="job.state === 'failed' ? 'The host reported' : undefined" data-testid="job-message">{{ job.message }}</Callout>

        <div v-if="!compact || log.length" class="logwrap">
          <div class="loghead">
            <span class="lbl">Log <span class="muted">({{ log.length }} lines)</span></span>
            <label class="follow"><input v-model="follow" type="checkbox" /> Follow</label>
          </div>
          <div ref="logEl" class="log" data-testid="job-log" tabindex="0" @scroll="onLogScroll">
            <p v-if="!log.length" class="muted empty">No log lines yet.</p>
            <div v-for="l in log" :key="l.seq" class="ll" :class="levelClass(l.level)"><time class="t">{{ l.at.slice(11, 19) }}</time><span class="lv">{{ l.level }}</span><span class="m">{{ l.line }}</span></div>
          </div>
        </div>
        <details v-if="resultText && !compact" class="result">
          <summary>Result</summary>
          <CodeBlock :code="resultText" />
        </details>
        <slot :job="job" />
        <div class="acts">
          <template v-if="!done">
            <Tooltip v-if="!canCancel" text="Cancelling a job needs the operator role.">
              <button type="button" class="btn btn-sm" disabled><PIcon name="octagon-x" :size="14" />Cancel job</button>
            </Tooltip>
            <button v-else type="button" class="btn btn-sm btn-danger" :disabled="cancelling || job.cancel_requested" data-testid="job-cancel" @click="doCancel">
              <PIcon name="octagon-x" :size="14" />{{ job.cancel_requested ? 'Cancellation requested' : 'Cancel job' }}
            </button>
          </template>
          <button type="button" class="btn btn-sm btn-ghost" @click="refresh"><PIcon name="refresh-cw" :size="14" />Refresh</button>
        </div>
      </template>
    </div>
  </section>
</template>

<style scoped>
.jp .panel-head h2 { margin-right: var(--s-1); }
.subject { font-weight: var(--fw-medium); overflow-wrap: anywhere; }
.help { font-size: var(--fs-sm); margin: 2px 0 var(--s-3); }
.bar { height: 8px; border-radius: var(--r-pill); background: var(--surface-3); overflow: hidden; position: relative; }
.bar .fill { height: 100%; background: var(--accent); border-radius: inherit; transition: width var(--t) var(--ease); }
.bar.s-succeeded .fill { background: var(--ok); }
.bar.s-failed .fill, .bar.s-expired .fill { background: var(--critical); }
.bar.s-cancelled .fill { background: var(--neutral); }
.bar.ind .fill { position: absolute; animation: slide 1.4s ease-in-out infinite; }
@keyframes slide { from { left: -30%; } to { left: 100%; } }
@media (prefers-reduced-motion: reduce) { .bar.ind .fill { animation: none; left: 0; width: 100% !important; opacity: .4; } }
.facts { display: flex; flex-wrap: wrap; gap: var(--s-2) var(--s-4); margin: var(--s-2) 0 var(--s-3); color: var(--text-2); font-size: var(--fs-sm); align-items: center; }
.logwrap { margin-top: var(--s-3); }
.loghead { display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px; font-size: var(--fs-sm); }
.loghead .lbl { font-weight: var(--fw-medium); }
.follow { display: inline-flex; gap: 6px; align-items: center; color: var(--text-muted); }
.log { font-family: var(--font-mono); font-size: var(--fs-xs); background: var(--code-bg); border: 1px solid var(--border); border-radius: var(--r); max-height: 280px; min-height: 64px; overflow: auto; padding: var(--s-2) var(--s-3); }
.log .empty { font-family: var(--font-sans); }
.ll { display: grid; grid-template-columns: auto 3.6em 1fr; gap: var(--s-2); line-height: 1.55; }
.ll .t { color: var(--text-faint); }
.ll .lv { color: var(--text-muted); }
.ll .m { white-space: pre-wrap; overflow-wrap: anywhere; }
.lv-warn .lv, .lv-warn .m { color: var(--warn-text); }
.lv-err .lv, .lv-err .m { color: var(--critical-text); }
.result { margin-top: var(--s-3); }
.result summary { cursor: pointer; font-weight: var(--fw-medium); font-size: var(--fs-sm); margin-bottom: var(--s-2); }
.acts { display: flex; gap: var(--s-2); margin-top: var(--s-3); flex-wrap: wrap; }
.spacer { flex: 1; }
</style>
