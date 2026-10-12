<script setup lang="ts">
// The daemon's recent log (the last 5000 lines kept in memory; secret attributes are redacted when
// captured). Level filter, search, follow (polls ?after= every 2 s). Admin only.
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import Callout from '@/components/Callout.vue'
import EmptyState from '@/components/EmptyState.vue'
import Spinner from '@/components/Spinner.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import { opsApi, type LogLine } from '@/api/sections/ops'
import { isAbort } from '@/api/client'
import { errorText } from '@/stores/toast'
import { downloadText } from '@/composables/useClipboard'
import { levelStatus, mergeLog } from '@/lib/tasks'
import { formatNumber } from '@/lib/format'

const level = ref('info')
const q = ref('')
const follow = ref(true)
const lines = ref<LogLine[]>([])
const meta = ref({ capacity: 0, dropped: 0, note: '' })
const loading = ref(true)
const error = ref('')
const box = ref<HTMLElement | null>(null)
const atBottom = ref(true)
let after = 0
let timer: ReturnType<typeof setTimeout> | null = null
let ctrl: AbortController | null = null
let gen = 0

async function load(reset: boolean) {
  if (timer) { clearTimeout(timer); timer = null }
  const my = reset ? ++gen : gen
  if (reset) { after = 0; lines.value = []; loading.value = true }
  ctrl?.abort()
  ctrl = new AbortController()
  try {
    const r = await opsApi.system.logs({ level: level.value, q: q.value.trim() || undefined, after: after || undefined, limit: 2000 }, ctrl.signal)
    if (my !== gen) return
    lines.value = mergeLog(lines.value, r.lines ?? [], 5000)
    after = Math.max(after, r.last_seq)
    meta.value = { capacity: r.capacity, dropped: r.dropped, note: r.note ?? '' }
    error.value = ''
    if (atBottom.value) void nextTick(() => { if (box.value) box.value.scrollTop = box.value.scrollHeight })
  } catch (e) {
    if (isAbort(e)) return
    error.value = errorText(e)
  } finally {
    if (my === gen) loading.value = false
  }
  if (follow.value && my === gen) timer = setTimeout(() => load(false), 2000)
}
let deb: ReturnType<typeof setTimeout> | null = null
watch([level, q], () => { if (deb) clearTimeout(deb); deb = setTimeout(() => load(true), 250) })
watch(follow, (on) => { if (on) void load(false); else if (timer) { clearTimeout(timer); timer = null } })
void load(true)
onBeforeUnmount(() => { gen++; ctrl?.abort(); if (timer) clearTimeout(timer); if (deb) clearTimeout(deb) })

function onScroll() {
  const el = box.value
  if (el) atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 24
}
const time = (s: string) => {
  const d = new Date(s)
  return `${d.toLocaleDateString('sv-SE')} ${d.toLocaleTimeString('sv-SE')}`
}
const counts = computed(() => {
  const c: Record<string, number> = {}
  for (const l of lines.value) c[l.level] = (c[l.level] ?? 0) + 1
  return c
})
function exportText() {
  downloadText('incus-backup-server.log', lines.value.map((l) => `${l.time} ${l.level.padEnd(5)} ${l.message} ${l.attrs}`).join('\n') + '\n')
}
</script>

<template>
  <div class="page">
    <header class="page-head">
      <div>
        <h1>Server log</h1>
        <p class="lede">What the server daemon logged recently, newest at the bottom. It keeps the last {{ meta.capacity ? formatNumber(meta.capacity) : '5000' }} lines in memory; values of secret-looking fields (tokens, passwords, keys) are replaced by [redacted].</p>
      </div>
      <div class="actions">
        <button type="button" class="btn" :disabled="!lines.length" @click="exportText"><Icon name="file-down" />Export</button>
      </div>
    </header>

    <section class="panel">
      <div class="bar panel-body">
        <SelectInput v-model="level" size="sm" aria-label="Minimum level" :options="[{ value: 'debug', label: 'All levels' }, { value: 'info', label: 'Info and above' }, { value: 'warn', label: 'Warnings and errors' }, { value: 'error', label: 'Errors only' }]" data-testid="log-level" />
        <input v-model="q" type="search" class="input sm search" placeholder="Search message and fields" aria-label="Search the log" />
        <SwitchInput v-model="follow" label="Follow" />
        <span class="spacer" />
        <span class="small muted">{{ formatNumber(lines.length) }} lines<template v-if="counts.ERROR"> · <span class="e">{{ counts.ERROR }} errors</span></template><template v-if="counts.WARN"> · <span class="w">{{ counts.WARN }} warnings</span></template><template v-if="meta.dropped"> · {{ formatNumber(meta.dropped) }} older lines dropped</template></span>
        <Spinner v-if="follow && !loading" :size="11" label="Following" />
      </div>
      <Callout v-if="error" status="critical" title="Could not load the log" :retry="() => load(true)" class="m">{{ error }}</Callout>
      <Callout v-else-if="meta.note" status="info" class="m" title="No log in this process">{{ meta.note }}</Callout>
      <div v-if="loading" class="panel-body muted"><Spinner :size="12" /> Loading…</div>
      <EmptyState v-else-if="!lines.length && !error && !meta.note" compact icon="file" title="No matching lines">{{ q ? 'Nothing matches the search at this level.' : 'Nothing was logged at this level yet.' }}</EmptyState>
      <div v-else-if="lines.length" ref="box" class="log mono" tabindex="0" aria-label="Server log" data-testid="server-log" @scroll="onScroll">
        <div v-for="l in lines" :key="l.seq" class="ln" :class="`l-${levelStatus(l.level)}`"><span class="ts">{{ time(l.time) }}</span><span class="lv">{{ l.level }}</span><span class="tx"><span class="msg">{{ l.message }}</span> <span class="at">{{ l.attrs }}</span></span></div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: var(--s-4); }
.bar { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; border-bottom: 1px solid var(--border); }
.search { width: min(320px, 100%); height: var(--control-h-sm); }
.spacer { flex: 1; }
.m { margin: 12px 16px; }
.log { height: calc(100vh - 260px); min-height: 320px; overflow: auto; padding: 8px 12px; font-size: 12px; line-height: 1.55; background: var(--surface-2); border-radius: 0 0 var(--r-lg) var(--r-lg); }
.ln { display: grid; grid-template-columns: 9.5em 3.6em 1fr; gap: 10px; white-space: pre-wrap; overflow-wrap: anywhere; }
.ts { color: var(--text-faint); }
.lv { color: var(--text-muted); font-size: 11px; }
.at { color: var(--text-muted); }
.l-warn .lv, .l-warn .msg { color: var(--warn-text); }
.l-critical .lv, .l-critical .msg { color: var(--critical-text); }
.l-neutral .msg { color: var(--text-2); }
.e { color: var(--critical-text); }
.w { color: var(--warn-text); }
.muted { color: var(--text-muted); }
.small { font-size: var(--fs-sm); }
@media (max-width: 600px) { .ln { grid-template-columns: 1fr; gap: 0; } .lv { display: none; } .log { height: calc(100vh - 320px); } }
</style>
