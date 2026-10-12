<script setup lang="ts">
// Label/value list for detail pages. Pass `items` or use the default slot with <dt>/<dd> pairs.
// Values can be any slot: <template #value-mode>…</template> for item key "mode".
defineProps<{ items?: Array<{ key: string; label: string; value?: string | number | null; mono?: boolean }>; columns?: 1 | 2 }>()
</script>

<template>
  <dl class="kv" :class="`c${columns ?? 1}`">
    <template v-for="it in items ?? []" :key="it.key">
      <div class="pair">
        <dt>{{ it.label }}</dt>
        <dd :class="{ mono: it.mono }"><slot :name="`value-${it.key}`" :item="it">{{ it.value === null || it.value === undefined || it.value === '' ? '–' : it.value }}</slot></dd>
      </div>
    </template>
    <slot />
  </dl>
</template>

<style scoped>
.kv { margin: 0; display: grid; gap: 0; }
.kv.c2 { grid-template-columns: 1fr 1fr; column-gap: 32px; }
.pair { display: grid; grid-template-columns: minmax(120px, 38%) 1fr; gap: 12px; padding: 7px 0; border-bottom: 1px solid var(--border); align-items: baseline; }
.pair:last-child, .c2 .pair:nth-last-child(-n + 2) { border-bottom: 0; }
dt { color: var(--text-muted); font-size: var(--fs-sm); }
dd { margin: 0; min-width: 0; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
@media (max-width: 720px) { .kv.c2 { grid-template-columns: 1fr; } .c2 .pair:nth-last-child(2) { border-bottom: 1px solid var(--border); } }
</style>
