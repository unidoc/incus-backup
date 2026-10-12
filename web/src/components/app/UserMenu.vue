<script setup lang="ts">
import { ref, computed } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import { session, logout } from '@/stores/session'
import { theme, setTheme, type ThemePref } from '@/stores/theme'
import { useDismiss } from '@/composables/useDismiss'
import { stopLive } from '@/api/events'
import { resetRepos } from '@/stores/repos'

const open = ref(false)
const root = ref<HTMLElement | null>(null)
const trigger = ref<HTMLElement | null>(null)
useDismiss(root, open, trigger)
const router = useRouter()
const initials = computed(() => (session.user?.display_name || session.name || '?').split(/[\s._-]+/).filter(Boolean).slice(0, 2).map((s) => s[0]!.toUpperCase()).join(''))
const themes: Array<{ id: ThemePref; label: string; icon: 'monitor' | 'sun' | 'moon' }> = [
  { id: 'system', label: 'System', icon: 'monitor' }, { id: 'light', label: 'Light', icon: 'sun' }, { id: 'dark', label: 'Dark', icon: 'moon' },
]
async function signOut() {
  open.value = false
  stopLive()
  await logout()
  resetRepos()
  await router.push({ name: 'login' })
}
</script>

<template>
  <div ref="root" class="um">
    <button ref="trigger" type="button" class="trigger" :aria-expanded="open" aria-haspopup="menu" data-testid="user-menu" @click="open = !open">
      <span class="avatar" aria-hidden="true">{{ initials }}</span>
      <span class="who"><span class="n">{{ session.user?.display_name || session.name }}</span><span class="r">{{ session.role }}</span></span>
      <Icon name="chevron-down" :size="14" class="chev" />
    </button>
    <div v-if="open" class="menu" role="menu">
      <div class="hdr"><b>{{ session.name }}</b><span class="muted">Role: {{ session.role }}</span></div>
      <RouterLink :to="{ name: 'profile' }" class="mi" role="menuitem" @click="open = false"><Icon name="user" />Profile and security</RouterLink>
      <div class="sep" />
      <div class="theme" role="group" aria-label="Theme">
        <span class="tl">Theme</span>
        <div class="seg">
          <button v-for="t in themes" :key="t.id" type="button" :aria-pressed="theme.pref === t.id" :title="t.label" :data-theme-choice="t.id" @click="setTheme(t.id)"><Icon :name="t.icon" :size="14" /><span>{{ t.label }}</span></button>
        </div>
      </div>
      <div class="sep" />
      <button type="button" class="mi" role="menuitem" data-testid="sign-out" @click="signOut"><Icon name="log-out" />Sign out</button>
    </div>
  </div>
</template>

<style scoped>
.um { position: relative; }
.trigger { display: flex; align-items: center; gap: 8px; height: 34px; padding: 0 6px 0 3px; border: 1px solid transparent; border-radius: var(--r-pill); background: none; cursor: pointer; color: var(--text); }
.trigger:hover, .trigger[aria-expanded='true'] { background: var(--surface-3); }
.avatar { display: grid; place-items: center; width: 28px; height: 28px; border-radius: 50%; background: var(--accent-soft); color: var(--accent-text); font-size: 11.5px; font-weight: var(--fw-semibold); letter-spacing: .02em; }
.who { display: flex; flex-direction: column; align-items: flex-start; line-height: 1.15; }
.n { font-size: var(--fs-sm); font-weight: var(--fw-medium); max-width: 140px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.r { font-size: 11px; color: var(--text-muted); }
.chev { color: var(--text-muted); }
.menu { position: absolute; right: 0; top: calc(100% + 6px); width: 260px; padding: 6px; background: var(--surface); border: 1px solid var(--border-strong); border-radius: var(--r-lg); box-shadow: var(--shadow-pop); z-index: var(--z-palette); }
.hdr { display: flex; flex-direction: column; padding: 6px 8px 8px; font-size: var(--fs-sm); }
.mi { display: flex; align-items: center; gap: 9px; width: 100%; height: 32px; padding: 0 8px; border: 0; border-radius: var(--r); background: none; color: var(--text); font-size: var(--fs-md); text-decoration: none; cursor: pointer; text-align: left; }
.mi:hover { background: var(--surface-3); text-decoration: none; }
.mi .icon { color: var(--text-muted); }
.sep { height: 1px; background: var(--border); margin: 6px 0; }
.theme { padding: 4px 8px 6px; display: flex; flex-direction: column; gap: 6px; }
.tl { font-size: var(--fs-xs); color: var(--text-muted); }
.seg { display: grid; grid-template-columns: repeat(3, 1fr); border: 1px solid var(--border-strong); border-radius: var(--r); overflow: hidden; }
.seg button { display: inline-flex; align-items: center; justify-content: center; gap: 5px; height: 28px; border: 0; background: var(--surface); color: var(--text-2); font-size: var(--fs-sm); cursor: pointer; }
.seg button + button { border-left: 1px solid var(--border-strong); }
.seg button[aria-pressed='true'] { background: var(--accent-soft); color: var(--accent-text); font-weight: var(--fw-medium); }
@media (max-width: 720px) { .who, .chev { display: none; } .trigger { padding: 0 3px; } }
</style>
