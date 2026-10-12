<script setup lang="ts">
import Icon from '@/components/icons/Icon.vue'
import Breadcrumbs from './Breadcrumbs.vue'
import CommandPalette from './CommandPalette.vue'
import LiveIndicator from './LiveIndicator.vue'
import RepoSelector from './RepoSelector.vue'
import UserMenu from './UserMenu.vue'
import RunningTasks from '@/components/ops/RunningTasks.vue'
import { instance } from '@/stores/instance'
import { theme, cycleTheme } from '@/stores/theme'
import { RouterLink } from 'vue-router'

defineEmits<{ menu: []; signOut: [] }>()
const themeIcon = () => (theme.pref === 'system' ? 'monitor' : theme.pref === 'light' ? 'sun' : 'moon')
</script>

<template>
  <header class="topbar">
    <button type="button" class="btn btn-ghost btn-icon menu" aria-label="Open navigation" data-testid="nav-open" @click="$emit('menu')"><Icon name="menu" /></button>
    <RouterLink to="/" class="inst" :title="`Instance: ${instance.name}`" data-testid="instance-name">{{ instance.name }}</RouterLink>
    <span class="slash" aria-hidden="true">/</span>
    <Breadcrumbs class="crumbs" />
    <div class="right">
      <CommandPalette @sign-out="$emit('signOut')" />
      <RepoSelector />
      <RunningTasks />
      <LiveIndicator />
      <button type="button" class="btn btn-ghost btn-icon" :aria-label="`Theme: ${theme.pref}. Switch theme`" :title="`Theme: ${theme.pref} (click to switch)`" data-testid="theme-toggle" @click="cycleTheme"><Icon :name="themeIcon()" /></button>
      <UserMenu />
    </div>
  </header>
</template>

<style scoped>
.topbar { position: sticky; top: 0; z-index: var(--z-topbar); display: flex; align-items: center; gap: 8px; height: var(--topbar-h); padding: 0 12px 0 var(--gutter); background: color-mix(in srgb, var(--bg) 90%, transparent); backdrop-filter: blur(8px); border-bottom: 1px solid var(--border); }
.menu { display: none; margin-left: -6px; }
.inst { font-weight: var(--fw-semibold); color: var(--text); white-space: nowrap; max-width: 220px; overflow: hidden; text-overflow: ellipsis; padding: 2px 4px; border-radius: var(--r-sm); }
.inst:hover { background: var(--surface-3); text-decoration: none; }
.slash { color: var(--text-faint); }
.crumbs { flex: 1; min-width: 0; }
.right { display: flex; align-items: center; gap: 8px; margin-left: auto; }
@media (max-width: 900px) { .menu { display: inline-flex; } }
@media (max-width: 600px) { .inst, .slash { display: none; } .topbar { gap: 4px; padding-right: 8px; } .right { gap: 4px; } }
</style>
