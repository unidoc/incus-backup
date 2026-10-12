<script setup lang="ts">
// State of the live-update stream. Click reconnects now. Text is visible on wide screens; the dot shape
// differs per state (solid / ring / hollow) so it does not rely on colour alone.
import { computed } from 'vue'
import Tooltip from '@/components/Tooltip.vue'
import { live, reconnectNow } from '@/api/events'
import { formatRelative } from '@/lib/format'
import { useNow } from '@/composables/useNow'

const now = useNow()
const view = computed(() => {
  switch (live.state) {
    case 'live': return { s: 'ok', label: 'Live', tip: 'Live updates are on. Lists refresh by themselves.' }
    case 'connecting': return { s: 'info', label: 'Connecting', tip: 'Connecting to the live update stream.' }
    case 'reconnecting': return { s: 'warn', label: 'Reconnecting', tip: `Connection lost. Retrying in ${Math.ceil(live.retryIn / 1000)} s. Click to retry now.` }
    case 'offline': return { s: 'critical', label: 'Offline', tip: 'The server is not reachable. Data on screen may be out of date. Click to retry now.' }
    default: return { s: 'neutral', label: 'Paused', tip: 'Live updates are off.' }
  }
})
</script>

<template>
  <Tooltip placement="bottom" :focusable="false">
    <button type="button" class="live" :class="`s-${view.s}`" :data-state="live.state" :aria-label="`Live updates: ${view.label}. ${view.tip}`" @click="reconnectNow">
      <span class="dot" aria-hidden="true" /><span class="lbl">{{ view.label }}</span>
    </button>
    <template #content>{{ view.tip }}<template v-if="live.lastEventAt"><br />Last message {{ formatRelative(live.lastEventAt, now) }}.</template></template>
  </Tooltip>
</template>

<style scoped>
.live { display: inline-flex; align-items: center; gap: 7px; height: 28px; padding: 0 10px; border-radius: var(--r-pill); border: 1px solid var(--border); background: var(--surface); color: var(--text-2); font-size: var(--fs-sm); cursor: pointer; }
.live:hover { background: var(--surface-3); }
.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--c); }
.s-ok { --c: var(--ok); }
.s-ok .dot { box-shadow: 0 0 0 3px color-mix(in srgb, var(--ok) 25%, transparent); }
.s-info { --c: var(--info); }
.s-warn { --c: var(--warn); }
.s-warn .dot, .s-info .dot { background: transparent; border: 2px solid var(--c); animation: pulse 1.2s ease-in-out infinite; }
.s-critical { --c: var(--critical); border-color: var(--critical-border); color: var(--critical-text); }
.s-critical .dot { background: transparent; border: 2px solid var(--c); }
.s-neutral { --c: var(--neutral); }
@keyframes pulse { 50% { opacity: .35; } }
@media (max-width: 1100px) { .lbl { display: none; } .live { padding: 0 9px; } }
</style>
