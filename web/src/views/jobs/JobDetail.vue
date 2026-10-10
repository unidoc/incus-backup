<script setup lang="ts">
// One host job: live state, progress and log (JobPanel), the parameters it was started with (the
// browser's one-time public key shortened; jobs carry no secrets), its timeline and its result.
import { computed, shallowRef } from 'vue'
import { RouterLink } from 'vue-router'
import KeyValue from '@/components/KeyValue.vue'
import CodeBlock from '@/components/CodeBlock.vue'
import AbsoluteTime from '@/components/AbsoluteTime.vue'
import ByteSize from '@/components/ByteSize.vue'
import Callout from '@/components/Callout.vue'
import PIcon from '@/components/protect/PIcon.vue'
import JobPanel from '@/components/protect/JobPanel.vue'
import type { Job } from '@/api/sections/protect'
import { JOB_HELP, displayParams, jobLabel } from '@/lib/protect/jobs'

const props = defineProps<{ id: string }>()
const job = shallowRef<Job | null>(null)
const params = computed(() => JSON.stringify(displayParams(job.value?.params), null, 2))
const timeline = computed(() => {
  const j = job.value
  if (!j) return []
  return [
    { key: 'created', label: 'Created' }, { key: 'started', label: 'Started' }, { key: 'finished', label: 'Finished' },
    { key: 'lease', label: 'Lease until' }, { key: 'by', label: 'Created by', value: j.created_by }, { key: 'repo', label: 'Repository', value: j.repository },
    { key: 'perm', label: 'Permission', value: j.permission }, { key: 'id', label: 'Job id', value: j.id, mono: true },
  ]
})
const isFileJob = computed(() => job.value?.type === 'guest_files' || job.value?.type === 'fs_extract')
</script>

<template>
  <div data-testid="job-detail">
    <header class="page-head">
      <div>
        <h1>{{ job ? jobLabel(job.type) : 'Job' }}</h1>
        <p class="lede">{{ job ? JOB_HELP[job.type] ?? '' : '' }}</p>
      </div>
      <div class="actions"><RouterLink class="btn" :to="{ name: 'jobs' }"><PIcon name="arrow-left" :size="15" />All jobs</RouterLink></div>
    </header>

    <JobPanel :job-id="id" :show-link="false" @update="job = $event" />

    <div v-if="job" class="grid">
      <section class="panel">
        <header class="panel-head"><h2>Parameters</h2></header>
        <div class="panel-body"><CodeBlock :code="params" /></div>
      </section>
      <section class="panel">
        <header class="panel-head"><h2>Timeline</h2></header>
        <div class="panel-body">
          <KeyValue :items="timeline">
            <template #value-created><AbsoluteTime :time="job.created_at" seconds /></template>
            <template #value-started><AbsoluteTime :time="job.started_at" seconds never="not yet" /></template>
            <template #value-finished><AbsoluteTime :time="job.finished_at" seconds never="not yet" /></template>
            <template #value-lease><AbsoluteTime :time="job.lease_until" seconds /></template>
          </KeyValue>
          <div v-if="job.delivery" class="deliv">
            <p><PIcon name="file-lock" :size="14" /> Sealed delivery: <ByteSize :bytes="job.delivery.size" />, expires <AbsoluteTime :time="job.delivery.expires_at" /></p>
          </div>
          <Callout v-if="isFileJob && job.state === 'succeeded'" status="info" class="note">Only the browser tab that started this job holds the key to open its delivery. If that tab is gone, start the recovery again from the Restore page.</Callout>
        </div>
      </section>
    </div>
  </div>
</template>

<style scoped>
.grid { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: var(--s-4); margin-top: var(--s-4); align-items: start; }
@media (max-width: 1000px) { .grid { grid-template-columns: minmax(0, 1fr); } }
.deliv { margin-top: var(--s-3); font-size: var(--fs-sm); }
.deliv p { display: flex; gap: 6px; align-items: center; }
.note { margin-top: var(--s-3); }
</style>
