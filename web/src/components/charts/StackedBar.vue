<script setup lang="ts">
// One horizontal 100 % bar showing composition (protected / stale / never backed up). Segments are
// separated by 2 px gaps, labelled in the legend with counts and percentages (identity never by colour
// alone). Segment colour: a status (`status`) or a series slot.
import { computed, ref } from 'vue'
import { formatNumber, formatPercent } from '@/lib/format'
import type { Status } from '@/lib/status'

export interface Segment { label: string; value: number; status?: Status; color?: string }
const props = withDefaults(defineProps<{ segments: Segment[]; height?: number; showLegend?: boolean; total?: number; format?: (v: number) => string }>(), { height: 10, showLegend: true })
const sum = computed(() => props.total ?? props.segments.reduce((a, s) => a + Math.max(0, s.value), 0))
const statusVar: Record<Status, string> = { ok: 'var(--ok)', warn: 'var(--warn)', critical: 'var(--critical)', info: 'var(--info)', neutral: 'var(--neutral)' }
const series = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)', 'var(--series-4)']
const col = (s: Segment, i: number) => s.color ?? (s.status ? statusVar[s.status] : series[i] ?? 'var(--series-other)')
const fmt = (v: number) => (props.format ? props.format(v) : formatNumber(v))
const hover = ref(-1)
const label = computed(() => props.segments.map((s) => `${s.label} ${fmt(s.value)}`).join(', '))
</script>

<template>
  <div class="sb">
    <div class="bar" :style="{ height: `${height}px` }" role="img" :aria-label="label">
      <template v-for="(s, i) in segments" :key="s.label">
        <span v-if="s.value > 0 && sum > 0" class="seg" :class="{ dim: hover >= 0 && hover !== i }" :style="{ flexGrow: s.value, background: col(s, i) }" :title="`${s.label}: ${fmt(s.value)} (${formatPercent(s.value / sum)})`" @pointerenter="hover = i" @pointerleave="hover = -1" />
      </template>
      <span v-if="sum === 0" class="seg empty" />
    </div>
    <ul v-if="showLegend" class="lg">
      <li v-for="(s, i) in segments" :key="s.label" :class="{ dim: hover >= 0 && hover !== i }" @pointerenter="hover = i" @pointerleave="hover = -1">
        <span class="sw" :style="{ background: col(s, i) }" />{{ s.label }} <b class="num">{{ fmt(s.value) }}</b><span v-if="sum" class="pct num">{{ formatPercent(s.value / sum, 0) }}</span>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.sb { display: flex; flex-direction: column; gap: 8px; min-width: 0; }
.bar { display: flex; gap: 2px; width: 100%; border-radius: 3px; overflow: hidden; }
.seg { flex-basis: 0; min-width: 3px; transition: opacity var(--t-fast); }
.seg.empty { flex-grow: 1; background: var(--surface-3); }
.dim { opacity: .45; }
.lg { display: flex; flex-wrap: wrap; gap: 4px 16px; list-style: none; margin: 0; padding: 0; font-size: var(--fs-sm); color: var(--text-2); }
.lg li { display: inline-flex; align-items: center; gap: 6px; transition: opacity var(--t-fast); }
.lg b { font-weight: var(--fw-semibold); color: var(--text); }
.pct { color: var(--text-muted); font-size: var(--fs-xs); }
.sw { width: 10px; height: 10px; border-radius: 2px; }
</style>
