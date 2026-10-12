<script setup lang="ts">
// The delivery log: every message the server queued, exactly as rendered, with its state, attempts and
// the receiver's last error. Dead-lettered messages can be put back in the queue.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import CIcon from '@/components/config/CIcon.vue'
import SubNav from '@/components/config/SubNav.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import StatTile from '@/components/StatTile.vue'
import Modal from '@/components/Modal.vue'
import KeyValue from '@/components/KeyValue.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import { useResource } from '@/api/cache'
import { cfg, list, canCfg, type Delivery } from '@/api/sections/config'
import { session } from '@/stores/session'
import { toast } from '@/stores/toast'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const state = ref('')
const channel = ref('')
const key = computed(() => `cfg:deliveries:${state.value}:${channel.value}`)
const dl = useResource(() => key.value, (s) => cfg.deliveries.list({ state: state.value || undefined, channel: channel.value || undefined, limit: 1000 }, s), { refreshOn: ['alert'], ttl: 5000, poll: 30_000 })
const all = useResource(() => 'cfg:deliveries:recent', (s) => cfg.deliveries.list({ limit: 1000 }, s), { refreshOn: ['alert'], ttl: 10_000 })
const channels = useResource(() => 'cfg:channels', (s) => cfg.channels.list(s))
const canRetry = computed(() => canCfg(session.role, 'POST /notifications/deliveries/{id}/retry'))

const counts = computed(() => {
  const c = { sent: 0, pending: 0, dead: 0, retrying: 0 }
  for (const d of list(all.data.value)) {
    if (d.state === 'sent') c.sent++
    else if (d.state === 'dead') c.dead++
    else { c.pending++; if (d.attempts > 0) c.retrying++ }
  }
  return c
})
const stateWord = (d: Delivery) => (d.state === 'dead' ? 'failed' : d.state === 'sent' ? 'ok' : d.attempts > 0 ? 'pending' : 'queued')
const stateLabel = (d: Delivery) => (d.state === 'dead' ? 'dead' : d.state === 'sent' ? 'sent' : d.state === 'sending' ? 'sending' : d.attempts > 0 ? 'retrying' : 'queued')
const cols: ColumnDef<Delivery>[] = [
  { key: 'state', label: 'State', width: '110px', format: stateLabel },
  { key: 'kind', label: 'Kind', width: '100px' },
  { key: 'subject', label: 'Message', required: true },
  { key: 'channel_name', label: 'Channel', width: '140px' },
  { key: 'attempts', label: 'Attempts', align: 'right', width: '84px' },
  { key: 'last_error', label: 'Last error' },
  { key: 'created', label: 'Queued', width: '120px' },
  { key: 'sent_at', label: 'Sent', width: '120px', hidden: true },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '90px', required: true },
]
const stateOpts = [{ value: '', label: 'Every state' }, { value: 'pending', label: 'Queued or retrying' }, { value: 'sent', label: 'Sent' }, { value: 'dead', label: 'Dead (gave up)' }]
const chOpts = computed(() => [{ value: '', label: 'Every channel' }, ...list(channels.data.value).map((c) => ({ value: c.id, label: c.name }))])
const open = ref<Delivery | null>(null)
async function retry(d: Delivery) {
  try { await cfg.deliveries.retry(d.id); toast.success('Message queued again', `It goes out to ${d.channel_name} within a minute.`); void dl.refresh(); void all.refresh() } catch (e) { toast.error(e, 'Could not queue the message again') }
}
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Delivery log</h1>
        <p class="lede">Every message as it was sent. Failures retry with growing pauses; after the last attempt a message is dead and waits for you here.</p>
      </div>
    </header>
    <SubNav group="notifications" />

    <div class="cx-tiles">
      <StatTile label="Sent" :value="counts.sent" :loading="all.loading.value" />
      <StatTile label="Queued or retrying" :value="counts.pending" :hint="counts.retrying ? `${counts.retrying} after a failure` : ''" :status="counts.retrying ? 'warn' : undefined" :loading="all.loading.value" />
      <StatTile label="Dead" :value="counts.dead" :status="counts.dead ? 'critical' : 'ok'" hint="gave up; retry them" :loading="all.loading.value" data-testid="tile-dead" />
    </div>

    <DataTable :rows="list(dl.data.value)" :columns="cols" row-key="id" :loading="dl.loading.value" :error="dl.error.value" :retry="dl.refresh" storage-key="cfg-deliveries" csv-name="deliveries" row-clickable data-testid="delivery-table" @row-click="(d: Delivery) => (open = d)">
      <template #toolbar>
        <SelectInput v-model="state" :options="stateOpts" size="sm" aria-label="State" />
        <SelectInput v-model="channel" :options="chOpts" size="sm" aria-label="Channel" />
      </template>
      <template #cell-state="{ row }"><StatusPill :state="stateWord(row)" :label="stateLabel(row)" size="sm" /></template>
      <template #cell-kind="{ row }"><span class="cx-tag">{{ row.kind }}</span></template>
      <template #cell-subject="{ row }"><span class="sj" :title="row.subject">{{ row.subject }}</span></template>
      <template #cell-attempts="{ row }"><span class="num">{{ row.attempts }}</span></template>
      <template #cell-last_error="{ row }"><span v-if="row.last_error" class="cx-err er" :title="row.last_error">{{ row.last_error }}</span><span v-else class="muted">–</span></template>
      <template #cell-created="{ row }"><RelativeTime :time="row.created" /></template>
      <template #cell-sent_at="{ row }"><RelativeTime :time="row.sent_at" never="–" /></template>
      <template #cell-actions="{ row }">
        <button v-if="row.state === 'dead'" type="button" class="btn btn-sm" :disabled="!canRetry" :title="canRetry ? 'Put it back in the queue' : 'Needs the admin role'" data-testid="delivery-retry" @click.stop="retry(row)"><CIcon name="rotate-ccw" :size="13" />Retry</button>
      </template>
      <template #empty><EmptyState title="Nothing was sent yet" icon="send">Messages appear here when a rule matches an alert, or when you test a channel.</EmptyState></template>
    </DataTable>

    <Modal :open="!!open" drawer :title="open?.subject ?? ''" width="620px" @update:open="(v: boolean) => { if (!v) open = null }">
      <div v-if="open" class="cx-form" data-testid="delivery-detail">
        <KeyValue :columns="2" :items="[
          { key: 'state', label: 'State', value: stateLabel(open) },
          { key: 'channel', label: 'Channel', value: open.channel_name },
          { key: 'kind', label: 'Kind', value: open.kind },
          { key: 'attempts', label: 'Attempts', value: open.attempts },
          { key: 'created', label: 'Queued', value: open.created },
          { key: 'sent', label: 'Sent', value: open.sent_at },
          { key: 'next', label: 'Next attempt', value: open.next_attempt_at },
          { key: 'alert', label: 'Alert', value: open.alert_id },
        ]">
          <template #value-created><AbsoluteTime :time="open.created" seconds /></template>
          <template #value-sent><AbsoluteTime :time="open.sent_at" seconds /></template>
          <template #value-next><AbsoluteTime v-if="open.state === 'pending'" :time="open.next_attempt_at" seconds /><span v-else>–</span></template>
          <template #value-alert><RouterLink v-if="open.alert_id" :to="{ name: 'alert', params: { id: open.alert_id } }">#{{ open.alert_id }}</RouterLink><span v-else>–</span></template>
        </KeyValue>
        <div v-if="open.last_error" class="cx-answer bad"><strong>Last error</strong><pre>{{ open.last_error }}</pre></div>
        <div><h3 class="bh">Message</h3><pre class="body">{{ open.body_text }}</pre></div>
      </div>
      <template v-if="open?.state === 'dead'" #footer><button type="button" class="btn btn-primary" :disabled="!canRetry" @click="retry(open!); open = null">Retry</button></template>
    </Modal>
  </div>
</template>

<style scoped>
.sj { display: block; max-width: 52ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.er { display: block; max-width: 36ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.bh { font-size: var(--fs-md); margin-bottom: 6px; }
.body { white-space: pre-wrap; font-family: var(--font-mono); font-size: var(--fs-sm); background: var(--surface-2); border: 1px solid var(--border); border-radius: var(--r); padding: 10px 12px; margin: 0; overflow-wrap: anywhere; }
</style>
