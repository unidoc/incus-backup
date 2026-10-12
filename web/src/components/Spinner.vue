<script setup lang="ts">
withDefaults(defineProps<{ size?: number; label?: string }>(), { size: 14, label: 'Loading' })
</script>
<template>
  <span class="spin" role="status" :aria-label="label" :style="{ width: `${size}px`, height: `${size}px` }" />
</template>
<style scoped>
.spin { display: inline-block; border-radius: 50%; border: 2px solid var(--border-strong); border-top-color: var(--accent); animation: r .8s linear infinite; flex: none; vertical-align: middle; }
@keyframes r { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) { .spin { animation-duration: 2.4s !important; animation-iteration-count: infinite !important; } }
</style>
