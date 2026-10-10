<script setup lang="ts">
// Overflow menu for row and page actions: <RowMenu :items="[{ label: 'Disable', run, danger: true, disabled: 'Needs the operator role' }]" />.
// A string in `disabled` is shown as the reason. Arrow keys move, Escape closes and returns focus.
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import type { IconName } from '@/components/icons'
import { useDismiss } from '@/composables/useDismiss'

export interface MenuItem { label: string; icon?: IconName; run: () => unknown; danger?: boolean; disabled?: string | false; testid?: string }
const props = withDefaults(defineProps<{ items: MenuItem[]; label?: string; align?: 'left' | 'right'; button?: string }>(), { label: 'More actions', align: 'right' })
const open = ref(false)
const root = ref<HTMLElement | null>(null)
const trigger = ref<HTMLElement | null>(null)
useDismiss(root, open, trigger)
// position: fixed so the menu is never clipped by a scrolling table
const pos = ref<Record<string, string>>({})
function place() {
  const r = trigger.value?.getBoundingClientRect()
  if (!r) return
  const below = window.innerHeight - r.bottom > 220
  pos.value = {
    ...(below ? { top: `${r.bottom + 4}px` } : { bottom: `${window.innerHeight - r.top + 4}px` }),
    ...(props.align === 'right' ? { right: `${Math.max(8, window.innerWidth - r.right)}px` } : { left: `${Math.max(8, r.left)}px` }),
  }
}
async function toggle() {
  if (!open.value) place()
  open.value = !open.value
  if (open.value) { await nextTick(); (root.value?.querySelector('[role=menuitem]:not([aria-disabled=true])') as HTMLElement | null)?.focus() }
}
function pick(it: MenuItem) { if (it.disabled) return; open.value = false; trigger.value?.focus(); it.run() }
function onKey(e: KeyboardEvent) {
  const els = [...(root.value?.querySelectorAll<HTMLElement>('[role=menuitem]') ?? [])]
  const i = els.indexOf(document.activeElement as HTMLElement)
  if (e.key === 'ArrowDown') { e.preventDefault(); els[(i + 1) % els.length]?.focus() }
  if (e.key === 'ArrowUp') { e.preventDefault(); els[(i - 1 + els.length) % els.length]?.focus() }
}
const onScroll = (e: Event) => { if (open.value && !root.value?.contains(e.target as Node)) open.value = false }
onMounted(() => { window.addEventListener('scroll', onScroll, true); window.addEventListener('resize', onScroll) })
onBeforeUnmount(() => { window.removeEventListener('scroll', onScroll, true); window.removeEventListener('resize', onScroll) })
defineExpose({ close: () => (open.value = false), props })
</script>

<template>
  <div ref="root" class="rm">
    <button ref="trigger" type="button" class="btn btn-sm" :class="button ? '' : 'btn-ghost btn-icon'" :aria-label="button ? undefined : label" aria-haspopup="menu" :aria-expanded="open" data-testid="row-menu" @click="toggle">
      <Icon name="ellipsis" :size="15" /><template v-if="button">{{ button }}</template>
    </button>
    <div v-if="open" class="menu" :style="pos" role="menu" :aria-label="label" @keydown="onKey">
      <button v-for="it in items" :key="it.label" type="button" role="menuitem" class="mi" :class="{ danger: it.danger }" :aria-disabled="it.disabled ? 'true' : undefined" :title="it.disabled || undefined" :data-testid="it.testid" @click="pick(it)">
        <Icon v-if="it.icon" :name="it.icon" :size="14" /><span>{{ it.label }}</span><span v-if="it.disabled" class="why">{{ it.disabled }}</span>
      </button>
    </div>
  </div>
</template>

<style scoped>
.rm { position: relative; display: inline-flex; }
.menu { position: fixed; z-index: 60; min-width: 200px; padding: 4px; background: var(--surface); border: 1px solid var(--border-strong); border-radius: var(--r); box-shadow: var(--shadow-pop); display: flex; flex-direction: column; }
.mi { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; padding: 7px 10px; border: 0; background: none; border-radius: var(--r-sm); text-align: left; font-size: var(--fs-sm); color: var(--text); cursor: pointer; }
.mi:hover:not([aria-disabled]), .mi:focus-visible { background: var(--surface-3); outline: none; }
.mi.danger { color: var(--critical-text); }
.mi[aria-disabled] { color: var(--text-faint); cursor: not-allowed; }
.why { flex-basis: 100%; padding-left: 22px; font-size: var(--fs-xs); color: var(--text-muted); }
</style>
