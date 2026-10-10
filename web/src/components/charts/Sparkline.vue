<script setup lang="ts">
// Tiny trend line for tiles and table cells: no axes, the last point emphasised. Always next to a number.
import { computed } from 'vue'
import { linePath, areaPath, linearScale } from '@/lib/chart'

const props = withDefaults(defineProps<{ values: Array<number | null>; width?: number; height?: number; color?: string; fill?: boolean; label?: string }>(), { width: 96, height: 24, fill: true })
const pts = computed(() => {
  const nums = props.values.filter((v): v is number => v !== null && Number.isFinite(v))
  if (nums.length < 2) return []
  const min = Math.min(...nums)
  const max = Math.max(...nums)
  const xs = linearScale([0, props.values.length - 1], [2, props.width - 4])
  const ys = linearScale([min, max === min ? min + 1 : max], [props.height - 3, 3])
  return props.values.map((v, i) => (v === null || !Number.isFinite(v) ? null : { x: xs(i), y: ys(v) }))
})
const last = computed(() => [...pts.value].reverse().find(Boolean) ?? null)
const c = computed(() => props.color ?? 'var(--series-1)')
</script>

<template>
  <svg :width="width" :height="height" class="spark" role="img" :aria-label="label ?? 'trend'">
    <template v-if="pts.length">
      <path v-if="fill" :d="areaPath(pts, height)" :style="{ fill: c }" opacity="0.12" />
      <path :d="linePath(pts)" :style="{ stroke: c }" class="l" />
      <circle v-if="last" :cx="last.x" :cy="last.y" r="2.5" :style="{ fill: c }" />
    </template>
    <line v-else x1="2" :x2="width - 2" :y1="height / 2" :y2="height / 2" class="flat" />
  </svg>
</template>

<style scoped>
.spark { display: inline-block; vertical-align: middle; overflow: visible; }
.l { fill: none; stroke-width: 1.5; stroke-linejoin: round; stroke-linecap: round; }
.flat { stroke: var(--border-strong); stroke-dasharray: 2 3; }
</style>
