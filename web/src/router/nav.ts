// The sidebar: groups of entries pointing at route names. The role shown/hidden comes from the route's
// meta.role; `match` lists other route names that highlight the entry.
import type { IconName } from '@/components/icons'

export interface NavItem { route: string; label: string; icon: IconName }
export interface NavGroup { label: string; items: NavItem[] }

export const nav: NavGroup[] = [
  { label: '', items: [{ route: 'dashboard', label: 'Overview', icon: 'layout-dashboard' }] },
  { label: 'Infrastructure', items: [
    { route: 'hosts', label: 'Hosts', icon: 'server' },
    { route: 'objects', label: 'Objects', icon: 'box' },
  ] },
  { label: 'Protection', items: [
    { route: 'snapshots', label: 'Snapshots', icon: 'history' },
    { route: 'restore', label: 'Restore', icon: 'archive-restore' },
    { route: 'verify', label: 'Verification', icon: 'shield-check' },
  ] },
  { label: 'Operations', items: [
    { route: 'tasks', label: 'Tasks', icon: 'layers' },
    { route: 'jobs', label: 'Jobs', icon: 'activity' },
    { route: 'sync', label: 'Sync', icon: 'refresh-cw' },
    { route: 'alerts', label: 'Alerts', icon: 'bell' },
    { route: 'traffic', label: 'Traffic control', icon: 'arrow-up-down' },
  ] },
  { label: 'Storage', items: [
    { route: 'repositories', label: 'Repositories', icon: 'database' },
    { route: 'capacity', label: 'Capacity', icon: 'gauge' },
  ] },
  { label: 'Configuration', items: [
    { route: 'policies', label: 'Policies', icon: 'calendar-clock' },
    { route: 'notifications', label: 'Notifications', icon: 'send' },
    { route: 'users', label: 'Users & tokens', icon: 'users' },
    { route: 'authentication', label: 'Authentication', icon: 'key-round' },
    { route: 'certificates', label: 'Certificates', icon: 'badge-check' },
    { route: 'settings', label: 'Settings', icon: 'settings' },
    { route: 'system-status', label: 'Server status', icon: 'monitor' },
    { route: 'api-explorer', label: 'API', icon: 'command' },
  ] },
  { label: '', items: [{ route: 'audit', label: 'Audit log', icon: 'scroll-text' }] },
]
