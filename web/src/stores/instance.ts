// Instance name and server version for the top bar. GET /settings is admin-only, so other roles quietly
// get the host name of the page instead (no error toast).
import { reactive } from 'vue'
import { api } from '@/api/endpoints'
import { hasRole } from './session'

export const instance = reactive({ name: '', version: '' })

export async function loadInstance() {
  instance.name = location.hostname || 'incus-backup'
  try { instance.version = (await api.ping()).version } catch { /* shown as unknown */ }
  if (!hasRole('admin')) return
  try {
    const s = await api.settings.get()
    if (s.instance_name) instance.name = s.instance_name
  } catch { /* keep the fallback */ }
}
