<script setup lang="ts">
// Inline message block inside a page: errors that need reading, warnings, notes.
// For an API error: <Callout status="critical" title="Could not load hosts" :retry="refresh">{{ err.message }}</Callout>
import Icon from '@/components/icons/Icon.vue'
import { statusIcon, type Status } from '@/lib/status'
withDefaults(defineProps<{ status?: Status; title?: string; retry?: () => unknown }>(), { status: 'info' })
</script>

<template>
  <div class="callout" :class="`s-${status}`" :role="status === 'critical' ? 'alert' : 'status'">
    <Icon :name="statusIcon[status]" :size="16" class="ic" />
    <div class="body">
      <p v-if="title" class="t">{{ title }}</p>
      <div v-if="$slots.default" class="d"><slot /></div>
    </div>
    <button v-if="retry" type="button" class="btn btn-sm" @click="retry()"><Icon name="refresh-cw" :size="13" />Retry</button>
    <slot name="actions" />
  </div>
</template>

<style scoped>
.callout { display: flex; gap: 10px; align-items: flex-start; padding: 10px 12px; border: 1px solid var(--b); border-radius: var(--r); background: var(--bg); }
.ic { color: var(--c); margin-top: 2px; }
.body { flex: 1; min-width: 0; }
.t { font-weight: var(--fw-semibold); color: var(--text); }
.d { color: var(--text-2); font-size: var(--fs-sm); margin-top: 2px; overflow-wrap: anywhere; }
.s-ok { --c: var(--ok); --bg: var(--ok-bg); --b: var(--ok-border); }
.s-warn { --c: var(--warn); --bg: var(--warn-bg); --b: var(--warn-border); }
.s-critical { --c: var(--critical); --bg: var(--critical-bg); --b: var(--critical-border); }
.s-info { --c: var(--info); --bg: var(--info-bg); --b: var(--info-border); }
.s-neutral { --c: var(--neutral); --bg: var(--neutral-bg); --b: var(--neutral-border); }
</style>
