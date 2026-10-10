// The repositories and the one the console is looking at. Every route that acts on a repository gets
// ?repository=<current> from the API client (setRepositoryProvider). The choice is remembered per browser.
// With zero repositories the views show an empty state (`repos.none`) instead of calling routes that
// would answer 400.
import { reactive, computed } from 'vue'
import { api } from '@/api/endpoints'
import { setRepositoryProvider } from '@/api/client'
import { clearCache } from '@/api/cache'
import type { RepoView } from '@/api/types'

const KEY = 'ib.repository'

export const repos = reactive({
  list: [] as RepoView[],
  current: '',
  loaded: false,
  error: '' as string,
})

export const repoNone = computed(() => repos.loaded && repos.list.length === 0)
export const repoMany = computed(() => repos.list.length > 1)
export const currentRepo = computed(() => repos.list.find((r) => r.name === repos.current) ?? null)

setRepositoryProvider(() => repos.current)

export async function loadRepos() {
  try {
    repos.list = (await api.repositories.list()) ?? []
    repos.error = ''
  } catch (e) {
    repos.error = e instanceof Error ? e.message : String(e)
  }
  repos.loaded = true
  let want = ''
  try { want = localStorage.getItem(KEY) ?? '' } catch { /* storage blocked */ }
  if (!repos.list.some((r) => r.name === repos.current)) {
    repos.current = repos.list.some((r) => r.name === want) ? want : (repos.list[0]?.name ?? '')
  }
}

export function selectRepo(name: string) {
  if (name === repos.current) return
  repos.current = name
  try { localStorage.setItem(KEY, name) } catch { /* storage blocked */ }
  clearCache() // every repository-scoped list is different now
}

export function resetRepos() {
  repos.list = []
  repos.current = ''
  repos.loaded = false
}
