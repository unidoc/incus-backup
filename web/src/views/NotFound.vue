<script setup lang="ts">
import { RouterLink, useRoute } from 'vue-router'
import EmptyState from '@/components/EmptyState.vue'
const route = useRoute()
</script>

<template>
  <EmptyState icon="circle-help" title="Page not found" data-testid="not-found">
    There is no page at <code>{{ route.fullPath }}</code>. The link may be old, or the item was removed. Use the search (<kbd>Ctrl</kbd> <kbd>K</kbd>) to find it.
    <template #actions><RouterLink :to="{ name: 'dashboard' }" class="btn btn-primary">Go to the overview</RouterLink></template>
  </EmptyState>
</template>
