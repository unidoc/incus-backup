<script setup lang="ts">
// Restore flows. Each one creates a HOST JOB (POST /jobs): the server never holds a key, so restoring
// needs a host that has it. Four actions:
//   restore_instance  recreate an instance (config + volumes) under a new or the same name
//   restore_volume    restore one stored disk/filesystem into a NEW custom volume
//   guest_files       files out of a VM disk, through a disposable recovery VM on the host
//   fs_extract        files out of a filesystem snapshot, extracted on the host
// The two file actions deliver a tar SEALED (IBS2) to a one-time X25519 key generated inside the
// decryption module (generateSealKeypair: the private half never reaches JavaScript) and
// authenticated with the repository key for this job and the exact parameters this page sent. To
// open it the page asks for the recovery key (KeyGate, memory only), streams GET /jobs/{id}/delivery
// through unsealStream and saves the tar only after the whole delivery verified. Restores never overwrite: the host
// refuses an existing instance or volume. Query parameters preselect: ?action=&group=&snap=&instance=&project=
import { computed, onBeforeUnmount, onMounted, reactive, ref, shallowRef, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import Callout from '@/components/Callout.vue'
import Stepper from '@/components/Stepper.vue'
import FormField from '@/components/form/FormField.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import ByteSize from '@/components/ByteSize.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import Spinner from '@/components/Spinner.vue'
import EmptyState from '@/components/EmptyState.vue'
import PIcon from '@/components/protect/PIcon.vue'
import KeyGate from '@/components/protect/KeyGate.vue'
import JobPanel from '@/components/protect/JobPanel.vue'
import VerifyBadge from '@/components/protect/VerifyBadge.vue'
import { useSnapRows } from '@/components/protect/useSnapRows'
import { api, list } from '@/api/endpoints'
import { useResource } from '@/api/cache'
import { protectApi, type Job, type JobType, type DeliveryResult } from '@/api/sections/protect'
import { repos, repoNone, currentRepo } from '@/stores/repos'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { JOB_HELP, keyToBase64, parsePaths } from '@/lib/protect/jobs'
import { explainCryptoError, loadCrypto, wipeCrypto, type ExplainedError, type KeyHandle, type SealKey } from '@/lib/protect/crypto'
import { saveStream, canStreamToDisk, pickSaveFile, STREAM_TO_DISK_ABOVE } from '@/lib/protect/save'
import { kindLabel, type SnapRow } from '@/lib/protect/snapshots'
import { formatBytes } from '@/lib/format'
import type { ObjectRow } from '@/api/types'

type Action = 'restore_instance' | 'restore_volume' | 'guest_files' | 'fs_extract'
const route = useRoute()
const router = useRouter()

const ACTIONS: { id: Action; title: string; icon: string; what: string; risk: string }[] = [
  { id: 'restore_instance', title: 'Restore an instance', icon: 'archive-restore', what: 'Recreates a VM or container from its stored configuration and disks.', risk: 'Creates a new instance. An existing instance with the target name is never touched: the host refuses.' },
  { id: 'restore_volume', title: 'Restore a disk as a new volume', icon: 'hard-drive-download', what: 'Writes one stored disk or filesystem into a NEW custom volume you can attach or inspect.', risk: 'Creates a new volume on the pool you name. An existing volume with that name is refused.' },
  { id: 'guest_files', title: 'Recover files from a VM disk', icon: 'file-search', what: 'The host boots a disposable recovery VM from the backup, copies the paths you name and delivers them encrypted to this browser.', risk: 'Reads only. Needs the recovery image on the host. The files are encrypted to a key that exists only in this tab.' },
  { id: 'fs_extract', title: 'Extract files on the host', icon: 'file-lock', what: 'The host extracts the paths you name from a filesystem snapshot and delivers them encrypted to this browser.', risk: 'Reads only. For catalog snapshots, "Browse files" does the same in the browser without a host.' },
]

// ---- state
const action = ref<Action | ''>('')
const step = ref(0)
const objects = useResource(() => `objects:${repos.current}`, (s) => api.objects.list(s), { enabled: () => !!repos.current })
const hosts = useResource(() => `hosts:${repos.current}`, (s) => api.hosts.list(s), { enabled: () => !!repos.current })
const group = ref('')
const snapId = ref('')
const { rows: snapRows } = useSnapRows(() => group.value || undefined)

const form = reactive({
  host: '', asProject: '', asName: '', pool: '', volumes: true, start: false, keepNetwork: false, partition: 0, paths: '',
})

const allObjects = computed(() => list(objects.data.value))
const candidates = computed<ObjectRow[]>(() => {
  const a = action.value
  return allObjects.value.filter((o) => {
    if (a === 'restore_instance') return o.kind === 'instance'
    if (a === 'restore_volume') return o.kind === 'block' || o.kind === 'filesystem'
    if (a === 'guest_files') return o.kind === 'block'
    if (a === 'fs_extract') return o.kind === 'filesystem'
    return false
  }).filter((o) => o.snapshots > 0)
})
const obj = computed(() => allObjects.value.find((o) => o.group_id === group.value) ?? null)
const snaps = computed<SnapRow[]>(() => (group.value ? (snapRows.value ?? []).slice().sort((a, b) => (a.snapshot_id < b.snapshot_id ? 1 : -1)) : []))
const snap = computed(() => snaps.value.find((s) => s.snapshot_id === snapId.value) ?? null)
const backupHosts = computed(() => list(hosts.data.value).filter((h) => h.scope === 'backup' && !h.disabled))
const instanceName = computed(() => (obj.value?.name.startsWith('instance/') ? obj.value.name.split('/')[1] : ''))

// prefill from the query
function fromQuery() {
  const q = route.query
  const a = typeof q.action === 'string' ? q.action : ''
  if (ACTIONS.some((x) => x.id === a)) { action.value = a as Action; step.value = 1 }
  if (typeof q.group === 'string') group.value = q.group
  if (typeof q.snap === 'string') snapId.value = q.snap
}
fromQuery()
// an instance chosen from a disk snapshot: find its instance object
watch([allObjects, () => route.query.instance], () => {
  const inst = route.query.instance
  if (action.value === 'restore_instance' && typeof inst === 'string' && !obj.value) {
    const o = allObjects.value.find((x) => x.kind === 'instance' && x.name === `instance/${inst}` && (!route.query.project || x.project === route.query.project))
    if (o) group.value = o.group_id
  }
}, { immediate: true })
watch(group, (g, old) => { if (old && g !== old) snapId.value = '' })
watch(snaps, (l) => { if (l.length && !l.some((s) => s.snapshot_id === snapId.value)) snapId.value = l[0].snapshot_id })
watch(obj, (o) => {
  if (!o) return
  form.host = o.host
  if (action.value === 'restore_instance') form.asName = `${o.name.split('/')[1]}-restored`
  if (action.value === 'restore_volume') form.asName = `${o.name.split('/').filter((x) => x !== 'instance' && x !== 'volume').join('-')}-restored`
}, { immediate: true })

function pick(a: Action) {
  action.value = a
  step.value = 1
  group.value = ''
  snapId.value = ''
  void router.replace({ query: { action: a } })
}

// ---- validation
const pathCheck = computed(() => parsePaths(form.paths, action.value === 'guest_files'))
const nameRe = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/
const errors = computed(() => {
  const e: Record<string, string> = {}
  if (!form.host) e.host = 'Choose the host that runs the job.'
  if (action.value === 'restore_volume') {
    if (!form.pool.trim()) e.pool = 'Name the storage pool for the new volume.'
    if (!nameRe.test(form.asName)) e.asName = 'Letters, digits, dot, dash, underscore; up to 63 characters.'
  }
  if (action.value === 'restore_instance' && form.asName && !nameRe.test(form.asName)) e.asName = 'Letters, digits, dot, dash, underscore; up to 63 characters.'
  if (form.asProject && !nameRe.test(form.asProject)) e.asProject = 'Not a valid project name.'
  if (action.value === 'guest_files' || action.value === 'fs_extract') {
    if (pathCheck.value.error) e.paths = pathCheck.value.error
    else if (!pathCheck.value.paths.length) e.paths = 'Name at least one file or folder.'
  }
  if (action.value === 'guest_files' && (form.partition < 0 || form.partition > 128)) e.partition = '0 (find it) or 1..128.'
  return e
})
const canNext = computed(() => !!group.value && !!snapId.value)
const valid = computed(() => Object.keys(errors.value).length === 0)

// ---- start
const starting = ref(false)
const startErr = ref('')
const job = shallowRef<Job | null>(null)
// one-time delivery key (a handle into the module) and the exact parameters sent: both bind the delivery
let sealKey: SealKey | null = null
let sentParams: Record<string, unknown> | null = null
const repoKey = shallowRef<KeyHandle | null>(null)
const needsKey = computed(() => action.value === 'guest_files' || action.value === 'fs_extract')

function params(pub?: string): Record<string, unknown> {
  const o = obj.value!
  switch (action.value) {
    case 'restore_instance': return { project: o.project, instance: instanceName.value, snapshot: snapId.value, as_project: form.asProject || undefined, as_name: form.asName || undefined, volumes: form.volumes, start: form.start, keep_network: form.keepNetwork }
    case 'restore_volume': return { project: o.project, object: o.name, snapshot: snapId.value, pool: form.pool.trim(), as_project: form.asProject || undefined, as_name: form.asName }
    case 'guest_files': return { project: o.project, object: o.name, snapshot: snapId.value, paths: pathCheck.value.paths, partition: form.partition || undefined, recipient_public_key: pub }
    case 'fs_extract': return { project: o.project, object: o.name, snapshot: snapId.value, paths: pathCheck.value.paths, recipient_public_key: pub }
  }
  return {}
}

async function start() {
  if (!obj.value || !valid.value) return
  starting.value = true
  startErr.value = ''
  try {
    let pub: string | undefined
    if (needsKey.value) {
      const lib = await loadCrypto()
      await wipeKey()
      sealKey = await lib.generateSealKeypair()
      pub = keyToBase64(sealKey.publicKey)
    }
    const p = params(pub)
    sentParams = JSON.parse(JSON.stringify(p)) as Record<string, unknown> // exactly what goes on the wire
    job.value = await protectApi.jobs.create({ type: action.value as JobType, host: form.host, params: p })
    step.value = 4
    toast.success('Job queued', `${form.host} picks it up within seconds when it is online.`)
  } catch (e) {
    await wipeKey()
    startErr.value = e instanceof Error ? e.message : String(e)
  } finally {
    starting.value = false
  }
}
async function wipeKey() {
  const s = sealKey, k = repoKey.value
  sealKey = null; sentParams = null; repoKey.value = null
  await s?.close().catch(() => undefined)
  await k?.close().catch(() => undefined)
}
async function backToActions() {
  if (keyHeld.value && !(await confirm({ title: 'Leave this restore?', message: 'The job keeps running on the host, but this page discards the private key: its delivery can then never be opened. Run the job again later if you need the files.', confirmLabel: 'Leave', danger: true }))) return
  await wipeKey(); job.value = null; delivered.value = null; dl.state = 'idle'; action.value = ''; step.value = 0
  void router.replace({ query: {} })
}
function again() { void wipeKey(); job.value = null; delivered.value = null; dl.state = 'idle'; step.value = 1 }

// ---- delivery: stream, unseal in the page, save
const delivered = ref<DeliveryResult | null>(null)
const dl = reactive({ state: 'idle' as 'idle' | 'running' | 'done' | 'failed', bytes: 0, error: null as ExplainedError | null, ctl: null as AbortController | null })
function onFinished(j: Job) {
  job.value = j
  if (j.state === 'succeeded' && (j.type === 'guest_files' || j.type === 'fs_extract')) delivered.value = (j.result as DeliveryResult) ?? null
  if (j.state === 'succeeded') toast.success('Job finished', j.message || undefined)
  else if (j.state === 'failed') toast.error(new Error(j.message || 'see the log'), 'The job failed')
}
const deliveryName = computed(() => {
  const o = obj.value
  const base = o ? o.name.replace(/[^A-Za-z0-9._-]+/g, '-') : 'files'
  return `${base}-${snapId.value || 'latest'}.tar`
})
function onRepoKey(k: KeyHandle) { repoKey.value = k; dl.error = null }
async function download() {
  const j = job.value
  if (!j || !sealKey || !sentParams || !repoKey.value) return
  const size = delivered.value?.bytes ?? 0
  const wantsDisk = size > STREAM_TO_DISK_ABOVE && canStreamToDisk()
  const handleP = wantsDisk ? pickSaveFile(deliveryName.value) : Promise.resolve(null)
  dl.state = 'running'; dl.bytes = 0; dl.error = null; dl.ctl = new AbortController()
  try {
    const handle = await handleP
    if (wantsDisk && !handle) { dl.state = 'idle'; return }
    const lib = await loadCrypto()
    const src = await protectApi.jobs.delivery(j.id, dl.ctl.signal)
    // verified frame by frame; the stream closes only after the authenticated trailer, and saveStream
    // hands the file over (or commits the on-disk file) only then: a failed check leaves nothing behind
    const plain = lib.unsealStream(src.body, { key: repoKey.value, seal: sealKey, request: { jobId: j.id, type: j.type, params: sentParams }, signal: dl.ctl.signal })
    await saveStream(plain, deliveryName.value, { handle, signal: dl.ctl.signal, type: 'application/x-tar', onBytes: (n) => { dl.bytes = n } })
    dl.state = 'done'
    toast.success('Files saved', `${deliveryName.value}: decrypted and verified in this browser (made by a host with this repository's key, for this job).`)
  } catch (e) {
    if (dl.ctl?.signal.aborted) { dl.state = 'idle'; return }
    dl.state = 'failed'
    const x = explainCryptoError(e, true)
    const same = currentRepo.value?.domain_id && repoKey.value ? repoKey.value.domainId === currentRepo.value.domain_id : null
    const msg = (e as Error).message
    dl.error = x.kind === 'tampered' && !/IBS1/.test(msg)
      ? { ...x, title: 'The delivery did not verify: nothing was saved', detail: `It was not made by a host holding this repository's key for this job and these paths, or it was altered or cut short on the way (${msg}).${same === null ? ' The server records no key domain for this repository, so a recovery key of another repository looks the same: check that you pasted the key of the hosts that back up into it.' : ''} Run the job again; if it fails again, the server or the network is changing deliveries.` }
      : x.kind === 'wrong-key' || same === false ? { kind: 'wrong-key', title: 'This recovery key belongs to another repository', detail: 'Unlock with the recovery key of the hosts that back up into this repository. Nothing was saved.' } : x
  }
}

// leaving the page discards the private key: the delivery cannot be opened afterwards
const keyHeld = computed(() => !!job.value && needsKey.value && dl.state !== 'done')
function beforeUnload(e: BeforeUnloadEvent) { if (keyHeld.value) { e.preventDefault(); e.returnValue = '' } }
onMounted(() => window.addEventListener('beforeunload', beforeUnload))
onBeforeUnmount(() => { window.removeEventListener('beforeunload', beforeUnload); dl.ctl?.abort(); void wipeKey().then(() => (needsKey.value ? wipeCrypto() : undefined)) })

const current = computed(() => ACTIONS.find((a) => a.id === action.value) ?? null)
const steps = ['What to restore', 'Options', 'Review', 'Progress']
</script>

<template>
  <div data-testid="restore">
    <header class="page-head">
      <div>
        <h1>Restore</h1>
        <p class="lede">Get data back. Every restore here runs as a job on a host that holds the key; this server never sees a key or your files. Nothing existing is overwritten.</p>
      </div>
      <div class="actions">
        <RouterLink class="btn" :to="{ name: 'restore-tests' }"><PIcon name="badge-check" :size="15" />Restore tests</RouterLink>
        <RouterLink class="btn" :to="{ name: 'jobs', query: { type: 'restore' } }"><PIcon name="activity" :size="15" />Jobs</RouterLink>
      </div>
    </header>

    <EmptyState v-if="repoNone" title="No repository yet" icon="database">There is nothing to restore before a host backs up into a repository.</EmptyState>
    <template v-else>
      <section v-if="!action" class="cards" aria-label="Choose what to do">
        <button v-for="a in ACTIONS" :key="a.id" type="button" class="card panel" :data-action="a.id" @click="pick(a.id)">
          <PIcon :name="a.icon as never" :size="22" class="ci" />
          <span class="ct">{{ a.title }}</span>
          <span class="cw">{{ a.what }}</span>
          <span class="cr"><PIcon name="shield-check" :size="13" />{{ a.risk }}</span>
        </button>
        <RouterLink class="card panel alt" :to="{ name: 'snapshots', query: { kind: 'filesystem' } }">
          <PIcon name="folder-open" :size="22" class="ci" />
          <span class="ct">Browse files in this browser</span>
          <span class="cw">Open a filesystem snapshot, browse it and download single files or a ZIP. Decrypted here with the recovery key; no host involved.</span>
          <span class="cr"><PIcon name="key" :size="13" />Needs the repository's recovery key.</span>
        </RouterLink>
      </section>

      <template v-else>
        <div class="flowhead">
          <button type="button" class="btn btn-sm btn-ghost" :disabled="step === 4 && dl.state === 'running'" @click="backToActions"><PIcon name="arrow-left" :size="14" />All restore options</button>
          <h2>{{ current?.title }}</h2>
        </div>
        <p class="what">{{ JOB_HELP[action] }}</p>
        <Stepper :steps="steps" :current="Math.max(0, step - 1)" />

        <!-- 1: object + snapshot -->
        <section v-if="step === 1" class="panel">
          <div class="panel-body form-grid two">
            <FormField :label="action === 'restore_instance' ? 'Instance' : 'Object'" v-slot="f">
              <select :id="f.id" v-model="group" class="select" data-testid="pick-object">
                <option value="" disabled>{{ objects.loading.value ? 'Loading…' : candidates.length ? 'Choose…' : 'Nothing of this kind has a snapshot' }}</option>
                <option v-for="o in candidates" :key="o.group_id" :value="o.group_id">{{ o.project }}/{{ o.name }} ({{ o.host }}, {{ o.snapshots }})</option>
              </select>
            </FormField>
            <FormField label="Snapshot" :hint="snap ? `${kindLabel(snap.kind)}, ${formatBytes(snap.logical_bytes ?? 0)}` : 'Newest first'" v-slot="f">
              <select :id="f.id" v-model="snapId" class="select" :disabled="!group" data-testid="pick-snapshot">
                <option v-for="s in snaps" :key="s.snapshot_id" :value="s.snapshot_id">{{ s.created.slice(0, 16).replace('T', ' ') }} UTC · {{ s.snapshot_id }}{{ s.protected ? ' · protected' : '' }}{{ s.verifyClass === 'failed' ? ' · FAILED verification' : '' }}</option>
              </select>
            </FormField>
            <div v-if="snap" class="span2 snapinfo">
              <VerifyBadge :state="snap.verifyClass" :at="snap.verifiedAt" size="md" />
              <span>created <RelativeTime :time="snap.created" /></span>
              <span v-if="snap.notes" class="muted">"{{ snap.notes }}"</span>
              <RouterLink :to="{ name: 'snapshot', params: { group: snap.group_id, snap: snap.snapshot_id } }">Snapshot details</RouterLink>
            </div>
            <Callout v-if="snap?.verifyClass === 'failed'" status="critical" class="span2" title="This snapshot failed verification">A damaged chunk makes the restore fail part-way. Choose an older or newer snapshot that verified.</Callout>
            <Callout v-if="action === 'fs_extract'" status="info" class="span2">For catalog snapshots, <RouterLink :to="group && snapId ? { name: 'snapshot-browse', params: { group, snap: snapId } } : { name: 'snapshots' }">Browse files</RouterLink> downloads files directly in this browser, without a host job.</Callout>
            <div class="span2 nav"><span class="spacer" /><button type="button" class="btn btn-primary" :disabled="!canNext" data-testid="next-options" @click="step = 2">Next: options<PIcon name="chevron-right" :size="14" /></button></div>
          </div>
        </section>

        <!-- 2: options -->
        <section v-else-if="step === 2" class="panel">
          <div class="panel-body form-grid two">
            <FormField label="Run on host" :error="errors.host" :hint="obj && form.host !== obj.host ? `The snapshot was made by ${obj.host}. Another host needs the same key and Incus access.` : 'The host that made the backup.'" v-slot="f">
              <select :id="f.id" v-model="form.host" class="select" data-testid="pick-host">
                <option v-for="h in backupHosts" :key="h.name" :value="h.name">{{ h.name }}{{ h.last_seen ? '' : ' (never seen)' }}</option>
              </select>
            </FormField>
            <template v-if="action === 'restore_instance'">
              <FormField label="New instance name" optional :error="errors.asName" hint="Leave empty to restore under the original name (only if it no longer exists)." v-slot="f">
                <input :id="f.id" v-model="form.asName" class="input" :placeholder="instanceName" data-testid="as-name" />
              </FormField>
              <FormField label="Into project" optional :error="errors.asProject" :hint="`Default: ${obj?.project}`" v-slot="f">
                <input :id="f.id" v-model="form.asProject" class="input" :placeholder="obj?.project" />
              </FormField>
              <div class="switches span2">
                <SwitchInput v-model="form.volumes" label="Also restore its attached custom volumes" />
                <SwitchInput v-model="form.start" label="Start the instance after the restore" />
                <SwitchInput v-model="form.keepNetwork" label="Keep MAC and static addresses" />
              </div>
              <Callout v-if="form.keepNetwork && (form.asName && form.asName !== instanceName)" status="warn" class="span2" title="Address conflict risk">The copy keeps the original's MAC and static addresses. If the original still runs on the same network, both answer to the same addresses. Keep this off for a copy next to a running original.</Callout>
            </template>
            <template v-if="action === 'restore_volume'">
              <FormField label="Storage pool" :error="errors.pool" hint="The Incus pool the new volume is created on (incus storage list)." v-slot="f">
                <input :id="f.id" v-model="form.pool" class="input" placeholder="default" data-testid="pool" />
              </FormField>
              <FormField label="New volume name" :error="errors.asName" hint="Must not exist yet." v-slot="f">
                <input :id="f.id" v-model="form.asName" class="input" data-testid="as-name" />
              </FormField>
              <FormField label="Into project" optional :error="errors.asProject" :hint="`Default: ${obj?.project}`" v-slot="f">
                <input :id="f.id" v-model="form.asProject" class="input" :placeholder="obj?.project" />
              </FormField>
            </template>
            <template v-if="action === 'guest_files' || action === 'fs_extract'">
              <FormField class="span2" label="Paths to recover" :error="errors.paths" :hint="action === 'guest_files' ? 'One per line, absolute inside the guest (e.g. /etc/nginx or /home/anna/report.odt). Folders are copied whole. At most 256.' : 'One per line, relative to the volume root (e.g. docs/2025 or Q1/report.pdf). Folders are copied whole. At most 256.'" v-slot="f">
                <textarea :id="f.id" v-model="form.paths" class="textarea mono" rows="5" spellcheck="false" :placeholder="action === 'guest_files' ? '/etc/nginx\n/var/www/html/index.php' : 'Q1\nKynningar-50/notes.txt'" data-testid="paths" />
              </FormField>
              <FormField v-if="action === 'guest_files'" label="Partition" :error="errors.partition" hint="0 = let the recovery VM find the filesystem; otherwise the partition number." v-slot="f">
                <input :id="f.id" v-model.number="form.partition" type="number" min="0" max="128" class="input" />
              </FormField>
              <Callout status="info" class="span2" title="Delivered encrypted to this tab">
                When you start, this page creates a one-time key pair inside its decryption module. The host encrypts the files to its public half and signs them off with the repository key; only this tab holds the private half, in memory. To open the delivery you paste the repository's recovery key: it proves the files come from a host with that key, for this job and these paths, so the server cannot forge or swap them. Stay on this page until you saved the files: leaving it discards the key, and the server deletes the encrypted delivery one hour after the job ends.
              </Callout>
            </template>
            <div class="span2 nav">
              <button type="button" class="btn" @click="step = 1"><PIcon name="chevron-left" :size="14" />Back</button>
              <span class="spacer" />
              <button type="button" class="btn btn-primary" :disabled="!valid" data-testid="next-review" @click="step = 3">Next: review<PIcon name="chevron-right" :size="14" /></button>
            </div>
          </div>
        </section>

        <!-- 2.5: review -->
        <section v-else-if="step === 3" class="panel">
          <div class="panel-body stack">
            <dl class="review">
              <dt>Action</dt><dd>{{ current?.title }}</dd>
              <dt>From</dt><dd>{{ obj?.project }}/{{ obj?.name }} @ <span class="mono">{{ snapId }}</span></dd>
              <dt>Host</dt><dd>{{ form.host }}</dd>
              <template v-if="action === 'restore_instance'"><dt>Creates</dt><dd>instance <strong>{{ form.asProject || obj?.project }}/{{ form.asName || instanceName }}</strong>{{ form.volumes ? ' with its custom volumes' : '' }}{{ form.start ? ', started' : ', stopped' }}{{ form.keepNetwork ? ', original MAC and addresses' : ', new MAC addresses' }}</dd></template>
              <template v-if="action === 'restore_volume'"><dt>Creates</dt><dd>volume <strong>{{ form.asProject || obj?.project }}/{{ form.asName }}</strong> on pool <strong>{{ form.pool }}</strong></dd></template>
              <template v-if="needsKey"><dt>Paths</dt><dd><ul class="pl"><li v-for="p in pathCheck.paths" :key="p" class="mono">{{ p }}</li></ul></dd></template>
            </dl>
            <Callout status="info" :title="needsKey ? 'What the server learns' : 'Never overwrites'">
              <template v-if="needsKey">The server sees the paths you typed and the sizes; the file contents reach it only encrypted to this tab's key. The host must allow <code>{{ action }}</code> jobs in its client.yaml (<code>jobs.allow</code>); hosts refuse it by default.</template>
              <template v-else>If the target already exists, the host refuses the job and changes nothing. The host must allow <code>{{ action }}</code> jobs in its client.yaml (<code>jobs.allow</code>); hosts refuse restores by default.</template>
            </Callout>
            <Callout v-if="startErr" status="critical" title="Could not start the job">{{ startErr }}</Callout>
            <div class="nav">
              <button type="button" class="btn" @click="step = 2"><PIcon name="chevron-left" :size="14" />Back</button>
              <span class="spacer" />
              <button type="button" class="btn btn-primary" :disabled="starting" data-testid="start-job" @click="start"><Spinner v-if="starting" :size="13" /><PIcon v-else name="play" :size="14" />Start on {{ form.host }}</button>
            </div>
          </div>
        </section>

        <!-- 3: progress -->
        <template v-else-if="step === 4 && job">
          <JobPanel :job-id="job.id" @finished="onFinished">
            <template #default="{ job: j }">
              <div v-if="needsKey && j.state === 'succeeded'" class="deliver" data-testid="delivery">
                <div v-if="delivered" class="dfacts">
                  <PIcon name="package-open" :size="18" />
                  <span><strong>{{ delivered.files }}</strong> {{ delivered.files === 1 ? 'file' : 'files' }}, <ByteSize :bytes="delivered.bytes" /> · sealed <ByteSize :bytes="delivered.size" /></span>
                </div>
                <ul v-if="delivered?.paths" class="pst">
                  <li v-for="(st, p) in delivered.paths" :key="p"><span class="mono">{{ p }}</span> <span :class="st === 'ok' ? 'okt' : 'badt'">{{ st }}</span></li>
                </ul>
                <KeyGate v-if="!repoKey" purpose="delivery" :repo-domain="currentRepo?.domain_id" :repo-name="repos.current" :object-label="`${obj?.project}/${obj?.name}`" @unlocked="onRepoKey" />
                <div v-else class="row">
                  <button type="button" class="btn btn-primary" :disabled="dl.state === 'running'" data-testid="delivery-download" @click="download">
                    <Spinner v-if="dl.state === 'running'" :size="13" /><PIcon v-else name="download" :size="15" />{{ dl.state === 'done' ? 'Download again' : 'Decrypt and save' }} {{ deliveryName }}
                  </button>
                  <button v-if="dl.state === 'running'" type="button" class="btn" @click="dl.ctl?.abort()">Cancel</button>
                  <span v-if="dl.state === 'running'" class="muted"><ByteSize :bytes="dl.bytes" /> decrypted</span>
                  <span v-if="dl.state === 'done'" class="okt" data-testid="delivery-done"><PIcon name="circle-check" :size="14" /> saved, <ByteSize :bytes="dl.bytes" /> verified</span>
                </div>
                <Callout v-if="dl.error" status="critical" :title="dl.error.title">{{ dl.error.detail }}</Callout>
                <p class="muted small">The encrypted delivery is deleted from the server <RelativeTime v-if="j.delivery" :time="j.delivery.expires_at" />. The archive is a tar file (open it with tar, 7-Zip or your file manager).</p>
              </div>
            </template>
          </JobPanel>
          <div class="nav after"><button type="button" class="btn" @click="again"><PIcon name="rotate-ccw" :size="14" />Start another {{ current?.title.toLowerCase() }}</button></div>
        </template>
      </template>
    </template>
  </div>
</template>

<style scoped>
.cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: var(--s-3); }
.card { display: flex; flex-direction: column; gap: var(--s-2); text-align: left; padding: var(--s-5); cursor: pointer; font: inherit; color: var(--text); text-decoration: none; transition: border-color var(--t-fast), box-shadow var(--t-fast); }
.card:hover, .card:focus-visible { border-color: var(--accent); box-shadow: var(--shadow-1); }
.card.alt { background: var(--surface-2); }
.ci { color: var(--accent); }
.ct { font-weight: var(--fw-semibold); font-size: var(--fs-md); }
.cw { color: var(--text-2); font-size: var(--fs-sm); }
.cr { display: flex; gap: 6px; align-items: flex-start; color: var(--text-muted); font-size: var(--fs-xs); margin-top: auto; padding-top: var(--s-2); }
.cr :first-child { flex: none; margin-top: 1px; color: var(--ok); }
.flowhead { display: flex; align-items: center; gap: var(--s-3); margin-bottom: var(--s-1); }
.flowhead h2 { font-size: var(--fs-lg); }
.what { color: var(--text-2); margin-bottom: var(--s-4); max-width: 80ch; }
.form-grid.two { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); }
@media (max-width: 760px) { .form-grid.two { grid-template-columns: minmax(0, 1fr); } }
.span2 { grid-column: 1 / -1; }
.snapinfo { display: flex; gap: var(--s-3); align-items: center; flex-wrap: wrap; font-size: var(--fs-sm); }
.switches { display: flex; flex-direction: column; gap: var(--s-2); }
.nav { display: flex; gap: var(--s-2); align-items: center; }
.nav.after { margin-top: var(--s-3); }
.spacer { flex: 1; }
.mono { font-family: var(--font-mono); }
.review { display: grid; grid-template-columns: 120px 1fr; gap: var(--s-2) var(--s-4); margin: 0; }
.review dt { color: var(--text-muted); }
.review dd { margin: 0; overflow-wrap: anywhere; }
.pl { margin: 0; padding-left: 16px; }
.deliver { margin-top: var(--s-4); padding: var(--s-4); border: 1px solid var(--ok-border); background: var(--ok-bg); border-radius: var(--r); display: flex; flex-direction: column; gap: var(--s-3); }
.dfacts { display: flex; gap: var(--s-2); align-items: center; }
.pst { margin: 0; padding-left: 18px; font-size: var(--fs-sm); }
.okt { color: var(--ok-text); display: inline-flex; gap: 4px; align-items: center; }
.badt { color: var(--critical-text); }
.small { font-size: var(--fs-sm); }
code { font-family: var(--font-mono); font-size: .92em; }
</style>
