// End to end against the REAL incus-backup-server (UI embedded with go:embed, HTTPS, CSP, cookies,
// CSRF), started on an empty PostgreSQL by global-setup.ts. One browser page walks through the whole
// life of a first administrator: setup, navigation, theme, 2FA enrolment, sign-in with TOTP and with a
// recovery code, and a session revoked from elsewhere.
import { test, expect, request as apiRequest, type Page, type BrowserContext } from '@playwright/test'
import { totp, currentStep } from './totp'
import { PASSWORD, REAL_URL } from './backend'
import { watchConsole, shot, setTheme } from './helpers'

// Sections whose placeholder was replaced by the real page (they are tested in their own specs).
const BUILT = new Set(['alerts', 'alert-silences', 'notifications', 'notification-rules', 'users', 'tokens', 'certificates', 'settings', 'audit'])

test.describe.configure({ mode: 'serial' })

let ctx: BrowserContext
let page: Page
let con: ReturnType<typeof watchConsole>
let secret = ''
let codes: string[] = []

test.beforeAll(async ({ browser }) => {
  ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 }, colorScheme: 'light' })
  page = await ctx.newPage()
  con = watchConsole(page)
})
test.afterAll(async () => { await ctx.close() })

async function signIn(name: string, password: string) {
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
}

test('first-run setup creates the administrator and signs in', async () => {
  await page.goto('/')
  await expect(page).toHaveURL(/\/setup$/)
  await expect(page.getByRole('heading', { name: 'Set up incus-backup' })).toBeVisible()
  await shot(page, 'setup-desktop-light')
  // a wrong token is refused with the server's reason
  await page.getByTestId('setup-token').fill('wrong-token-wrong-token')
  await page.locator('input[name=new-password]').fill(PASSWORD)
  await page.locator('input[name=confirm-password]').fill(PASSWORD)
  await page.getByRole('button', { name: /Create administrator/ }).click()
  await expect(page.getByTestId('setup-error')).toBeVisible()
  // the real token
  await page.getByTestId('setup-token').fill(process.env.IB_E2E_SETUP_TOKEN ?? '')
  await page.getByRole('button', { name: /Create administrator/ }).click()
  await expect(page).toHaveURL(`${REAL_URL}/`)
  await expect(page.getByTestId('headline')).toContainText('objects protected')
  // two repositories exist, so the selector is shown
  await expect(page.getByTestId('repo-select')).toBeVisible()
  await expect(page.getByTestId('session-role')).toHaveText('admin')
  await expect(page.locator('.live')).toHaveAttribute('data-state', 'live')
})

test('every section opens with its route and role', async () => {
  const sections: Array<[string, string]> = [
    ['hosts', 'readonly'], ['objects', 'readonly'], ['snapshots', 'readonly'], ['restore', 'restore'], ['verify', 'readonly'],
    ['jobs', 'readonly'], ['sync', 'readonly'], ['alerts', 'readonly'], ['repositories', 'readonly'], ['capacity', 'readonly'],
    ['policies', 'readonly'], ['notifications', 'operator'], ['users', 'admin'], ['certificates', 'readonly'], ['settings', 'admin'], ['audit', 'admin'],
  ]
  for (const [name, role] of sections) {
    await page.locator(`[data-nav="${name}"]`).click()
    await expect(page.locator(`[data-nav="${name}"]`)).toHaveAttribute('aria-current', 'page')
    await expect(page.locator('main h1').first(), name).toBeVisible()
    // a page still on its placeholder says which route and role it is for; finished pages have their own tests
    const ph = page.getByTestId('placeholder')
    if (await ph.count()) {
      await expect(ph).toHaveAttribute('data-route', name)
      await expect(page.getByTestId('required-role')).toHaveText(role)
    }
  }
  // the web listener reports its own certificate (cmd/incus-backup-server/web.go -> GET /certificates `web`)
  await page.goto('/settings/certificates')
  await expect(page.getByTestId('cert-web')).toContainText('Valid until')
  await page.locator('[data-nav="dashboard"]').click()
  await expect(page.getByTestId('headline')).toBeVisible()

  // deep links, including a dotted last segment, survive a reload (server-side SPA fallback)
  for (const [path, route] of [['/hosts/web01.example.com', 'host'], ['/objects/0123456789abcdef', 'object'], ['/snapshots/g1/20261008030000-000000d5/browse', 'snapshot-browse'], ['/policies/new', 'policy-new'], ['/settings/tokens', 'tokens'], ['/restore/tests', 'restore-tests'], ['/alerts/silences', 'alert-silences'], ['/notifications/rules', 'notification-rules'], ['/repositories/new', 'repository-new'], ['/hosts/onboard', 'host-onboard'], ['/jobs/42', 'job'], ['/sync/offsite', 'sync-job']]) {
    await page.goto(path)
    await expect(page.locator('main h1').first(), path).toBeVisible()
    if (await page.getByTestId('placeholder').count()) await expect(page.getByTestId('placeholder'), path).toHaveAttribute('data-route', route)
  }
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' })).toContainText('Sync')
  await page.goto('/hosts/web01.example.com')
  await expect(page.getByRole('navigation', { name: 'Breadcrumb' })).toContainText('web01.example.com')
  await page.goto('/no/such/page')
  await expect(page.getByTestId('not-found')).toBeVisible()
  // a missing asset is a real 404, not the app
  const r = await page.request.get('/assets/does-not-exist-123.js')
  expect(r.status()).toBe(404)
})

test('theme toggles and persists', async () => {
  await page.goto('/')
  const html = page.locator('html')
  await expect(html).toHaveAttribute('data-theme', 'light') // system = light in this context
  await page.getByTestId('theme-toggle').click() // system -> light
  await expect(html).toHaveAttribute('data-theme', 'light')
  await page.getByTestId('theme-toggle').click() // light -> dark
  await expect(html).toHaveAttribute('data-theme', 'dark')
  await page.reload()
  await expect(html).toHaveAttribute('data-theme', 'dark') // applied before paint by theme-init.js
  const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor)
  expect(bg).not.toBe('rgb(255, 255, 255)')
  await page.getByTestId('user-menu').click()
  await page.locator('[data-theme-choice="system"]').click()
  await expect(html).toHaveAttribute('data-theme', 'light')
  expect(await page.evaluate(() => localStorage.getItem('ib.theme'))).toBeNull()
  await page.keyboard.press('Escape')
})

test('command palette navigates to pages', async () => {
  await page.keyboard.press('Control+k')
  await page.getByTestId('palette-input').fill('audit')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/\/audit$/)
  await page.keyboard.press('/')
  await page.getByTestId('palette-input').fill('profile')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/\/profile$/)
})

test('two-factor enrolment end to end', async () => {
  await page.goto('/profile')
  await expect(page.getByTestId('totp-status')).toContainText('Off')
  await page.getByTestId('totp-enable').click()
  const key = page.getByTestId('totp-secret').getByTestId('copy-value')
  await expect(key).toHaveAttribute('data-value', /^[A-Z2-7]{16,}=*$/)
  secret = (await key.getAttribute('data-value')) ?? ''
  await expect(page.locator('.qr svg')).toBeVisible()
  // a wrong code is refused and the form stays
  await page.getByTestId('totp-confirm-code').fill('000000')
  await page.getByTestId('totp-confirm').click()
  await expect(page.locator('[data-testid=totp-panel] .err, [data-testid=totp-panel] [role=alert]').first()).toBeVisible()
  await shot(page, 'profile-2fa-scan-desktop-light')
  // the right code (computed here from the secret)
  await page.getByTestId('totp-confirm-code').fill(totp(secret, currentStep()))
  await page.getByTestId('totp-confirm').click()
  const list = page.getByTestId('recovery-codes').locator('ol li')
  await expect(list).toHaveCount(10)
  codes = await list.allInnerTexts()
  codes = codes.map((c) => c.trim())
  expect(codes.every((c) => /^[a-z2-7]{5}-[a-z2-7]{5}$/.test(c))).toBe(true)
  await shot(page, 'profile-recovery-codes-desktop-light')
  await expect(page.getByTestId('totp-done')).toBeDisabled()
  await page.getByTestId('codes-saved').check()
  await page.getByTestId('totp-done').click()
  await expect(page.getByTestId('totp-status')).toContainText('On')
  await expect(page.getByTestId('recovery-codes')).toHaveCount(0) // never shown again
})

test('sign out and back in with a TOTP code', async () => {
  await page.getByTestId('user-menu').click()
  await page.getByTestId('sign-out').click()
  await expect(page).toHaveURL(/\/login/)
  // wrong password first: a message, no navigation
  await signIn('admin', 'not-the-password-123')
  await expect(page.getByTestId('login-error')).toContainText('wrong')
  await signIn('admin', PASSWORD)
  await expect(page.getByTestId('totp-form')).toBeVisible()
  await shot(page, 'login-totp-desktop-light')
  // the code of the next 30 s step: the server accepts ±1 step and never a step already used
  await page.getByTestId('totp-code').fill(totp(secret, currentStep() + 1))
  await expect(page).toHaveURL(`${REAL_URL}/`)
  await expect(page.getByTestId('session-role')).toHaveText('admin')
})

test('sign in with a recovery code', async () => {
  await page.getByTestId('user-menu').click()
  await page.getByTestId('sign-out').click()
  await signIn('admin', PASSWORD)
  await page.getByTestId('use-recovery').click()
  await page.getByTestId('totp-code').fill(codes[0])
  await page.getByRole('button', { name: 'Verify' }).click()
  await expect(page).toHaveURL(`${REAL_URL}/`)
})

test('a revoked session sends the user to sign-in and back to the same page', async () => {
  await page.goto('/profile')
  await expect(page.getByRole('heading', { name: 'Profile' })).toBeVisible()
  // a second client (another device) signs in and revokes the browser's session on the server
  const api = await apiRequest.newContext({ baseURL: REAL_URL, ignoreHTTPSErrors: true })
  const login = await api.post('/api/v1/session', { data: { name: 'admin', password: PASSWORD, code: codes[1] } })
  expect(login.status()).toBe(200)
  const { csrf } = await login.json()
  const list = await (await api.get('/api/v1/me/sessions')).json() as Array<{ id: string; current: boolean }>
  const others = list.filter((s) => !s.current)
  expect(others.length).toBeGreaterThan(0)
  for (const s of others) expect((await api.delete(`/api/v1/me/sessions/${s.id}`, { headers: { 'X-CSRF-Token': csrf } })).status()).toBe(204)
  await api.dispose()
  // the next call from the page answers 401: back to sign-in, keeping the target
  await page.getByRole('tab', { name: 'Sessions' }).click()
  await expect(page).toHaveURL(/\/login\?next=(%2F|\/)profile/)
  await expect(page.getByTestId('session-expired')).toBeVisible()
  await shot(page, 'login-session-expired-desktop-light')
  await signIn('admin', PASSWORD)
  await page.getByTestId('use-recovery').click()
  await page.getByTestId('totp-code').fill(codes[2])
  await page.getByRole('button', { name: 'Verify' }).click()
  await expect(page).toHaveURL(`${REAL_URL}/profile`)
})

test('own API token: created, secret shown once, revoked with typed confirmation', async () => {
  await page.getByRole('tab', { name: 'API tokens' }).click()
  await page.getByTestId('token-new').click()
  await page.getByTestId('token-name').fill('e2e-monitoring')
  await page.getByTestId('token-create').click()
  const v = page.getByTestId('token-secret').getByTestId('copy-value')
  await expect(v).toHaveAttribute('data-value', /^ibt_/)
  const tok = (await v.getAttribute('data-value')) ?? ''
  await page.getByRole('button', { name: 'Done' }).click()
  await expect(page.getByRole('cell', { name: 'e2e-monitoring' })).toBeVisible()
  // the token works as a Bearer credential (no CSRF needed)
  const api = await apiRequest.newContext({ baseURL: REAL_URL, ignoreHTTPSErrors: true })
  expect((await api.get('/api/v1/session', { headers: { Authorization: `Bearer ${tok}` } })).status()).toBe(200)
  await page.getByRole('row', { name: /e2e-monitoring/ }).getByRole('button', { name: 'Revoke' }).click()
  await expect(page.getByTestId('confirm-button')).toBeDisabled()
  await page.getByTestId('confirm-input').fill('e2e-monitoring')
  await page.getByTestId('confirm-button').click()
  await expect(page.getByRole('cell', { name: 'e2e-monitoring' })).toHaveCount(0)
  expect((await api.get('/api/v1/session', { headers: { Authorization: `Bearer ${tok}` } })).status()).toBe(401)
  await api.dispose()
})

test('profile screenshots, light and dark', async () => {
  await page.getByRole('tab', { name: 'Password and 2FA' }).click()
  await shot(page, 'profile-desktop-light')
  await setTheme(page, 'dark')
  await shot(page, 'profile-desktop-dark')
  await setTheme(page, 'light')
  con.assertClean() // no CSP violation or console error during the whole run against the real server
})
