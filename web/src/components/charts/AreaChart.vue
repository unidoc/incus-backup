<script setup lang="ts">
// Time series as area (one series) or lines with light fills (2-4 series). Faint grid, few ticks, the last
// point emphasised with its value, crosshair + tooltip on hover/touch, arrow keys move the crosshair when
// focused. <AreaChart :x="timestamps" :series="[{ name: 'Stored', values }]" :y-format="formatBytes" bytes />
import { computed, ref } from 'vue'
import ChartFrame from './ChartFrame.vue'
import { useElementWidth } from '@/composables/useElementWidth'
import { areaPath, byteTicks, linePath, linearScale, nearestIndex, niceTicks, timeTicks } from '@/lib/chart'
import { formatDate, formatDateTime, formatNumber } from '@/lib/format'

export interface Series { name: string; values: Array<number | null>; color?: string }
const props = withDefaults(defineProps<{
  x: number[]; series: Series[]; height?: number; title?: string; subtitle?: string
  yFormat?: (v: number) => string; xFormat?: (t: number) => string; bytes?: boolean; zero?: boolean; hours?: boolean
}>(), { height: 200, zero: true })

const el = ref<HTMLElement | null>(null)
const width = useElementWidth(el)
const M = { t: 12, r: 16, b: 24, l: 56 }
const colors = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)', 'var(--series-4)']
const color = (s: Series, i: number) => s.color ?? colors[i] ?? 'var(--series-other)'
const fy = (v: number) => (props.yFormat ? props.yFormat(v) : formatNumber(v, 2))
const fx = (t: number) => (props.xFormat ? props.xFormat(t) : props.hours ? formatDateTime(t) : formatDate(t))

const vals = computed(() => props.series.flatMap((s) => s.values.filter((v): v is number => v !== null && Number.isFinite(v))))
const yt = computed(() => {
  const max = vals.value.length ? Math.max(...vals.value) : 1
  const min = props.zero ? 0 : vals.value.length ? Math.min(...vals.value) : 0
  return props.bytes ? byteTicks(max, 4) : niceTicks(min, max, 4)
})
const xs = computed(() => linearScale([props.x[0] ?? 0, props.x[props.x.length - 1] ?? 1], [M.l, width.value - M.r]))
const ys = computed(() => linearScale(yt.value.domain, [props.height - M.b, M.t]))
const xTicks = computed(() => timeTicks(props.x[0] ?? 0, props.x[props.x.length - 1] ?? 0, Math.max(2, Math.floor(width.value / 110))))
const paths = computed(() => props.series.map((s, i) => {
  const pts = s.values.map((v, j) => (v === null || !Number.isFinite(v) ? null : { x: xs.value(props.x[j]), y: ys.value(v) }))
  return { s, i, line: linePath(pts), area: areaPath(pts, ys.value(yt.value.domain[0])), last: [...pts].reverse().find(Boolean) ?? null, lastV: [...s.values].reverse().find((v) => v !== null) ?? null }
}))
const pxs = computed(() => props.x.map((t) => xs.value(t)))

const hover = ref(-1)
function onMove(e: PointerEvent) {
  const r = (e.currentTarget as SVGElement).getBoundingClientRect()
  hover.value = nearestIndex(pxs.value, e.clientX - r.left)
}
function onKey(e: KeyboardEvent) {
  if (!props.x.length) return
  if (e.key === 'ArrowRight') hover.value = Math.min(props.x.length - 1, (hover.value < 0 ? props.x.length - 1 : hover.value + 1))
  else if (e.key === 'ArrowLeft') hover.value = Math.max(0, (hover.value < 0 ? props.x.length - 1 : hover.value - 1))
  else if (e.key === 'Escape') hover.value = -1
  else return
  e.preventDefault()
}
const tipLeft = computed(() => (hover.value >= 0 ? Math.min(Math.max(pxs.value[hover.value] + 10, 0), width.value - 170) : 0))
const legend = computed(() => props.series.map((s, i) => ({ label: s.name, color: color(s, i) })))
const tableRows = computed(() => props.x.map((t, j) => [fx(t), ...props.series.map((s) => (s.values[j] === null ? '–' : fy(s.values[j] as number)))]))
</script>

<template>
  <ChartFrame :title="title" :subtitle="subtitle" :legend="legend" :columns="['Time', ...series.map((s) => s.name)]" :rows="tableRows">
    <div ref="el" class="ac" :style="{ height: `${height}px` }">
      <svg v-if="x.length > 1" :width="width" :height="height" class="svg" tabindex="0" role="img" :aria-label="title ?? 'chart'" @pointermove="onMove" @pointerleave="hover = -1" @keydown="onKey" @blur="hover = -1">
        <g class="grid">
          <line v-for="t in yt.ticks" :key="t" :x1="M.l" :x2="width - M.r" :y1="ys(t)" :y2="ys(t)" />
        </g>
        <g class="yaxis">
          <text v-for="t in yt.ticks" :key="t" :x="M.l - 8" :y="ys(t)" dy="0.32em" text-anchor="end">{{ fy(t) }}</text>
        </g>
        <g class="xaxis">
          <text v-for="t in xTicks" :key="t" :x="xs(t)" :y="height - 6" text-anchor="middle">{{ fx(t) }}</text>
        </g>
        <g v-for="p in paths" :key="p.s.name">
          <path :d="p.area" class="area" :style="{ fill: color(p.s, p.i), opacity: series.length > 1 ? 0.08 : 0.14 }" />
          <path :d="p.line" class="line" :style="{ stroke: color(p.s, p.i) }" />
          <template v-if="p.last && hover < 0">
            <circle :cx="p.last.x" :cy="p.last.y" r="4" class="end" :style="{ fill: color(p.s, p.i) }" />
            <text v-if="series.length === 1 && p.lastV !== null" :x="p.last.x - 8" :y="p.last.y - 10" text-anchor="end" class="endlabel">{{ fy(p.lastV) }}</text>
          </template>
        </g>
        <g v-if="hover >= 0" class="cross">
          <line :x1="pxs[hover]" :x2="pxs[hover]" :y1="M.t" :y2="height - M.b" />
          <template v-for="p in paths" :key="p.s.name">
            <circle v-if="p.s.values[hover] !== null" :cx="pxs[hover]" :cy="ys(p.s.values[hover] as number)" r="4" class="end" :style="{ fill: color(p.s, p.i) }" />
          </template>
        </g>
      </svg>
      <p v-else class="none">Not enough data points yet.</p>
      <div v-if="hover >= 0" class="tip" :style="{ left: `${tipLeft}px` }">
        <div class="tt">{{ formatDateTime(x[hover]) }}</div>
        <div v-for="(s, i) in series" :key="s.name" class="tr"><span class="sw" :style="{ background: color(s, i) }" />{{ s.name }}<b class="num">{{ s.values[hover] === null ? '–' : fy(s.values[hover] as number) }}</b></div>
      </div>
    </div>
  </ChartFrame>
</template>

<style scoped>
.ac { position: relative; width: 100%; }
.svg { display: block; overflow: visible; touch-action: pan-y; }
.svg:focus-visible { outline: 2px solid var(--focus); outline-offset: 2px; }
.grid line { stroke: var(--grid); stroke-width: 1; }
.yaxis text, .xaxis text { fill: var(--text-muted); font-size: 11px; font-variant-numeric: tabular-nums; }
.line { fill: none; stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
.end { stroke: var(--surface); stroke-width: 2; }
.endlabel { fill: var(--text); font-size: 12px; font-weight: 600; font-variant-numeric: tabular-nums; paint-order: stroke; stroke: var(--surface); stroke-width: 3px; }
.cross line { stroke: var(--text-faint); stroke-dasharray: 3 3; }
.tip { position: absolute; top: 4px; min-width: 150px; padding: 7px 9px; border-radius: var(--r); background: var(--surface); border: 1px solid var(--border-strong); box-shadow: var(--shadow-pop); font-size: var(--fs-xs); pointer-events: none; z-index: 5; }
.tt { color: var(--text-muted); margin-bottom: 4px; }
.tr { display: flex; align-items: center; gap: 6px; color: var(--text-2); }
.tr b { margin-left: auto; padding-left: 12px; color: var(--text); font-weight: 600; }
.sw { width: 8px; height: 8px; border-radius: 2px; }
.none { display: grid; place-items: center; height: 100%; color: var(--text-muted); font-size: var(--fs-sm); border: 1px dashed var(--border); border-radius: var(--r); margin: 0; }
</style>
