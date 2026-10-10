// Passkeys / security keys and single sign-on through the REAL UI against the REAL server
// (global-setup.ts), docs/authentication.md:
//  * WebAuthn with Chrome's virtual authenticator (CDP WebAuthn.addVirtualAuthenticator): register a
//    key in the profile, sign in with the passkey (no user name), sign in with password + key.
//  * OIDC against a real local OpenID provider (internal/webfixture/testidp, mockoidc: discovery,
//    JWKS, PKCE, signed ID tokens) started here: add the provider in Settings → Authentication, sign
//    in through it (role from a group mapping), a refused sign-in, sign out.
//  * require_2fa: a user without a second factor is sent to the enrolment page and enrols a key.
//  * local_login off (guarded), and the break-glass CLI `user local-login on`.
// Everything runs on https://localhost (a passkey's relying party cannot be an IP address). The file
// sorts last: it changes server settings and restores them in afterAll.
import { test, expect, type Browser, type BrowserContext, type Page } from '@playwright/test'
import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { createInterface } from 'node:readline'
import { join } from 'node:path'
import { ROOT, STATE, REAL_PORT } from './backend'
import { watchConsole, shot, setTheme } from './helpers'

const PW = 'amber-signal-quarry-61'
const ORIGIN = `https://localhost:${REAL_PORT}`
const dir = join(STATE, 'real')
const bin = process.env.IB_SERVER_BIN ?? join(dir, 'incus-backup-server')
const cli = (args: string[], input?: string) => execFileSync(bin, ['-c', join(dir, 'server.yaml'), ...args], { input, encoding: 'utf8' })

let idp: ChildProcess | null = null
let idpInfo = { issuer: '', client_id: '', client_secret: '', control: '' }
let admin: Page

test.describe.configure({ mode: 'serial' })

// The server allows 20 sign-in attempts a minute per address; every context here is 127.0.0.1.
const coolDown = () => new Promise((r) => setTimeout(r, 61_000))

async function api(page: Page, method: string, path: string, body?: unknown): Promise<{ status: number; json: unknown }> {
  return page.evaluate(async ({ method, path, body }) => {
    const s = await fetch('/api/v1/session')
    const csrf = s.ok ? ((await s.json()) as { csrf: string }).csrf : ''
    const r = await fetch(`/api/v1${path}`, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: body === undefined ? undefined : JSON.stringify(body) })
    const t = await r.text()
    let json: unknown = null
    try { json = JSON.parse(t) } catch { json = t }
    return { status: r.status, json }
  }, { method, path, body })
}

async function sessionOf(page: Page): Promise<{ status: number; method?: string; name?: string; role?: string }> {
  return page.evaluate(async () => {
    const r = await fetch('/api/v1/session')
    if (!r.ok) return { status: r.status }
    const s = (await r.json()) as { method: string; user: { name: string; role: string } }
    return { status: r.status, method: s.method, name: s.user.name, role: s.user.role }
  })
}

async function context(browser: Browser, viewport = { width: 1440, height: 900 }, scheme: 'light' | 'dark' = 'light'): Promise<BrowserContext> {
  return browser.newContext({ ignoreHTTPSErrors: true, viewport, colorScheme: scheme, baseURL: ORIGIN })
}

/** A Chrome virtual authenticator (a platform passkey with user verification) on this page. */
async function virtualAuthenticator(page: Page) {
  const cdp = await page.context().newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } })
  return { cdp, authenticatorId }
}

async function passwordSignIn(page: Page, name: string) {
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(PW)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
}

async function signOut(page: Page) {
  await page.getByTestId('user-menu').click()
  await page.getByTestId('sign-out').click()
  await expect(page).toHaveURL(/\/login/)
  expect((await sessionOf(page)).status).toBe(401)
}

async function nextIdPUser(u: { sub: string; email: string; email_verified: boolean; groups: string[] }) {
  const r = await fetch(idpInfo.control, { method: 'POST', body: JSON.stringify(u) })
  expect(r.status).toBe(204)
}

test.beforeAll(async ({ browser }) => {
  test.setTimeout(240_000)
  for (const [name, role] of [['au-admin', 'admin'], ['au-key', 'operator'], ['au-ops', 'operator']]) {
    try { cli(['user', 'create', name, '--role', role, '--password-stdin'], PW + '\n') } catch (e) { if (!/exists/.test(String((e as { stderr?: string }).stderr ?? e))) throw e }
  }
  // the test identity provider (built from this tree)
  const idpBin = join(dir, 'testidp')
  execFileSync('go', ['build', '-o', idpBin, './internal/webfixture/testidp'], { cwd: ROOT, env: { ...process.env, CGO_ENABLED: '0' } })
  idp = spawn(idpBin, ['-listen', '127.0.0.1:0'], { stdio: ['ignore', 'pipe', 'inherit'] })
  const line = await new Promise<string>((ok, fail) => {
    const rl = createInterface({ input: idp!.stdout! })
    rl.once('line', ok)
    idp!.once('exit', () => fail(new Error('testidp exited')))
  })
  idpInfo = JSON.parse(line)
  await coolDown() // the specs before this one signed in many times from this address
  const ctx = await context(browser)
  admin = await ctx.newPage()
  await passwordSignIn(admin, 'au-admin')
  await expect(admin).not.toHaveURL(/login/)
  // passkeys and the OIDC redirect URI need the console address: this origin
  const r = await api(admin, 'PUT', '/settings', { base_url: ORIGIN })
  expect(r.status, JSON.stringify(r.json)).toBe(204)
})

test.afterAll(async () => {
  try { if (admin) await api(admin, 'PUT', '/auth/settings', { require_2fa: 'none' }) } catch { /* the page may be gone */ }
  try { cli(['user', 'local-login', 'on']) } catch { /* reported by the test that needs it */ }
  try { if (admin) await api(admin, 'PUT', '/settings', { base_url: '' }) } catch { /* ignore */ }
  idp?.kill('SIGTERM')
})

test('passkeys: register in the profile, sign in with the passkey, then password + key', async ({ browser }) => {
  test.setTimeout(120_000)
  const ctx = await context(browser)
  const page = await ctx.newPage()
  const con = watchConsole(page)
  const { cdp, authenticatorId } = await virtualAuthenticator(page)
  await passwordSignIn(page, 'au-key')
  await expect(page).not.toHaveURL(/login/)
  await page.goto('/profile')
  await expect(page.getByTestId('webauthn-panel')).toBeVisible()
  await page.getByTestId('key-add').click()
  await page.getByTestId('key-name').fill('Laptop passkey')
  await page.getByTestId('key-register').click()
  await expect(page.getByTestId('key-list')).toContainText('Laptop passkey')
  const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId })
  expect(credentials.length).toBe(1)
  expect(credentials[0].isResidentCredential).toBe(true)
  await shot(page, 'authn-profile-keys-light')
  await setTheme(page, 'dark')
  await shot(page, 'authn-profile-keys-dark')
  await setTheme(page, 'light')

  // sign out, then the passkey alone: no user name typed
  await signOut(page)
  await expect(page.getByTestId('passkey-signin')).toBeVisible()
  await page.getByTestId('passkey-signin').click()
  await expect(page).not.toHaveURL(/login/)
  let s = await sessionOf(page)
  expect(s).toMatchObject({ status: 200, method: 'passkey', name: 'au-key', role: 'operator' })
  await page.reload()
  expect((await sessionOf(page)).status).toBe(200)

  // password: the key is now the second step (started at once by the page)
  await signOut(page)
  await passwordSignIn(page, 'au-key')
  await expect(page).not.toHaveURL(/login/, { timeout: 15_000 })
  s = await sessionOf(page)
  expect(s).toMatchObject({ status: 200, method: 'password', name: 'au-key' })
  const { credentials: after } = await cdp.send('WebAuthn.getCredentials', { authenticatorId })
  expect(after[0].signCount).toBeGreaterThanOrEqual(2)
  await signOut(page)
  con.assertClean()
  await ctx.close()
})

test('single sign-on: add the provider, sign in through it, a refused sign-in, sign out', async ({ browser }) => {
  test.setTimeout(180_000)
  await coolDown()
  const con = watchConsole(admin)
  const bodies: string[] = []
  admin.on('response', async (r) => { if (r.url().includes('/api/')) bodies.push(await r.text().catch(() => '')) })
  await admin.goto('/settings/authentication')
  await expect(admin.getByRole('heading', { name: 'Authentication', exact: true })).toBeVisible()
  await admin.getByTestId('provider-new').click()
  await admin.getByTestId('prov-name').fill('e2e')
  await admin.getByTestId('prov-label').fill('E2E SSO')
  await admin.getByTestId('prov-issuer').fill(idpInfo.issuer)
  await admin.getByTestId('prov-client').fill(idpInfo.client_id)
  await admin.getByTestId('prov-secret').locator('input').fill(idpInfo.client_secret)
  await admin.getByLabel('Scopes', { exact: true }).fill('groups') // the groups claim needs its scope at this provider
  await admin.getByLabel('Scopes', { exact: true }).press('Enter')
  await admin.getByTestId('prov-default-role').selectOption('readonly')
  await admin.getByTestId('prov-autocreate').click()
  await admin.getByTestId('prov-add-role').click()
  await admin.getByLabel('Group 1', { exact: true }).fill('backup-admins')
  await admin.getByLabel('Role for group 1', { exact: true }).selectOption('admin')
  await shot(admin, 'authn-provider-editor')
  await admin.getByTestId('prov-save').click()
  await expect(admin.getByTestId('provider-result-e2e')).toContainText('Discovery and key set reachable', { timeout: 20_000 })
  await admin.getByTestId('provider-test-e2e').click()
  await expect(admin.getByTestId('provider-result-e2e')).toContainText('signing key')
  for (const theme of ['light', 'dark'] as const) {
    await setTheme(admin, theme)
    await shot(admin, `authn-settings-${theme}`, { fullPage: true })
  }
  await setTheme(admin, 'light')
  await admin.setViewportSize({ width: 390, height: 844 })
  await shot(admin, 'authn-settings-mobile', { fullPage: true })
  await admin.setViewportSize({ width: 1440, height: 900 })
  expect(bodies.length).toBeGreaterThan(3)
  for (const b of bodies) expect(b.includes(idpInfo.client_secret), 'the client secret came back in a response').toBe(false)

  // ada is in backup-admins at the provider: an administrator here, created at the first sign-in
  const ctx = await context(browser)
  const page = await ctx.newPage()
  const pc = watchConsole(page)
  await nextIdPUser({ sub: 'sub-ada', email: 'ada@example.com', email_verified: true, groups: ['backup-admins'] })
  await page.goto('/login?next=/settings/users')
  await expect(page.getByTestId('sso-e2e')).toBeVisible()
  await shot(page, 'authn-login-light')
  await page.getByTestId('sso-e2e').click()
  await expect(page).toHaveURL(/\/settings\/users$/)
  expect(await sessionOf(page)).toMatchObject({ status: 200, method: 'oidc', name: 'ada', role: 'admin' })
  await page.reload() // the Strict session cookie works after the cross-site redirect chain
  await expect(page.getByRole('heading', { name: 'Users', exact: true })).toBeVisible()
  await expect(page.getByTestId('user-table')).toContainText('SSO e2e')
  await shot(page, 'authn-users-light')
  await signOut(page)
  pc.assertClean()

  // bob has no mapped group: the default role (read-only), no administration
  await nextIdPUser({ sub: 'sub-bob', email: 'bob@example.com', email_verified: true, groups: [] })
  await page.getByTestId('sso-e2e').click()
  await expect(page).not.toHaveURL(/login/)
  expect(await sessionOf(page)).toMatchObject({ status: 200, method: 'oidc', name: 'bob', role: 'readonly' })
  expect((await api(page, 'GET', '/users')).status).toBe(403)
  await signOut(page)

  // an e-mail address the provider did not verify: refused, explained on the sign-in page
  await nextIdPUser({ sub: 'sub-eve', email: 'eve@example.com', email_verified: false, groups: ['backup-admins'] })
  await page.getByTestId('sso-e2e').click()
  await expect(page.getByTestId('sso-error')).toContainText('did not confirm your e-mail address')
  expect((await sessionOf(page)).status).toBe(401)
  await shot(page, 'authn-login-sso-error')
  await ctx.close()

  // the login page on a phone, light and dark
  for (const scheme of ['light', 'dark'] as const) {
    const m = await context(browser, { width: 390, height: 844 }, scheme)
    const mp = await m.newPage()
    await mp.goto('/login')
    await expect(mp.getByTestId('sso-e2e')).toBeVisible()
    await shot(mp, `authn-login-mobile-${scheme}`, { fullPage: true })
    await m.close()
  }
  con.assertClean()
})

test('require_2fa: a user without a second factor enrols a key before anything else', async ({ browser }) => {
  test.setTimeout(120_000)
  // through the UI: the administrator has no second factor either, so its own console goes to enrolment
  await admin.goto('/settings/authentication')
  await admin.getByTestId('require-all').click()
  await admin.getByRole('button', { name: 'Require it' }).click()
  await expect(admin).toHaveURL(/\/enrol-2fa/)
  expect((await api(admin, 'GET', '/repositories')).status).toBe(403)
  const ctx = await context(browser, { width: 1280, height: 860 })
  const page = await ctx.newPage()
  const con = watchConsole(page)
  await virtualAuthenticator(page)
  await passwordSignIn(page, 'au-ops')
  await expect(page).toHaveURL(/\/enrol-2fa/)
  await expect(page.getByTestId('enrol-choices')).toBeVisible()
  await shot(page, 'authn-enrol-light')
  await setTheme(page, 'dark')
  await shot(page, 'authn-enrol-dark')
  await setTheme(page, 'light')
  await page.getByTestId('key-add').click()
  await page.getByTestId('key-name').fill('Enrolled key')
  await page.getByTestId('key-register').click()
  await expect(page).not.toHaveURL(/enrol-2fa/, { timeout: 15_000 })
  expect((await api(page, 'GET', '/repositories')).status).toBe(200)
  con.assertClean()
  await ctx.close()
  // back to "not required" over the admin socket path's equivalent: the server CLI cannot (no socket in
  // the e2e config), so the admin enrols nothing and the policy is reset by the auth store directly
  // through the API of a session that is allowed: ada (SSO, the IdP owns the second factor)
  const sso = await (await context(browser)).newPage()
  await nextIdPUser({ sub: 'sub-ada', email: 'ada@example.com', email_verified: true, groups: ['backup-admins'] })
  await sso.goto('/login')
  await sso.getByTestId('sso-e2e').click()
  await expect(sso).not.toHaveURL(/login/)
  expect((await api(sso, 'PUT', '/auth/settings', { require_2fa: 'none' })).status).toBe(200)
  expect((await api(admin, 'GET', '/repositories')).status).toBe(200)
  await sso.context().close()
})

test('local sign-in off: single sign-on only, then the break-glass CLI turns it back on', async ({ browser }) => {
  test.setTimeout(120_000)
  await coolDown()
  await admin.goto('/settings/authentication')
  await admin.getByTestId('local-login').click()
  await admin.getByRole('button', { name: 'Turn off' }).click()
  await expect(admin.getByTestId('local-login')).toHaveAttribute('aria-checked', 'false')
  const ctx = await context(browser)
  const page = await ctx.newPage()
  await page.goto('/login')
  await expect(page.getByTestId('local-off')).toBeVisible()
  await expect(page.getByTestId('passkey-signin')).toHaveCount(0)
  await shot(page, 'authn-login-sso-only')
  const r = await page.evaluate(async (pw) => (await fetch('/api/v1/session', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: 'au-admin', password: pw }) })).status, PW)
  expect(r).toBe(403)
  // break-glass on the server
  expect(cli(['user', 'local-login', 'on'])).toContain('on')
  await passwordSignIn(page, 'au-admin')
  await expect(page).not.toHaveURL(/login/)
  await ctx.close()
})
