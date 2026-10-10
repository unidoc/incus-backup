<script setup lang="ts">
// What to show when a list is empty: say what is missing and the next step (one action).
import Icon from '@/components/icons/Icon.vue'
import type { IconName } from '@/components/icons'
withDefaults(defineProps<{ title: string; icon?: IconName; compact?: boolean }>(), { icon: 'circle-dashed' })
</script>

<template>
  <div class="empty" :class="{ compact }">
    <span class="ic"><Icon :name="icon" :size="compact ? 18 : 22" /></span>
    <p class="t">{{ title }}</p>
    <p v-if="$slots.default" class="d"><slot /></p>
    <div v-if="$slots.actions" class="a"><slot name="actions" /></div>
  </div>
</template>

<style scoped>
.empty { display: flex; flex-direction: column; align-items: center; text-align: center; gap: 6px; padding: 40px 24px; color: var(--text-muted); }
.empty.compact { padding: 20px 16px; }
.ic { display: grid; place-items: center; width: 40px; height: 40px; border-radius: 50%; background: var(--surface-3); color: var(--text-muted); margin-bottom: 4px; }
.compact .ic { width: 32px; height: 32px; }
.t { color: var(--text); font-weight: var(--fw-semibold); }
.d { max-width: 52ch; font-size: var(--fs-sm); }
.a { margin-top: 8px; display: flex; gap: 8px; }
</style>
