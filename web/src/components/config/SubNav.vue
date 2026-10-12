<script setup lang="ts">
// Routed sub-navigation of the section: Alerts | Silences, and Channels | Rules | Delivery log | Alert conditions.
import { computed } from 'vue'
import Tabs from '@/components/Tabs.vue'
import { canOpen } from '@/router'

const props = defineProps<{ group: 'alerts' | 'notifications' }>()
const all = {
  alerts: [{ id: 'alerts', label: 'Alerts' }, { id: 'alert-silences', label: 'Silences' }],
  notifications: [
    { id: 'notifications', label: 'Channels' }, { id: 'notification-rules', label: 'Rules' },
    { id: 'notification-deliveries', label: 'Delivery log' }, { id: 'notification-settings', label: 'Alert conditions' },
  ],
}
const tabs = computed(() => all[props.group].filter((t) => canOpen(t.id)).map((t) => ({ ...t, to: { name: t.id } })))
</script>

<template>
  <Tabs :tabs="tabs" :label="group === 'alerts' ? 'Alert pages' : 'Notification pages'" class="cx-subnav" />
</template>

<style scoped>
.cx-subnav { margin-bottom: 16px; }
</style>
