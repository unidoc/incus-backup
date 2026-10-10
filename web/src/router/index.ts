import { createRouter, createWebHistory } from 'vue-router'
import { routes } from './routes'
import { session, initSession, hasRole } from '@/stores/session'

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: (to, from, saved) => saved ?? (to.path !== from.path ? { top: 0 } : undefined),
})

// Access rules, in order: first-run setup, sign-in, forced password change. A page above the user's role
// still opens (the shell renders a "no access" notice) so a shared link explains itself.
router.beforeEach(async (to) => {
  await initSession()
  if (session.setupNeeded && session.status !== 'authenticated') {
    return to.name === 'setup' ? true : { name: 'setup' }
  }
  if (to.name === 'setup') return session.status === 'authenticated' ? { name: 'dashboard' } : { name: 'login' }
  if (to.meta.public) {
    if (to.name === 'login' && session.status === 'authenticated') {
      const next = typeof to.query.next === 'string' && to.query.next.startsWith('/') ? to.query.next : '/'
      return next
    }
    return true
  }
  if (session.status !== 'authenticated') {
    return { name: 'login', query: to.fullPath !== '/' ? { next: to.fullPath } : {} }
  }
  if (session.user?.must_change_password && to.name !== 'change-password') {
    return { name: 'change-password', query: { next: to.fullPath } }
  }
  // the sign-in policy requires a second factor first (docs/authentication.md)
  if (session.enrol2fa && to.name !== 'enrol-2fa') return { name: 'enrol-2fa', query: to.fullPath !== '/' ? { next: to.fullPath } : {} }
  if (!session.enrol2fa && to.name === 'enrol-2fa') return '/'
  return true
})

router.afterEach((to) => {
  document.title = `${to.meta.title ?? 'incus-backup'} · incus-backup`
})

/** The page may be opened by the current user (role check only; the server enforces again). */
export function canOpen(name: string): boolean {
  const r = router.getRoutes().find((x) => x.name === name)
  return !r?.meta.role || hasRole(r.meta.role)
}
