<script setup lang="ts">
// Shell banner when the opt-in update check found a newer release. Dismissed per version (remembered in
// this browser only); nothing shows while checks are off.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import { useResource } from '@/api/cache'
import { opsApi } from '@/api/sections/ops'

const KEY = 'ib.update.dismissed'
const u = useResource(() => 'system:update', (s) => opsApi.system.update(s), { ttl: 3_600_000, poll: 3_600_000 })
const dismissed = ref('')
try { dismissed.value = localStorage.getItem(KEY) ?? '' } catch { /* storage blocked */ }
const show = computed(() => !!u.data.value?.update_available && !!u.data.value.latest && dismissed.value !== u.data.value.latest)
function dismiss() {
  dismissed.value = u.data.value?.latest ?? ''
  try { localStorage.setItem(KEY, dismissed.value) } catch { /* storage blocked */ }
}
</script>

<template>
  <div v-if="show && u.data.value" class="ub" role="status" data-testid="update-banner">
    <Icon name="info" :size="15" />
    <span>incus-backup <b>{{ u.data.value.latest }}</b> is available (this server runs {{ u.data.value.current }}).</span>
    <a v-if="u.data.value.release_url" :href="u.data.value.release_url" target="_blank" rel="noopener noreferrer">Release notes</a>
    <RouterLink :to="{ name: 'system-status' }">Server status</RouterLink>
    <button type="button" class="btn btn-ghost btn-icon btn-sm x" aria-label="Dismiss until the next release" title="Dismiss until the next release" @click="dismiss"><Icon name="x" :size="14" /></button>
  </div>
</template>

<style scoped>
.ub { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 12px; padding: 8px 12px; margin-bottom: var(--s-4); border: 1px solid var(--info-border); background: var(--info-bg); color: var(--info-text); border-radius: var(--r-lg); font-size: var(--fs-sm); }
.x { margin-left: auto; }
</style>
