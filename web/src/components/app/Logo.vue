<script setup lang="ts">
// Product mark: three stacked bars (the repository) with a status dot. Inline so it themes with currentColor.
withDefaults(defineProps<{ size?: number }>(), { size: 22 })
</script>
<template>
  <svg :width="size" :height="size" viewBox="0 0 24 24" aria-hidden="true" class="logo">
    <rect x="1" y="1" width="22" height="22" rx="5" class="bg" />
    <path d="M6 8h12M6 12h12M6 16h7" class="bars" />
    <circle cx="17.2" cy="16" r="2.1" class="dot" />
  </svg>
</template>
<style scoped>
.bg { fill: var(--nav-marker); opacity: .18; }
.bars { stroke: var(--nav-text-active); stroke-width: 2; stroke-linecap: round; }
.dot { fill: var(--ok); }
</style>
