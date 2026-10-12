<script setup lang="ts">
// Time zone picker with search (city, region or "utc+2"), keyboard navigation and the current offset.
// Combobox pattern: the input filters, arrow keys move, Enter picks, Escape closes.
import { computed, ref, watch, useId, onMounted, onBeforeUnmount } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { searchZones, timeZones, zoneOffset } from '@/lib/storage'

const props = defineProps<{ modelValue: string; id?: string; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
const uid = useId()
const listId = `tz-list-${uid}`
const all = timeZones()
const open = ref(false)
const q = ref('')
const active = ref(0)
const root = ref<HTMLElement | null>(null)
const hits = computed(() => searchZones(all, q.value, 80))
watch(hits, () => { active.value = 0 })
const browserZone = (() => { try { return Intl.DateTimeFormat().resolvedOptions().timeZone } catch { return '' } })()

function pick(z: string) {
  emit('update:modelValue', z)
  open.value = false
  q.value = ''
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'ArrowDown') { e.preventDefault(); open.value = true; active.value = Math.min(hits.value.length - 1, active.value + 1); scroll() }
  else if (e.key === 'ArrowUp') { e.preventDefault(); active.value = Math.max(0, active.value - 1); scroll() }
  else if (e.key === 'Enter') { if (open.value && hits.value[active.value]) { e.preventDefault(); pick(hits.value[active.value]) } }
  else if (e.key === 'Escape') { open.value = false; q.value = '' }
}
function scroll() { requestAnimationFrame(() => root.value?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' })) }
function onDoc(e: MouseEvent) { if (open.value && root.value && !root.value.contains(e.target as Node)) { open.value = false; q.value = '' } }
onMounted(() => document.addEventListener('mousedown', onDoc))
onBeforeUnmount(() => document.removeEventListener('mousedown', onDoc))
</script>

<template>
  <div ref="root" class="tz">
    <div class="field">
      <Icon name="search" :size="14" class="si" />
      <input :id="id" class="input" role="combobox" :aria-expanded="open" :aria-controls="listId" aria-autocomplete="list" :disabled="disabled"
        :value="open ? q : modelValue" :placeholder="open ? 'City, region or UTC+2' : modelValue" autocomplete="off" spellcheck="false" data-testid="tz-input"
        :aria-activedescendant="open && hits[active] ? `${listId}-${active}` : undefined"
        @focus="open = true" @click="open = true" @input="q = ($event.target as HTMLInputElement).value; open = true" @keydown="onKey" />
      <span class="off num">{{ zoneOffset(modelValue) }}</span>
    </div>
    <ul v-if="open" :id="listId" class="list" role="listbox" aria-label="Time zones">
      <li v-if="!q && browserZone && browserZone !== modelValue" class="hint">
        <button type="button" class="link" @mousedown.prevent="pick(browserZone)">Use your browser's zone: {{ browserZone }}</button>
      </li>
      <li v-for="(z, i) in hits" :id="`${listId}-${i}`" :key="z" role="option" :aria-selected="i === active" :class="{ cur: z === modelValue }" @mousedown.prevent="pick(z)" @mousemove="active = i">
        <span>{{ z.replace(/_/g, ' ') }}</span><span class="off num">{{ zoneOffset(z) }}</span>
      </li>
      <li v-if="!hits.length" class="hint">No zone matches “{{ q }}”.</li>
    </ul>
  </div>
</template>

<style scoped>
.tz { position: relative; max-width: 340px; }
.field { position: relative; display: flex; align-items: center; }
.si { position: absolute; left: 9px; color: var(--text-muted); pointer-events: none; }
.field .input { padding-left: 30px; padding-right: 90px; }
.field .off { position: absolute; right: 10px; color: var(--text-muted); font-size: var(--fs-xs); pointer-events: none; }
.list { position: absolute; z-index: 30; top: calc(100% + 4px); left: 0; right: 0; max-height: 280px; overflow: auto; list-style: none; margin: 0; padding: 4px; background: var(--surface); border: 1px solid var(--border-strong); border-radius: var(--r); box-shadow: var(--shadow-pop); }
li[role='option'] { display: flex; justify-content: space-between; gap: 8px; padding: 6px 8px; border-radius: var(--r-sm); cursor: pointer; font-size: var(--fs-sm); }
li[aria-selected='true'] { background: var(--surface-3); }
li.cur { font-weight: var(--fw-semibold); }
li .off { color: var(--text-muted); font-size: var(--fs-xs); }
.hint { padding: 6px 8px; color: var(--text-muted); font-size: var(--fs-sm); }
.link { border: 0; background: none; padding: 0; color: var(--accent-text); cursor: pointer; font: inherit; }
</style>
