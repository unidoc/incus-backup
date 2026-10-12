<script setup lang="ts">
// <StatusPill status="ok" label="Online" />  or  <StatusPill state="partial" /> (server word, mapped).
// Colour is never the only signal: an icon (or dot) plus the text always come with it.
import { computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { statusOf, statusIcon, type Status } from '@/lib/status'

const props = withDefaults(defineProps<{ status?: Status; state?: string | null; label?: string; variant?: 'pill' | 'dot' | 'plain'; size?: 'sm' | 'md' }>(), { variant: 'pill', size: 'md' })
const s = computed<Status>(() => props.status ?? statusOf(props.state))
const text = computed(() => props.label ?? props.state ?? s.value)
</script>

<template>
  <span class="pill" :class="[`s-${s}`, `v-${variant}`, `z-${size}`]" :data-status="s">
    <span v-if="variant === 'dot'" class="dot" aria-hidden="true" />
    <Icon v-else-if="variant === 'pill'" :name="statusIcon[s]" :size="size === 'sm' ? 12 : 13" :stroke="2.2" />
    <span class="t">{{ text }}</span>
  </span>
</template>

<style scoped>
.pill { display: inline-flex; align-items: center; gap: 5px; white-space: nowrap; font-size: var(--fs-sm); font-weight: var(--fw-medium); line-height: 1; vertical-align: middle; }
.v-pill { height: 22px; padding: 0 8px 0 6px; border-radius: var(--r-pill); border: 1px solid var(--b); background: var(--bg); color: var(--fg); }
.v-pill.z-sm { height: 19px; padding: 0 7px 0 5px; font-size: var(--fs-xs); }
.v-dot, .v-plain { color: var(--text-2); }
.v-plain { color: var(--fg); }
.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--c); box-shadow: 0 0 0 2px var(--bg); }
.s-ok { --c: var(--ok); --bg: var(--ok-bg); --b: var(--ok-border); --fg: var(--ok-text); }
.s-warn { --c: var(--warn); --bg: var(--warn-bg); --b: var(--warn-border); --fg: var(--warn-text); }
.s-critical { --c: var(--critical); --bg: var(--critical-bg); --b: var(--critical-border); --fg: var(--critical-text); }
.s-info { --c: var(--info); --bg: var(--info-bg); --b: var(--info-border); --fg: var(--info-text); }
.s-neutral { --c: var(--neutral); --bg: var(--neutral-bg); --b: var(--neutral-border); --fg: var(--neutral-text); }
.v-pill .icon { color: var(--c); }
</style>
