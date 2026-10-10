<script setup lang="ts">
// Everything about one alert: state, where, when, what to do, the timeline and every message sent
// about it. Used by the drawer on the alert list and by the alert page (/alerts/:id, the link in
// notifications). Live: refetches on 'alert' events.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import CIcon from './CIcon.vue'
import SeverityBadge from '@/components/SeverityBadge.vue'
import StatusPill from '@/components/StatusPill.vue'
import KeyValue from '@/components/KeyValue.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import Callout from '@/components/Callout.vue'
import Skeleton from '@/components/Skeleton.vue'
import Spinner from '@/components/Spinner.vue'
import AckDialog from './AckDialog.vue'
import SilenceForm from './SilenceForm.vue'
import { useResource } from '@/api/cache'
import { cfg, list, canCfg, type Alert, type SilenceInput } from '@/api/sections/config'
import { alertType, statusOfAlert, shortAge } from '@/lib/alertTypes'
import { session } from '@/stores/session'
import { toast } from '@/stores/toast'
import { useNow } from '@/composables/useNow'
import { refreshAlertCounts } from './alertCounts'
import './section.css'

const props = defineProps<{ id: number; page?: boolean }>()
const emit = defineEmits<{ changed: [] }>()
const now = useNow()
const res = useResource(() => `cfg:alert:${props.id}`, (s) => cfg.alerts.get(props.id, s), { refreshOn: ['alert'], ttl: 2000 })
const a = computed(() => res.data.value?.alert)
const def = computed(() => alertType(a.value?.type ?? ''))
const events = computed(() => [...list(res.data.value?.events)].reverse())
const deliveries = computed(() => list(res.data.value?.deliveries))
const canAck = computed(() => canCfg(session.role, 'POST /alerts/{id}/ack'))
const canSilence = computed(() => canCfg(session.role, 'POST /silences'))
const canSeeDeliveries = computed(() => canCfg(session.role, 'GET /notifications/deliveries'))

const ackFor = ref<Alert | null>(null)
const silOpen = ref(false)
const busy = ref(false)
const silPrefill = computed<Partial<SilenceInput>>(() => a.value ? { key_glob: a.value.key, reason: '' } : {})

function changed() { void res.refresh(); refreshAlertCounts(0); emit('changed') }
async function unack() {
  if (!a.value) return
  busy.value = true
  try { await cfg.alerts.unack(a.value.id); toast.success('Acknowledgement removed', 'Reminders resume.'); changed() } catch (e) { toast.error(e, 'Could not remove the acknowledgement') } finally { busy.value = false }
}
const dotOf = (k: string) => ({ fired: 'critical', escalated: 'critical', resolved: 'ok', expired: 'ok', acknowledged: 'info', unacknowledged: 'warn', silenced: 'neutral', unsilenced: 'warn', notified: 'info', flapping: 'warn', stable: 'ok' } as Record<string, string>)[k] ?? ''
const age = computed(() => a.value ? shortAge((a.value.resolved_at ? Date.parse(a.value.resolved_at) : now.value) - Date.parse(a.value.first_seen)) : '')
</script>

<template>
  <div class="adp" data-testid="alert-detail">
    <Callout v-if="res.error.value && !res.data.value" status="critical" title="Could not load the alert" :retry="res.refresh">{{ res.error.value.message }}</Callout>
    <Skeleton v-else-if="!a" :lines="6" />
    <template v-else>
      <div class="hd">
        <div class="badges">
          <SeverityBadge :severity="a.severity" />
          <StatusPill :status="statusOfAlert(a.state, a.severity)" :label="a.state" data-testid="alert-state" />
          <span v-if="a.flapping" class="cx-tag warn" title="Fired several times within the flapping window: nothing is sent until it settles">flapping</span>
          <span class="cx-tag">{{ def.label }}</span>
        </div>
        <h2 class="title">{{ a.title }}</h2>
        <p v-if="a.detail" class="detail">{{ a.detail }}</p>
        <p class="since">
          <template v-if="a.state === 'resolved'">Resolved <RelativeTime :time="a.resolved_at" /> after {{ age }}<template v-if="a.resolved_silently"> (silently: nobody had been told)</template>.</template>
          <template v-else>Open for {{ age }}, last checked <RelativeTime :time="a.last_seen" />.</template>
          <template v-if="a.count > 1"> Seen {{ a.count }} times.</template>
        </p>
        <div class="acts">
          <button v-if="a.state === 'firing' || a.state === 'silenced'" type="button" class="btn btn-primary btn-sm" :disabled="!canAck" :title="canAck ? '' : 'Needs the operator role'" data-testid="detail-ack" @click="ackFor = a"><CIcon name="check-check" :size="14" />Acknowledge</button>
          <button v-if="a.state === 'acknowledged'" type="button" class="btn btn-sm" :disabled="!canAck || busy" data-testid="detail-unack" @click="unack"><Spinner v-if="busy" :size="12" /><CIcon v-else name="undo-2" :size="14" />Undo acknowledge</button>
          <button v-if="a.state !== 'resolved'" type="button" class="btn btn-sm" :disabled="!canSilence" :title="canSilence ? '' : 'Needs the operator role'" data-testid="detail-silence" @click="silOpen = true"><CIcon name="bell-off" :size="14" />Silence…</button>
          <RouterLink v-if="!page" :to="{ name: 'alert', params: { id: a.id } }" class="btn btn-ghost btn-sm"><Icon name="external-link" :size="14" />Open page</RouterLink>
        </div>
      </div>

      <Callout v-if="a.state === 'acknowledged'" status="info" :title="`Acknowledged by ${a.acked_by}`">
        <RelativeTime :time="a.acked_at" />. {{ a.ack_comment || 'No comment.' }}
      </Callout>
      <Callout v-if="a.state === 'silenced'" status="neutral" title="Silenced">
        A silence holds every message about this alert. <RouterLink :to="{ name: 'alert-silences' }">See silences</RouterLink>.
      </Callout>

      <section class="blk">
        <h3>What to do</h3>
        <ol class="cx-steps"><li v-for="(t, i) in def.todo" :key="i">{{ t }}</li></ol>
        <p class="cx-help why"><strong>Why it fired:</strong> {{ def.fires }}</p>
      </section>

      <section class="blk">
        <h3>Details</h3>
        <KeyValue :columns="2" :items="[
          { key: 'host', label: 'Host', value: a.host || '–' },
          { key: 'repository', label: 'Repository', value: a.repository || '–' },
          { key: 'object', label: 'Object', value: a.object || '–', mono: !!a.object },
          { key: 'source', label: 'Source', value: a.source === 'event' ? 'Event (resolves when the same key resolves, or expires)' : 'Condition (checked every minute)' },
          { key: 'first', label: 'Episode started', value: a.first_seen },
          { key: 'last', label: 'Last seen', value: a.last_seen },
          { key: 'episode', label: 'Episode', value: a.episode },
          { key: 'key', label: 'Key', value: a.key, mono: true },
        ]">
          <template #value-host><RouterLink v-if="a.host" :to="{ name: 'host', params: { name: a.host } }">{{ a.host }}</RouterLink><span v-else>–</span></template>
          <template #value-first><AbsoluteTime :time="a.first_seen" /></template>
          <template #value-last><RelativeTime :time="a.last_seen" /></template>
        </KeyValue>
      </section>

      <section class="blk">
        <h3>Timeline</h3>
        <ol v-if="events.length" class="cx-timeline" data-testid="alert-timeline">
          <li v-for="(e, i) in events" :key="i"><span class="dot" :class="dotOf(e.kind)" /><span class="what">{{ e.kind }}</span><span class="when"><AbsoluteTime :time="e.at" seconds /></span><div v-if="e.detail" class="det">{{ e.detail }}</div></li>
        </ol>
        <p v-else class="muted small">No events recorded.</p>
      </section>

      <section class="blk">
        <h3>Notifications sent <span class="muted small">({{ deliveries.length }})</span></h3>
        <p v-if="!canSeeDeliveries" class="muted small">The delivery log needs the operator role.</p>
        <p v-else-if="!deliveries.length" class="muted small">Nothing was sent about this alert. No rule matched, it was silenced or held, or it resolved before anyone had to be told.</p>
        <ul v-else class="dl" data-testid="alert-deliveries">
          <li v-for="d in deliveries" :key="d.id">
            <StatusPill :state="d.state === 'dead' ? 'failed' : d.state" :label="d.state" size="sm" />
            <span class="cx-tag">{{ d.kind }}</span>
            <span class="ch">{{ d.channel_name }}</span>
            <span class="muted small"><RelativeTime :time="d.sent_at || d.created" /><template v-if="d.attempts > 1"> · {{ d.attempts }} attempts</template></span>
            <div v-if="d.last_error" class="cx-err err">{{ d.last_error }}</div>
          </li>
        </ul>
      </section>

      <AckDialog :alert="ackFor" @close="ackFor = null" @done="changed" />
      <SilenceForm v-model:open="silOpen" :prefill="silPrefill" title="Silence this alert" @created="changed" />
    </template>
  </div>
</template>

<style scoped>
.adp { display: grid; grid-template-columns: minmax(0, 1fr); gap: 16px; }
.hd { display: grid; grid-template-columns: minmax(0, 1fr); gap: 8px; }
.badges { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.title { font-size: var(--fs-xl, 18px); line-height: 1.3; overflow-wrap: anywhere; }
.detail { color: var(--text-2); white-space: pre-wrap; overflow-wrap: anywhere; }
.since { color: var(--text-muted); font-size: var(--fs-sm); }
.acts { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 4px; }
.blk h3 { font-size: var(--fs-md); margin-bottom: 8px; }
.why { margin-top: 8px; }
.dl { list-style: none; padding: 0; margin: 0; display: grid; grid-template-columns: minmax(0, 1fr); gap: 8px; }
.dl li { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; font-size: var(--fs-sm); padding-bottom: 8px; border-bottom: 1px solid var(--border); }
.dl li:last-child { border-bottom: 0; }
.dl .ch { font-weight: var(--fw-medium); }
.dl .err { flex-basis: 100%; }
</style>
