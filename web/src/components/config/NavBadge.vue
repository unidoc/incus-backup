<script setup lang="ts">
// The count next to a sidebar entry. Only "Alerts" has one: firing critical + warning alerts
// (acknowledged and silenced ones are being handled and do not count). Red when any is critical.
import { computed } from 'vue'
import { alertCounts, startAlertCounts } from './alertCounts'

const props = defineProps<{ route: string }>()
if (props.route === 'alerts') startAlertCounts()
const n = computed(() => (props.route === 'alerts' ? alertCounts.critical + alertCounts.warning : 0))
const title = computed(() => {
  const parts = [`${alertCounts.critical} critical`, `${alertCounts.warning} warning`]
  if (alertCounts.acknowledged) parts.push(`${alertCounts.acknowledged} acknowledged`)
  if (alertCounts.silenced) parts.push(`${alertCounts.silenced} silenced`)
  return `Firing alerts: ${parts.join(', ')}`
})
</script>

<template>
  <span v-if="n > 0" class="nb" :class="alertCounts.critical ? 'crit' : 'warn'" :title="title" data-testid="alert-badge" :aria-label="title">{{ n > 99 ? '99+' : n }}</span>
</template>

<style scoped>
.nb { margin-left: auto; min-width: 20px; height: 18px; padding: 0 6px; border-radius: 9px; display: inline-grid; place-items: center; font-size: 11px; font-weight: var(--fw-semibold); font-variant-numeric: tabular-nums; line-height: 1; }
.crit { background: var(--critical); color: var(--nav-text-active); }
.warn { background: var(--warn); color: var(--nav-bg); }
:global(.sidebar.rail) .nb { position: absolute; top: 2px; right: 4px; min-width: 16px; height: 15px; padding: 0 4px; font-size: 9.5px; margin: 0; }
@media (max-width: 900px) { :global(.sidebar.rail) .nb { position: static; margin-left: auto; } }
</style>
