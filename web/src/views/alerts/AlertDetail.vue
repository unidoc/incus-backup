<script setup lang="ts">
// One alert on its own page: the target of the links in notifications (base_url + /alerts/<id>).
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import AlertDetailPanel from '@/components/config/AlertDetailPanel.vue'

const route = useRoute()
const id = computed(() => Number(route.params.id))
</script>

<template>
  <div class="alert-page">
    <header class="page-head">
      <div>
        <h1>Alert #{{ id }}</h1>
        <p class="lede">State, timeline and every notification sent about it.</p>
      </div>
      <div class="actions"><RouterLink :to="{ name: 'alerts' }" class="btn"><Icon name="chevron-left" :size="14" />All alerts</RouterLink></div>
    </header>
    <section class="panel panel-body"><AlertDetailPanel :id="id" page /></section>
  </div>
</template>

<style scoped>
.alert-page { max-width: 980px; }
</style>
