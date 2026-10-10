<script setup lang="ts">
// <Icon name="server" />: 16 px, currentColor, 1.75 stroke. Decorative (aria-hidden) unless `label` is set.
import { computed } from 'vue'
import { icons, type IconName } from './index'

const props = withDefaults(defineProps<{ name: IconName; size?: number; stroke?: number; label?: string }>(), { size: 16, stroke: 1.75 })
const inner = computed(() => icons[props.name] ?? '')
</script>

<template>
  <svg
    class="icon"
    xmlns="http://www.w3.org/2000/svg"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    :stroke-width="stroke"
    stroke-linecap="round"
    stroke-linejoin="round"
    :aria-hidden="label ? undefined : 'true'"
    :role="label ? 'img' : undefined"
    :aria-label="label"
    focusable="false"
    v-html="inner"
  />
</template>

<style scoped>
.icon { display: inline-block; vertical-align: middle; flex: none; }
</style>
