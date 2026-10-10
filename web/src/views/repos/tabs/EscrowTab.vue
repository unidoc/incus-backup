<script setup lang="ts">
// Key escrow: a host may store its master key on the server WRAPPED to an offline public key
// (incus-backup key escrow upload). The server cannot open it. Replacing it is deliberately an
// administrator action: clear it here, then upload the new one from the host.
import { ref } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import Callout from '@/components/Callout.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import Spinner from '@/components/Spinner.vue'
import KeyValue from '@/components/KeyValue.vue'
import type { RepoApi } from '@/api/sections/storage'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { RepoView } from '@/api/types'
import { useResource } from '@/api/cache'

const props = defineProps<{ repo: RepoView; api: RepoApi }>()
const info = useResource(() => `repo-info:${props.repo.name}`, (s) => props.api.info(s), { ttl: 60_000 })
const busy = ref(false)
const done = ref(false)
async function clear() {
  if (!(await confirm({ title: `Clear the key escrow of ${props.repo.name}?`, danger: true, typeToConfirm: props.repo.name, confirmLabel: 'Clear escrow',
    message: 'The wrapped master key stored for this repository\'s key domain is deleted. Until a host uploads a new one, recovering with the offline escrow private key is impossible; the recovery key (IBR1-…) still works.' }))) return
  busy.value = true
  try {
    await props.api.clearEscrow()
    done.value = true
    toast.success('Escrow cleared', 'Upload the new wrapped key from a host now.')
  } catch (e) { toast.error(e, 'Could not clear the escrow') } finally { busy.value = false }
}
</script>

<template>
  <section class="panel">
    <div class="panel-head"><h2>Key escrow</h2><span class="sub">admin</span></div>
    <div class="panel-body stack">
      <p class="txt">Optional and recommended: a host stores its master key on every server <b>wrapped to an offline public key</b>. The server cannot open it; with the offline private key (IBE1-…) a new host can recover without the recovery key. One escrow blob exists per key domain.</p>
      <KeyValue :items="[{ key: 'domain', label: 'Key domain', value: info.data.value?.domain_id || 'not pinned yet: no backup committed, so there is no escrow to clear', mono: !!info.data.value?.domain_id }]" />
      <p class="txt">This console cannot show whether a blob is stored (only hosts read it, through the agent API). Clearing when none exists does nothing.</p>
      <Callout status="info" title="Replace the escrow key">
        Clear it here, then on one host of this key domain run:
        <CodeBlock code="incus-backup key escrow upload --public IBP1-…" prompt />
      </Callout>
      <Callout v-if="done" status="ok" title="Cleared">The escrow blob of domain {{ info.data.value?.domain_id }} was removed.</Callout>
      <div class="form-actions">
        <button type="button" class="btn btn-danger" :disabled="busy || repo.mode !== 'online' || !info.data.value?.domain_id" :title="repo.mode !== 'online' ? `The repository is ${repo.mode}` : ''" data-testid="escrow-clear" @click="clear">
          <Spinner v-if="busy" :size="13" /><Icon v-else name="trash-2" />Clear escrow</button>
      </div>
    </div>
  </section>
</template>

<style scoped>
.txt { color: var(--text-2); max-width: 80ch; }
</style>
