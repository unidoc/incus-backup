<script setup lang="ts">
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import { confirmState, settleConfirm } from '@/composables/useConfirm'
</script>

<template>
  <ConfirmDialog
    v-if="confirmState.req"
    :open="confirmState.open"
    :title="confirmState.req.title"
    :message="confirmState.req.message"
    :confirm-label="confirmState.req.confirmLabel"
    :danger="confirmState.req.danger"
    :type-to-confirm="confirmState.req.typeToConfirm"
    @confirm="settleConfirm(true)"
    @cancel="settleConfirm(false)"
  />
</template>
