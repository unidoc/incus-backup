<script setup lang="ts">
// Segmented filter with counts: <FilterChips v-model="state" :options="[{ value: 'all', label: 'All', count: 12 }]" label="State" />.
// A radio group (arrow keys move), the count is part of the accessible name.
import type { Status } from '@/lib/status'
export interface ChipOption { value: string; label: string; count?: number; status?: Status }
const props = defineProps<{ options: ChipOption[]; modelValue: string; label: string }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
function onKey(e: KeyboardEvent, i: number) {
  const d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0
  if (!d) return
  e.preventDefault()
  const n = props.options[(i + d + props.options.length) % props.options.length]
  emit('update:modelValue', n.value)
  ;((e.currentTarget as HTMLElement).parentElement?.querySelector(`[data-chip="${n.value}"]`) as HTMLElement | null)?.focus()
}
</script>

<template>
  <div class="chips" role="radiogroup" :aria-label="label">
    <button v-for="(o, i) in options" :key="o.value" type="button" role="radio" class="chip" :class="[{ on: o.value === modelValue }, o.status && o.count ? `s-${o.status}` : '']"
      :aria-checked="o.value === modelValue" :tabindex="o.value === modelValue ? 0 : -1" :data-chip="o.value" @click="emit('update:modelValue', o.value)" @keydown="onKey($event, i)">
      <span class="dot" v-if="o.status && o.count" aria-hidden="true" />{{ o.label }}<span v-if="o.count !== undefined" class="c num">{{ o.count }}</span>
    </button>
  </div>
</template>

<style scoped>
.chips { display: inline-flex; flex-wrap: wrap; gap: 4px; }
.chip { display: inline-flex; align-items: center; gap: 6px; height: 28px; padding: 0 10px; border: 1px solid var(--border-strong); border-radius: 999px; background: var(--surface); color: var(--text-2); font-size: var(--fs-sm); font-weight: var(--fw-medium); cursor: pointer; white-space: nowrap; }
.chip:hover { background: var(--surface-3); }
.chip.on { background: var(--accent-soft); border-color: var(--accent); color: var(--accent-text); }
.c { color: var(--text-muted); font-weight: var(--fw-regular); }
.chip.on .c { color: inherit; }
.dot { width: 7px; height: 7px; border-radius: 50%; background: var(--neutral); }
.s-ok .dot { background: var(--ok); } .s-warn .dot { background: var(--warn); } .s-critical .dot { background: var(--critical); } .s-info .dot { background: var(--info); }
</style>
