<script setup lang="ts">
// The frame around every signed-in page: sidebar, top bar, content, global hosts (toasts, confirm,
// idle warning). Pages above the user's role render a notice instead (the server would refuse anyway).
import { computed, ref, watch, onMounted } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import Sidebar from './Sidebar.vue'
import TopBar from './TopBar.vue'
import IdleWarning from './IdleWarning.vue'
import NoAccess from './NoAccess.vue'
import ErrorBoundary from './ErrorBoundary.vue'
import UpdateBanner from '@/components/ops/UpdateBanner.vue'
import Callout from '@/components/Callout.vue'
import { session, hasRole, logout } from '@/stores/session'
import { repos, repoNone, loadRepos, resetRepos } from '@/stores/repos'
import { loadInstance } from '@/stores/instance'
import { startLive, stopLive } from '@/api/events'
import { api } from '@/api/endpoints'
import { ApiError } from '@/api/client'
import { RouterLink } from 'vue-router'

const route = useRoute()
const router = useRouter()
const RAIL_KEY = 'ib.sidebar.rail'
const rail = ref(false)
try { rail.value = localStorage.getItem(RAIL_KEY) === '1' } catch { /* storage blocked */ }
const mobileOpen = ref(false)
function toggleRail() {
  rail.value = !rail.value
  try { localStorage.setItem(RAIL_KEY, rail.value ? '1' : '0') } catch { /* storage blocked */ }
}
watch(() => route.fullPath, () => { mobileOpen.value = false })

const allowed = computed(() => !route.meta.role || hasRole(route.meta.role))
// Pages that need a repository say so instead of failing when none exists yet.
const needsRepo = computed(() => repoNone.value && !['dashboard', 'repositories', 'repository-new', 'profile', 'users', 'tokens', 'settings', 'audit', 'certificates', 'notifications', 'notification-rules', 'styleguide', 'not-found', 'alerts', 'alert-silences', 'jobs', 'job', 'tasks', 'system-status', 'server-logs'].includes(String(route.name)))

onMounted(() => {
  void loadRepos()
  void loadInstance()
  startLive({
    checkSession: async () => {
      try { await api.session.get(); return true } catch (e) { return !(e instanceof ApiError && e.status === 401) }
    },
  })
})

async function signOut() {
  stopLive()
  await logout()
  resetRepos()
  await router.push({ name: 'login' })
}
</script>

<template>
  <div class="shell" :class="{ rail }">
    <a href="#main" class="skip">Skip to content</a>
    <Sidebar :rail="rail" :mobile-open="mobileOpen" @toggle-rail="toggleRail" @close="mobileOpen = false" />
    <div v-if="mobileOpen" class="scrim" aria-hidden="true" @click="mobileOpen = false" />
    <div class="main-col">
      <TopBar @menu="mobileOpen = true" @sign-out="signOut" />
      <main id="main" class="content" tabindex="-1">
        <UpdateBanner />
        <Callout v-if="repos.error" status="critical" title="Could not load the repositories" class="top-note" :retry="loadRepos">{{ repos.error }}</Callout>
        <ErrorBoundary>
          <NoAccess v-if="!allowed" :need="String(route.meta.role)" :title="route.meta.title" />
          <div v-else-if="needsRepo" class="norepo">
            <Callout status="info" title="No repository yet">
              This page shows data of a repository, and none exists yet.
              <template v-if="hasRole('admin')"> <RouterLink :to="{ name: 'repository-new' }">Add a repository</RouterLink> first.</template>
              <template v-else> Ask an administrator to add one.</template>
            </Callout>
          </div>
          <RouterView v-else v-slot="{ Component, route: r }">
            <component :is="Component" :key="r.name === 'policy' || r.name === 'policy-new' ? r.fullPath : undefined" />
          </RouterView>
        </ErrorBoundary>
      </main>
    </div>
    <IdleWarning />
  </div>
  <span hidden data-testid="session-role">{{ session.role }}</span>
</template>

<style scoped>
.shell { display: flex; min-height: 100dvh; }
.main-col { flex: 1; min-width: 0; overflow-x: clip; display: flex; flex-direction: column; }
.content { flex: 1; width: 100%; max-width: var(--content-max); padding: 20px var(--gutter) 48px; outline: none; }
.top-note { margin-bottom: 16px; }
.norepo { max-width: 640px; }
.scrim { position: fixed; inset: 0; background: var(--scrim); z-index: calc(var(--z-drawer) - 1); }
.skip { position: absolute; left: 8px; top: -40px; z-index: 200; padding: 6px 10px; background: var(--accent); color: var(--on-accent); border-radius: var(--r); }
.skip:focus { top: 8px; }
</style>
