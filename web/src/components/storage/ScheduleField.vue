<script setup lang="ts">
// Schedule editor for the schedule language of internal/schedule ("03:00", "every 4h", "mon 02:30",
// "hourly", "daily", "weekly", "monthly", "manual"). Validates as the server does while typing, says in
// plain English what it means, and lists the next runs computed by the SERVER (GET /schedule/preview:
// its parser and its clock). `clock` says whose clock fires it: maintenance and sync run on the
// server's, backup policies on each host's own.
import { computed, ref, watch, onBeforeUnmount, useId } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import Spinner from '@/components/Spinner.vue'
import { describeSchedule, parseSchedule, formatDateTime } from '@/lib/format'
import { storageApi, type SchedulePreview } from '@/api/sections/storage'
import { isAbort } from '@/api/client'

const props = withDefaults(defineProps<{
  modelValue: string
  id?: string
  clock?: 'server' | 'host'
  count?: number
  disabled?: boolean
  /** Last run (RFC 3339): the preview says when the job is overdue. */
  after?: string | null
  presets?: string[]
  /** Smaller layout for tables of schedules. */
  dense?: boolean
  allowManual?: boolean
}>(), { clock: 'server', count: 5, presets: () => ['manual', 'hourly', 'every 4h', 'daily', '02:00', 'weekly', 'monthly'], allowManual: true })
const emit = defineEmits<{ 'update:modelValue': [string]; valid: [boolean] }>()
const uid = useId()
const inputId = computed(() => props.id ?? `sched-${uid}`)

const parsed = computed(() => parseSchedule(props.modelValue))
const clientError = computed(() => (parsed.value instanceof Error ? parsed.value.message : ''))
const isManual = computed(() => !(parsed.value instanceof Error) && parsed.value.kind === 'manual')
const text = computed(() => (clientError.value ? '' : describeSchedule(props.modelValue)))

const preview = ref<SchedulePreview | null>(null)
const loading = ref(false)
const serverError = ref('')
let ctl: AbortController | null = null
let timer: ReturnType<typeof setTimeout> | null = null
function load() {
  if (timer) clearTimeout(timer)
  if (clientError.value) { preview.value = null; return }
  timer = setTimeout(async () => {
    ctl?.abort()
    ctl = new AbortController()
    loading.value = true
    try {
      preview.value = await storageApi.schedulePreview(props.modelValue, props.count, props.after ?? undefined, ctl.signal)
      serverError.value = ''
    } catch (e) {
      if (!isAbort(e)) serverError.value = (e as Error).message
    } finally { loading.value = false }
  }, 250)
}
watch(() => [props.modelValue, props.after], load, { immediate: true })
onBeforeUnmount(() => { ctl?.abort(); if (timer) clearTimeout(timer) })

const error = computed(() => clientError.value || (preview.value && !preview.value.valid ? preview.value.error ?? 'invalid schedule' : ''))
watch(error, (e) => emit('valid', !e), { immediate: true })

const clockNote = computed(() => {
  const z = preview.value?.zone ?? 'the server\'s time zone'
  return props.clock === 'host' ? `Shown in the server's time zone (${z}); each host starts backups by its own clock.` : `Server time (${z}).`
})
const nextTimes = computed(() => (preview.value?.next ?? []).map((t) => formatDateTime(t, { tz: tzOf(t) })))
// show the server's wall-clock time (the offset in the RFC 3339 string), not the browser's
function tzOf(t: string): string | undefined {
  const m = /([+-])(\d\d):(\d\d)$/.exec(t)
  if (!m) return 'UTC'
  if (m[2] === '00' && m[3] === '00') return 'UTC'
  // Etc/GMT zones only cover whole hours (and have inverted signs); fall back to the browser's zone otherwise
  if (m[3] !== '00') return undefined
  return `Etc/GMT${m[1] === '+' ? '-' : '+'}${parseInt(m[2], 10)}`
}
const presetsShown = computed(() => props.presets.filter((p) => props.allowManual || p !== 'manual'))
</script>

<template>
  <div class="sf" :class="{ dense }">
    <div class="line">
      <input :id="inputId" class="input mono" :value="modelValue" :disabled="disabled" spellcheck="false" autocomplete="off" placeholder="03:00, every 4h, mon 02:30, manual"
        :aria-invalid="error ? 'true' : undefined" :aria-describedby="`${inputId}-desc`" data-testid="schedule-input" @input="emit('update:modelValue', ($event.target as HTMLInputElement).value)" />
      <div v-if="!dense && !disabled" class="presets" role="group" aria-label="Common schedules">
        <button v-for="p in presetsShown" :key="p" type="button" class="chip" :class="{ on: modelValue.trim().toLowerCase() === p }" @click="emit('update:modelValue', p)">{{ p }}</button>
      </div>
    </div>
    <div :id="`${inputId}-desc`" class="desc" aria-live="polite">
      <p v-if="error" class="err"><Icon name="circle-alert" :size="13" />{{ error }}</p>
      <template v-else>
        <p class="human"><Icon name="calendar-clock" :size="13" /><b>{{ text }}</b>
          <span v-if="preview?.due_now" class="due">Overdue: runs within a minute</span>
          <Spinner v-if="loading" :size="11" />
        </p>
        <p v-if="isManual" class="muted small">Never runs by itself; start it by hand.</p>
        <template v-else-if="nextTimes.length">
          <ol class="next num" :aria-label="`Next ${nextTimes.length} runs`" data-testid="schedule-next">
            <li v-for="(t, i) in nextTimes" :key="i">{{ t }}</li>
          </ol>
          <p class="muted small">{{ clockNote }}</p>
        </template>
        <p v-else-if="serverError" class="muted small">Next runs unavailable: {{ serverError }}</p>
      </template>
    </div>
  </div>
</template>

<style scoped>
.sf { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.line { display: flex; flex-direction: column; gap: 6px; }
.line .input { max-width: 260px; }
.presets { display: flex; flex-wrap: wrap; gap: 4px; }
.chip { height: 22px; padding: 0 8px; border: 1px solid var(--border); border-radius: var(--r-pill); background: var(--surface); color: var(--text-2); font-size: var(--fs-xs); font-family: var(--font-mono); cursor: pointer; }
.chip:hover { background: var(--surface-3); }
.chip.on { border-color: var(--accent); color: var(--accent-text); background: var(--accent-soft); }
.desc { font-size: var(--fs-sm); display: flex; flex-direction: column; gap: 4px; }
.human { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; color: var(--text-2); }
.human b { color: var(--text); font-weight: var(--fw-medium); }
.due { color: var(--warn-text); font-size: var(--fs-xs); font-weight: var(--fw-medium); }
.err { display: flex; align-items: center; gap: 6px; color: var(--critical-text); }
.next { display: flex; flex-wrap: wrap; gap: 4px 6px; list-style: none; margin: 0; padding: 0; }
.next li { padding: 1px 7px; border-radius: var(--r-sm); background: var(--surface-2); border: 1px solid var(--border); font-size: var(--fs-xs); color: var(--text-2); }
.dense .next li:nth-child(n + 3) { display: none; }
</style>
