// Theme: 'system' (default, follows the OS), 'light' or 'dark'; the choice is kept in localStorage.
// public/theme-init.js applies it before the first paint; this store keeps it in sync afterwards.
import { reactive } from 'vue'

export type ThemePref = 'system' | 'light' | 'dark'
const KEY = 'ib.theme'

function read(): ThemePref {
  try {
    const v = localStorage.getItem(KEY)
    return v === 'light' || v === 'dark' ? v : 'system'
  } catch { return 'system' }
}

const mq = typeof window !== 'undefined' && window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null

export const theme = reactive({
  pref: read() as ThemePref,
  resolved: 'light' as 'light' | 'dark',
})

function apply() {
  theme.resolved = theme.pref === 'system' ? (mq?.matches ? 'dark' : 'light') : theme.pref
  document.documentElement.dataset.theme = theme.resolved
}

export function setTheme(p: ThemePref) {
  theme.pref = p
  try { p === 'system' ? localStorage.removeItem(KEY) : localStorage.setItem(KEY, p) } catch { /* storage blocked */ }
  apply()
}

/** Cycle system -> light -> dark -> system (the top-bar button). */
export function cycleTheme() {
  setTheme(theme.pref === 'system' ? 'light' : theme.pref === 'light' ? 'dark' : 'system')
}

export function initTheme() {
  apply()
  mq?.addEventListener?.('change', () => { if (theme.pref === 'system') apply() })
}
