<script setup lang="ts">
// Daily activity over the last N days as a calendar (weeks as columns, Monday on top). The level is
// the count relative to the busiest day; `extra` days (e.g. a failed verification) get a marker, so
// meaning never rests on colour alone. Every cell has a tooltip and the whole grid a text summary.
import { computed, ref } from 'vue'
import { calendarWeeks, heatLevel, type DayCount } from '@/lib/infra'
import { formatDate } from '@/lib/format'
const props = withDefaults(defineProps<{ days: DayCount[]; unit?: string; extraLabel?: string }>(), { unit: 'snapshot', extraLabel: 'failed verification' })
const max = computed(() => Math.max(0, ...props.days.map((d) => d.count)))
const weeks = computed(() => calendarWeeks(props.days))
const total = computed(() => props.days.reduce((a, d) => a + d.count, 0))
const covered = computed(() => props.days.filter((d) => d.count > 0).length)
const hover = ref<DayCount | null>(null)
// a month label above the first week that starts in a new month
const months = computed(() => weeks.value.map((w, i) => {
  const cur = w.find(Boolean)
  const prev = i > 0 ? weeks.value[i - 1].find(Boolean) : null
  if (!cur) return ''
  return !prev || prev.date.getMonth() !== cur.date.getMonth() ? cur.date.toLocaleString('en', { month: 'short' }) : ''
}))
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`
const tip = (d: DayCount) => `${formatDate(d.date)}: ${d.count ? plural(d.count, props.unit) : `no ${props.unit}`}${d.extra ? `, ${d.extra} ${props.extraLabel}` : ''}`
</script>

<template>
  <figure class="heat" :aria-label="`${plural(total, unit)} on ${covered} of ${days.length} days`">
    <div class="grid" role="img" :aria-label="`${plural(total, unit)} on ${covered} of ${days.length} days`">
      <div class="dows" aria-hidden="true"><span>Mon</span><span /><span>Wed</span><span /><span>Fri</span><span /><span /></div>
      <div v-for="(w, i) in weeks" :key="i" class="wk">
        <span class="mo" aria-hidden="true">{{ months[i] }}</span>
        <span v-for="(c, j) in w" :key="j" class="cell" :class="c ? [`l${heatLevel(c.count, max)}`, { x: c.extra > 0 }] : 'pad'" :title="c ? tip(c) : undefined"
          :data-day="c?.key" :data-count="c?.count" @mouseenter="hover = c" @mouseleave="hover = null" />
      </div>
    </div>
    <figcaption class="cap small">
      <span v-if="hover" class="num">{{ tip(hover) }}</span>
      <span v-else class="muted">{{ plural(total, unit) }} on {{ covered }} of {{ days.length }} days</span>
      <span class="legend muted" aria-hidden="true">Less <i class="cell l0" /><i class="cell l1" /><i class="cell l2" /><i class="cell l3" /><i class="cell l4" /> More<template v-if="days.some((d) => d.extra)"> · <i class="cell l2 x" /> {{ extraLabel }}</template></span>
    </figcaption>
  </figure>
</template>

<style scoped>
.heat { margin: 0; display: flex; flex-direction: column; gap: 8px; min-width: 0; }
.heat { --c: 16px; }
.grid { display: flex; gap: 3px; overflow-x: auto; padding-bottom: 2px; }
.dows, .wk { display: grid; grid-template-rows: 14px repeat(7, var(--c)); gap: 3px; }
.dows span { font-size: 10px; line-height: var(--c); color: var(--text-faint); padding-right: 4px; }
.dows span:first-child { grid-row: 2; }
.mo { font-size: 10px; line-height: 14px; color: var(--text-muted); white-space: nowrap; width: var(--c); overflow: visible; }
.cell { display: inline-block; width: var(--c); height: var(--c); border-radius: 3px; background: var(--surface-3); position: relative; }
.cell.pad { background: transparent; }
.cell.l1 { background: color-mix(in srgb, var(--series-1) 30%, var(--surface-2)); }
.cell.l2 { background: color-mix(in srgb, var(--series-1) 52%, var(--surface-2)); }
.cell.l3 { background: color-mix(in srgb, var(--series-1) 76%, var(--surface-2)); }
.cell.l4 { background: var(--series-1); }
.cell.x::after { content: ''; position: absolute; inset: 4px; border-radius: 50%; background: var(--critical); box-shadow: 0 0 0 1px var(--surface); }
.cap { display: flex; flex-wrap: wrap; gap: 6px 16px; align-items: center; min-height: 18px; }
.legend { margin-left: auto; display: inline-flex; align-items: center; gap: 3px; font-size: var(--fs-xs); }
.legend .cell { width: 10px; height: 10px; }
.legend .cell.x::after { inset: 2px; }
@media (max-width: 600px) { .heat { --c: 12px; } }
</style>
