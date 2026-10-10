<script setup lang="ts">
// Notification channels: where messages go (e-mail, webhook, ntfy, Gotify), whether the last message
// got through, a real test message with the receiver's own answer, create/edit/delete.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import CIcon from '@/components/config/CIcon.vue'
import SubNav from '@/components/config/SubNav.vue'
import ChannelEditor from '@/components/config/ChannelEditor.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import Modal from '@/components/Modal.vue'
import Spinner from '@/components/Spinner.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import { useResource, invalidate } from '@/api/cache'
import { cfg, list, canCfg, type Channel, type ChannelTestResult, type Delivery } from '@/api/sections/config'
import { KIND_META } from '@/components/config/channelKinds'
import { session } from '@/stores/session'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const channels = useResource(() => 'cfg:channels', (s) => cfg.channels.list(s))
const deliveries = useResource(() => 'cfg:deliveries:recent', (s) => cfg.deliveries.list({ limit: 1000 }, s), { refreshOn: ['alert'], ttl: 10_000 })
const rules = useResource(() => 'cfg:rules', (s) => cfg.rules.list(s))
const canEdit = computed(() => canCfg(session.role, 'POST /notifications/channels'))

const last = computed(() => {
  const m = new Map<string, Delivery>()
  for (const d of list(deliveries.data.value)) {
    if (d.state !== 'sent' && d.state !== 'dead' && d.attempts === 0) continue // queued, nothing happened yet
    const cur = m.get(d.channel_id)
    if (!cur || Date.parse(d.sent_at ?? d.created) > Date.parse(cur.sent_at ?? cur.created)) m.set(d.channel_id, d)
  }
  return m
})
const usedBy = (id: string) => list(rules.data.value).filter((r) => list(r.channel_ids).includes(id))

const cols: ColumnDef<Channel>[] = [
  { key: 'name', label: 'Channel', required: true },
  { key: 'kind', label: 'Kind', width: '110px', format: (r) => KIND_META[r.kind]?.label ?? r.kind },
  { key: 'target', label: 'Sends to' },
  { key: 'enabled', label: 'Enabled', width: '100px', value: (r) => (r.enabled ? 1 : 0) },
  { key: 'last', label: 'Last delivery', value: (r) => last.value.get(r.id)?.state ?? '' },
  { key: 'rules', label: 'Rules', align: 'right', width: '70px', value: (r) => usedBy(r.id).length },
  { key: 'rate_per_hour', label: 'Rate', hidden: true, align: 'right', format: (r) => (r.rate_per_hour ? `${r.rate_per_hour}/h` : 'unlimited') },
  { key: 'actions', label: '', sortable: false, noExport: true, align: 'right', width: '200px', required: true },
]

const editor = ref<{ open: boolean; channel: Channel | null }>({ open: false, channel: null })
function onSaved() { invalidate('cfg:channels'); void channels.refresh() }

const testing = ref<string | null>(null)
const result = ref<{ channel: Channel; res: ChannelTestResult } | null>(null)
async function test(c: Channel) {
  testing.value = c.id
  try {
    const res = await cfg.channels.test(c.id)
    result.value = { channel: c, res }
    if (res.ok) toast.success('Test message delivered', `${c.name} accepted it.`)
    void deliveries.refresh()
  } catch (e) { toast.error(e, 'Could not send the test message') } finally { testing.value = null }
}
async function setEnabled(c: Channel, on: boolean) {
  try { await cfg.channels.update(c.id, { enabled: on }); toast.success(on ? 'Channel enabled' : 'Channel disabled', on ? undefined : 'Queued messages for it are dead-lettered.'); onSaved() } catch (e) { toast.error(e, 'Could not change the channel') }
}
async function remove(c: Channel) {
  const n = usedBy(c.id).length
  if (!(await confirm({ title: `Delete the channel “${c.name}”?`, message: `Its secret, its queue and its delivery log are deleted${n ? `, and ${n} rule${n === 1 ? '' : 's'} stop sending to it` : ''}. This cannot be undone.`, danger: true, typeToConfirm: c.name, confirmLabel: 'Delete channel' }))) return
  try { await cfg.channels.remove(c.id); toast.success('Channel deleted'); onSaved(); invalidate('cfg:rules') } catch (e) { toast.error(e, 'Could not delete the channel') }
}
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Notifications</h1>
        <p class="lede">Channels carry alert messages to people. Messages queue in the database, retry with backoff and never get lost on a restart.</p>
      </div>
      <div class="actions">
        <button v-if="canEdit" type="button" class="btn btn-primary" data-testid="channel-new" @click="editor = { open: true, channel: null }"><Icon name="plus" :size="14" />Add a channel</button>
      </div>
    </header>
    <SubNav group="notifications" />

    <DataTable :rows="list(channels.data.value)" :columns="cols" row-key="id" :loading="channels.loading.value" :error="channels.error.value" :retry="channels.refresh" storage-key="cfg-channels" csv-name="channels" data-testid="channel-table">
      <template #cell-name="{ row }"><span class="nm"><CIcon :name="KIND_META[row.kind]?.icon ?? 'send'" :size="16" class="ki" /><strong>{{ row.name }}</strong><span v-if="row.has_secret" class="cx-tag" title="A secret is stored; it is never shown"><CIcon name="key" :size="11" />secret</span></span></template>
      <template #cell-target="{ row }"><span class="tg mono" :title="row.target">{{ row.target }}</span></template>
      <template #cell-enabled="{ row }"><SwitchInput :model-value="row.enabled" :disabled="!canEdit" :label="row.enabled ? 'On' : 'Off'" @update:model-value="(v: boolean) => setEnabled(row, v)" /></template>
      <template #cell-last="{ row }">
        <template v-if="last.get(row.id)">
          <span class="ld"><StatusPill :state="last.get(row.id)!.state === 'dead' ? 'failed' : last.get(row.id)!.state === 'sent' ? 'ok' : 'pending'" :label="last.get(row.id)!.state === 'sent' ? 'delivered' : last.get(row.id)!.state === 'dead' ? 'failed' : 'retrying'" size="sm" />
            <span class="muted small">{{ last.get(row.id)!.kind }} · <RelativeTime :time="last.get(row.id)!.sent_at || last.get(row.id)!.created" /></span></span>
          <div v-if="last.get(row.id)!.last_error && last.get(row.id)!.state !== 'sent'" class="cx-err le" :title="last.get(row.id)!.last_error">{{ last.get(row.id)!.last_error }}</div>
        </template>
        <span v-else class="muted small">nothing sent yet</span>
      </template>
      <template #cell-rules="{ row }"><RouterLink :to="{ name: 'notification-rules' }" class="num">{{ usedBy(row.id).length }}</RouterLink></template>
      <template #cell-actions="{ row }">
        <span class="cx-actions">
          <button type="button" class="btn btn-sm" :disabled="!canEdit || testing === row.id" :title="canEdit ? 'Send a real test message now' : 'Needs the admin role'" :data-testid="`ch-test-${row.name}`" @click="test(row)"><Spinner v-if="testing === row.id" :size="12" /><CIcon v-else name="zap" :size="14" />Test</button>
          <button type="button" class="btn btn-sm btn-ghost" :disabled="!canEdit" :data-testid="`ch-edit-${row.name}`" @click="editor = { open: true, channel: row }"><Icon name="pencil" :size="14" />Edit</button>
          <button type="button" class="btn btn-sm btn-ghost btn-icon" :disabled="!canEdit" :aria-label="`Delete ${row.name}`" @click="remove(row)"><Icon name="trash-2" :size="14" /></button>
        </span>
      </template>
      <template #empty>
        <EmptyState title="No channels yet" icon="send">Without a channel, alerts are only visible in this console. Add an e-mail or webhook channel, then a rule that sends to it.
          <template v-if="canEdit" #actions><button type="button" class="btn btn-primary btn-sm" @click="editor = { open: true, channel: null }">Add a channel</button></template>
        </EmptyState>
      </template>
    </DataTable>

    <ChannelEditor v-model:open="editor.open" :channel="editor.channel" @saved="onSaved" />

    <Modal :open="!!result" :title="result?.res.ok ? 'Test message delivered' : 'The test message was refused'" :description="result ? `${result.channel.name} · ${result.channel.target}` : ''" width="560px" @update:open="(v: boolean) => { if (!v) result = null }">
      <div v-if="result" class="cx-form" data-testid="test-result">
        <div class="cx-answer" :class="result.res.ok ? 'ok' : 'bad'">
          <template v-if="result.res.ok">The receiver accepted the message. Check that it arrived where people will see it.</template>
          <template v-else><strong>The receiver answered:</strong><pre data-testid="test-error">{{ result.res.error }}</pre></template>
        </div>
        <p v-if="!result.res.ok" class="cx-help">This is the server's own answer. Fix the setting it names and test again; the attempt is in the <RouterLink :to="{ name: 'notification-deliveries' }">delivery log</RouterLink>.</p>
      </div>
      <template #footer>
        <button v-if="result && !result.res.ok" type="button" class="btn" @click="editor = { open: true, channel: result!.channel }; result = null">Edit channel</button>
        <button type="button" class="btn btn-primary" @click="result = null">Close</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.nm { display: inline-flex; align-items: center; gap: 8px; white-space: nowrap; }
.ki { color: var(--text-muted); }
.tg { display: block; max-width: 38ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: var(--fs-sm); color: var(--text-2); }
.ld { display: inline-flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.le { max-width: 40ch; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-top: 2px; }
</style>
