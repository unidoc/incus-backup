<script setup lang="ts">
// The verification state of a snapshot as one pill: verified / stale / failed / never verified.
import { computed } from 'vue'
import StatusPill from '@/components/StatusPill.vue'
import Tooltip from '@/components/Tooltip.vue'
import { formatDateTime, formatRelative } from '@/lib/format'
import type { VerifyClass } from '@/lib/protect/snapshots'

const props = defineProps<{ state: VerifyClass; at?: string | null; size?: 'sm' | 'md' }>()
const view = computed(() => ({
  ok: { status: 'ok' as const, label: 'verified', tip: 'The last verification read every chunk and found nothing wrong.' },
  stale: { status: 'warn' as const, label: 'stale', tip: 'Verified once, but longer ago than the policy\'s re-verify age. The next scheduled verification picks it up.' },
  failed: { status: 'critical' as const, label: 'failed', tip: 'The last verification found a damaged or missing chunk. This snapshot is never used as a base for new backups.' },
  never: { status: 'neutral' as const, label: 'never verified', tip: 'No verification has read this snapshot yet.' },
})[props.state])
</script>

<template>
  <Tooltip>
    <StatusPill :status="view.status" :label="view.label" :size="size ?? 'sm'" :data-verify="state" />
    <template #content>
      {{ view.tip }}
      <template v-if="at"><br />Last verified {{ formatRelative(at) }} ({{ formatDateTime(at) }}).</template>
    </template>
  </Tooltip>
</template>
