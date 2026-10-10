<script setup lang="ts">
// A duration from milliseconds, seconds or Go nanoseconds: <DurationText :ns="job.duration" />.
import { computed } from 'vue'
import { formatDuration } from '@/lib/format'
const props = defineProps<{ ms?: number | null; s?: number | null; ns?: number | null; units?: number }>()
const ms = computed(() => props.ms ?? (props.s !== undefined && props.s !== null ? props.s * 1000 : props.ns !== undefined && props.ns !== null ? props.ns / 1e6 : null))
</script>

<template><span class="num nowrap">{{ formatDuration(ms, { units }) }}</span></template>
