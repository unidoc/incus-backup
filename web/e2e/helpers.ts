import { expect, type Page, type ConsoleMessage } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath as toPath } from 'node:url'
const HERE = toPath(new URL('.', import.meta.url))

export const SHOTS = join(HERE, 'screenshots')
mkdirSync(SHOTS, { recursive: true })

/** Collects console errors and CSP violations; call assertClean() at the end of a test. */
export function watchConsole(page: Page) {
  const problems: string[] = []
  page.on('console', (m: ConsoleMessage) => {
    const t = m.text()
    if (/Content Security Policy|Refused to (load|execute|apply)/i.test(t)) problems.push(`CSP: ${t}`)
    else if (m.type() === 'error' && !/the server responded with a status of (401|403|404|409)/.test(t) && !/Failed to load resource/.test(t)) problems.push(`console.error: ${t}`)
  })
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`))
  return { problems, assertClean: () => expect(problems, problems.join('\n')).toEqual([]) }
}

export async function setTheme(page: Page, theme: 'light' | 'dark') {
  await page.evaluate((t) => { localStorage.setItem('ib.theme', t); document.documentElement.dataset.theme = t }, theme)
}

export async function shot(page: Page, name: string, opts: { fullPage?: boolean } = {}) {
  await page.waitForTimeout(250) // let transitions settle
  await page.screenshot({ path: join(SHOTS, `${name}.png`), fullPage: opts.fullPage ?? false, animations: 'disabled' })
}
