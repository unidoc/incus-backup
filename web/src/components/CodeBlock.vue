<script setup lang="ts">
// Monospace block (commands, config, logs) with a copy button. `prompt` prefixes each line with "$ ".
import { computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { useClipboard } from '@/composables/useClipboard'

const props = defineProps<{ code: string; prompt?: boolean; label?: string; wrap?: boolean }>()
const { copy, copied } = useClipboard()
const lines = computed(() => props.code.replace(/\n$/, '').split('\n'))
</script>

<template>
  <figure class="code" :class="{ wrap }">
    <figcaption v-if="label" class="cap"><span>{{ label }}</span>
      <button type="button" class="btn btn-sm btn-ghost" :aria-label="copied ? 'Copied' : 'Copy to clipboard'" @click="copy(code)"><Icon :name="copied ? 'check' : 'copy'" :size="14" /><span>{{ copied ? 'Copied' : 'Copy' }}</span></button>
    </figcaption>
    <pre><code><span v-for="(l, i) in lines" :key="i" class="ln"><span v-if="prompt" class="p" aria-hidden="true">$ </span>{{ l }}
</span></code></pre>
    <button v-if="!label" type="button" class="btn btn-sm btn-ghost cp" :aria-label="copied ? 'Copied' : 'Copy to clipboard'" @click="copy(code)">
      <Icon :name="copied ? 'check' : 'copy'" :size="14" /><span>{{ copied ? 'Copied' : 'Copy' }}</span>
    </button>
  </figure>
</template>

<style scoped>
.code { position: relative; margin: 0; border: 1px solid var(--border); border-radius: var(--r); background: var(--code-bg); }
.cap { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 2px 4px 2px 12px; border-bottom: 1px solid var(--border); font-size: var(--fs-xs); color: var(--text-muted); font-weight: var(--fw-medium); }
pre { margin: 0; padding: 10px 12px; overflow-x: auto; font: 12.5px/1.6 var(--font-mono); color: var(--text); }
.wrap pre { white-space: pre-wrap; word-break: break-all; }
.ln { display: block; }
.p { color: var(--text-faint); user-select: none; }
.code:not(:has(.cap)) pre { padding-right: 84px; }
.cp { position: absolute; right: 6px; top: 50%; transform: translateY(-50%); background: var(--code-bg); }
</style>
