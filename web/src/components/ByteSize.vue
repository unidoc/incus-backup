<script setup lang="ts">
// 1.5 GiB with the exact byte count in the title.
import { computed } from 'vue'
import { formatBytes, formatNumber } from '@/lib/format'
const props = defineProps<{ bytes?: number | null; signed?: boolean }>()
const exact = computed(() => (props.bytes === null || props.bytes === undefined ? '' : `${formatNumber(props.bytes)} bytes`))
</script>

<template><span class="num nowrap" :title="exact">{{ formatBytes(bytes, { signed }) }}</span></template>
