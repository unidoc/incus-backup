<script setup lang="ts">
// Catches render errors of the page below it so one broken view never blanks the whole console.
// The error is shown in place with a way out; navigation resets it.
import { ref, onErrorCaptured, watch } from 'vue'
import { useRoute } from 'vue-router'
import Callout from '@/components/Callout.vue'

const err = ref<Error | null>(null)
const route = useRoute()
const reload = () => window.location.reload()
watch(() => route.fullPath, () => { err.value = null })
onErrorCaptured((e) => {
  err.value = e instanceof Error ? e : new Error(String(e))
  console.error(e)
  return false
})
</script>

<template>
  <div v-if="err" class="eb" data-testid="error-boundary">
    <Callout status="critical" title="This page failed to display">
      {{ err.message }}. The rest of the console still works. Reload the page; if it happens again, report it with the steps that led here.
      <template #actions>
        <button type="button" class="btn btn-sm" @click="err = null">Try again</button>
        <button type="button" class="btn btn-sm" @click="reload">Reload</button>
      </template>
    </Callout>
  </div>
  <slot v-else />
</template>

<style scoped>
.eb { max-width: 720px; }
</style>
