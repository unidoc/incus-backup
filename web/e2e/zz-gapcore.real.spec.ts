// Repository access, traffic control and the API explorer against the REAL server (global-setup.ts).
// Independent of real.spec.ts: it creates its own users with the server CLI.
import { test, expect, type Browser, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { join } from 'node:path'
import { REAL_URL, STATE } from './backend'
import { watchConsole, shot, setTheme } from './helpers'

const PW = 'violet-harbor-cinder-82'
const dir = join(STATE, 'real')
const bin = process.env.IB_SERVER_BIN ?? join(dir, 'incus-backup-server')
const cli = (args: string[], input?: string) => execFileSync(bin, ['-c', join(dir, 'server.yaml'), ...args], { input, encoding: 'utf8' })

test.describe.configure({ mode: 'serial' })

test.beforeAll(() => {
  for (const [name, role] of [['gc-admin', 'admin'], ['gc-scoped', 'none']]) {
    try { cli(['user', 'create', name, '--role', role, '--password-stdin'], PW + '\n') } catch (e) { if (!/exists/.test(String((e as { stderr?: string }).stderr ?? e))) throw e }
  }
})

async function signedIn(browser: Browser, name: string, viewport = { width: 1440, height: 900 }, scheme: 'light' | 'dark' = 'light'): Promise<Page> {
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport, colorScheme: scheme })
  const page = await ctx.newPage()
  await page.goto(`${REAL_URL}/login`)
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(PW)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).not.toHaveURL(/login/)
  return page
}

test('traffic control: add a rule in the editor, see it listed with its window state', async ({ browser }) => {
  const page = await signedIn(browser, 'gc-admin')
  const con = watchConsole(page)
  await page.goto(`${REAL_URL}/traffic`)
  await expect(page.getByRole('heading', { name: 'Traffic control' })).toBeVisible()
  await page.getByTestId('traffic-new').click()
  await page.getByTestId('traffic-name').fill('office-hours')
  await page.getByTestId('traffic-rate').fill('slow')
  await expect(page.getByTestId('traffic-save')).toBeDisabled()
  await page.getByTestId('traffic-rate').fill('20 MiB/s')
  await page.getByTestId('traffic-schedule').fill('mon-fri 08:00-18:00')
  await expect(page.getByTestId('traffic-preview')).toContainText('each host matching')
  await shot(page, 'gapcore-traffic-editor')
  await page.getByTestId('traffic-save').click()
  await expect(page.getByTestId('traffic-rules')).toContainText('office-hours')
  await expect(page.getByTestId('traffic-rules')).toContainText('20 MiB/s')
  for (const theme of ['light', 'dark'] as const) {
    await setTheme(page, theme)
    await shot(page, `gapcore-traffic-${theme}`)
  }
  con.assertClean()
  await page.context().close()
})

test('API explorer: routes by tag, try GET /ping with the session, curl snippet', async ({ browser }) => {
  const page = await signedIn(browser, 'gc-admin')
  const con = watchConsole(page)
  await page.goto(`${REAL_URL}/settings/api`)
  await page.getByTestId('api-filter').fill('ping')
  await page.getByTestId('api-op-get_ping').click()
  await expect(page.getByTestId('api-detail')).toContainText('Liveness and version')
  await page.getByTestId('api-send').click()
  await expect(page.getByTestId('api-result')).toContainText('200')
  await expect(page.getByTestId('api-result')).toContainText('"ok": true')
  await expect(page.locator('pre, code').filter({ hasText: 'Bearer $IB_TOKEN' }).first()).toBeVisible()
  // a writing route needs the explicit confirmation
  await page.getByTestId('api-filter').fill('traffic/rules')
  await page.getByTestId('api-op-post_traffic_rules').click()
  await expect(page.getByTestId('api-send')).toBeDisabled()
  await page.getByTestId('api-filter').fill('')
  for (const theme of ['light', 'dark'] as const) {
    await setTheme(page, theme)
    await shot(page, `gapcore-api-${theme}`)
  }
  con.assertClean()
  await page.context().close()
  const mobile = await signedIn(browser, 'gc-admin', { width: 390, height: 844 })
  await mobile.goto(`${REAL_URL}/settings/api`)
  await mobile.getByTestId('api-op-get_overview').click()
  await shot(mobile, 'gapcore-api-mobile')
  await mobile.context().close()
})

test('repository access: the admin scopes a user to one repository; that user sees only it', async ({ browser }) => {
  const page = await signedIn(browser, 'gc-admin')
  const con = watchConsole(page)
  await page.goto(`${REAL_URL}/settings/users`)
  await page.getByTestId('user-access-gc-scoped').click()
  await expect(page.getByTestId('access-dialog')).toBeVisible()
  await page.getByTestId('access-scoped').check()
  await page.getByTestId('access-role-archive').selectOption('operator')
  await expect(page.getByTestId('access-summary')).toContainText('Only 1 of 2 repositories')
  await shot(page, 'gapcore-access-dialog')
  await page.getByTestId('access-save').click()
  await expect(page.getByTestId('access-dialog')).toBeHidden()
  con.assertClean()
  await page.context().close()

  const sp = await signedIn(browser, 'gc-scoped')
  const spCon = watchConsole(sp)
  const r = await sp.request.get(`${REAL_URL}/api/v1/repositories`)
  expect((await r.json()).map((x: { name: string }) => x.name)).toEqual(['archive'])
  const denied = await sp.request.get(`${REAL_URL}/api/v1/objects?repository=primary`)
  expect(denied.status()).toBe(403)
  await sp.goto(`${REAL_URL}/hosts`)
  await shot(sp, 'gapcore-scoped-hosts')
  await expect(sp.getByTestId('session-role')).toHaveText('operator')
  await sp.goto(`${REAL_URL}/`)
  await expect(sp.getByTestId('headline')).toBeVisible()
  spCon.assertClean()
  await sp.context().close()
})
