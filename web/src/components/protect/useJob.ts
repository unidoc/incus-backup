// Live view of one host job: GET /jobs/{id} (with its log), refreshed on every SSE `job` event for it
// and polled as a fallback while the job is not finished (every 2 s when the live stream is down,
// every 10 s otherwise, so a lost event never leaves a stale page).
import { onBeforeUnmount, shallowRef, watch, type Ref } from 'vue'
import { bus, live } from '@/api/events'
import { isAbort } from '@/api/client'
import { protectApi, terminal, type Job, type JobEvent } from '@/api/sections/protect'

export function useJob(id: Ref<string | null | undefined>) {
  const job = shallowRef<Job | null>(null)
  const error = shallowRef<Error | null>(null)
  const loading = shallowRef(false)
  let ctl: AbortController | null = null
  let timer: ReturnType<typeof setTimeout> | null = null
  let debounce: ReturnType<typeof setTimeout> | null = null
  let alive = true

  async function load() {
    const cur = id.value
    if (!cur) { job.value = null; return }
    ctl?.abort()
    ctl = new AbortController()
    if (!job.value || job.value.id !== cur) loading.value = true
    try {
      const j = await protectApi.jobs.get(cur, ctl.signal)
      if (cur === id.value) { job.value = j; error.value = null }
    } catch (e) {
      if (!isAbort(e)) error.value = e as Error
    } finally {
      loading.value = false
      schedule()
    }
  }

  function schedule() {
    if (timer) clearTimeout(timer)
    timer = null
    if (!alive || !id.value || (job.value && terminal(job.value.state))) return
    timer = setTimeout(load, live.state === 'live' ? 10_000 : 2_000)
  }

  const off = bus.on('job', (d) => {
    const e = d as JobEvent
    if (!id.value || (e.job_id !== id.value && !e.lagged)) return
    // apply the cheap fields at once, fetch the log and result right after
    if (job.value && e.job_id === job.value.id) job.value = { ...job.value, state: e.state, progress: e.progress, message: e.message, attempt: e.attempt, cancel_requested: e.cancel_requested }
    if (debounce) clearTimeout(debounce)
    debounce = setTimeout(load, 250)
  })
  const offResync = bus.on('resync', () => { void load() })

  watch(id, () => { job.value = null; void load() }, { immediate: true })
  onBeforeUnmount(() => {
    alive = false
    off(); offResync()
    ctl?.abort()
    if (timer) clearTimeout(timer)
    if (debounce) clearTimeout(debounce)
  })

  async function cancel(): Promise<void> {
    if (!id.value) return
    job.value = await protectApi.jobs.cancel(id.value)
    void load()
  }

  return { job, error, loading, refresh: load, cancel }
}
