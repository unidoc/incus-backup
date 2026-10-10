<script setup lang="ts">
// Tabs. Two modes:
//  * local: <Tabs v-model="tab" :tabs="[{ id: 'a', label: 'General' }]"> then show content by `tab`.
//  * routed: tabs with `to` render as links (sub-pages such as Alerts | Silences); active = current route.
// Arrow keys move between tabs (WAI-ARIA tabs pattern).
import { RouterLink, useRoute, type RouteLocationRaw } from 'vue-router'

export interface TabDef { id: string; label: string; count?: number; to?: RouteLocationRaw; disabled?: boolean }
const props = defineProps<{ tabs: TabDef[]; modelValue?: string; label?: string }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
const route = useRoute()

function isActive(t: TabDef) {
  if (t.to) return typeof t.to === 'object' && 'name' in t.to ? route.name === t.to.name : false
  return props.modelValue === t.id
}
function onKey(e: KeyboardEvent, i: number) {
  const dir = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0
  if (!dir) return
  e.preventDefault()
  const enabled = props.tabs.filter((t) => !t.disabled)
  const cur = enabled.indexOf(props.tabs[i])
  const next = enabled[(cur + dir + enabled.length) % enabled.length]
  const el = (e.currentTarget as HTMLElement).parentElement?.querySelector<HTMLElement>(`[data-tab="${next.id}"]`)
  el?.focus()
  if (!next.to) emit('update:modelValue', next.id)
}
</script>

<template>
  <div class="tabs" role="tablist" :aria-label="label">
    <template v-for="(t, i) in tabs" :key="t.id">
      <RouterLink v-if="t.to" :to="t.to" class="tab" :class="{ active: isActive(t) }" role="tab" :aria-selected="isActive(t)" :data-tab="t.id" @keydown="onKey($event, i)">
        {{ t.label }}<span v-if="t.count !== undefined" class="count num">{{ t.count }}</span>
      </RouterLink>
      <button v-else type="button" class="tab" :class="{ active: isActive(t) }" role="tab" :aria-selected="isActive(t)" :tabindex="isActive(t) ? 0 : -1" :disabled="t.disabled" :data-tab="t.id" @click="emit('update:modelValue', t.id)" @keydown="onKey($event, i)">
        {{ t.label }}<span v-if="t.count !== undefined" class="count num">{{ t.count }}</span>
      </button>
    </template>
  </div>
</template>

<style scoped>
.tabs { display: flex; gap: 2px; border-bottom: 1px solid var(--border); overflow-x: auto; scrollbar-width: none; margin-bottom: var(--s-4); }
.tab { position: relative; display: inline-flex; align-items: center; gap: 6px; height: 36px; padding: 0 12px; border: 0; background: none; color: var(--text-muted); font-weight: var(--fw-medium); cursor: pointer; white-space: nowrap; text-decoration: none; border-radius: var(--r) var(--r) 0 0; }
.tab:hover { color: var(--text); background: var(--surface-3); text-decoration: none; }
.tab.active { color: var(--text); }
.tab.active::after { content: ''; position: absolute; left: 8px; right: 8px; bottom: -1px; height: 2px; background: var(--accent); border-radius: 2px 2px 0 0; }
.tab:disabled { opacity: .45; cursor: not-allowed; }
.count { font-size: var(--fs-xs); padding: 1px 6px; border-radius: var(--r-pill); background: var(--surface-3); color: var(--text-2); }
</style>
