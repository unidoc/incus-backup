<script setup lang="ts">
// Ring gauge for one headline ratio (e.g. "84 / 85 protected"). Text is in the middle; the ring is
// decoration for the number, never the only carrier of it.
import { computed } from 'vue'
import { fraction, thresholdStatus } from '@/lib/meter'

const props = withDefaults(defineProps<{ value?: number | null; max?: number | null; warn?: number; critical?: number; invert?: boolean; size?: number; label?: string; center?: string }>(), { size: 72 })
const f = computed(() => fraction(props.value, props.max))
const s = computed(() => thresholdStatus(f.value, { warn: props.warn, critical: props.critical, invert: props.invert }))
const r = 15.5
const c = 2 * Math.PI * r
</script>

<template>
  <div class="gauge" :class="`s-${s}`" :style="{ width: `${size}px`, height: `${size}px` }" role="img" :aria-label="`${label ?? 'value'}: ${center ?? Math.round(f * 100) + ' %'}`">
    <svg viewBox="0 0 36 36" :width="size" :height="size" aria-hidden="true">
      <circle cx="18" cy="18" :r="r" class="bg" />
      <circle cx="18" cy="18" :r="r" class="fg" :stroke-dasharray="`${f * c} ${c}`" transform="rotate(-90 18 18)" />
    </svg>
    <span class="c num">{{ center ?? `${Math.round(f * 100)}%` }}</span>
  </div>
</template>

<style scoped>
.gauge { position: relative; display: inline-grid; place-items: center; flex: none; }
svg { position: absolute; inset: 0; }
.bg { fill: none; stroke: var(--surface-3); stroke-width: 3.2; }
.fg { fill: none; stroke: var(--c); stroke-width: 3.2; stroke-linecap: round; transition: stroke-dasharray var(--t) var(--ease); }
.c { position: relative; font-size: 13px; font-weight: var(--fw-semibold); }
.s-ok { --c: var(--ok); } .s-warn { --c: var(--warn); } .s-critical { --c: var(--critical); }
</style>
