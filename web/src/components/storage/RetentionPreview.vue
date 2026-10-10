<script setup lang="ts">
// "What this keeps": simulates the policy's schedule for long enough to fill every rule and runs the
// server's retention algorithm (lib/retention.ts, checked against the Go code) on it. Shows how many
// restore points exist and how far back they reach, on a time line with a logarithmic age axis
// (recent days stay readable next to years), plus the same numbers as a table.
import { computed } from 'vue'
import ChartFrame from '@/components/charts/ChartFrame.vue'
import { simulate, describeRules, anyRule, type Rules } from '@/lib/retention'
import { parseSchedule, formatDuration, formatDateTime } from '@/lib/format'
import { scheduleIntervalMs } from '@/lib/storage'

const props = defineProps<{ rules: Rules; tz: string; schedule: string }>()
const HOUR = 3_600_000
const DAY = 24 * HOUR

const interval = computed(() => {
  const p = parseSchedule(props.schedule)
  if (p instanceof Error) return { ms: DAY, assumed: true }
  const ms = scheduleIntervalMs(p)
  return ms ? { ms, assumed: false } : { ms: DAY, assumed: true }
})
const horizonDays = computed(() => {
  const r = props.rules
  const need = Math.max(r.last * interval.value.ms / DAY, r.hourly / 24, r.daily, r.weekly * 7, r.monthly * 31, r.yearly * 366, 14)
  const cap = (19_000 * interval.value.ms) / DAY // simulation size limit
  return Math.ceil(Math.min(need * 1.25 + 2, cap, 366 * 12))
})
const now = Math.floor(Date.now() / HOUR) * HOUR
const sim = computed(() => simulate(props.rules, props.tz || 'UTC', interval.value.ms, now, horizonDays.value))
const kept = computed(() => sim.value.decisions.filter((d) => d.keep))

const RULES = ['last', 'hourly', 'daily', 'weekly', 'monthly', 'yearly', 'newest'] as const
const color: Record<string, string> = { last: 'var(--series-1)', hourly: 'var(--series-1)', daily: 'var(--series-2)', weekly: 'var(--series-3)', monthly: 'var(--series-4)', yearly: 'var(--series-other)', newest: 'var(--text-muted)' }
const legend = computed(() => RULES.filter((r) => sim.value.byRule[r]).map((r) => ({ label: r, color: color[r], value: String(sim.value.byRule[r]) })))

// log age axis
const W = 640, H = 64, L = 8, R = 8
const maxAge = computed(() => Math.max(DAY, horizonDays.value * DAY))
const x = (age: number) => L + (Math.log1p(age / HOUR) / Math.log1p(maxAge.value / HOUR)) * (W - L - R)
const ticks = computed(() => [
  { age: 0, label: 'now' }, { age: DAY, label: '1 d' }, { age: 7 * DAY, label: '1 wk' }, { age: 30 * DAY, label: '1 mo' },
  { age: 182 * DAY, label: '6 mo' }, { age: 365 * DAY, label: '1 yr' }, { age: 730 * DAY, label: '2 yr' }, { age: 1825 * DAY, label: '5 yr' },
].filter((t) => t.age <= maxAge.value))
const dots = computed(() => kept.value.map((d) => ({ x: x(now - d.time), rule: d.reason.split(',')[0], t: d.time })))
const rows = computed(() => RULES.filter((r) => sim.value.byRule[r]).map((r) => {
  const ds = kept.value.filter((d) => d.reason.split(',')[0] === r)
  return [r, String(ds.length), span(now - ds[ds.length - 1].time)]
}))
/** "6 d 4 h", "7 weeks", "13 months", "2 years 1 month" */
function span(ms: number): string {
  const d = ms / DAY
  if (d < 14) return formatDuration(ms, { units: 2 })
  if (d < 61) return `${Math.round(d / 7)} weeks`
  const mo = Math.round(d / 30.44)
  if (mo < 24) return `${mo} months`
  const y = Math.floor(mo / 12), r = mo % 12
  return `${y} years${r ? ` ${r} month${r === 1 ? '' : 's'}` : ''}`
}
const sentence = computed(() => {
  if (!anyRule(props.rules)) return 'No retention rule: every snapshot is kept forever and the repository only grows.'
  const every = interval.value.assumed ? 'one backup a day (the schedule does not run by itself; assumed)' : `one backup ${interval.value.ms === DAY ? 'a day' : `every ${formatDuration(interval.value.ms, { units: 1 })}`}`
  return `With ${every}, this keeps ${kept.value.length} restore points reaching back ${span(sim.value.reach)}: ${describeRules(props.rules)}.`
})
</script>

<template>
  <div class="rp" data-testid="retention-preview">
    <p class="sentence">{{ sentence }}</p>
    <ChartFrame v-if="anyRule(rules)" title="Restore points kept" :subtitle="`Age of each kept snapshot (log scale), periods cut in ${tz || 'UTC'}`" :legend="legend" :columns="['Rule', 'Keeps', 'Reaches back']" :rows="rows">
      <svg :viewBox="`0 0 ${W} ${H}`" class="tl" role="img" :aria-label="sentence">
        <line :x1="L" :x2="W - R" :y1="28" :y2="28" class="axis" />
        <g v-for="t in ticks" :key="t.label">
          <line :x1="x(t.age)" :x2="x(t.age)" y1="22" y2="34" class="tick" />
          <text :x="x(t.age)" y="52" text-anchor="middle" class="tl-l">{{ t.label }}</text>
        </g>
        <circle v-for="(d, i) in dots" :key="i" :cx="d.x" cy="28" r="4.2" :style="{ fill: color[d.rule] }" class="dot"><title>{{ d.rule }}: {{ formatDateTime(d.t) }}</title></circle>
      </svg>
    </ChartFrame>
    <p class="muted small">A preview: real restore points depend on when hosts actually back up, and retention uses the server's commit time.</p>
  </div>
</template>

<style scoped>
.rp { display: flex; flex-direction: column; gap: 8px; }
.sentence { color: var(--text); }
.tl { width: 100%; height: auto; max-height: 90px; display: block; overflow: visible; }
.axis { stroke: var(--border-strong); stroke-width: 1; }
.tick { stroke: var(--border-strong); }
.tl-l { fill: var(--text-muted); font-size: 10px; }
.dot { stroke: var(--surface); stroke-width: 1.2; }
</style>
