<script setup lang="ts">
// Add a sync job (admin): this server opens every connection, in both directions.
//   1 Peer     name, direction, the remote's agent address, the command to create the token there
//   2 Token    paste the one-time token; "Test" checks address, TLS and that the remote IS the server
//              that issued the token (CA pinned in the token) WITHOUT spending it
//   3 Options  schedule, verified-only, resync-corrupt, transfer-last, rate limit, object filter
//   4 Add      POST /sync enrols at the remote with the token (scope must match the direction)
// The token is kept in this component only and cleared after adding or closing.
import { computed, reactive, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import Modal from '@/components/Modal.vue'
import Stepper from '@/components/Stepper.vue'
import FormField from '@/components/form/FormField.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import TagInput from '@/components/form/TagInput.vue'
import Callout from '@/components/Callout.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import Spinner from '@/components/Spinner.vue'
import KeyValue from '@/components/KeyValue.vue'
import CheckList from '@/components/storage/CheckList.vue'
import ScheduleField from '@/components/storage/ScheduleField.vue'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { storageApi, type RemoteCheckResult } from '@/api/sections/storage'
import { repos } from '@/stores/repos'
import { toast, errorText } from '@/stores/toast'
import { describeSchedule } from '@/lib/format'
import { formatRateLimit, parseRate } from '@/lib/storage'

const props = defineProps<{ open: boolean; existing: string[] }>()
const emit = defineEmits<{ 'update:open': [boolean]; added: [string] }>()
const step = ref(0)
const f = reactive({ name: '', direction: 'push' as 'push' | 'pull', remote: '', remoteRepo: '', schedule: '02:00', verified_only: true, resync_corrupt: false, transfer_last: 0, rate: '', groups: [] as string[] })
const token = ref('')
const schedOk = ref(true)
function reset() {
  Object.assign(f, { name: '', direction: 'push', remote: '', remoteRepo: '', schedule: '02:00', verified_only: true, resync_corrupt: false, transfer_last: 0, rate: '', groups: [] })
  token.value = ''
  step.value = 0
  check.value = null
  error.value = ''
}
watch(() => props.open, (o) => { if (!o) { token.value = '' } else reset() })
function close() { token.value = ''; emit('update:open', false) }

const objects = useResource(() => `objects:${repos.current}`, (s) => api.objects.list(s))
const groupHelp = computed(() => {
  const m = new Map(list(objects.data.value).map((o) => [o.group_id, `${o.project}/${o.name}`]))
  return f.groups.map((g) => m.get(g) ?? 'unknown group')
})
const nameError = computed(() => (!f.name ? '' : !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(f.name) || f.name === '.' || f.name === '..' ? 'Letters, digits, dot, dash and underscore.' : props.existing.includes(f.name) ? 'A sync job with this name exists.' : ''))
const remoteError = computed(() => (!f.remote ? '' : /^[^\s/]+:\d{1,5}$/.test(f.remote.replace(/^https:\/\//, '')) ? '' : 'host:port of the remote agent API, e.g. backup2.example.net:8007'))
const scope = computed(() => (f.direction === 'push' ? 'sync-write' : 'sync-read'))
const tokenCmd = computed(() => `incus-backup-server token create sync-${repos.current}-${f.name || 'NAME'} --scope ${scope.value}${f.remoteRepo ? ` --repository ${f.remoteRepo}` : ''}`)
const rate = computed(() => parseRate(f.rate))
const step0Ok = computed(() => !!f.name && !nameError.value && !!f.remote && !remoteError.value)

const check = ref<RemoteCheckResult | null>(null)
const checking = ref(false)
watch(() => [f.remote, token.value], () => { check.value = null })
async function test() {
  checking.value = true
  try { check.value = await storageApi.sync.checkRemote({ remote: f.remote.trim(), token: token.value.trim() || undefined }) } catch (e) { toast.error(e, 'Could not test the remote') } finally { checking.value = false }
}

const adding = ref(false)
const error = ref('')
async function add() {
  if (rate.value instanceof Error) return
  adding.value = true
  error.value = ''
  try {
    await api.sync.create({ name: f.name.trim(), direction: f.direction, remote: f.remote.trim().replace(/^https:\/\//, ''), token: token.value.trim(), schedule: f.schedule.trim(),
      verified_only: f.verified_only, resync_corrupt: f.resync_corrupt, transfer_last: f.transfer_last || 0, rate_limit_bps: rate.value, group_filter: f.groups.length ? f.groups : null })
    token.value = ''
    toast.success(`Sync job ${f.name} added`, `${f.direction === 'push' ? 'Pushes to' : 'Pulls from'} ${f.remote}; ${describeSchedule(f.schedule).toLowerCase()}.`)
    emit('added', f.name)
    emit('update:open', false)
  } catch (e) {
    error.value = errorText(e)
  } finally { adding.value = false }
}
const validGroup = (v: string) => (/^[0-9a-f]{32}$/.test(v) ? '' : 'A group id is 32 hex characters (copy it from the object page).')
</script>

<template>
  <Modal :open="open" title="Add a sync job" :description="`Replicate repository ${repos.current} to or from another incus-backup server.`" drawer width="760px" @update:open="(v) => !v && close()">
    <Stepper :steps="['Peer', 'Token', 'Options', 'Add']" :current="step" />

    <div v-if="step === 0" class="form-grid" data-testid="sync-step-peer">
      <FormField v-slot="fl" label="Job name" :error="nameError" required hint="Unique per repository; also names the certificate this server gets at the remote.">
        <input :id="fl.id" v-model="f.name" class="input short" autocomplete="off" spellcheck="false" placeholder="offsite" data-testid="sync-name" />
      </FormField>
      <fieldset class="dirs">
        <legend>Direction</legend>
        <label class="dir" :class="{ on: f.direction === 'push' }"><input v-model="f.direction" type="radio" value="push" />
          <span class="flow"><b>{{ repos.current }}</b><Icon name="chevron-right" :size="14" /><Icon name="chevron-right" :size="14" /><b>remote</b></span>
          <span class="muted small">Push: this server sends its snapshots to the remote. Needs a <code>sync-write</code> token from the remote.</span></label>
        <label class="dir" :class="{ on: f.direction === 'pull' }"><input v-model="f.direction" type="radio" value="pull" data-testid="sync-pull" />
          <span class="flow"><b>{{ repos.current }}</b><Icon name="chevron-left" :size="14" /><Icon name="chevron-left" :size="14" /><b>remote</b></span>
          <span class="muted small">Pull: this server fetches the remote's snapshots. Needs a <code>sync-read</code> token from the remote.</span></label>
      </fieldset>
      <div class="grid2">
        <FormField v-slot="fl" label="Remote agent address" :error="remoteError" required hint="host:port of the other server's agent API (port 8007 by default), not its web console.">
          <input :id="fl.id" v-model="f.remote" class="input mono" autocomplete="off" spellcheck="false" placeholder="backup2.example.net:8007" data-testid="sync-remote" />
        </FormField>
        <FormField v-slot="fl" label="Remote repository" optional hint="Only when the remote has several repositories.">
          <input :id="fl.id" v-model="f.remoteRepo" class="input mono" autocomplete="off" spellcheck="false" placeholder="offsite" />
        </FormField>
      </div>
      <Callout status="info" title="Create the token on the remote server">
        Run this on the remote server (as root or the service account). The token is valid for 24 hours and can be used once.
        <CodeBlock :code="tokenCmd" prompt />
      </Callout>
      <div class="form-actions"><button type="button" class="btn btn-primary" :disabled="!step0Ok" data-testid="sync-next" @click="step = 1">Next<Icon name="chevron-right" /></button></div>
    </div>

    <div v-else-if="step === 1" class="form-grid" data-testid="sync-step-token">
      <FormField v-slot="fl" label="Enrolment token from the remote" required hint="ibt1.… · kept in this page only; sent once when the job is added.">
        <PasswordInput :id="fl.id" v-model="token" autocomplete="off" data-testid="sync-token" />
      </FormField>
      <div class="form-actions">
        <button type="button" class="btn" :disabled="checking" data-testid="sync-test" @click="test"><Spinner v-if="checking" :size="13" /><Icon v-else name="play" />Test the remote</button>
        <span class="muted small">Checks the address, TLS and the remote's identity against the token. The token is not used up.</span>
      </div>
      <template v-if="check">
        <Callout v-if="check.ok" status="ok" title="The remote answers and is the server that issued this token" data-testid="sync-test-ok">Whether the token is still unused and has the {{ scope }} scope is checked when you add the job.</Callout>
        <Callout v-else status="critical" title="The remote is not usable yet" data-testid="sync-test-fail">Fix what failed below, then test again.</Callout>
        <CheckList :checks="check.checks" kind="sync" compact />
      </template>
      <div class="form-actions">
        <button type="button" class="btn" @click="step = 0"><Icon name="chevron-left" />Back</button>
        <button type="button" class="btn btn-primary" :disabled="!token.trim()" data-testid="sync-next" @click="step = 2">Next<Icon name="chevron-right" /></button>
      </div>
    </div>

    <div v-else-if="step === 2" class="form-grid" data-testid="sync-step-options">
      <FormField v-slot="fl" label="Schedule">
        <ScheduleField :id="fl.id" v-model="f.schedule" clock="server" :count="3" @valid="(v) => (schedOk = v)" />
      </FormField>
      <div class="opts">
        <SwitchInput v-model="f.verified_only" label="Verified only" />
        <p class="muted small">Only snapshots whose last verification on the source is ok. A snapshot that FAILED verification is never sent, whatever this says.</p>
        <SwitchInput v-model="f.resync_corrupt" label="Resync corrupt" />
        <p class="muted small">Repair snapshots the destination holds but marked failed, from a healthy source copy; the destination verifies what it repaired.</p>
      </div>
      <div class="grid2">
        <FormField v-slot="fl" label="Transfer last" hint="Only the newest N snapshots of each object (0 = all).">
          <input :id="fl.id" v-model.number="f.transfer_last" type="number" min="0" class="input num short" />
        </FormField>
        <FormField v-slot="fl" label="Rate limit" :error="rate instanceof Error ? rate.message : ''" :hint="rate instanceof Error ? '' : `Blob traffic cap: ${formatRateLimit(rate)}. e.g. 20MB, 500KiB; empty = unlimited.`">
          <input :id="fl.id" v-model="f.rate" class="input mono short" placeholder="unlimited" />
        </FormField>
      </div>
      <FormField v-if="f.direction === 'push'" v-slot="fl" label="Only these objects" optional hint="Group ids (32 hex characters, shown on each object's page). Empty: every object.">
        <TagInput :id="fl.id" v-model="f.groups" :validate="validGroup" placeholder="paste a group id, Enter" />
        <p v-if="f.groups.length" class="muted small">{{ groupHelp.join(', ') }}</p>
      </FormField>
      <div class="form-actions">
        <button type="button" class="btn" @click="step = 1"><Icon name="chevron-left" />Back</button>
        <button type="button" class="btn btn-primary" :disabled="!schedOk || rate instanceof Error || f.transfer_last < 0" data-testid="sync-next" @click="step = 3">Next<Icon name="chevron-right" /></button>
      </div>
    </div>

    <div v-else class="form-grid" data-testid="sync-step-add">
      <KeyValue :items="[
        { key: 'n', label: 'Job', value: f.name },
        { key: 'd', label: 'Direction', value: f.direction === 'push' ? `push: ${repos.current} → ${f.remote}` : `pull: ${f.remote} → ${repos.current}` },
        { key: 's', label: 'Schedule', value: describeSchedule(f.schedule) },
        { key: 'o', label: 'Options', value: [f.verified_only && 'verified only', f.resync_corrupt && 'resync corrupt', f.transfer_last && `last ${f.transfer_last}`, !(rate instanceof Error) && rate && formatRateLimit(rate)].filter(Boolean).join(', ') || 'none' },
        { key: 'g', label: 'Objects', value: f.groups.length ? `${f.groups.length} selected` : 'all' },
      ]" />
      <Callout v-if="error" status="critical" title="Could not add the sync job">{{ error }}
        <template v-if="/scope/.test(error)"> Create a {{ scope }} token on the remote.</template>
        <template v-else-if="/token|enrol/i.test(error)"> The token may be used up or expired: create a new one on the remote.</template>
      </Callout>
      <div class="form-actions">
        <button type="button" class="btn" :disabled="adding" @click="step = 2"><Icon name="chevron-left" />Back</button>
        <button type="button" class="btn btn-primary" :disabled="adding" data-testid="sync-add" @click="add"><Spinner v-if="adding" :size="13" />Enrol and add the job</button>
      </div>
    </div>
  </Modal>
</template>

<style scoped>
.short { max-width: 220px; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: var(--s-4); }
.dirs { border: 0; margin: 0; padding: 0; display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
.dirs legend { font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--text-2); margin-bottom: 6px; }
.dir { display: flex; flex-direction: column; gap: 6px; padding: 10px 12px; border: 1px solid var(--border-strong); border-radius: var(--r); cursor: pointer; position: relative; }
.dir input { position: absolute; opacity: 0; }
.dir.on { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent) inset; background: var(--accent-soft); }
.dir:focus-within { box-shadow: 0 0 0 3px color-mix(in srgb, var(--focus) 30%, transparent); }
.flow { display: flex; align-items: center; gap: 4px; }
.flow .icon { color: var(--accent-text); }
.opts { display: flex; flex-direction: column; gap: 4px; }
.opts p { margin: 0 0 8px 38px; }
code { font-family: var(--font-mono); font-size: 12px; }
@media (max-width: 640px) { .grid2, .dirs { grid-template-columns: 1fr; } }
</style>
