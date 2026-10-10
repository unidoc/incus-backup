<script setup lang="ts">
// A read-only value with a copy button: tokens, fingerprints, commands. `secret` masks it until revealed
// (still copyable). `once` adds the "shown only now" warning for secrets the server will not show again.
import { ref, computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { useClipboard } from '@/composables/useClipboard'

const props = defineProps<{ value: string; label?: string; secret?: boolean; once?: boolean }>()
const { copy, copied, failed } = useClipboard()
const revealed = ref(!props.secret)
const shown = computed(() => (revealed.value ? props.value : '•'.repeat(Math.min(32, props.value.length))))
</script>

<template>
  <div class="cf">
    <span v-if="label" class="lbl">{{ label }}</span>
    <div class="box">
      <code class="val" data-testid="copy-value" :data-value="revealed ? value : undefined">{{ shown }}</code>
      <button v-if="secret" type="button" class="btn btn-sm btn-ghost btn-icon" :aria-label="revealed ? 'Hide' : 'Show'" @click="revealed = !revealed">
        <Icon :name="revealed ? 'eye-off' : 'eye'" :size="14" />
      </button>
      <button type="button" class="btn btn-sm" @click="copy(value)">
        <Icon :name="copied ? 'check' : 'copy'" :size="14" />{{ copied ? 'Copied' : failed ? 'Copy failed: select and copy' : 'Copy' }}
      </button>
    </div>
    <p v-if="once" class="once"><Icon name="triangle-alert" :size="13" /> Shown only now. Copy it to a safe place: it cannot be displayed again.</p>
  </div>
</template>

<style scoped>
.cf { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.lbl { font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--text-2); }
.box { display: flex; align-items: center; gap: 4px; padding: 3px 3px 3px 10px; border: 1px solid var(--border-strong); border-radius: var(--r); background: var(--code-bg); min-width: 0; }
.val { flex: 1; min-width: 0; overflow-x: auto; white-space: nowrap; font-size: 12.5px; padding: 4px 0; user-select: all; scrollbar-width: thin; }
.once { display: flex; gap: 6px; align-items: center; font-size: var(--fs-sm); color: var(--warn-text); }
</style>
