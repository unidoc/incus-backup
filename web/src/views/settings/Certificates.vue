<script setup lang="ts">
// Certificates: the private CA, the server certificate (and its SANs), the web listener certificate,
// every host and sync-peer certificate with its expiry, upcoming expiries on a timeline, and the CA
// rotation (rotate, then promote once every identity renewed).
import { computed, ref, watch } from 'vue'
import CIcon from '@/components/config/CIcon.vue'
import DataTable from '@/components/DataTable.vue'
import StatusPill from '@/components/StatusPill.vue'
import RelativeTime from '@/components/RelativeTime.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import EmptyState from '@/components/EmptyState.vue'
import Callout from '@/components/Callout.vue'
import Skeleton from '@/components/Skeleton.vue'
import Spinner from '@/components/Spinner.vue'
import CopyField from '@/components/CopyField.vue'
import TagInput from '@/components/form/TagInput.vue'
import FormField from '@/components/form/FormField.vue'
import { useResource } from '@/api/cache'
import { cfg, list, canCfg, type CertSummary, type HostCertStatus, type PeerCertStatus } from '@/api/sections/config'
import { session } from '@/stores/session'
import { toast } from '@/stores/toast'
import { confirm } from '@/composables/useConfirm'
import type { ColumnDef } from '@/lib/table'
import '@/components/config/section.css'

const res = useResource(() => 'cfg:certs', (s) => cfg.certificates.get(s), { poll: 60_000 })
const rep = computed(() => res.data.value?.report)
const web = computed(() => res.data.value?.web ?? null)
const isAdmin = computed(() => canCfg(session.role, 'PUT /certificates/sans'))
const lvl = (l: string) => (l === 'critical' ? 'critical' : l === 'warn' ? 'warn' : 'ok') as 'critical' | 'warn' | 'ok'
const days = (d: number) => (d < 0 ? `expired ${-d} d ago` : d === 0 ? 'expires today' : `${d} days left`)
const webDays = computed(() => (web.value?.not_after && !web.value.not_after.startsWith('0001') ? Math.floor((Date.parse(web.value.not_after) - Date.now()) / 86400e3) : null))
const webLevel = computed(() => (webDays.value === null ? 'neutral' : webDays.value < 7 ? 'critical' : webDays.value < 21 ? 'warn' : 'ok'))

// ---- timeline of expiries (next 400 days)
interface Exp { name: string; kind: string; at: string; days: number; level: string }
const expiries = computed<Exp[]>(() => {
  const r = rep.value
  if (!r) return []
  const out: Exp[] = [{ name: 'CA', kind: 'ca', at: r.ca.not_after, days: r.ca.days_left, level: r.ca.level }, { name: 'Server certificate', kind: 'server', at: r.server.not_after, days: r.server.days_left, level: r.server.level }]
  if (r.rotation) out.push({ name: 'Next CA', kind: 'ca', at: r.rotation.next.not_after, days: r.rotation.next.days_left, level: r.rotation.next.level })
  if (web.value && webDays.value !== null) out.push({ name: 'Web listener', kind: 'web', at: web.value.not_after!, days: webDays.value, level: webLevel.value === 'neutral' ? 'ok' : webLevel.value })
  for (const h of list(r.hosts)) if (!h.disabled) out.push({ name: `${h.name} (${h.repository})`, kind: 'host', at: h.not_after, days: h.days_left, level: h.level })
  for (const p of list(r.sync_peers)) out.push({ name: `sync ${p.repository}/${p.job}`, kind: 'peer', at: p.not_after, days: p.days_left, level: p.level })
  return out.sort((a, b) => a.days - b.days)
})
const HORIZON = 400
const onLine = computed(() => expiries.value.filter((e) => e.days <= HORIZON))
const pos = (d: number) => `${Math.max(0, Math.min(100, (d / HORIZON) * 100))}%`

// ---- tables
const hostCols: ColumnDef<HostCertStatus>[] = [
  { key: 'name', label: 'Identity', required: true, value: (h) => `${h.name} ${h.repository}` },
  { key: 'scope', label: 'Scope', width: '100px' },
  { key: 'days_left', label: 'Expires', width: '160px' },
  { key: 'level', label: 'State', width: '170px', value: (h) => ({ critical: 2, warn: 1, ok: 0 } as Record<string, number>)[h.level] ?? 0, format: (h) => h.state },
  { key: 'last_renewed', label: 'Last renewal', width: '130px' },
  { key: 'last_seen', label: 'Last seen', width: '120px' },
  { key: 'other_valid', label: 'Overlap', align: 'right', width: '80px', hidden: true },
  { key: 'fingerprint', label: 'Fingerprint', mono: true, hidden: true },
]
const peerCols: ColumnDef<PeerCertStatus>[] = [
  { key: 'job', label: 'Sync job', required: true, value: (p) => `${p.repository}/${p.job}` },
  { key: 'days_left', label: 'Expires', width: '160px' },
  { key: 'level', label: 'State', width: '140px', format: (p) => p.state },
  { key: 'error', label: 'Problem' },
]
const hostNote = (h: HostCertStatus) => {
  if (h.untrusted) return 'Issued by a retired CA: enrol the host again.'
  if (h.disabled) return 'Host disabled.'
  if (h.level !== 'ok' && h.days_left < 45) return 'Renewal is overdue: the host renews at 45 days. Check that it reaches the server.'
  if (h.on_old_ca) return 'Still on the old CA.'
  return ''
}

// ---- SANs
const sans = ref<string[]>([])
watch(() => rep.value?.want_sans, (w) => { sans.value = [...list(w)] }, { immediate: true })
const sansDirty = computed(() => JSON.stringify(sans.value) !== JSON.stringify(list(rep.value?.want_sans)))
const sanMismatch = computed(() => rep.value && JSON.stringify([...list(rep.value.server_sans)].sort()) !== JSON.stringify([...list(rep.value.want_sans)].sort()))
const validSAN = (v: string) => (/^[A-Za-z0-9.*:-]+$/.test(v) && v.length <= 253 ? '' : 'A DNS name or an IP address')
const savingSans = ref(false)
async function saveSans() {
  if (!sans.value.length) { toast.error('Keep at least one name or address.', 'Could not save the names'); return }
  savingSans.value = true
  try { await cfg.certificates.setSANs(sans.value); toast.success('Names saved', 'The server issues a new certificate within a minute; open connections keep the old one.'); void res.refresh() } catch (e) { toast.error(e, 'Could not save the names') } finally { savingSans.value = false }
}

// ---- CA rotation
const pending = computed(() => list(rep.value?.rotation?.pending))
const busy = ref(false)
async function rotate() {
  if (!(await confirm({ title: 'Start a CA rotation?', message: 'A new CA is created and signs every certificate from now on. The server keeps presenting the current CA, so nothing breaks: every host and sync peer renews early at its next daily check and learns the new CA. Promote when nothing is pending, usually after a day or two.', confirmLabel: 'Start rotation', typeToConfirm: 'rotate' }))) return
  busy.value = true
  try { const r = await cfg.certificates.rotate(); toast.success('Rotation started', `New CA ${r.new_ca_fingerprint.slice(0, 16)}…`); void res.refresh() } catch (e) { toast.error(e, 'Could not start the rotation') } finally { busy.value = false }
}
async function promote(force: boolean) {
  const n = pending.value.length
  const ok = force
    ? await confirm({ title: `Promote and cut off ${n} identit${n === 1 ? 'y' : 'ies'}?`, message: `${pending.value.join(', ')} still use the old CA. After promotion they can no longer reach this server and must be enrolled again with a new token. Backups of those hosts stop until then.`, confirmLabel: 'Promote anyway', danger: true, typeToConfirm: 'cut off' })
    : await confirm({ title: 'Promote the new CA?', message: 'The new CA becomes the presented CA, the server certificate is reissued from it and the old CA is no longer trusted. Every identity already renewed, so nobody is cut off. New enrolment tokens pin the new CA.', confirmLabel: 'Promote', typeToConfirm: 'promote' })
  if (!ok) return
  busy.value = true
  try { await cfg.certificates.promote(force); toast.success('New CA promoted', 'The running server switched within a second.'); void res.refresh() } catch (e) { toast.error(e, 'Could not promote the CA') } finally { busy.value = false }
}
const caCard = (c: CertSummary) => c
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Certificates</h1>
        <p class="lede">Hosts and sync peers authenticate with certificates from this server's own CA and renew them by themselves 45 days before expiry. This page shows that it works.</p>
      </div>
      <div v-if="rep" class="actions"><StatusPill :status="lvl(rep.level)" :label="rep.level === 'ok' ? 'All certificates fine' : rep.level === 'warn' ? 'Needs a look' : 'Action needed'" data-testid="cert-level" /></div>
    </header>

    <Callout v-if="res.error.value && !rep" status="critical" title="Could not load the certificates" :retry="res.refresh">{{ res.error.value.message }}</Callout>
    <Skeleton v-else-if="!rep" :lines="8" />
    <div v-else class="cx-stack">
      <div class="cards">
        <section class="panel card" data-testid="cert-ca">
          <div class="panel-head"><h2><CIcon name="shield" :size="15" /> CA</h2><StatusPill :status="lvl(caCard(rep.ca).level)" :label="days(rep.ca.days_left)" size="sm" /></div>
          <div class="panel-body kvs">
            <div><span class="k">Subject</span>{{ rep.ca.subject || '–' }}</div>
            <div><span class="k">Valid until</span><AbsoluteTime :time="rep.ca.not_after" /></div>
            <CopyField :value="rep.ca.fingerprint" label="Fingerprint (SHA-256), pinned by enrolment tokens" />
            <p class="cx-help">Warns a year before expiry: start a rotation then.</p>
          </div>
        </section>
        <section class="panel card" data-testid="cert-server">
          <div class="panel-head"><h2><CIcon name="file-key" :size="15" /> Server certificate</h2><StatusPill :status="lvl(rep.server.level)" :label="days(rep.server.days_left)" size="sm" /></div>
          <div class="panel-body kvs">
            <div><span class="k">Used by</span>the agent API (hosts, sync peers)</div>
            <div><span class="k">Valid until</span><AbsoluteTime :time="rep.server.not_after" /> · renews itself at 45 days</div>
            <div><span class="k">Names</span><span class="sans"><span v-for="s in list(rep.server_sans)" :key="s" class="cx-tag mono">{{ s }}</span></span></div>
            <p v-if="sanMismatch" class="cx-err">The configured names differ from the certificate. The server reissues it within a minute; if this stays, the daemon is not running.</p>
          </div>
        </section>
        <section class="panel card" data-testid="cert-web">
          <div class="panel-head"><h2><CIcon name="globe" :size="15" /> Web console</h2><StatusPill v-if="web" :status="webLevel" :label="webDays === null ? 'not issued yet' : days(webDays)" size="sm" /></div>
          <div v-if="web" class="panel-body kvs">
            <div><span class="k">Mode</span>{{ web.mode === 'acme' ? 'ACME (automatic)' : web.mode === 'files' ? 'certificate files' : 'self-signed' }}<span v-if="web.auto_renew" class="cx-tag ok">renews itself</span></div>
            <div v-if="web.subject"><span class="k">Subject</span>{{ web.subject }} <span class="muted">· issued by {{ web.issuer }}</span></div>
            <div v-if="webDays !== null"><span class="k">Valid until</span><AbsoluteTime :time="web.not_after" /></div>
            <div v-if="list(web.dns_names).length"><span class="k">Names</span><span class="sans"><span v-for="s in list(web.dns_names)" :key="s" class="cx-tag mono">{{ s }}</span></span></div>
            <CopyField v-if="web.fingerprint" :value="web.fingerprint" label="Fingerprint (SHA-256)" />
            <p v-if="web.mode === 'files'" class="cx-help">Replace the files and send SIGHUP (or wait a minute): the listener reloads them without a restart.</p>
          </div>
          <div v-else class="panel-body cx-help">This server does not report a web listener certificate (no HTTPS listener, or an older daemon).</div>
        </section>
      </div>

      <section class="panel" data-testid="cert-timeline">
        <div class="panel-head"><h2>Upcoming expiries</h2><span class="sub">next {{ HORIZON }} days · hosts renew themselves at 45 days</span></div>
        <div class="panel-body">
          <div class="tl" role="img" :aria-label="`${onLine.length} certificates expire within ${HORIZON} days`">
            <div class="band crit" :style="{ width: pos(14) }" title="critical: under 14 days" />
            <div class="band warn" :style="{ left: pos(14), width: `calc(${pos(60)} - ${pos(14)})` }" title="warning: under 60 days" />
            <div class="renew" :style="{ left: pos(45) }" title="hosts renew at 45 days" />
            <span v-for="(e, i) in onLine" :key="i" class="mark" :class="e.level" :style="{ left: pos(e.days) }" :title="`${e.name}: ${days(e.days)}`" />
            <div class="ticks"><span v-for="d in [0, 90, 180, 365]" :key="d" :style="{ left: pos(d) }">{{ d === 0 ? 'today' : d < 365 ? `${d} d` : '1 year' }}</span></div>
          </div>
          <ul class="next">
            <li v-for="(e, i) in expiries.slice(0, 8)" :key="i"><StatusPill :status="lvl(e.level)" :label="days(e.days)" size="sm" /><span class="nm">{{ e.name }}</span><span class="muted small"><AbsoluteTime :time="e.at" /></span></li>
          </ul>
        </div>
      </section>

      <section>
        <h2 class="sh">Hosts <span class="muted small">{{ list(rep.hosts).length }}</span></h2>
        <DataTable :rows="list(rep.hosts)" :columns="hostCols" :row-key="(h: HostCertStatus) => `${h.repository}/${h.name}/${h.scope}`" storage-key="cfg-certs-hosts" csv-name="host-certificates" :initial-sort="{ key: 'days_left', dir: 'asc' }" data-testid="cert-hosts">
          <template #cell-name="{ row }"><div class="cx-title"><span class="t">{{ row.name }}</span><span class="d">{{ row.repository }}<template v-if="hostNote(row)"> · {{ hostNote(row) }}</template></span></div></template>
          <template #cell-days_left="{ row }"><span :title="new Date(row.not_after).toLocaleString()">{{ days(row.days_left) }}</span></template>
          <template #cell-level="{ row }"><StatusPill :status="row.disabled ? 'neutral' : lvl(row.level)" :label="row.untrusted ? 'untrusted' : row.disabled ? 'disabled' : row.state" size="sm" /><span v-if="row.on_old_ca" class="cx-tag warn">old CA</span></template>
          <template #cell-last_renewed="{ row }"><RelativeTime :time="row.last_renewed" /></template>
          <template #cell-last_seen="{ row }"><RelativeTime :time="row.last_seen" /></template>
          <template #empty><EmptyState title="No hosts enrolled" icon="server" compact>Certificates appear here when hosts enrol.</EmptyState></template>
        </DataTable>
      </section>

      <section v-if="list(rep.sync_peers).length">
        <h2 class="sh">This server's sync credentials</h2>
        <DataTable :rows="list(rep.sync_peers)" :columns="peerCols" :row-key="(p: PeerCertStatus) => `${p.repository}/${p.job}`" :toolbar="false" data-testid="cert-peers">
          <template #cell-days_left="{ row }">{{ row.error ? '–' : days(row.days_left) }}</template>
          <template #cell-level="{ row }"><StatusPill :status="lvl(row.level)" :label="row.state" size="sm" /></template>
          <template #cell-error="{ row }"><span v-if="row.error" class="cx-err">{{ row.error }}</span><span v-else class="muted">–</span></template>
        </DataTable>
      </section>

      <div class="cx-grid2">
        <section class="panel" data-testid="cert-sans">
          <div class="panel-head"><h2>Server names (SANs)</h2></div>
          <div class="panel-body cx-form">
            <p class="cx-help">The names and addresses in the server certificate. Backup hosts do not check them (they pin the CA), but tools that verify the name, like curl or a monitoring probe, do. Add every name and address hosts use to reach the agent port.</p>
            <FormField v-slot="f" label="Names and addresses" hint="Enter or comma adds one; e.g. backup.example.com, 10.0.0.5">
              <TagInput :id="f.id" v-model="sans" :validate="validSAN" placeholder="backup.example.com" />
            </FormField>
            <div class="form-actions"><button type="button" class="btn btn-primary" :disabled="!isAdmin || !sansDirty || savingSans" :title="isAdmin ? '' : 'Needs the admin role'" data-testid="sans-save" @click="saveSans"><Spinner v-if="savingSans" :size="13" />Save names</button></div>
          </div>
        </section>

        <section class="panel" data-testid="cert-rotation">
          <div class="panel-head"><h2>CA rotation</h2><StatusPill v-if="rep.rotation" status="info" label="in progress" size="sm" /></div>
          <div class="panel-body cx-form">
            <template v-if="!rep.rotation">
              <p class="cx-help">The CA is valid for 10 years. Rotate in its last year (this page warns then). Rotation has two steps and nothing breaks between them:</p>
              <ol class="cx-steps">
                <li><strong>Rotate:</strong> a new CA signs every certificate from now on. Hosts and peers renew early at their next daily check and learn it.</li>
                <li><strong>Promote,</strong> when no identity is pending (a day or two later): the new CA replaces the old one.</li>
              </ol>
              <div class="form-actions"><button type="button" class="btn" :disabled="!isAdmin || busy" :title="isAdmin ? '' : 'Needs the admin role'" data-testid="ca-rotate" @click="rotate"><CIcon name="repeat" :size="14" />Start rotation</button></div>
            </template>
            <template v-else>
              <div class="kvs"><div><span class="k">New CA valid until</span><AbsoluteTime :time="rep.rotation.next.not_after" /></div><CopyField :value="rep.rotation.next.fingerprint" label="New CA fingerprint" /></div>
              <Callout v-if="pending.length" status="warn" :title="`${pending.length} identit${pending.length === 1 ? 'y' : 'ies'} still on the old CA`">
                {{ pending.join(', ') }}. They renew at their next daily check (sync peers at their next run). A host that stays here is offline or cannot renew: check it before promoting.
              </Callout>
              <Callout v-else status="ok" title="Every identity renewed">Promote now; nobody is cut off.</Callout>
              <div class="form-actions">
                <button type="button" class="btn btn-primary" :disabled="!isAdmin || busy || pending.length > 0" data-testid="ca-promote" @click="promote(false)">Promote new CA</button>
                <button v-if="pending.length" type="button" class="btn btn-danger" :disabled="!isAdmin || busy" data-testid="ca-promote-force" @click="promote(true)">Promote anyway…</button>
              </div>
            </template>
          </div>
        </section>
      </div>
    </div>
  </div>
</template>

<style scoped>
.cards { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 16px; }
.card h2 { display: inline-flex; align-items: center; gap: 6px; }
.kvs { display: grid; grid-template-columns: minmax(0, 1fr); gap: 8px; font-size: var(--fs-sm); }
.kvs .k { display: block; font-size: var(--fs-xs); color: var(--text-muted); text-transform: uppercase; letter-spacing: .04em; margin-bottom: 1px; }
.sans { display: flex; flex-wrap: wrap; gap: 4px; }
.sh { font-size: var(--fs-lg); margin-bottom: 8px; }
.tl { position: relative; height: 54px; margin: 6px 4px 8px; border-bottom: 1px solid var(--border-strong); }
.band { position: absolute; top: 8px; height: 26px; border-radius: 2px; }
.band.crit { left: 0; background: var(--critical-bg); }
.band.warn { background: var(--warn-bg); }
.renew { position: absolute; top: 4px; height: 34px; border-left: 1px dashed var(--text-faint); }
.mark { position: absolute; top: 13px; width: 12px; height: 12px; margin-left: -6px; border-radius: 50%; background: var(--ok); border: 2px solid var(--surface); box-shadow: 0 0 0 1px var(--border-strong); }
.mark.warn { background: var(--warn); }
.mark.critical { background: var(--critical); }
.ticks { position: absolute; left: 0; right: 0; bottom: -18px; height: 16px; }
.ticks span { position: absolute; transform: translateX(-50%); font-size: var(--fs-xs); color: var(--text-muted); white-space: nowrap; }
.ticks span:first-child { transform: none; }
.next { list-style: none; margin: 26px 0 0; padding: 0; display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 400px), 1fr)); gap: 6px 24px; }
.next li { display: flex; align-items: center; gap: 8px; min-width: 0; font-size: var(--fs-sm); }
.next .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1; min-width: 0; }
@media (max-width: 1100px) { .cards { grid-template-columns: minmax(0, 1fr); } }
</style>
