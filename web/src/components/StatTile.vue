<script setup lang="ts">
// A number that matters, with its label, an optional status, a delta and a sparkline slot.
// <StatTile label="Snapshots" :value="1284" hint="+12 today" status="ok" :to="{ name: 'snapshots' }" />
import { computed } from 'vue'
import { RouterLink, type RouteLocationRaw } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import { statusIcon, type Status } from '@/lib/status'
import Skeleton from './Skeleton.vue'

const props = defineProps<{ label: string; value?: string | number | null; unit?: string; hint?: string; status?: Status; to?: RouteLocationRaw; loading?: boolean }>()
const tag = computed(() => (props.to ? RouterLink : 'div'))
</script>

<template>
  <component :is="tag" :to="to" class="tile" :class="[status && `s-${status}`, { link: !!to }]">
    <span class="lbl">
      <Icon v-if="status && status !== 'ok'" :name="statusIcon[status]" :size="13" :stroke="2.2" class="si" />{{ label }}
    </span>
    <span v-if="loading" class="v"><Skeleton width="64px" height="22px" /></span>
    <span v-else class="v num">{{ value ?? '–' }}<small v-if="unit" class="u">{{ unit }}</small></span>
    <span v-if="hint || $slots.hint" class="hint"><slot name="hint">{{ hint }}</slot></span>
    <span v-if="$slots.default" class="extra"><slot /></span>
  </component>
</template>

<style scoped>
.tile { display: flex; flex-direction: column; gap: 4px; min-width: 0; padding: 12px 14px; background: var(--surface); border: 1px solid var(--border); border-radius: var(--r-lg); color: inherit; text-decoration: none; position: relative; }
.tile.link:hover { border-color: var(--border-strong); background: var(--surface-2); text-decoration: none; }
.lbl { display: flex; align-items: center; gap: 5px; font-size: var(--fs-sm); color: var(--text-muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.v { font-size: var(--fs-2xl); font-weight: var(--fw-semibold); line-height: 1.1; letter-spacing: -0.01em; color: var(--text); }
.u { font-size: var(--fs-md); font-weight: var(--fw-medium); color: var(--text-muted); margin-left: 3px; }
.hint { font-size: var(--fs-sm); color: var(--text-muted); }
.extra { margin-top: 4px; }
.s-warn .si { color: var(--warn); } .s-critical .si { color: var(--critical); } .s-info .si { color: var(--info); }
.s-warn { box-shadow: inset 3px 0 0 var(--warn); } .s-critical { box-shadow: inset 3px 0 0 var(--critical); }
</style>
