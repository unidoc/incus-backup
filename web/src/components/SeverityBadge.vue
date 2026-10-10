<script setup lang="ts">
// Severity of a problem or alert: a compact square-cornered tag ("CRITICAL"), distinct from state pills.
import { computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { statusIcon, type Status } from '@/lib/status'

const props = defineProps<{ severity: string }>()
const s = computed<Status>(() => {
  const v = props.severity.toLowerCase()
  if (v === 'critical' || v === 'error') return 'critical'
  if (v === 'warn' || v === 'warning') return 'warn'
  if (v === 'info') return 'info'
  if (v === 'ok') return 'ok'
  return 'neutral'
})
const label = computed(() => ({ critical: 'Critical', warn: 'Warning', info: 'Info', ok: 'Resolved', neutral: props.severity })[s.value])
</script>

<template>
  <span class="sev" :class="`s-${s}`" :data-severity="s"><Icon :name="statusIcon[s]" :size="12" :stroke="2.4" />{{ label }}</span>
</template>

<style scoped>
.sev { display: inline-flex; align-items: center; gap: 4px; height: 20px; padding: 0 6px; border-radius: var(--r-sm); font-size: var(--fs-xs); font-weight: var(--fw-semibold); letter-spacing: .03em; text-transform: uppercase; white-space: nowrap; color: var(--fg); background: var(--bg); box-shadow: inset 3px 0 0 var(--c); padding-left: 8px; }
.s-ok { --c: var(--ok); --bg: var(--ok-bg); --fg: var(--ok-text); }
.s-warn { --c: var(--warn); --bg: var(--warn-bg); --fg: var(--warn-text); }
.s-critical { --c: var(--critical); --bg: var(--critical-bg); --fg: var(--critical-text); }
.s-info { --c: var(--info); --bg: var(--info-bg); --fg: var(--info-text); }
.s-neutral { --c: var(--neutral); --bg: var(--neutral-bg); --fg: var(--neutral-text); }
</style>
