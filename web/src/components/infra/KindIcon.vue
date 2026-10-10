<script setup lang="ts">
// Icon for a storage object's kind (instance record, disk, volume, filesystem), with the kind in the
// accessible name and tooltip; `label` also prints the short kind next to it.
import { computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { kindInfo } from '@/lib/infra'
const props = defineProps<{ kind: string; name?: string; label?: boolean }>()
const k = computed(() => kindInfo(props.kind, props.name ?? ''))
</script>

<template>
  <span class="kind" :title="k.label" :data-kind="kind">
    <Icon :name="k.icon" :size="14" /><span :class="label ? 'kl' : 'sr-only'">{{ label ? k.short : k.label }}</span>
  </span>
</template>

<style scoped>
.kind { display: inline-flex; align-items: center; gap: 5px; color: var(--text-muted); vertical-align: middle; }
.kl { font-size: var(--fs-sm); color: var(--text-2); }
</style>
