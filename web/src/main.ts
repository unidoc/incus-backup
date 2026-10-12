import { createApp } from 'vue'
// Self-hosted fonts (no runtime CDN): latin subset, only the weights in use.
import '@fontsource/ibm-plex-sans/latin-400.css'
import '@fontsource/ibm-plex-sans/latin-500.css'
import '@fontsource/ibm-plex-sans/latin-600.css'
import '@fontsource/ibm-plex-mono/latin-400.css'
import '@fontsource/ibm-plex-mono/latin-500.css'
import './styles/tokens.css'
import './styles/base.css'
import App from './App.vue'
import { router } from './router'
import { initTheme } from './stores/theme'
import { onUnauthorized } from './api/client'
import { sessionLost, startIdleWatch } from './stores/session'
import { onSignedOut, stopLive } from './api/events'
import { resetRepos } from './stores/repos'
import { toast, push } from './stores/toast'

initTheme()

// A 401 from any authenticated call: the session ended (idle, revoked, server restart with a new key).
// Back to sign-in, keeping where the user was.
function toLogin(reason?: 'signed_out') {
  stopLive()
  resetRepos()
  const cur = router.currentRoute.value
  if (cur.meta.public) return
  void router.replace({ name: 'login', query: { next: cur.fullPath, expired: '1', ...(reason ? { reason } : {}) } })
}
onUnauthorized(() => { if (sessionLost()) toLogin() })
// the live event stream learned that this session ended (signed out elsewhere, revoked, account disabled)
onSignedOut(() => { if (sessionLost()) toLogin('signed_out') })
startIdleWatch(toLogin)

const app = createApp(App)
app.config.errorHandler = (err, _instance, info) => {
  console.error(err, info)
  toast.error(err, 'Unexpected error in the page')
}
router.onError((err: Error) => {
  // After an upgrade the old chunk names are gone: offer a reload instead of a dead click.
  if (/Failed to fetch dynamically imported module|Importing a module script failed|error loading dynamically imported module/i.test(err.message)) {
    push({ kind: 'warn', title: 'The console was updated', message: 'Reload to load the new version.', timeout: 0, action: { label: 'Reload', run: () => window.location.reload() } })
    return
  }
  toast.error(err, 'Could not open the page')
})
app.use(router)
app.mount('#app')
