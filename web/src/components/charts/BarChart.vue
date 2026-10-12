<script setup lang="ts">
// Vertical bars for counts per category or per day (runs per day, failures per host). One series;
// `statuses` colours individual bars by state (e.g. a failed day) instead of the series colour.
// Bars have 4 px rounded tops anchored to the baseline and a 2 px gap; hover shows a tooltip.
import { computed, ref } from 'vue'
import ChartFrame from './ChartFrame.vue'
import { useElementWidth } from '@/composables/useElementWidth'
import { linearScale, niceTicks } from '@/lib/chart'
import { formatNumber } from '@/lib/format'
import type { Status } from '@/lib/status'

const props = withDefaults(defineProps<{ labels: string[]; values: number[]; name?: string; statuses?: Array<Status | null>; height?: number; title?: string; subtitle?: string; yFormat?: (v: number) => string; color?: string }>(), { height: 180, name: 'Value' })
const el = ref<HTMLElement | null>(null)
const width = useElementWidth(el)
const M = { t: 16, r: 8, b: 24, l: 44 }
const fy = (v: number) => (props.yFormat ? props.yFormat(v) : formatNumber(v))
const yt = computed(() => niceTicks(0, Math.max(1, ...props.values), 4))
const ys = computed(() => linearScale(yt.value.domain, [props.height - M.b, M.t]))
const band = computed(() => (width.value - M.l - M.r) / Math.max(1, props.labels.length))
const barW = computed(() => Math.max(2, Math.min(40, band.value - Math.max(2, band.value * 0.28))))
const statusVar: Record<Status, string> = { ok: 'var(--ok)', warn: 'var(--warn)', critical: 'var(--critical)', info: 'var(--info)', neutral: 'var(--neutral)' }
const fill = (i: number) => (props.statuses?.[i] ? statusVar[props.statuses[i] as Status] : props.color ?? 'var(--series-1)')
const every = computed(() => Math.max(1, Math.ceil(props.labels.length / Math.max(1, Math.floor(width.value / 64)))))
const hover = ref(-1)
function bar(i: number) {
  const v = props.values[i] ?? 0
  const x = M.l + band.value * i + (band.value - barW.value) / 2
  const y = ys.value(v)
  const h = Math.max(0, ys.value(0) - y)
  const r = Math.min(4, barW.value / 2, h)
  // rounded top corners only, flat on the baseline
  const d = h <= 0 ? '' : `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + barW.value - r}Q${x + barW.value},${y} ${x + barW.value},${y + r}V${y + h}Z`
  return { x, y, d }
}
const rows = computed(() => props.labels.map((l, i) => [l, fy(props.values[i] ?? 0)]))
</script>

<template>
  <ChartFrame :title="title" :subtitle="subtitle" :columns="['', name]" :rows="rows">
    <div ref="el" class="bc" :style="{ height: `${height}px` }">
      <svg :width="width" :height="height" role="img" :aria-label="title ?? name" @pointerleave="hover = -1">
        <g class="grid"><line v-for="t in yt.ticks" :key="t" :x1="M.l" :x2="width - M.r" :y1="ys(t)" :y2="ys(t)" /></g>
        <g class="yaxis"><text v-for="t in yt.ticks" :key="t" :x="M.l - 8" :y="ys(t)" dy="0.32em" text-anchor="end">{{ fy(t) }}</text></g>
        <g v-for="(l, i) in labels" :key="i">
          <rect class="hit" :x="M.l + band * i" :y="M.t" :width="band" :height="height - M.t - M.b" @pointerenter="hover = i" />
          <path :d="bar(i).d" :style="{ fill: fill(i), opacity: hover >= 0 && hover !== i ? 0.55 : 1 }" class="bar" />
          <text v-if="i % every === 0" class="xl" :x="M.l + band * i + band / 2" :y="height - 6" text-anchor="middle">{{ l }}</text>
        </g>
      </svg>
      <div v-if="hover >= 0" class="tip" :style="{ left: `${Math.min(bar(hover).x, width - 140)}px`, top: `${Math.max(0, bar(hover).y - 44)}px` }">
        <div class="tt">{{ labels[hover] }}</div><b class="num">{{ fy(values[hover] ?? 0) }}</b> <span class="muted">{{ name }}</span>
      </div>
    </div>
  </ChartFrame>
</template>

<style scoped>
.bc { position: relative; width: 100%; }
svg { display: block; overflow: visible; }
.grid line { stroke: var(--grid); }
.yaxis text, .xl { fill: var(--text-muted); font-size: 11px; font-variant-numeric: tabular-nums; }
.hit { fill: transparent; }
.bar { transition: opacity var(--t-fast); pointer-events: none; }
.tip { position: absolute; padding: 6px 9px; border-radius: var(--r); background: var(--surface); border: 1px solid var(--border-strong); box-shadow: var(--shadow-pop); font-size: var(--fs-xs); pointer-events: none; white-space: nowrap; z-index: 5; }
.tt { color: var(--text-muted); }
</style>
