<script setup lang="ts">
// Server settings: the instance name, the console address used in notification links, the address
// hosts use to reach the agent API; what runs (version, uptime, database); and the setup checklist.
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'
import FormField from '@/components/form/FormField.vue'
import Callout from '@/components/Callout.vue'
import Skeleton from '@/components/Skeleton.vue'
import Spinner from '@/components/Spinner.vue'
import StatusPill from '@/components/StatusPill.vue'
import KeyValue from '@/components/KeyValue.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import SetupHealthCard from '@/components/config/SetupHealthCard.vue'
import { useResource, invalidate } from '@/api/cache'
import { api } from '@/api/endpoints'
import { cfg } from '@/api/sections/config'
import { formatDuration } from '@/lib/format'
import { toast, errorText } from '@/stores/toast'
import '@/components/config/section.css'

const settings = useResource(() => 'cfg:settings', (s) => api.settings.get(s))
const about = useResource(() => 'cfg:about', (s) => cfg.about(s), { poll: 60_000 })
const f = ref({ instance_name: '', base_url: '', advertise_address: '' })
watch(() => settings.data.value, (d) => { if (d) f.value = { instance_name: d.instance_name ?? '', base_url: d.base_url ?? '', advertise_address: d.advertise_address ?? '' } }, { immediate: true })

// the server's validators (internal/server/service/settings_kv.go), so mistakes show while typing
const errors = computed(() => {
  const e: Record<string, string> = {}
  const v = f.value
  if (v.instance_name.length > 64 || /[\r\n]/.test(v.instance_name)) e.instance_name = 'At most 64 characters on one line.'
  if (v.base_url) {
    try {
      const u = new URL(v.base_url.trim())
      if (!/^https?:$/.test(u.protocol) || !u.host || (u.pathname !== '/' && u.pathname !== '') || u.search || u.hash) e.base_url = 'Only scheme and host, like https://backup.example.com (no path).'
    } catch { e.base_url = 'A full address, like https://backup.example.com.' }
  }
  if (v.advertise_address && (/[\s/]/.test(v.advertise_address) || !v.advertise_address.includes(':'))) e.advertise_address = 'Host and port, like backup.example.com:8007.'
  return e
})
const dirty = computed(() => {
  const d = settings.data.value
  return !!d && (f.value.instance_name !== (d.instance_name ?? '') || f.value.base_url !== (d.base_url ?? '') || f.value.advertise_address !== (d.advertise_address ?? ''))
})
const hints = computed(() => {
  const out: string[] = []
  const b = f.value.base_url.trim()
  if (b.startsWith('http://')) out.push('The base URL uses plain HTTP: links in notifications would send people to an unencrypted page.')
  if (b && !b.startsWith(location.origin) && location.protocol === 'https:') out.push(`You opened this console at ${location.origin}; the base URL points elsewhere. That is fine behind a proxy, otherwise links will not work.`)
  const a = f.value.advertise_address.trim()
  if (/^(127\.|localhost|\[?::1\]?)/.test(a)) out.push('The advertised address is a loopback address: other machines cannot reach it.')
  return out
})
const busy = ref(false)
const err = ref('')
async function save() {
  if (Object.keys(errors.value).length) return
  busy.value = true
  err.value = ''
  try {
    await api.settings.put({ instance_name: f.value.instance_name.trim(), base_url: f.value.base_url.trim().replace(/\/$/, ''), advertise_address: f.value.advertise_address.trim() })
    toast.success('Settings saved')
    invalidate('cfg:checklist')
    void settings.refresh()
    void about.refresh()
  } catch (e) { err.value = errorText(e) } finally { busy.value = false }
}
const example = computed(() => (f.value.base_url.trim() || 'https://backup.example.com').replace(/\/$/, '') + '/alerts/42')
</script>

<template>
  <div>
    <header class="page-head">
      <div>
        <h1>Server settings</h1>
        <p class="lede">How people and hosts reach this server. Everything else lives with its page: notifications, policies, certificates, repositories.</p>
      </div>
    </header>

    <div class="cx-grid2">
      <div class="cx-stack">
        <section class="panel" data-testid="server-settings">
          <div class="panel-head"><h2>Addresses and name</h2></div>
          <Skeleton v-if="settings.loading.value" :lines="6" class="panel-body" />
          <Callout v-else-if="settings.error.value && !settings.data.value" status="critical" title="Could not load the settings" :retry="settings.refresh">{{ settings.error.value.message }}</Callout>
          <form v-else class="panel-body cx-form" @submit.prevent="save">
            <FormField v-slot="ff" label="Instance name" optional :error="errors.instance_name" hint="Shown in the console header; tells several servers apart">
              <input :id="ff.id" v-model="f.instance_name" class="input" maxlength="64" placeholder="backup-hel1" data-testid="set-instance" />
            </FormField>
            <FormField v-slot="ff" label="Console address (base URL)" optional :error="errors.base_url" :hint="`Links in notifications point here, e.g. ${example}. Without it messages carry no link.`">
              <input :id="ff.id" v-model="f.base_url" class="input mono" placeholder="https://backup.example.com" spellcheck="false" data-testid="set-base-url" />
            </FormField>
            <FormField v-slot="ff" label="Address for hosts (advertise address)" optional :error="errors.advertise_address" hint="host:port of the agent API that new hosts are given in the onboarding command. Empty: the name you use for this console with the agent port.">
              <input :id="ff.id" v-model="f.advertise_address" class="input mono" placeholder="backup.example.com:8007" spellcheck="false" data-testid="set-advertise" />
            </FormField>
            <ul v-if="hints.length" class="hints"><li v-for="h in hints" :key="h"><Icon name="triangle-alert" :size="13" />{{ h }}</li></ul>
            <Callout v-if="err" status="critical" title="Could not save the settings">{{ err }}</Callout>
            <div class="form-actions">
              <button type="button" class="btn btn-ghost" :disabled="!dirty" @click="settings.data.value && (f = { instance_name: settings.data.value.instance_name ?? '', base_url: settings.data.value.base_url ?? '', advertise_address: settings.data.value.advertise_address ?? '' })">Discard changes</button>
              <button type="submit" class="btn btn-primary" :disabled="!dirty || busy || Object.keys(errors).length > 0" data-testid="set-save"><Spinner v-if="busy" :size="13" />Save</button>
            </div>
          </form>
        </section>

        <section class="panel" data-testid="about">
          <div class="panel-head"><h2>This server</h2><StatusPill v-if="about.data.value" :status="about.data.value.database_ok ? 'ok' : 'critical'" :label="about.data.value.database_ok ? 'database answers' : 'database unreachable'" size="sm" /></div>
          <div class="panel-body">
            <Skeleton v-if="!about.data.value && !about.error.value" :lines="4" />
            <Callout v-else-if="about.error.value && !about.data.value" status="critical" title="Could not load the server information" :retry="about.refresh">{{ about.error.value.message }}</Callout>
            <template v-else-if="about.data.value">
              <KeyValue :columns="2" :items="[
                { key: 'version', label: 'Version', value: about.data.value.version || 'development build', mono: true },
                { key: 'go', label: 'Built with', value: about.data.value.go_version, mono: true },
                { key: 'started', label: 'Running since', value: about.data.value.started },
                { key: 'uptime', label: 'Uptime', value: formatDuration(about.data.value.uptime_seconds * 1000, { units: 2 }) },
                { key: 'repos', label: 'Repositories', value: about.data.value.datastore_count },
                { key: 'db', label: 'PostgreSQL', value: about.data.value.database_ok ? 'reachable' : about.data.value.database_error },
              ]">
                <template #value-started><AbsoluteTime :time="about.data.value.started" /></template>
                <template #value-repos><RouterLink :to="{ name: 'repositories' }">{{ about.data.value.datastore_count }}</RouterLink></template>
              </KeyValue>
              <p class="cx-help foot">The running process of the web console. Maintenance schedules and modes are set per repository on its page.</p>
            </template>
          </div>
        </section>
      </div>
      <SetupHealthCard />
    </div>
  </div>
</template>

<style scoped>
.hints { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
.hints li { display: flex; gap: 6px; align-items: flex-start; color: var(--warn-text); font-size: var(--fs-sm); }
.hints .icon { margin-top: 2px; flex: none; }
.foot { margin-top: 10px; }
</style>
