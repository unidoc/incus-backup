<script setup lang="ts">
// Summary: is it healthy and why not, how big, who owns it, and a deep connection check on demand.
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { selectRepo } from '@/stores/repos'
import Icon from '@/components/icons/Icon.vue'
import StatTile from '@/components/StatTile.vue'
import StatusPill from '@/components/StatusPill.vue'
import KeyValue from '@/components/KeyValue.vue'
import Meter from '@/components/Meter.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import CopyField from '@/components/CopyField.vue'
import CheckList from '@/components/storage/CheckList.vue'
import { useResource } from '@/api/cache'
import type { RepoApi, RepoCheckReport } from '@/api/sections/storage'
import { hasRole } from '@/stores/session'
import { toast, errorText } from '@/stores/toast'
import { formatBytes, formatNumber, formatRatio, formatPercent, shortId } from '@/lib/format'
import type { Health } from '@/lib/storage'
import type { RepoOverview, RepoView } from '@/api/types'

const props = defineProps<{ repo: RepoView; api: RepoApi; overview?: RepoOverview; health: Health | null }>()
const emit = defineEmits<{ go: [string] }>()
const router = useRouter()
const info = useResource(() => `repo-info:${props.repo.name}`, (s) => props.api.info(s), { ttl: 60_000, refreshOn: ['repository'] })
const status = useResource(() => `status:${props.repo.name}`, (s) => props.api.status(s), { refreshOn: ['run', 'host'] })
const o = computed(() => props.overview)
const used = computed(() => (o.value && o.value.fs_total > 0 ? o.value.fs_total - o.value.fs_free : null))
const ownerOk = computed(() => !!info.data.value?.owner && info.data.value.owner.server_id === info.data.value.server_id)

const facts = computed(() => {
  const i = info.data.value
  const s3 = props.repo.s3
  const items: Array<{ key: string; label: string; value?: string | number | null; mono?: boolean }> = [
    { key: 'datastore', label: 'Datastore id', value: i?.datastore_id || '–', mono: true },
    { key: 'domain', label: 'Key domain', value: i?.domain_id || 'not pinned yet (set by the first backup)', mono: !!i?.domain_id },
    { key: 'owner', label: 'Owner (owner.json)' },
    { key: 'created', label: 'Created' },
  ]
  if (s3) {
    items.push(
      { key: 'endpoint', label: 'Endpoint', value: s3.endpoint, mono: true },
      { key: 'bucket', label: 'Bucket / prefix', value: `${s3.bucket}${s3.prefix ? ' / ' + s3.prefix : ''}`, mono: true },
      { key: 'region', label: 'Region', value: s3.region || 'default' },
      { key: 'tls', label: 'Transport', value: s3.no_tls ? 'plain HTTP' : s3.insecure_skip_verify ? 'HTTPS, certificate NOT verified' : s3.ca_pem ? 'HTTPS, private CA' : 'HTTPS' },
      { key: 'style', label: 'Addressing', value: s3.path_style ? 'path-style' : 'virtual-host (auto)' },
      { key: 'cred', label: 'Credential', value: props.repo.credential || '–' },
    )
  } else items.push({ key: 'path', label: 'Directory', value: props.repo.location, mono: true })
  return items
})

const check = ref<RepoCheckReport | null>(null)
const checking = ref(false)
async function runCheck() {
  checking.value = true
  try {
    check.value = await props.api.check()
    if (check.value.ready) toast.success('Connection check passed', `${check.value.checks.length} checks on ${props.repo.name}.`)
    else toast.warn('Connection check found problems', 'See the list on the Summary tab.')
  } catch (e) { toast.error(e, 'Could not run the check') } finally { checking.value = false }
}
function openSync() { selectRepo(props.repo.name); void router.push({ name: 'sync' }) }
const reasonTab = (x: string) => (/corrupt|verification/.test(x) ? 'integrity' : /sync/.test(x) ? 'sync' : /Offline|Read-only/.test(x) ? 'mode' : /verified|garbage/i.test(x) ? 'tasks' : '')
</script>

<template>
  <div class="sum">
    <section class="tiles">
      <StatTile label="Stored" :value="formatBytes(o?.physical_bytes)" :hint="`${formatBytes(o?.logical_bytes)} logical`" :loading="!o" />
      <StatTile label="Dedup ratio" :value="formatRatio(o?.dedup_ratio)" :hint="`${formatNumber(o?.chunks)} chunks`" :loading="!o" />
      <StatTile label="Snapshots" :value="formatNumber(o?.snapshots)" :hint="`${formatNumber(o?.objects)} objects`" :loading="!o" />
      <StatTile label="Hosts" :value="formatNumber(o?.hosts)" :hint="status.data.value ? `${status.data.value.open_sessions} backups running` : ''" :loading="!o" />
      <StatTile label="Backup runs, 24 h" :value="status.data.value ? `${status.data.value.runs_ok24h} ok` : null" :status="status.data.value?.runs_fail24h ? 'warn' : undefined"
        :hint="status.data.value?.runs_fail24h ? `${status.data.value.runs_fail24h} failed or partial` : 'none failed'" :loading="!status.data.value" />
    </section>

    <div class="grid">
      <section class="panel">
        <div class="panel-head"><h2>Health</h2><StatusPill v-if="health" :status="health.status" :label="health.label" size="sm" /></div>
        <div class="panel-body stack">
          <p v-if="health && !health.reasons.length" class="ok"><Icon name="circle-check" :size="16" />Verified and garbage-collected within the last week, no quarantined chunks, no failed verification, no failing sync.</p>
          <ul v-else-if="health" class="reasons">
            <li v-for="x in health.reasons" :key="x"><Icon :name="health.status === 'critical' ? 'circle-alert' : 'triangle-alert'" :size="14" :class="`s-${health.status}`" /><span>{{ x }}</span>
              <button v-if="reasonTab(x)" type="button" class="btn btn-sm btn-ghost" @click="reasonTab(x) === 'sync' ? openSync() : emit('go', reasonTab(x))">Open<Icon name="chevron-right" :size="12" /></button></li>
          </ul>
          <Meter v-if="o && o.fs_total > 0" :value="used" :max="o.fs_total" :warn="0.8" :critical="0.9" label="Disk"
            :detail="`${formatBytes(o.fs_free)} free of ${formatBytes(o.fs_total)} (${formatPercent((used ?? 0) / o.fs_total, 0)} used)`" />
          <p v-else-if="repo.backend === 's3'" class="muted small">Object storage reports usage only: free space and a fill forecast are not available.</p>
          <dl class="times">
            <div><dt>Last backup</dt><dd><RelativeTime :time="o?.last_backup" /></dd></div>
            <div><dt>Last verify</dt><dd><RelativeTime :time="status.data.value?.last_verify ?? o?.last_verify" /></dd></div>
            <div><dt>Last GC</dt><dd><RelativeTime :time="status.data.value?.last_gc ?? o?.last_gc" /></dd></div>
            <div><dt>Last retention</dt><dd><RelativeTime :time="o?.last_retention" /></dd></div>
          </dl>
        </div>
      </section>

      <section class="panel">
        <div class="panel-head"><h2>Identity</h2><span class="sub">what proves this is the right datastore</span></div>
        <div class="panel-body">
          <Callout v-if="info.error.value" status="warn" title="Could not read the identity" :retry="info.refresh">{{ info.error.value.message }}</Callout>
          <KeyValue :items="facts">
            <template #value-datastore><span v-if="info.data.value?.datastore_id" class="row"><code class="mono">{{ info.data.value.datastore_id }}</code></span><span v-else>–</span></template>
            <template #value-owner>
              <template v-if="info.data.value?.owner">
                <span class="row wrap"><code class="mono">{{ shortId(info.data.value.owner.server_id, 12) }}</code>
                  <StatusPill :status="ownerOk ? 'ok' : 'critical'" :label="ownerOk ? 'this server' : 'ANOTHER server'" size="sm" /></span>
                <span class="muted small">{{ info.data.value.owner.host ? `host ${info.data.value.owner.host}, ` : '' }}since <AbsoluteTime :time="info.data.value.owner.since" /><template v-if="info.data.value.owner.previous"> · adopted from {{ shortId(info.data.value.owner.previous, 12) }}</template></span>
              </template>
              <span v-else-if="info.data.value?.owner_error" :class="repo.mode === 'offline' ? 'muted small' : 'crit small'">{{ repo.mode === 'offline' ? 'Not read while the repository is offline.' : `Unreadable: ${info.data.value.owner_error}` }}</span>
              <span v-else class="muted">–</span>
            </template>
            <template #value-created><AbsoluteTime :time="info.data.value?.datastore_created" /></template>
          </KeyValue>
          <div v-if="info.data.value" class="sid"><CopyField :value="info.data.value.server_id" label="This server's id" /></div>
        </div>
      </section>
    </div>

    <section class="panel">
      <div class="panel-head"><h2>Connection check</h2><span class="sub">{{ repo.backend === 's3' ? 'connect, write, conditional write, read, list, delete, bucket versioning, lifecycle and object lock' : 'directory, write test, free space, owner' }}</span>
        <span class="spacer" />
        <button type="button" class="btn btn-sm" :disabled="checking || !hasRole('operator')" :title="hasRole('operator') ? '' : 'Needs the operator role'" data-testid="repo-check" @click="runCheck">
          <Spinner v-if="checking" :size="12" /><Icon v-else name="play" :size="13" />Run check</button></div>
      <div class="panel-body">
        <CheckList v-if="check" :checks="check.checks" :kind="repo.backend" />
        <p v-else class="muted small">Runs every check the server makes when it creates a repository, plus the bucket settings that can silently cost space or destroy chunks. It writes one small test {{ repo.backend === 's3' ? 'object' : 'file' }} and deletes it again.</p>
      </div>
    </section>
  </div>
</template>

<style scoped>
.sum { display: flex; flex-direction: column; gap: var(--s-4); }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: var(--s-3); }
.grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--s-4); }
.ok { display: flex; gap: 8px; color: var(--text-2); }
.ok .icon { color: var(--ok); margin-top: 2px; }
.reasons { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
.reasons li { display: flex; align-items: center; gap: 8px; }
.reasons li span { flex: 1; }
.s-critical { color: var(--critical); } .s-warn { color: var(--warn); }
.times { display: grid; grid-template-columns: repeat(2, 1fr); gap: 4px 16px; margin: 0; font-size: var(--fs-sm); }
.times div { display: flex; justify-content: space-between; gap: 8px; border-bottom: 1px dashed var(--border); padding: 2px 0; }
.times dt { color: var(--text-muted); }
.times dd { margin: 0; }
.wrap { flex-wrap: wrap; }
.crit { color: var(--critical-text); }
.sid { margin-top: 12px; max-width: 520px; }
code.mono { font-size: 12.5px; }
@media (max-width: 960px) { .grid { grid-template-columns: 1fr; } }
</style>
