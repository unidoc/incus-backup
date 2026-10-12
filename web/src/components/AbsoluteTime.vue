<script setup lang="ts">
// "2026-10-08 14:03" in the browser's zone; tooltip with UTC and the relative time.
import { computed } from 'vue'
import Tooltip from './Tooltip.vue'
import { useNow } from '@/composables/useNow'
import { formatRelative, formatDateTime, toDate, localZone } from '@/lib/format'

const props = withDefaults(defineProps<{ time?: string | number | Date | null; seconds?: boolean; never?: string }>(), { never: '–' })
const now = useNow()
const d = computed(() => toDate(props.time ?? null))
</script>

<template>
  <span v-if="!d" class="muted">{{ never }}</span>
  <Tooltip v-else>
    <time class="at num" :datetime="d.toISOString()">{{ formatDateTime(d, { seconds }) }}</time>
    <template #content>
      {{ formatRelative(d, now) }}<br />
      <span class="num">{{ formatDateTime(d, { seconds: true, tz: 'UTC' }) }}</span> UTC · shown in {{ localZone() }}
    </template>
  </Tooltip>
</template>

<style scoped>
.at { white-space: nowrap; }
</style>
