<script setup lang="ts">
// Grouped navigation. Entries the user's role cannot open are hidden (the server enforces anyway).
// `rail` = collapsed to icons (desktop); on narrow screens the sidebar is an off-canvas drawer.
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Logo from './Logo.vue'
import { nav } from '@/router/nav'
import { canOpen } from '@/router'
import { session } from '@/stores/session'
import { instance } from '@/stores/instance'
import NavBadge from '@/components/config/NavBadge.vue'

defineProps<{ rail: boolean; mobileOpen: boolean }>()
const emit = defineEmits<{ toggleRail: []; close: [] }>()
const route = useRoute()

const groups = computed(() => {
  void session.role // re-evaluate on role change
  return nav.map((g) => ({ ...g, items: g.items.filter((i) => canOpen(i.route)) })).filter((g) => g.items.length)
})
const activeSection = computed(() => String(route.meta.section ?? route.name ?? ''))
</script>

<template>
  <aside class="sidebar" :class="{ rail, open: mobileOpen }" aria-label="Main navigation">
    <div class="brand">
      <RouterLink to="/" class="home" @click="emit('close')"><Logo /><span class="word">incus-backup</span></RouterLink>
      <button type="button" class="close-m btn-icon" aria-label="Close navigation" @click="emit('close')"><Icon name="x" /></button>
    </div>
    <nav class="groups">
      <div v-for="(g, gi) in groups" :key="gi" class="group">
        <div v-if="g.label" class="glabel">{{ g.label }}</div>
        <div v-else-if="gi > 0" class="gsep" />
        <RouterLink
          v-for="it in g.items"
          :key="it.route"
          :to="{ name: it.route }"
          class="item"
          :class="{ active: activeSection === it.route }"
          :aria-current="activeSection === it.route ? 'page' : undefined"
          :title="rail ? it.label : undefined"
          :data-nav="it.route"
          @click="emit('close')"
        >
          <Icon :name="it.icon" :size="16" />
          <span class="label">{{ it.label }}</span>
          <NavBadge :route="it.route" />
        </RouterLink>
      </div>
    </nav>
    <div class="foot">
      <span class="ver" :title="`Server version ${instance.version || 'unknown'}`">v{{ instance.version || '?' }}</span>
      <button type="button" class="railbtn" :aria-label="rail ? 'Expand sidebar' : 'Collapse sidebar'" :title="rail ? 'Expand sidebar' : 'Collapse sidebar'" @click="emit('toggleRail')">
        <Icon :name="rail ? 'panel-left-open' : 'panel-left-close'" :size="16" />
      </button>
    </div>
  </aside>
</template>

<style scoped>
.sidebar { position: sticky; top: 0; height: 100dvh; width: var(--sidebar-w); flex: none; display: flex; flex-direction: column; background: var(--nav-bg); color: var(--nav-text); border-right: 1px solid var(--nav-border); z-index: var(--z-sidebar); transition: width var(--t) var(--ease); }
.brand { display: flex; align-items: center; height: var(--topbar-h); padding: 0 12px 0 14px; border-bottom: 1px solid var(--nav-border); }
.home { display: flex; align-items: center; gap: 10px; color: var(--nav-text-active); text-decoration: none; min-width: 0; }
.home:hover { text-decoration: none; }
.word { font-weight: var(--fw-semibold); font-size: 14.5px; letter-spacing: -0.01em; white-space: nowrap; }
.close-m { display: none; margin-left: auto; width: 32px; height: 32px; border: 0; background: none; color: var(--nav-text); border-radius: var(--r); cursor: pointer; place-items: center; }
.groups { flex: 1; overflow-y: auto; padding: 8px 8px 12px; scrollbar-width: thin; scrollbar-color: var(--nav-bg-active) transparent; }
.group { display: flex; flex-direction: column; gap: 1px; }
.glabel { padding: 14px 10px 5px; font-size: 10.5px; font-weight: var(--fw-semibold); letter-spacing: .08em; text-transform: uppercase; color: var(--nav-text-muted); white-space: nowrap; }
.gsep { height: 1px; margin: 10px 8px; background: var(--nav-border); }
.item { position: relative; display: flex; align-items: center; gap: 10px; height: 32px; padding: 0 10px; border-radius: var(--r); color: var(--nav-text); text-decoration: none; font-size: var(--fs-md); white-space: nowrap; }
.item .icon { color: var(--nav-text-muted); }
.item:hover { background: var(--nav-bg-hover); color: var(--nav-text-active); text-decoration: none; }
.item:hover .icon { color: var(--nav-text-active); }
.item.active { background: var(--nav-bg-active); color: var(--nav-text-active); font-weight: var(--fw-medium); }
.item.active .icon { color: var(--nav-marker); }
.item.active::before { content: ''; position: absolute; left: -8px; top: 7px; bottom: 7px; width: 3px; border-radius: 0 2px 2px 0; background: var(--nav-marker); }
.item:focus-visible { outline-color: var(--nav-marker); }
.foot { display: flex; align-items: center; gap: 8px; padding: 8px 10px 8px 16px; border-top: 1px solid var(--nav-border); }
.ver { font-size: var(--fs-xs); color: var(--nav-text-muted); font-family: var(--font-mono); white-space: nowrap; overflow: hidden; }
.railbtn { margin-left: auto; display: grid; place-items: center; width: 28px; height: 28px; border: 0; border-radius: var(--r); background: none; color: var(--nav-text-muted); cursor: pointer; }
.railbtn:hover { background: var(--nav-bg-hover); color: var(--nav-text-active); }

.rail { width: 56px; }
.rail .word, .rail .label, .rail .glabel, .rail .ver { display: none; }
.rail .gsep, .rail .group + .group { }
.rail .group:not(:first-child) { border-top: 1px solid var(--nav-border); margin-top: 8px; padding-top: 8px; }
.rail .gsep { display: none; }
.rail .item { justify-content: center; padding: 0; }
.rail .brand { justify-content: center; padding: 0; }
.rail .foot { justify-content: center; padding: 8px 0; }
.rail .railbtn { margin: 0; }

@media (max-width: 900px) {
  .sidebar { position: fixed; left: 0; top: 0; bottom: 0; width: min(280px, 86vw); transform: translateX(-100%); transition: transform var(--t) var(--ease), visibility var(--t); z-index: var(--z-drawer); visibility: hidden; }
  .sidebar.open { transform: none; visibility: visible; box-shadow: var(--shadow-pop); }
  .sidebar.rail { width: min(280px, 86vw); }
  .rail .word, .rail .label, .rail .glabel { display: initial; }
  .rail .item { justify-content: flex-start; padding: 0 10px; }
  .rail .brand { justify-content: flex-start; padding: 0 12px 0 14px; }
  .close-m { display: grid; }
  .railbtn { display: none; }
}
</style>
