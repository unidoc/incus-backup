<script setup lang="ts">
// Add a host (or re-enrol / replace one): name + scope + token lifetime -> a one-time token and the
// exact command to run on the host -> a live wait that turns green when the host has enrolled ->
// next steps (policy, first backup, recovery key). The token lives in this component only and is
// never shown again. Pending (unused) tokens are listed below and can be revoked.
import { usePoll } from '@/lib/poll'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import Stepper from '@/components/Stepper.vue'
import FormField from '@/components/form/FormField.vue'
import CopyField from '@/components/CopyField.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import Callout from '@/components/Callout.vue'
import DataTable from '@/components/DataTable.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import Spinner from '@/components/Spinner.vue'
import StatusPill from '@/components/StatusPill.vue'
import { api, list } from '@/api/endpoints'
import { useResource, invalidate } from '@/api/cache'
import { infraApi, type PendingEnrolment } from '@/api/sections/infra'
import { confirm } from '@/composables/useConfirm'
import { toast } from '@/stores/toast'
import { repos } from '@/stores/repos'
import { hasRole } from '@/stores/session'
import { hostNameProblem } from '@/lib/infra'
import { describeSchedule } from '@/lib/format'
import type { HostRow, OnboardingResult } from '@/api/types'
import type { ColumnDef } from '@/lib/table'

const route = useRoute()
const q = (k: string) => (typeof route.query[k] === 'string' ? (route.query[k] as string) : '')

// ---- step 1: the form
const step = ref(0)
const name = ref(q('host'))
const scope = ref(['backup', 'sync-read', 'sync-write'].includes(q('scope')) ? q('scope') : 'backup')
const ttl = ref(24)
const replaceOk = ref(false)
const touched = ref(false)
const busy = ref(false)
const err = ref('')
const repo = computed(() => repos.current)

const hosts = useResource(() => `hosts:${repo.value}`, (s) => api.hosts.list(s), { enabled: () => !!repo.value, refreshOn: ['host'] })
const existing = computed<HostRow | null>(() => list(hosts.data.value).find((h) => h.name === name.value.trim() && h.scope === scope.value) ?? null)
const nameErr = computed(() => (touched.value ? hostNameProblem(name.value) : ''))
const canSubmit = computed(() => !hostNameProblem(name.value) && (!existing.value || replaceOk.value) && !!repo.value && !busy.value)
watch([name, scope], () => { replaceOk.value = false })

const scopes = [
  { value: 'backup', label: 'Backup host', help: 'An Incus host that backs up its instances and volumes here.' },
  { value: 'sync-read', label: 'Sync peer, reads', help: 'Another backup server that pulls copies from this repository.' },
  { value: 'sync-write', label: 'Sync peer, writes', help: 'Another backup server that pushes copies into this repository.' },
]
const ttls = [{ v: 1, l: '1 hour' }, { v: 24, l: '24 hours' }, { v: 72, l: '3 days' }, { v: 168, l: '7 days' }]

// ---- step 2: the token (component memory only)
const result = ref<OnboardingResult | null>(null)
const issuedAt = ref(0)
const prevFingerprint = ref('')
const enrolled = ref<HostRow | null>(null)
const expired = ref(false)
let timer: (() => void) | null = null

async function submit() {
  touched.value = true
  if (!canSubmit.value) return
  busy.value = true
  err.value = ''
  try {
    prevFingerprint.value = existing.value?.fingerprint ?? ''
    result.value = await api.onboarding({ host: name.value.trim(), scope: scope.value, ttl_hours: ttl.value })
    issuedAt.value = Date.now()
    enrolled.value = null
    expired.value = false
    step.value = 1
    toast.success(`Token for ${name.value.trim()} created`, `Valid for ${ttls.find((t) => t.v === ttl.value)?.l ?? `${ttl.value} hours`}, once.`)
    invalidate('onboarding-pending')
    void pending.refresh()
    startWatch()
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
    toast.error(e, 'Could not create the enrolment token')
  } finally {
    busy.value = false
  }
}

// Wait for the host: poll GET /hosts every 3 s (no event is published for an enrolment). A new
// identity appears; a re-enrolled one gets a new certificate fingerprint.
function startWatch() {
  stopWatch()
  timer = usePoll(poll, 3000)
  void poll()
}
function stopWatch() { if (timer) { timer(); timer = null } }
async function poll() {
  const r = result.value
  if (!r || enrolled.value) { stopWatch(); return }
  if (Date.now() > issuedAt.value + r.expires_in_hours * 3600_000) { expired.value = true; stopWatch(); return }
  try {
    const hs = list(await infraApi.hostsOf(repo.value))
    const h = hs.find((x) => x.name === name.value.trim() && x.scope === scope.value)
    if (h && (prevFingerprint.value ? h.fingerprint !== prevFingerprint.value : true)) {
      enrolled.value = h
      stopWatch()
      invalidate('hosts'); invalidate('infra-hosts'); invalidate('overview'); invalidate('certificates'); invalidate('onboarding-pending')
      void pending.refresh()
      toast.success(`${h.name} enrolled`, h.last_addr ? `Connected from ${h.last_addr}.` : undefined)
    }
  } catch { /* keep waiting; the next poll retries */ }
}
onBeforeUnmount(stopWatch)

function startOver() {
  stopWatch()
  result.value = null
  enrolled.value = null
  expired.value = false
  step.value = 0
  name.value = ''
  touched.value = false
}

// ---- step 3: next steps
const policies = useResource(() => `policies:${repo.value}`, (s) => api.policies.list(s), { enabled: () => !!repo.value })
const assignments = useResource(() => `assignments:${repo.value}`, (s) => api.assignments.list(s), { enabled: () => !!repo.value })
const policy = ref('')
watch(() => policies.data.value, (p) => { if (!policy.value && p?.length) policy.value = p[0].name }, { immediate: true })
const hostDefault = computed(() => list(assignments.data.value).find((a) => a.host === name.value.trim() && !a.project && !a.object)?.policy ?? '')
const assignBusy = ref(false)
async function assign() {
  if (!policy.value) return
  assignBusy.value = true
  try {
    await api.assignments.put({ host: name.value.trim(), project: '', object: '', policy: policy.value })
    toast.success(`Policy ${policy.value} assigned to ${name.value.trim()}`, 'It applies to every object of the host unless a project or object has its own.')
    invalidate('assignments'); void assignments.refresh()
  } catch (e) { toast.error(e, 'Could not assign the policy') } finally { assignBusy.value = false }
}
const firstBusy = ref(false)
async function firstBackup() {
  const p = hostDefault.value || policy.value
  if (!p) return
  firstBusy.value = true
  try {
    await infraApi.createJob({ type: 'backup_now', host: name.value.trim(), repository: repo.value, params: { policy: p } })
    invalidate('jobs')
    toast.success(`First backup queued on ${name.value.trim()}`, `Every object under policy ${p}. Progress is on the host's Jobs tab.`)
  } catch (e) { toast.error(e, 'Could not queue the first backup') } finally { firstBusy.value = false }
}
const keyStored = ref(false)

// ---- pending tokens
const pending = useResource(() => `onboarding-pending:${repo.value}`, (s) => infraApi.pending(s), { enabled: () => !!repo.value })
const pendCols: ColumnDef<PendingEnrolment>[] = [
  { key: 'host', label: 'Host', required: true },
  { key: 'scope', label: 'Scope' },
  { key: 'created', label: 'Issued' },
  { key: 'expires', label: 'Expires' },
  { key: 'replaces', label: 'Kind', value: (p) => (p.replaces ? 're-enrolment' : 'new host') },
  { key: 'acts', label: '', sortable: false, noExport: true, align: 'right', value: () => '' },
]
async function revoke(p: PendingEnrolment) {
  if (!(await confirm({ title: `Revoke the token for ${p.host}?`, message: 'The token stops working at once. A host that has not enrolled yet needs a new one.', confirmLabel: 'Revoke token', danger: true }))) return
  try {
    await infraApi.revokePending(p.id)
    toast.success(`Token for ${p.host} revoked`)
    invalidate('onboarding-pending'); void pending.refresh()
  } catch (e) { toast.error(e, 'Could not revoke the token') }
}

const expiresAt = computed(() => (result.value ? new Date(issuedAt.value + result.value.expires_in_hours * 3600_000) : null))
const isReplace = computed(() => !!prevFingerprint.value)
// one key domain per repository: a further host must bring that key (design 7.3)
const domain = computed(() => repos.list.find((r) => r.name === repo.value)?.domain_id ?? '')
const title = computed(() => (q('replace') ? 'Re-enrol or replace a host' : 'Add a host'))
</script>

<template>
  <div class="onb">
    <header class="page-head">
      <div>
        <h1>{{ title }}</h1>
        <p class="lede">A host enrols once with a one-time token and gets its own certificate. From then on it backs up on its own schedule; this server never receives its encryption key.</p>
      </div>
      <div class="actions"><RouterLink :to="{ name: 'hosts' }" class="btn btn-sm"><Icon name="chevron-left" :size="13" />Hosts</RouterLink></div>
    </header>

    <Stepper :steps="['Host', 'Run the command', 'Next steps']" :current="step" />

    <!-- 1. Host -->
    <section v-if="step === 0" class="panel" data-testid="onb-form">
      <form class="panel-body form-grid" novalidate @submit.prevent="submit">
        <FormField label="Host name" :error="nameErr" hint="The name this host has in the console and in its certificate. Usually its hostname." required v-slot="f">
          <input :id="f.id" v-model="name" class="input" autocomplete="off" spellcheck="false" placeholder="incus-01" :aria-describedby="f.describedby" :aria-invalid="f.invalid" data-testid="onb-name" @blur="touched = true" />
        </FormField>
        <fieldset class="scopes">
          <legend class="lg">What it is</legend>
          <label v-for="s in scopes" :key="s.value" class="opt"><input v-model="scope" type="radio" name="scope" :value="s.value" /><span><b>{{ s.label }}</b><span class="muted small">{{ s.help }}</span></span></label>
        </fieldset>
        <div class="two">
          <FormField label="Token valid for" hint="The token works once; unused, it expires." v-slot="f">
            <select :id="f.id" v-model.number="ttl" class="select" data-testid="onb-ttl"><option v-for="t in ttls" :key="t.v" :value="t.v">{{ t.l }}</option></select>
          </FormField>
          <FormField label="Repository" hint="Choose another one in the top bar." v-slot="f">
            <input :id="f.id" :value="repo" class="input" disabled />
          </FormField>
        </div>
        <Callout v-if="existing" status="warn" :title="`${existing.name} is already enrolled (${existing.scope})`" data-testid="onb-replace">
          Using the new token re-enrols this identity: every certificate it holds now is revoked, and the machine that runs the command becomes {{ existing.name }}. Its backups and objects stay.
          <label class="ack"><input v-model="replaceOk" type="checkbox" data-testid="onb-replace-ok" /> Replace the current certificate of {{ existing.name }}</label>
        </Callout>
        <Callout v-if="err" status="critical" title="Could not create the token">{{ err }}</Callout>
        <div class="form-actions">
          <button type="submit" class="btn btn-primary" :disabled="!canSubmit" data-testid="onb-submit"><Icon name="key-round" :size="14" />{{ existing ? 'Create re-enrolment token' : 'Create token' }}</button>
          <span v-if="!repo" class="muted small">Add a repository first.</span>
        </div>
      </form>
    </section>

    <!-- 2. Command + live wait -->
    <section v-if="step === 1 && result" class="panel" data-testid="onb-command">
      <div class="panel-body stack">
        <p>On <b>{{ name }}</b>, as root:</p>
        <ol class="steps">
          <li>
            <p class="st">Install incus-backup</p>
            <CodeBlock :code="result.install.split('#')[0].trim()" prompt label="Install" />
            <p class="muted small">{{ result.install.includes('#') ? result.install.split('#')[1].trim() : '' }}</p>
          </li>
          <li>
            <p class="st">Enrol it</p>
            <CodeBlock :code="result.command" prompt label="Enrol" data-testid="onb-cmd" />
            <p v-if="!domain" class="muted small">On the first host of a repository it creates the encryption key and prints the <b>recovery key</b> once. Store it before you confirm.</p>
          </li>
        </ol>
        <Callout v-if="domain && scope === 'backup'" status="warn" title="This repository already has a key: the host must use it" data-testid="onb-domain">
          Every host of <b>{{ repo }}</b> shares key domain <code>{{ domain }}</code> (cross-host deduplication). A host with a new key is refused after it enrols, and the token is used up. Before the command above, give the host the repository's key in one of two ways:
          <ul class="ways">
            <li>Paste the recovery key of the repository: <code>incus-backup key import</code> (it reads the key on standard input).</li>
            <li>If a host stored an escrow copy (<code>incus-backup key escrow upload</code>): add <code>--escrow-private-file FILE</code> to the command, with the offline escrow private key in FILE.</li>
          </ul>
        </Callout>
        <CopyField :value="result.token" label="Enrolment token" secret once data-testid="onb-token" />
        <p class="muted small">Agent address <span class="mono">{{ result.server }}</span> · expires <AbsoluteTime :time="expiresAt" /> (<RelativeTime :time="expiresAt" />)</p>

        <div class="wait" :class="enrolled ? 'ok' : expired ? 'bad' : 'pending'" data-testid="onb-wait" :data-state="enrolled ? 'enrolled' : expired ? 'expired' : 'waiting'" aria-live="polite">
          <template v-if="enrolled">
            <Icon name="circle-check" :size="22" />
            <div><p class="wt">{{ enrolled.name }} {{ isReplace ? 're-enrolled' : 'enrolled' }} on the server</p><p class="small">Certificate <span class="mono">{{ enrolled.fingerprint.slice(0, 16) }}…</span><template v-if="enrolled.last_addr"> · from {{ enrolled.last_addr }}</template> · <RelativeTime :time="enrolled.enrolled" />. If the command still ended with an error on the host, this token is used up: create a re-enrolment token.</p></div>
            <button type="button" class="btn btn-primary ml" data-testid="onb-next" @click="step = 2">Next steps<Icon name="chevron-right" :size="14" /></button>
          </template>
          <template v-else-if="expired">
            <Icon name="circle-alert" :size="22" />
            <div><p class="wt">The token expired before {{ name }} enrolled</p><p class="small">Create a new one.</p></div>
            <button type="button" class="btn ml" @click="step = 0">New token</button>
          </template>
          <template v-else>
            <Spinner />
            <div><p class="wt">Waiting for {{ name }} to enrol…</p><p class="small">This page checks every 3 seconds. You can leave it: the token stays valid until it expires.</p></div>
            <button type="button" class="btn btn-ghost btn-sm ml" @click="step = 2">Skip</button>
          </template>
        </div>
      </div>
    </section>

    <!-- 3. Next steps -->
    <section v-if="step === 2" class="panel" data-testid="onb-next-steps">
      <ol class="next">
        <li>
          <div class="nh"><span class="n num">1</span><h3>Choose what it backs up and how long to keep it</h3></div>
          <p class="muted small">A policy sets the schedule and retention. Assigned to the host it covers every object; projects and objects can have their own.</p>
          <div v-if="list(policies.data.value).length" class="row wrap">
            <select v-model="policy" class="select pol" aria-label="Policy">
              <option v-for="p in list(policies.data.value)" :key="p.name" :value="p.name">{{ p.name }}: {{ describeSchedule(p.schedule, p.timezone) }}, keep {{ p.keep_daily }} daily</option>
            </select>
            <button type="button" class="btn" :disabled="assignBusy || !hasRole('operator')" data-testid="onb-assign" @click="assign">Assign to {{ name }}</button>
            <StatusPill v-if="hostDefault" status="ok" :label="`Assigned: ${hostDefault}`" size="sm" />
          </div>
          <p v-else class="small">No policy exists yet. <RouterLink :to="{ name: 'policy-new' }">Create a policy</RouterLink>.</p>
        </li>
        <li>
          <div class="nh"><span class="n num">2</span><h3>Run the first backup</h3></div>
          <p class="muted small">The host backs up at its next scheduled time. To start now, queue a backup of every object under {{ hostDefault || policy || 'the policy' }}.</p>
          <div class="row"><button type="button" class="btn" :disabled="firstBusy || !(hostDefault || policy) || !enrolled" :title="enrolled ? undefined : 'Available once the host has enrolled'" data-testid="onb-first-backup" @click="firstBackup"><Icon name="play" :size="14" />Back up now</button></div>
        </li>
        <li>
          <div class="nh"><span class="n num">3</span><h3>Store the recovery key offline</h3></div>
          <Callout status="warn" title="Without the recovery key, nobody can read these backups">
            This server never holds the key. If the host is lost, its backups can only be decrypted with its recovery key. Print it on the host with <code>incus-backup key recovery</code> and keep it in a password manager or a safe, away from the host.
          </Callout>
          <label class="ack"><input v-model="keyStored" type="checkbox" data-testid="onb-key-stored" /> I stored the recovery key of {{ name }}</label>
        </li>
      </ol>
      <div class="panel-body form-actions">
        <RouterLink :to="{ name: 'host', params: { name: name.trim() } }" class="btn btn-primary" :class="{ dim: !keyStored }" data-testid="onb-open-host">Open {{ name }}</RouterLink>
        <button type="button" class="btn" @click="startOver"><Icon name="plus" :size="14" />Add another host</button>
        <span v-if="!keyStored" class="muted small">Confirm the recovery key first.</span>
      </div>
    </section>

    <!-- Pending tokens -->
    <section class="pend">
      <div class="sec-head"><h2>Unused tokens</h2><span class="muted small">issued in {{ repo }}, not used yet and not expired</span></div>
      <DataTable :rows="pending.data.value" :columns="pendCols" row-key="id" :loading="pending.loading.value" :error="pending.error.value" :retry="pending.refresh" :toolbar="(pending.data.value?.length ?? 0) > 6" storage-key="onboarding-pending" dense data-testid="onb-pending">
        <template #cell-host="{ row }"><span class="strong">{{ row.host }}</span></template>
        <template #cell-created="{ row }"><RelativeTime :time="row.created" /></template>
        <template #cell-expires="{ row }"><RelativeTime :time="row.expires" /></template>
        <template #cell-replaces="{ row }"><StatusPill v-if="row.replaces" status="warn" label="re-enrolment" size="sm" variant="dot" /><span v-else class="muted small">new host</span></template>
        <template #cell-acts="{ row }"><button type="button" class="btn btn-sm btn-ghost" :data-revoke="row.host" @click="revoke(row)">Revoke</button></template>
        <template #empty><EmptyState title="No unused tokens" icon="key-round" compact>Every token issued here was used or has expired.</EmptyState></template>
      </DataTable>
    </section>
  </div>
</template>

<style scoped>
.onb { display: flex; flex-direction: column; gap: 16px; max-width: 980px; }
.page-head { margin-bottom: 0; }
.scopes { border: 0; margin: 0; padding: 0; display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; }
.lg { font-weight: var(--fw-medium); margin-bottom: 6px; padding: 0; grid-column: 1 / -1; }
.opt { display: flex; gap: 10px; align-items: flex-start; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--r); cursor: pointer; }
.opt:has(input:checked) { border-color: var(--accent); background: var(--accent-soft); }
.opt > span { display: flex; flex-direction: column; gap: 2px; }
.opt input { margin-top: 3px; accent-color: var(--accent); }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.ack { display: flex; align-items: center; gap: 8px; margin-top: 8px; font-weight: var(--fw-medium); color: var(--text); cursor: pointer; }
.ack input { accent-color: var(--accent); }
.steps { margin: 0; padding-left: 20px; display: flex; flex-direction: column; gap: 14px; }
.st { font-weight: var(--fw-medium); margin-bottom: 6px; }
.mono { font-family: var(--font-mono); }
.wait { display: flex; align-items: center; gap: 14px; padding: 14px 16px; border-radius: var(--r-lg); border: 1px solid var(--border); background: var(--surface-2); flex-wrap: wrap; }
.wait.ok { background: var(--ok-bg); border-color: var(--ok-border); color: var(--ok-text); }
.wait.bad { background: var(--critical-bg); border-color: var(--critical-border); color: var(--critical-text); }
.wt { font-weight: var(--fw-semibold); color: inherit; }
.wait .small { color: inherit; opacity: .85; }
.ml { margin-left: auto; }
.next { list-style: none; margin: 0; padding: 0; }
.next li { padding: 16px; border-bottom: 1px solid var(--border); display: flex; flex-direction: column; gap: 8px; }
.nh { display: flex; align-items: center; gap: 10px; }
.n { display: inline-grid; place-items: center; width: 22px; height: 22px; border-radius: 50%; background: var(--accent-soft); color: var(--accent-text); font-size: var(--fs-sm); font-weight: var(--fw-semibold); }
.row.wrap { flex-wrap: wrap; }
.pol { width: auto; min-width: 280px; max-width: 100%; }
.dim { opacity: .6; }
.ways { margin: 6px 0 0; padding-left: 18px; display: flex; flex-direction: column; gap: 4px; }
.sec-head { display: flex; align-items: baseline; gap: 10px; margin-bottom: 8px; flex-wrap: wrap; }
.sec-head h2 { font-size: var(--fs-md); }
.strong { font-weight: var(--fw-medium); }
@media (max-width: 720px) { .scopes, .two { grid-template-columns: 1fr; } .pol { min-width: 0; width: 100%; } }
</style>
