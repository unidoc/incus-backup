<script setup lang="ts">
import Icon from '@/components/icons/Icon.vue'
import { toasts, dismiss, type ToastKind } from '@/stores/toast'
import type { IconName } from '@/components/icons'
const icon: Record<ToastKind, IconName> = { success: 'circle-check', error: 'circle-alert', warn: 'triangle-alert', info: 'info' }
</script>

<template>
  <div class="toasts" aria-live="polite" aria-relevant="additions">
    <TransitionGroup name="t">
      <div v-for="t in toasts" :key="t.id" class="toast" :class="`k-${t.kind}`" :role="t.kind === 'error' ? 'alert' : 'status'" data-testid="toast">
        <Icon :name="icon[t.kind]" :size="16" class="ic" />
        <div class="body">
          <p class="title">{{ t.title }}</p>
          <p v-if="t.message" class="msg">{{ t.message }}</p>
        </div>
        <button v-if="t.action" type="button" class="btn btn-sm" @click="t.action.run(); dismiss(t.id)">{{ t.action.label }}</button>
        <button type="button" class="x" aria-label="Dismiss" @click="dismiss(t.id)"><Icon name="x" :size="14" /></button>
      </div>
    </TransitionGroup>
  </div>
</template>

<style scoped>
.toasts { position: fixed; right: 16px; bottom: 16px; z-index: var(--z-toast); display: flex; flex-direction: column; gap: 8px; width: min(400px, calc(100vw - 32px)); pointer-events: none; }
.toast { pointer-events: auto; display: flex; align-items: flex-start; gap: 10px; padding: 10px 8px 10px 12px; background: var(--surface); border: 1px solid var(--border-strong); border-left: 3px solid var(--c); border-radius: var(--r-lg); box-shadow: var(--shadow-pop); }
.ic { color: var(--c); margin-top: 1px; }
.body { flex: 1; min-width: 0; }
.title { font-weight: var(--fw-semibold); font-size: var(--fs-md); }
.msg { color: var(--text-2); font-size: var(--fs-sm); margin-top: 2px; overflow-wrap: anywhere; }
.x { display: grid; place-items: center; width: 24px; height: 24px; border: 0; border-radius: var(--r-sm); background: none; color: var(--text-muted); cursor: pointer; }
.x:hover { background: var(--surface-3); color: var(--text); }
.k-success { --c: var(--ok); } .k-error { --c: var(--critical); } .k-warn { --c: var(--warn); } .k-info { --c: var(--info); }
.t-enter-active, .t-leave-active { transition: opacity var(--t), transform var(--t) var(--ease); }
.t-enter-from, .t-leave-to { opacity: 0; transform: translateY(8px); }
</style>
