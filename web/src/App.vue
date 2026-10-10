<script setup lang="ts">
import { RouterView, useRoute } from 'vue-router'
import AppShell from '@/components/app/AppShell.vue'
import ToastHost from '@/components/app/ToastHost.vue'
import ConfirmHost from '@/components/app/ConfirmHost.vue'
import { session } from '@/stores/session'

const route = useRoute()
</script>

<template>
  <template v-if="session.status !== 'unknown'">
    <RouterView v-if="route.meta.bare || route.meta.public" />
    <AppShell v-else-if="session.status === 'authenticated'" />
  </template>
  <ToastHost />
  <ConfirmHost />
</template>
