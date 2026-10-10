<script setup lang="ts">
// "3 min ago" that keeps itself current; the tooltip gives the exact local and UTC time and says which
// time zone the console shows (the browser's). Go's zero time or null renders `never`.
import { computed } from 'vue'
import Tooltip from './Tooltip.vue'
import { useNow } from '@/composables/useNow'
import { formatRelative, formatDateTime, toDate, localZone } from '@/lib/format'

const props = withDefaults(defineProps<{ time?: string | number | Date | null; never?: string }>(), { never: 'never' })
const now = useNow()
const d = computed(() => toDate(props.time ?? null))
const text = computed(() => formatRelative(d.value, now.value, props.never))
const iso = computed(() => d.value?.toISOString() ?? '')
</script>

<template>
  <span v-if="!d" class="rt muted">{{ never }}</span>
  <Tooltip v-else>
    <time class="rt" :datetime="iso">{{ text }}</time>
    <template #content>
      <span class="num">{{ formatDateTime(d, { seconds: true }) }}</span> {{ localZone() }}<br />
      <span class="num">{{ formatDateTime(d, { seconds: true, tz: 'UTC' }) }}</span> UTC<br />
      <span class="note">Times are shown in your browser's time zone. Schedules run in the policy's time zone.</span>
    </template>
  </Tooltip>
</template>

<style scoped>
.rt { white-space: nowrap; font-variant-numeric: tabular-nums; text-decoration: underline dotted var(--text-faint); text-underline-offset: 3px; }
.rt.muted { text-decoration: none; }
.note { opacity: .75; }
</style>
