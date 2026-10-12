<script setup lang="ts">
// Ctrl+K / Cmd+K (or "/") anywhere: jump to a page, host or object, or run a command (theme, sign out).
// Hosts and objects come from the cached lists of the current repository (fetched when the palette opens).
import { computed, nextTick, ref, watch, onMounted, onBeforeUnmount } from 'vue'
import { useRouter, type RouteLocationRaw } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import type { IconName } from '@/components/icons'
import { fuzzyFilter } from '@/lib/fuzzy'
import { canOpen } from '@/router'
import { nav } from '@/router/nav'
import { fetchCached } from '@/api/cache'
import { api, list } from '@/api/endpoints'
import { repos, repoNone } from '@/stores/repos'
import { cycleTheme } from '@/stores/theme'
import type { HostRow, ObjectRow } from '@/api/types'

const emit = defineEmits<{ signOut: [] }>()
const router = useRouter()
const open = ref(false)
const q = ref('')
const active = ref(0)
const input = ref<HTMLInputElement | null>(null)
const dlg = ref<HTMLDialogElement | null>(null)
const hosts = ref<HostRow[]>([])
const objects = ref<ObjectRow[]>([])

interface Item { id: string; kind: 'Page' | 'Host' | 'Object' | 'Command'; label: string; hint?: string; icon: IconName; to?: RouteLocationRaw; run?: () => void }

const pages = computed<Item[]>(() => {
  const out: Item[] = []
  for (const g of nav) for (const it of g.items) if (canOpen(it.route)) out.push({ id: `p:${it.route}`, kind: 'Page', label: it.label, hint: g.label || undefined, icon: it.icon, to: { name: it.route } })
  const extra: Array<[string, string, IconName]> = [['profile', 'Profile and security', 'user'], ['host-onboard', 'Add a host', 'plus'], ['repository-new', 'Add a repository', 'plus'], ['policy-new', 'New policy', 'plus'], ['restore-tests', 'Restore tests', 'archive-restore'], ['alert-silences', 'Alert silences', 'bell'], ['notification-rules', 'Notification rules', 'send'], ['tokens', 'API tokens', 'key-round']]
  for (const [name, label, icon] of extra) if (canOpen(name)) out.push({ id: `p:${name}`, kind: 'Page', label, icon, to: { name } })
  return out
})
const commands = computed<Item[]>(() => [
  { id: 'c:theme', kind: 'Command', label: 'Switch theme (system, light, dark)', icon: 'sun', run: cycleTheme },
  { id: 'c:signout', kind: 'Command', label: 'Sign out', icon: 'log-out', run: () => emit('signOut') },
])
const all = computed<Item[]>(() => [
  ...pages.value,
  ...hosts.value.map((h): Item => ({ id: `h:${h.name}:${h.scope}`, kind: 'Host', label: h.name, hint: h.scope, icon: 'server', to: { name: 'host', params: { name: h.name } } })),
  ...objects.value.map((o): Item => ({ id: `o:${o.group_id}`, kind: 'Object', label: `${o.project}/${o.name}`, hint: `${o.kind} on ${o.host}`, icon: 'box', to: { name: 'object', params: { group: o.group_id } } })),
  ...commands.value,
])
const results = computed(() => fuzzyFilter(all.value, q.value, (i) => `${i.label} ${i.hint ?? ''}`, 40))
watch(results, () => { active.value = 0 })

async function show() {
  open.value = true
  q.value = ''
  await nextTick()
  dlg.value?.showModal()
  input.value?.focus()
  if (repos.current && !repoNone.value) {
    const r = repos.current
    fetchCached(`hosts:${r}`, (s) => api.hosts.list(s), 30_000).then((v) => { hosts.value = list(v) }, () => {})
    fetchCached(`objects:${r}`, (s) => api.objects.list(s), 30_000).then((v) => { objects.value = list(v) }, () => {})
  }
}
function hide() { open.value = false; dlg.value?.close() }
function choose(i: Item | undefined) {
  if (!i) return
  hide()
  if (i.to) void router.push(i.to)
  else i.run?.()
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'ArrowDown') { active.value = Math.min(results.value.length - 1, active.value + 1); e.preventDefault(); scrollActive() }
  else if (e.key === 'ArrowUp') { active.value = Math.max(0, active.value - 1); e.preventDefault(); scrollActive() }
  else if (e.key === 'Enter') { e.preventDefault(); choose(results.value[active.value]) }
}
function scrollActive() { void nextTick(() => dlg.value?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' })) }

function onGlobalKey(e: KeyboardEvent) {
  const t = e.target as HTMLElement
  const typing = t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable)
  if ((e.key === 'k' || e.key === 'K') && (e.ctrlKey || e.metaKey)) { e.preventDefault(); open.value ? hide() : void show() }
  else if (e.key === '/' && !typing && !open.value) { e.preventDefault(); void show() }
}
onMounted(() => document.addEventListener('keydown', onGlobalKey))
onBeforeUnmount(() => document.removeEventListener('keydown', onGlobalKey))
defineExpose({ show })
const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform)
</script>

<template>
  <button type="button" class="trigger" data-testid="palette-open" @click="show">
    <Icon name="search" :size="14" /><span class="tl">Search or jump to…</span><kbd class="k">{{ isMac ? '⌘' : 'Ctrl' }} K</kbd>
  </button>
  <dialog ref="dlg" class="palette" aria-label="Search and commands" @close="open = false" @mousedown.self="hide">
    <div v-if="open" class="box">
      <div class="in">
        <Icon name="search" :size="16" />
        <input ref="input" v-model="q" type="text" placeholder="Pages, hosts, objects, commands" role="combobox" aria-expanded="true" aria-controls="palette-list" :aria-activedescendant="results[active] ? `pi-${active}` : undefined" autocomplete="off" spellcheck="false" data-testid="palette-input" @keydown="onKey" />
        <kbd>Esc</kbd>
      </div>
      <ul id="palette-list" role="listbox" class="list">
        <li v-for="(r, i) in results" :id="`pi-${i}`" :key="r.id" role="option" :aria-selected="i === active" class="opt" :class="{ on: i === active }" @mousemove="active = i" @click="choose(r)">
          <Icon :name="r.icon" :size="15" class="oi" />
          <span class="ol">{{ r.label }}</span>
          <span v-if="r.hint" class="oh">{{ r.hint }}</span>
          <span class="ok">{{ r.kind }}</span>
        </li>
        <li v-if="!results.length" class="none">Nothing matches “{{ q }}”. Try a host name, an object such as default/web01, or a page name.</li>
      </ul>
      <div class="foot"><span><kbd>↑</kbd><kbd>↓</kbd> move</span><span><kbd>↵</kbd> open</span><span><kbd>/</kbd> or <kbd>{{ isMac ? '⌘' : 'Ctrl' }} K</kbd> anywhere</span></div>
    </div>
  </dialog>
</template>

<style scoped>
.trigger { display: flex; align-items: center; gap: 8px; height: 30px; width: 280px; padding: 0 6px 0 10px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--surface-2); color: var(--text-muted); font-size: var(--fs-sm); cursor: pointer; }
.trigger:hover { border-color: var(--text-faint); color: var(--text-2); }
.tl { flex: 1; text-align: left; white-space: nowrap; overflow: hidden; }
.k { margin-left: auto; }
.palette { padding: 0; border: 1px solid var(--border-strong); border-radius: var(--r-lg); width: min(640px, calc(100vw - 24px)); margin-top: 12vh; background: var(--surface); color: var(--text); box-shadow: var(--shadow-pop); }
.palette::backdrop { background: var(--scrim); }
.in { display: flex; align-items: center; gap: 10px; padding: 0 14px; height: 50px; border-bottom: 1px solid var(--border); color: var(--text-muted); }
.in input { flex: 1; border: 0; outline: 0; background: transparent; font-size: 15px; color: var(--text); }
.list { list-style: none; margin: 0; padding: 6px; max-height: min(420px, 60vh); overflow-y: auto; }
.opt { display: flex; align-items: center; gap: 10px; height: 36px; padding: 0 10px; border-radius: var(--r); cursor: pointer; }
.opt.on { background: var(--accent-soft); }
.oi { color: var(--text-muted); }
.opt.on .oi { color: var(--accent-text); }
.ol { font-weight: var(--fw-medium); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.oh { color: var(--text-muted); font-size: var(--fs-sm); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.ok { margin-left: auto; font-size: var(--fs-xs); color: var(--text-faint); flex: none; }
.none { padding: 18px 12px; color: var(--text-muted); font-size: var(--fs-sm); }
.foot { display: flex; gap: 16px; padding: 8px 14px; border-top: 1px solid var(--border); font-size: var(--fs-xs); color: var(--text-muted); background: var(--surface-2); border-radius: 0 0 var(--r-lg) var(--r-lg); }
.foot kbd { margin-right: 3px; }
@media (max-width: 1100px) { .trigger { width: 200px; } }
@media (max-width: 720px) { .trigger { width: 32px; padding: 0; justify-content: center; } .tl, .k { display: none; } .foot span:last-child { display: none; } }
</style>
