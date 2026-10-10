<script setup lang="ts">
// <CIcon name="mail" />: an icon of this section's own set (icons.ts); otherwise identical to Icon.vue.
import { computed } from 'vue'
import { cfgIcons, type CfgIconName } from './icons'

const props = withDefaults(defineProps<{ name: CfgIconName; size?: number; stroke?: number; label?: string }>(), { size: 16, stroke: 1.75 })
const inner = computed(() => cfgIcons[props.name] ?? '')
</script>

<template>
  <svg class="icon" xmlns="http://www.w3.org/2000/svg" :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor" :stroke-width="stroke"
    stroke-linecap="round" stroke-linejoin="round" :aria-hidden="label ? undefined : 'true'" :role="label ? 'img' : undefined" :aria-label="label" focusable="false" v-html="inner" />
</template>

<style scoped>
.icon { flex: none; display: inline-block; vertical-align: middle; }
</style>
