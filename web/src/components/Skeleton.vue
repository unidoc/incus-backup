<script setup lang="ts">
// Loading placeholder shaped like the content: <Skeleton :lines="3" /> or <Skeleton width="120px" height="24px" />.
withDefaults(defineProps<{ lines?: number; width?: string; height?: string }>(), { lines: 1, height: '12px' })
</script>

<template>
  <span class="sk-wrap" aria-hidden="true">
    <span v-for="i in lines" :key="i" class="sk" :style="{ width: width ?? (i === lines && lines > 1 ? '60%' : '100%'), height }" />
  </span>
  <span class="sr-only">Loading</span>
</template>

<style scoped>
.sk-wrap { display: flex; flex-direction: column; gap: 8px; width: 100%; }
.sk { display: block; border-radius: var(--r-sm); background: linear-gradient(90deg, var(--skeleton) 0%, var(--skeleton-hi) 50%, var(--skeleton) 100%); background-size: 200% 100%; animation: sk 1.4s ease-in-out infinite; }
@keyframes sk { from { background-position: 100% 0; } to { background-position: -100% 0; } }
</style>
