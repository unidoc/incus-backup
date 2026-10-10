<script setup lang="ts">
// Shown 5 minutes before the server ends an idle session. "Stay signed in" makes one request (renews it).
import { computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { session, stayActive, IDLE_TIMEOUT_MS } from '@/stores/session'
import { useNow } from '@/composables/useNow'
import { toast } from '@/stores/toast'

const now = useNow()
const left = computed(() => Math.max(0, Math.ceil((session.lastActivity + IDLE_TIMEOUT_MS - now.value) / 60_000)))
async function stay() {
  try { await stayActive() } catch (e) { toast.error(e, 'Could not keep the session') }
}
</script>

<template>
  <div v-if="session.idleWarning" class="idle" role="alertdialog" aria-live="assertive" aria-label="Session about to end" data-testid="idle-warning">
    <Icon name="clock" :size="16" />
    <span>You have been inactive. You will be signed out in about {{ left }} min.</span>
    <button type="button" class="btn btn-sm btn-primary" @click="stay">Stay signed in</button>
  </div>
</template>

<style scoped>
.idle { position: fixed; left: 50%; bottom: 20px; transform: translateX(-50%); z-index: var(--z-toast); display: flex; align-items: center; gap: 12px; padding: 10px 12px 10px 14px; max-width: calc(100vw - 24px); background: var(--surface); border: 1px solid var(--warn-border); border-left: 3px solid var(--warn); border-radius: var(--r-lg); box-shadow: var(--shadow-pop); font-size: var(--fs-sm); }
.idle .icon { color: var(--warn-text); }
</style>
