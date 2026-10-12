<script setup lang="ts">
// Hidden page (/_styleguide): every shared component with its states, for section agents and for
// visual review in both themes. Not linked from the navigation.
import { ref, computed } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { iconNames } from '@/components/icons'
import StatusPill from '@/components/StatusPill.vue'
import SeverityBadge from '@/components/SeverityBadge.vue'
import StatTile from '@/components/StatTile.vue'
import Meter from '@/components/Meter.vue'
import Gauge from '@/components/Gauge.vue'
import Sparkline from '@/components/charts/Sparkline.vue'
import AreaChart from '@/components/charts/AreaChart.vue'
import BarChart from '@/components/charts/BarChart.vue'
import StackedBar from '@/components/charts/StackedBar.vue'
import DataTable from '@/components/DataTable.vue'
import Modal from '@/components/Modal.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import Tabs from '@/components/Tabs.vue'
import Stepper from '@/components/Stepper.vue'
import KeyValue from '@/components/KeyValue.vue'
import CopyField from '@/components/CopyField.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import DurationText from '@/components/DurationText.vue'
import EmptyState from '@/components/EmptyState.vue'
import Skeleton from '@/components/Skeleton.vue'
import Callout from '@/components/Callout.vue'
import Spinner from '@/components/Spinner.vue'
import FormField from '@/components/form/FormField.vue'
import SelectInput from '@/components/form/SelectInput.vue'
import SwitchInput from '@/components/form/SwitchInput.vue'
import TagInput from '@/components/form/TagInput.vue'
import PasswordInput from '@/components/form/PasswordInput.vue'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import { describeSchedule, formatBytes, formatDate } from '@/lib/format'
import type { ColumnDef } from '@/lib/table'

const sections = ['Colour', 'Type', 'Controls', 'Status', 'Numbers', 'Charts', 'Table', 'Overlays', 'Content', 'Icons']
const tab = ref('one')
const step = ref(1)
const modal = ref(false)
const drawer = ref(false)
const typed = ref(false)
const sw = ref(true)
const sel = ref('operator')
const tags = ref(['offsite-a', 'local'])
const pw = ref('Tide-lantern-47')
const name = ref('')
const now = Date.now()
const day = 86_400_000

// synthetic data (deterministic)
let seed = 7
const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647)
const xs = Array.from({ length: 30 }, (_, i) => now - (29 - i) * day)
const phys = xs.map((_, i) => 1.2e12 + i * 2.1e10 + rnd() * 8e9)
const logi = phys.map((v, i) => v * (3.1 + i * 0.01))
const bars = Array.from({ length: 14 }, () => Math.round(20 + rnd() * 40))
const barStatus = bars.map((_, i) => (i === 9 ? 'critical' : i === 4 ? 'warn' : null)) as Array<'critical' | 'warn' | null>

interface Row { id: string; host: string; object: string; kind: string; size: number; snapshots: number; last: string; state: string }
const kinds = ['instance', 'disk', 'filesystem']
const states = ['ok', 'ok', 'ok', 'ok', 'partial', 'failed', 'running']
const rows: Row[] = Array.from({ length: 1500 }, (_, i) => ({
  id: `obj-${i}`, host: `host-${String((i % 23) + 1).padStart(2, '0')}`, object: `default/vm-${i}`, kind: kinds[i % 3],
  size: Math.round(rnd() * 5e11), snapshots: Math.round(rnd() * 120), last: new Date(now - rnd() * 6 * day).toISOString(), state: states[i % states.length],
}))
const cols: ColumnDef<Row>[] = [
  { key: 'object', label: 'Object', required: true },
  { key: 'host', label: 'Host' },
  { key: 'kind', label: 'Kind' },
  { key: 'state', label: 'Last run' },
  { key: 'snapshots', label: 'Snapshots', align: 'right' },
  { key: 'size', label: 'Logical size', align: 'right', format: (r) => formatBytes(r.size) },
  { key: 'last', label: 'Last backup' },
  { key: 'id', label: 'ID', hidden: true, mono: true },
]
const selected = ref<string[]>([])
const tableState = ref<'data' | 'loading' | 'empty' | 'error'>('data')
const tableRows = computed(() => (tableState.value === 'data' ? rows : tableState.value === 'empty' ? [] : undefined))

const swatches = ['--bg', '--surface', '--surface-2', '--surface-3', '--border', '--border-strong', '--text', '--text-2', '--text-muted', '--text-faint', '--accent', '--accent-soft', '--nav-bg', '--focus']
const status = ['ok', 'warn', 'critical', 'info', 'neutral'] as const
const seriesVars = ['--series-1', '--series-2', '--series-3', '--series-4', '--series-other']

const demoRetry = () => { toast.info('Retry') }
async function askDelete() {
  const ok = await confirm({ title: 'Delete the policy “nightly”?', message: '12 objects use it and will fall back to the host default.', confirmLabel: 'Delete policy', danger: true, typeToConfirm: 'nightly' })
  toast.info(ok ? 'Confirmed' : 'Cancelled')
}
</script>

<template>
  <div class="sg" data-testid="styleguide">
    <header class="page-head">
      <div>
        <h1>Style guide</h1>
        <p class="lede">The shared components and tokens of the console. Use these; do not restyle them per page.</p>
      </div>
    </header>
    <nav class="toc"><a v-for="s in sections" :key="s" :href="`#sg-${s.toLowerCase()}`">{{ s }}</a></nav>

    <section id="sg-colour" class="sec">
      <h2>Colour</h2>
      <p class="note">Neutrals are a cool blue-slate ramp; one petrol accent marks interaction; five status colours carry state and are never decoration. Chart series are a separate validated set.</p>
      <div class="sw-grid">
        <div v-for="v in swatches" :key="v" class="sw"><span class="chip" :style="{ background: `var(${v})` }" /><code>{{ v }}</code></div>
      </div>
      <div class="sw-grid">
        <div v-for="s in status" :key="s" class="sw"><span class="chip" :style="{ background: `var(--${s})` }" /><code>--{{ s }}</code><span class="chip sm" :style="{ background: `var(--${s}-bg)`, borderColor: `var(--${s}-border)` }" /></div>
        <div v-for="v in seriesVars" :key="v" class="sw"><span class="chip" :style="{ background: `var(${v})` }" /><code>{{ v }}</code></div>
      </div>
    </section>

    <section id="sg-type" class="sec">
      <h2>Type</h2>
      <p class="note">IBM Plex Sans and Plex Mono, self-hosted. Body is 13.5 px (dense console); all numbers use tabular figures.</p>
      <div class="type">
        <p style="font-size: var(--fs-3xl); font-weight: 600" class="num">84 / 85 <span class="muted" style="font-size: var(--fs-md); font-weight: 400">--fs-3xl headline</span></p>
        <p style="font-size: var(--fs-2xl); font-weight: 600" class="num">1.42 TiB <span class="muted" style="font-size: var(--fs-md); font-weight: 400">--fs-2xl stat</span></p>
        <h1>Page title <span class="muted" style="font-weight: 400; font-size: var(--fs-md)">--fs-xl</span></h1>
        <h2>Panel title <span class="muted" style="font-weight: 400; font-size: var(--fs-md)">--fs-lg</span></h2>
        <p>Body text explains what happened and what to do next. <code>inline code</code> <kbd>Ctrl</kbd> <kbd>K</kbd></p>
        <p class="small muted">Secondary text --fs-sm, muted.</p>
        <p class="num mono">0123456789 · 3f9a1c0e4b7d2a68</p>
      </div>
    </section>

    <section id="sg-controls" class="sec">
      <h2>Controls</h2>
      <div class="row wrap">
        <button class="btn btn-primary"><Icon name="plus" />Primary</button>
        <button class="btn">Secondary</button>
        <button class="btn btn-ghost">Ghost</button>
        <button class="btn btn-danger"><Icon name="trash-2" />Delete</button>
        <button class="btn" disabled>Disabled</button>
        <button class="btn btn-sm">Small</button>
        <button class="btn btn-icon" aria-label="Settings"><Icon name="settings" /></button>
        <button class="btn btn-primary" disabled><Spinner :size="13" />Saving</button>
      </div>
      <div class="form-demo">
        <FormField v-slot="f" label="Policy name" hint="Lower-case letters, digits and dashes." required>
          <input :id="f.id" v-model="name" class="input" placeholder="nightly" :aria-describedby="f.describedby" />
        </FormField>
        <FormField v-slot="f" label="Schedule" :error="'invalid interval &quot;every 30s&quot; (use 1m..24h, e.g. &quot;every 4h&quot;)'">
          <input :id="f.id" value="every 30s" class="input mono" :aria-invalid="f.invalid" :aria-describedby="f.describedby" />
        </FormField>
        <FormField v-slot="f" label="Role"><SelectInput :id="f.id" v-model="sel" :options="['readonly', 'restore', 'operator', 'admin']" /></FormField>
        <FormField v-slot="f" label="Targets" hint="Enter or comma adds one."><TagInput :id="f.id" v-model="tags" placeholder="Add a target" /></FormField>
        <FormField v-slot="f" label="New password"><PasswordInput :id="f.id" v-model="pw" strength autocomplete="new-password" /></FormField>
        <FormField label="Verification"><SwitchInput v-model="sw" :label="sw ? 'Re-verify every 30 days' : 'Off'" /></FormField>
      </div>
      <Tabs v-model="tab" :tabs="[{ id: 'one', label: 'General' }, { id: 'two', label: 'Retention', count: 4 }, { id: 'three', label: 'Assignments', count: 12 }, { id: 'four', label: 'Disabled', disabled: true }]" />
      <Stepper :steps="['Host', 'Policy', 'Command', 'Wait for first backup']" :current="step" />
      <div class="row"><button class="btn btn-sm" @click="step = Math.max(0, step - 1)">Back</button><button class="btn btn-sm" @click="step = Math.min(3, step + 1)">Next</button></div>
    </section>

    <section id="sg-status" class="sec">
      <h2>Status</h2>
      <div class="row wrap">
        <StatusPill v-for="s in status" :key="s" :status="s" :label="s" />
      </div>
      <div class="row wrap">
        <StatusPill state="online" /><StatusPill state="partial" /><StatusPill state="failed" /><StatusPill state="running" /><StatusPill state="disabled" />
        <StatusPill state="ok" variant="dot" label="dot variant" /><StatusPill state="failed" variant="dot" label="failed" /><StatusPill state="ok" size="sm" label="small" />
      </div>
      <div class="row wrap"><SeverityBadge severity="critical" /><SeverityBadge severity="warning" /><SeverityBadge severity="info" /><SeverityBadge severity="ok" /></div>
      <div class="stack" style="max-width: 640px">
        <Callout status="critical" title="Could not load hosts" :retry="demoRetry">The server answered 503: the repository is offline. Set it back online on the repository page.</Callout>
        <Callout status="warn" title="Repository is read-only">Backups are refused until the mode is online again.</Callout>
        <Callout status="ok" title="Verification passed">2,431 chunks checked, none corrupt.</Callout>
        <Callout status="info" title="Planned feature">The jobs API lands with the jobs backend.</Callout>
      </div>
      <div class="row wrap">
        <button class="btn btn-sm" @click="toast.success('Policy saved', 'It applies from the next run.')">Toast success</button>
        <button class="btn btn-sm" @click="toast.error(new Error('policy nightly: keep_daily must be >= 0'), 'Could not save the policy')">Toast error</button>
        <button class="btn btn-sm" @click="toast.warn('Repository read-only')">Toast warning</button>
      </div>
    </section>

    <section id="sg-numbers" class="sec">
      <h2>Numbers</h2>
      <div class="tiles">
        <StatTile label="Objects protected" value="84" unit="/ 85" hint="1 overdue" status="warn" />
        <StatTile label="Stored" :value="formatBytes(1.42e12)" hint="+18.2 GiB/day">
          <Sparkline :values="phys" :width="120" />
        </StatTile>
        <StatTile label="Dedup" value="3.4×" hint="logical 4.8 TiB" />
        <StatTile label="Corrupt chunks" value="2" status="critical" hint="quarantined" />
        <StatTile label="Loading" loading />
      </div>
      <div class="meters">
        <Meter :value="52" :max="100" :warn="0.8" :critical="0.9" label="Disk (ok)" />
        <Meter :value="84" :max="100" :warn="0.8" :critical="0.9" label="Disk (warn)" />
        <Meter :value="95" :max="100" :warn="0.8" :critical="0.9" label="Disk (critical)" />
        <Meter :value="null" :max="null" label="Object store" />
        <div class="row"><Gauge :value="84" :max="85" :warn="0.995" :critical="0.9" invert center="99%" label="protected" /><Gauge :value="40" :max="100" /><Gauge :value="12" :max="100" :warn="0.3" :critical="0.15" invert /></div>
      </div>
      <KeyValue :columns="2" :items="[
        { key: 'a', label: 'Bytes', value: '' }, { key: 'b', label: 'Duration', value: '' },
        { key: 'c', label: 'Relative time', value: '' }, { key: 'd', label: 'Absolute time', value: '' },
        { key: 'e', label: 'Schedule', value: describeSchedule('mon 02:30', 'Europe/Berlin') }, { key: 'f', label: 'Never', value: '' },
      ]">
        <template #value-a><ByteSize :bytes="1536000000" /></template>
        <template #value-b><DurationText :ms="3_725_000" /></template>
        <template #value-c><RelativeTime :time="new Date(now - 3 * 60_000)" /></template>
        <template #value-d><AbsoluteTime :time="new Date(now - 26 * 3_600_000)" /></template>
        <template #value-f><RelativeTime time="0001-01-01T00:00:00Z" /></template>
      </KeyValue>
    </section>

    <section id="sg-charts" class="sec">
      <h2>Charts</h2>
      <p class="note">Own SVG, no chart library. Faint grid, few ticks, the last value labelled, hover crosshair, arrow keys when focused, and a table view behind “Table”.</p>
      <div class="charts">
        <div class="panel panel-body"><AreaChart title="Stored data" subtitle="last 30 days" :x="xs" :series="[{ name: 'Physical', values: phys }]" :y-format="(v) => formatBytes(v)" bytes /></div>
        <div class="panel panel-body"><AreaChart title="Logical vs physical" :x="xs" :series="[{ name: 'Physical', values: phys }, { name: 'Logical', values: logi }]" :y-format="(v) => formatBytes(v)" bytes /></div>
        <div class="panel panel-body"><BarChart title="Backup runs per day" :labels="xs.slice(-14).map((t) => formatDate(t).slice(5))" :values="bars" :statuses="barStatus" name="runs" /></div>
        <div class="panel panel-body"><StackedBar :segments="[{ label: 'Protected', value: 84, status: 'ok' }, { label: 'Overdue', value: 1, status: 'warn' }, { label: 'Never backed up', value: 0, status: 'critical' }]" />
          <hr class="divider" /><StackedBar :segments="[{ label: 'host-01', value: 40 }, { label: 'host-02', value: 25 }, { label: 'host-03', value: 20 }, { label: 'host-04', value: 9 }, { label: 'Other', value: 6, color: 'var(--series-other)' }]" :format="(v) => formatBytes(v * 1e10)" />
        </div>
      </div>
    </section>

    <section id="sg-table" class="sec">
      <h2>Table</h2>
      <div class="row wrap"><span class="muted small">State:</span><button v-for="s in ['data', 'loading', 'empty', 'error'] as const" :key="s" class="btn btn-sm" :aria-pressed="tableState === s" @click="tableState = s">{{ s }}</button></div>
      <DataTable v-model:selected="selected" :rows="tableRows" :columns="cols" row-key="id" selectable storage-key="styleguide" csv-name="objects-demo" :loading="tableState === 'loading'" :error="tableState === 'error' ? 'The server answered 503: repository offline.' : null" :retry="() => (tableState = 'data')" :initial-sort="{ key: 'last', dir: 'desc' }" filter-placeholder="Filter, e.g. host:host-02 -failed">
        <template #cell-object="{ row }"><a href="#" class="strong" @click.prevent>{{ row.object }}</a></template>
        <template #cell-state="{ row }"><StatusPill :state="row.state" size="sm" /></template>
        <template #cell-size="{ row }"><ByteSize :bytes="row.size" /></template>
        <template #cell-last="{ row }"><RelativeTime :time="row.last" /></template>
        <template #bulk><button class="btn btn-sm">Verify selected</button></template>
        <template #toolbar><button class="btn btn-sm btn-primary"><Icon name="play" :size="13" />Back up now</button></template>
        <template #empty><EmptyState title="No objects yet" icon="box">Objects appear after a host's first backup.</EmptyState></template>
      </DataTable>
    </section>

    <section id="sg-overlays" class="sec">
      <h2>Overlays</h2>
      <div class="row wrap">
        <button class="btn" @click="modal = true">Modal</button>
        <button class="btn" @click="drawer = true">Drawer</button>
        <button class="btn" @click="typed = true">Typed confirmation</button>
        <button class="btn btn-danger" @click="askDelete">confirm() promise</button>
      </div>
      <Modal v-model:open="modal" title="Rename host" description="The name is what operators see; the identity stays the same.">
        <FormField v-slot="f" label="Name"><input :id="f.id" class="input" value="web-01" /></FormField>
        <template #footer><button class="btn" @click="modal = false">Cancel</button><button class="btn btn-primary" @click="modal = false">Save</button></template>
      </Modal>
      <Modal v-model:open="drawer" title="Snapshot 2026-10-08T03:00" drawer width="480px">
        <KeyValue :items="[{ key: 'a', label: 'Object', value: 'default/web01' }, { key: 'b', label: 'Chunks', value: '4,213' }, { key: 'c', label: 'Verified', value: 'yes' }]" />
      </Modal>
      <ConfirmDialog v-model:open="typed" title="Forget snapshot?" message="The snapshot is removed now; its chunks are freed by the next GC." danger type-to-confirm="web01" confirm-label="Forget snapshot" @confirm="typed = false; toast.success('Forgotten (demo)')" />
    </section>

    <section id="sg-content" class="sec">
      <h2>Content</h2>
      <div class="content-grid">
        <CopyField value="ibt_3Zp1x9QmVt0aNcK7wLr2Yf8sGhJ4uBdE" label="API token" secret once />
        <CopyField value="SHA256:9f:2c:41:aa:07:5e:bb:13" label="Fingerprint" />
        <CodeBlock code="incus-backup init --server backup.example.net:8007 --token 8f2a… --name web-01" prompt label="Run on the new host" />
        <CodeBlock :code="'2026-10-08T03:00:02Z backup default/web01 started\n2026-10-08T03:00:41Z 1.2 GiB read, 94 MiB new\n2026-10-08T03:00:42Z committed snapshot 7c1e…'" label="Log" />
        <div class="panel"><EmptyState title="No sync jobs" icon="refresh-cw">A sync job keeps a second copy on another server.<template #actions><button class="btn btn-primary btn-sm">Add sync job</button></template></EmptyState></div>
        <div class="panel panel-body"><Skeleton :lines="4" /></div>
      </div>
    </section>

    <section id="sg-icons" class="sec">
      <h2>Icons</h2>
      <p class="note">Lucide, imported per icon (src/components/icons/index.ts). 16 px, 1.75 stroke, currentColor.</p>
      <div class="icons"><div v-for="n in iconNames" :key="n" class="ic"><Icon :name="n" :size="18" /><span>{{ n }}</span></div></div>
    </section>
  </div>
</template>

<style scoped>
.sg { display: flex; flex-direction: column; gap: 8px; }
.toc { display: flex; flex-wrap: wrap; gap: 4px 14px; font-size: var(--fs-sm); padding-bottom: 12px; border-bottom: 1px solid var(--border); }
.sec { padding: 20px 0 24px; border-bottom: 1px solid var(--border); display: flex; flex-direction: column; gap: 14px; scroll-margin-top: 60px; }
.sec h2 { font-size: var(--fs-lg); }
.note { color: var(--text-muted); max-width: 80ch; }
.sw-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 8px; }
.sw { display: flex; align-items: center; gap: 8px; font-size: var(--fs-xs); }
.chip { width: 28px; height: 28px; border-radius: var(--r); border: 1px solid var(--border); flex: none; }
.chip.sm { width: 16px; height: 16px; }
.type { display: flex; flex-direction: column; gap: 10px; }
.row.wrap { flex-wrap: wrap; }
.form-demo { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 16px; max-width: 900px; }
.tiles { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: 10px; }
.meters { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 16px; align-items: center; }
.charts { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 440px), 1fr)); gap: 12px; }
.content-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 360px), 1fr)); gap: 12px; }
.icons { display: grid; grid-template-columns: repeat(auto-fill, minmax(120px, 1fr)); gap: 6px; }
.ic { display: flex; align-items: center; gap: 8px; padding: 6px 8px; border: 1px solid var(--border); border-radius: var(--r); font-size: var(--fs-xs); color: var(--text-muted); background: var(--surface); min-width: 0; }
.ic span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ic .icon { color: var(--text); }
.strong { font-weight: var(--fw-medium); color: var(--text); }
button[aria-pressed='true'] { background: var(--accent-soft); border-color: var(--accent); }
</style>
