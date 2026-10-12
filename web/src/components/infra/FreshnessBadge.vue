<script setup lang="ts">
// Freshness of an object against its schedule: Current / Overdue / Never backed up / No schedule.
// The tooltip says when the next snapshot is due and from when it counts as overdue.
import { computed } from 'vue'
import StatusPill from '@/components/StatusPill.vue'
import Tooltip from '@/components/Tooltip.vue'
import { freshnessStatus, freshnessText } from '@/lib/infra'
import { formatDateTime, formatRelative, describeSchedule } from '@/lib/format'
const props = defineProps<{ freshness: string; due?: string | null; overdueAt?: string | null; schedule?: string; size?: 'sm' | 'md' }>()
const tip = computed(() => {
  switch (props.freshness) {
    case 'ok': return props.due ? `Next snapshot due ${formatDateTime(props.due)} (${formatRelative(props.due)}); overdue after ${formatDateTime(props.overdueAt)}.` : 'Has a current snapshot.'
    case 'stale': return `Overdue since ${formatDateTime(props.overdueAt)}: the last snapshot is older than the schedule${props.schedule ? ` (${describeSchedule(props.schedule)})` : ''} allows.`
    case 'never': return 'No snapshot exists yet.'
    case 'unscheduled': return 'No schedule is known for its policy, so freshness cannot be judged.'
    case 'reference': return 'Instance record: the configuration is stored with each backup; its disks are judged instead.'
    default: return ''
  }
})
</script>

<template>
  <Tooltip :text="tip">
    <StatusPill v-if="freshness !== 'reference'" :status="freshnessStatus[freshness] ?? 'neutral'" :label="freshnessText[freshness] ?? freshness" :size="size ?? 'sm'" :data-freshness="freshness" />
    <span v-else class="muted small" data-freshness="reference">–</span>
  </Tooltip>
</template>
