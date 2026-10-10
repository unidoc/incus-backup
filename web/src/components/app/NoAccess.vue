<script setup lang="ts">
import { RouterLink } from 'vue-router'
import EmptyState from '@/components/EmptyState.vue'
import { session } from '@/stores/session'
defineProps<{ need: string; title?: string }>()
</script>

<template>
  <EmptyState icon="lock" :title="`${title ?? 'This page'} needs the ${need} role`" data-testid="no-access">
    You are signed in as {{ session.name }} with the {{ session.role }} role. Ask an administrator to change your role if you need this page.
    <template #actions><RouterLink :to="{ name: 'dashboard' }" class="btn">Go to the overview</RouterLink></template>
  </EmptyState>
</template>
