<script setup lang="ts">
// Horizontal usage bar: <Meter :value="used" :max="total" :warn=".8" :critical=".9" label="Disk" />.
// The fill turns warn/critical at the thresholds; threshold ticks are drawn faintly.
import { computed } from 'vue'
import { fraction, thresholdStatus } from '@/lib/meter'
import { formatPercent } from '@/lib/format'

const props = withDefaults(defineProps<{ value?: number | null; max?: number | null; warn?: number; critical?: number; invert?: boolean; label?: string; detail?: string; size?: 'sm' | 'md'; neutral?: boolean }>(), { size: 'md' })
const f = computed(() => fraction(props.value, props.max))
const s = computed(() => (props.neutral ? 'neutral' : thresholdStatus(f.value, { warn: props.warn, critical: props.critical, invert: props.invert })))
const known = computed(() => !!props.max && props.value !== null && props.value !== undefined)
</script>

<template>
  <div class="meter" :class="[`s-${s}`, `z-${size}`]">
    <div v-if="label || detail" class="top">
      <span class="lbl">{{ label }}</span>
      <span class="val num">{{ detail ?? (known ? formatPercent(f) : 'unknown') }}</span>
    </div>
    <div class="track" role="meter" :aria-label="label ?? 'usage'" :aria-valuenow="known ? Math.round(f * 100) : undefined" aria-valuemin="0" aria-valuemax="100" :aria-valuetext="known ? formatPercent(f) : 'unknown'">
      <div class="fill" :style="{ width: `${f * 100}%` }" />
      <span v-if="warn !== undefined && !invert" class="tick" :style="{ left: `${warn * 100}%` }" />
      <span v-if="critical !== undefined && !invert" class="tick" :style="{ left: `${critical * 100}%` }" />
    </div>
  </div>
</template>

<style scoped>
.meter { display: flex; flex-direction: column; gap: 5px; min-width: 0; }
.top { display: flex; justify-content: space-between; gap: 8px; font-size: var(--fs-sm); }
.lbl { color: var(--text-2); }
.val { color: var(--text); font-weight: var(--fw-medium); }
.track { position: relative; height: 6px; border-radius: 3px; background: var(--surface-3); overflow: hidden; }
.z-sm .track { height: 4px; }
.fill { height: 100%; border-radius: inherit; background: var(--c); transition: width var(--t) var(--ease); }
.tick { position: absolute; top: 0; bottom: 0; width: 1px; background: var(--border-strong); }
.s-ok { --c: var(--accent); }
.s-neutral { --c: var(--neutral); }
.s-warn { --c: var(--warn); }
.s-critical { --c: var(--critical); }
</style>
