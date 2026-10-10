// Alerts, notifications and administration against the seeded demo server. The delivery path is proven
// end to end with a real receiver: this test starts an HTTP listener, creates a webhook channel to it in
// the UI, sends a test (the listener's refusal comes back verbatim), creates a rule in the UI, has the
// engine evaluate (POST /notifications/tick) and waits for the firing message, whose link opens the
// alert page. Then acknowledge/undo, silence from an alert, the live update over SSE, users (last-admin
// protection), tokens, certificates, audit, settings and the setup checklist, roles, and screenshots of
// every page in both themes at desktop and phone size.
import { test, expect, type Page, type APIRequestContext } from '@playwright/test'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { DEMO_PASSWORD } from './backend'
import { shot, watchConsole } from './helpers'

test.describe.configure({ mode: 'serial' })

const RUN = Date.now().toString(36).slice(-5)
const CHANNEL = `e2e-hook-${RUN}`
const RULE = `e2e-rule-${RUN}`
const USER = `e2e-user-${RUN}`

interface Got { kind: string; subject: string; link: string; alert?: { id: number; type: string } }
const got: Got[] = []
let answer = 200
let srv: Server
let hookURL = ''

test.beforeAll(async () => {
  srv = createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      try { got.push(JSON.parse(body)) } catch { got.push({ kind: 'unparsable', subject: body, link: '' }) }
      res.writeHead(answer, { 'Content-Type': 'text/plain' })
      res.end(answer === 200 ? 'ok' : 'token rejected by e2e receiver')
    })
  })
  await new Promise<void>((ok) => srv.listen(0, '127.0.0.1', () => ok()))
  hookURL = `http://127.0.0.1:${(srv.address() as AddressInfo).port}/hook?token=e2e-secret`
})
test.afterAll(async () => { await new Promise((ok) => srv.close(ok)) })

async function login(page: Page, name: string) {
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(DEMO_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('session-role')).not.toHaveText('')
}

/** API call in the page's session (cookie + CSRF), as the console makes it. */
async function call(req: APIRequestContext, method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, data?: unknown) {
  const s = await (await req.get('/api/v1/session')).json()
  const r = await req.fetch(`/api/v1${path}`, { method, data, headers: { 'X-CSRF-Token': s.csrf } })
  expect(r.status(), `${method} ${path}: ${await r.text()}`).toBeLessThan(300)
  return r.status() === 204 ? null : r.json()
}

let page: Page
let con: ReturnType<typeof watchConsole>

test('admin signs in; the setup checklist and the alert badge show', async ({ browser }) => {
  page = await browser.newPage()
  con = watchConsole(page)
  await login(page, 'admin')
  await expect(page.getByTestId('setup-checklist')).toBeVisible()
  await expect(page.getByTestId('alert-badge')).toHaveText(/\d+/)
})

test('server settings: base URL with validation, about card', async () => {
  await page.goto('/settings')
  await page.getByTestId('set-base-url').fill('ftp://x')
  await expect(page.getByText('A full address, like').or(page.getByText('Only scheme and host'))).toBeVisible()
  await expect(page.getByTestId('set-save')).toBeDisabled()
  const base = new URL(page.url()).origin
  await page.getByTestId('set-base-url').fill(base + '/') // differs from the stored value on reruns; saved without the slash
  await page.getByTestId('set-save').click()
  await expect(page.getByText('Settings saved')).toBeVisible()
  await expect(page.locator('[data-item="base_url"]')).toHaveAttribute('data-ok', 'true')
  await expect(page.getByTestId('about')).toContainText('reachable')
})

test('webhook channel: create, real test message, refused test shows the receiver answer', async () => {
  await page.goto('/notifications')
  await page.getByTestId('channel-new').click()
  const form = page.getByTestId('channel-form')
  await form.locator('[data-kind="webhook"]').click()
  await page.getByTestId('ch-name').fill(CHANNEL)
  await form.locator('input[name="webhook-url"]').fill(hookURL)
  await page.getByTestId('ch-save').click()
  await expect(page.getByText('Channel created')).toBeVisible()
  const row = page.getByTestId('channel-table').locator('tr', { hasText: CHANNEL })
  await expect(row).toBeVisible()
  await expect(row).not.toContainText('e2e-secret') // the URL is a secret: never shown back
  await expect(row).toContainText('secret')

  await page.getByTestId(`ch-test-${CHANNEL}`).click()
  await expect(page.getByTestId('test-result')).toContainText('accepted')
  expect(got.at(-1)?.kind).toBe('test')
  await page.keyboard.press('Escape')

  answer = 401
  await page.getByTestId(`ch-test-${CHANNEL}`).click()
  await expect(page.getByTestId('test-error')).toContainText('401')
  await expect(page.getByTestId('test-error')).toContainText('token rejected by e2e receiver')
  await expect(page.getByTestId('test-error')).not.toContainText('e2e-secret')
  answer = 200
  await page.keyboard.press('Escape')

  // edit keeps the stored secret unless it is replaced
  await page.getByTestId(`ch-edit-${CHANNEL}`).click()
  await expect(page.getByTestId('secret-box')).toContainText('A secret is stored')
  await expect(page.getByTestId('secret-keep')).toHaveAttribute('aria-pressed', 'true')
  await page.getByTestId('ch-save').click()
  await expect(page.getByText('Channel saved')).toBeVisible()
})

test('rule: create in the editor, the simulator names the channel', async () => {
  await page.goto('/notifications/rules')
  await page.getByTestId('rule-new').click()
  await page.getByTestId('rule-name').fill(RULE)
  await page.getByTestId('rule-channels').locator(`[data-channel="${CHANNEL}"]`).check()
  await page.getByTestId('rule-save').click()
  await expect(page.getByText('Rule created')).toBeVisible()
  await expect(page.getByTestId('rule-table')).toContainText(RULE)

  await page.getByTestId('sim-type').selectOption('host_offline')
  await page.getByTestId('sim-run').click()
  await expect(page.getByTestId('sim-result')).toContainText(RULE)
  await expect(page.getByTestId('sim-result')).toContainText(CHANNEL)
})

test('an alert reaches the receiver; its link opens the alert page; the delivery log shows it', async () => {
  const deadline = Date.now() + 90_000
  let firing: Got | undefined
  while (!(firing = got.find((g) => g.kind === 'firing'))) {
    expect(Date.now(), `no firing message; receiver got ${JSON.stringify(got.map((g) => g.kind))}`).toBeLessThan(deadline)
    await call(page.request, 'POST', '/notifications/tick') // may answer skipped while the daemon's tick holds the lock
    await page.waitForTimeout(1000)
  }
  expect(firing.alert?.id).toBeGreaterThan(0)
  expect(firing.link).toMatch(new RegExp(`/alerts/${firing.alert!.id}$`))
  await page.goto(new URL(firing.link).pathname)
  await expect(page.getByTestId('alert-detail')).toBeVisible()
  await expect(page.getByTestId('alert-timeline')).toContainText('fired')
  await expect(page.getByTestId('alert-deliveries')).toContainText(CHANNEL)

  await page.goto('/notifications/deliveries')
  const row = page.getByTestId('delivery-table').locator('tr', { hasText: firing.subject.slice(0, 30) }).first()
  await expect(row).toContainText('sent')
  await expect(row).toContainText(CHANNEL)
  await row.click()
  await expect(page.getByTestId('delivery-detail')).toBeVisible()
  await page.keyboard.press('Escape')
})

test('acknowledge with a comment, undo, live update over SSE, silence from an alert', async () => {
  // silences left by an earlier, interrupted run would hide the alerts this test acts on
  for (const s of (await call(page.request, 'GET', '/silences') as { id: number; reason: string }[]).filter((x) => x.reason.startsWith('e2e silence'))) await call(page.request, 'DELETE', `/silences/${s.id}`)
  await page.goto('/alerts')
  const table = page.getByTestId('alert-table')
  const first = table.locator('tbody tr').filter({ hasText: 'firing' }).filter({ has: page.getByTestId('row-ack') }).first()
  const title = (await first.locator('.cx-title .t').first().textContent())!.trim()
  await first.getByTestId('row-ack').click()
  await page.getByTestId('ack-comment').fill('e2e: looking at it')
  await page.getByTestId('ack-submit').click()
  const acked = table.locator('tbody tr', { hasText: title }).first()
  await expect(acked).toContainText('acknowledged')
  await expect(acked).toContainText('by admin')
  await acked.getByTestId('row-unack').click()
  await expect(acked).toContainText('firing')

  // a change made elsewhere (API) appears without reloading: the 'alert' SSE event refetches
  const list = await call(page.request, 'GET', '/alerts?active=1&limit=1000') as { id: number; title: string; state: string }[]
  const target = list.find((a) => a.title === title && a.state === 'firing')!
  await call(page.request, 'POST', `/alerts/${target.id}/ack`, { comment: 'from the API' })
  await expect(acked).toContainText('acknowledged', { timeout: 15_000 })
  await call(page.request, 'POST', `/alerts/${target.id}/ack`, { undo: true })
  await expect(acked).toContainText('firing', { timeout: 15_000 })

  // the drawer: what to do + timeline
  await acked.locator('.cx-title .t').first().click()
  await expect(page.getByTestId('alert-detail')).toContainText('What to do')
  await expect(page.getByTestId('alert-timeline')).toContainText('acknowledged')
  await shot(page, 'cfg-alert-drawer-desktop-light')
  await page.getByTestId('detail-silence').click()
  await expect(page.getByTestId('silence-preview')).toContainText('1 open alert')
  await page.getByTestId('sil-reason').fill(`e2e silence ${RUN}`)
  await page.getByTestId('silence-submit').click()
  await expect(page.getByText('Silence created')).toBeVisible()
  await expect(page.getByTestId('alert-state')).toHaveText('silenced')
  await page.locator('dialog[open]').getByRole('button', { name: 'Close' }).click()
  await expect(table.locator('tbody tr', { hasText: title }).first()).toContainText('silenced', { timeout: 15_000 })

  await page.goto('/alerts/silences')
  const srow = page.getByTestId('silence-table').locator('tr', { hasText: `e2e silence ${RUN}` })
  await expect(srow).toContainText('active')
  await srow.getByTestId('silence-expire').click()
  await page.getByTestId('confirm-button').click()
  await expect(page.getByText('Silence expired')).toBeVisible()
})

test('alert conditions: change a threshold and restore it', async () => {
  await page.goto('/notifications/settings')
  await page.getByTestId('cond-edit-sync_lag').click()
  await page.getByTestId('cond-form').locator('[data-param="lag_hours"]').fill('72')
  await page.getByTestId('cond-save').click()
  await expect(page.locator('[data-condition="sync_lag"]')).toContainText('lag allowed 72 h (default 48)')
  await page.getByTestId('cond-edit-sync_lag').click()
  await page.getByTestId('cond-form').locator('[data-param="lag_hours"]').fill('48')
  await page.getByTestId('cond-save').click()
  await expect(page.locator('[data-condition="sync_lag"]')).toContainText('lag allowed 48 h')
  await expect(page.locator('[data-condition="sync_lag"]')).not.toContainText('default')
})

test('users: create, change role, disable, delete; the last admin is protected', async () => {
  await page.goto('/settings/users')
  await expect(page.getByTestId('role-panel')).toContainText('Administrator')
  await page.getByTestId('user-new').click()
  await page.getByTestId('user-name').fill(USER)
  await page.getByTestId('user-create').click()
  await expect(page.getByText(`${USER} was created`)).toBeVisible()
  await page.getByTestId('user-done').click()
  await page.getByTestId(`user-edit-${USER}`).click()
  await page.getByTestId('user-role').selectOption('readonly')
  await page.getByTestId('user-save').click()
  await expect(page.getByTestId('user-table').locator('tr', { hasText: USER })).toContainText('readonly')
  await page.getByTestId(`user-more-${USER}`).click()
  await page.getByTestId(`user-disable-${USER}`).click()
  await page.getByTestId('confirm-button').click()
  await expect(page.getByTestId('user-table').locator('tr', { hasText: USER })).toContainText('disabled')
  await page.getByTestId(`user-more-${USER}`).click()
  await page.getByTestId(`user-delete-${USER}`).click()
  await page.getByTestId('confirm-input').fill(USER)
  await page.getByTestId('confirm-button').click()
  await expect(page.getByText('User deleted')).toBeVisible()
  await expect(page.getByTestId('user-table')).not.toContainText(USER)
  // the only admin cannot be deleted or disabled from the console
  await page.getByTestId('user-more-admin').click()
  await expect(page.getByTestId('user-delete-admin')).toBeDisabled()
  await expect(page.getByTestId('user-disable-admin')).toBeDisabled()
  await page.keyboard.press('Escape')
})

test('tokens overview, certificates, audit log', async () => {
  await page.goto('/settings/tokens')
  await expect(page.getByText('How tokens work')).toBeVisible()
  await expect(page.getByText('Authorization: Bearer').first()).toBeVisible()

  await page.goto('/settings/certificates')
  await expect(page.getByTestId('cert-ca')).toContainText('days left')
  await expect(page.getByTestId('cert-hosts')).toContainText('bosch')
  await expect(page.getByTestId('cert-web')).toBeVisible()
  // the rotation is guarded by a typed confirmation (cancelled here: it would change the demo's CA)
  await page.getByTestId('ca-rotate').click()
  await expect(page.getByTestId('confirm-button')).toBeDisabled()
  await page.keyboard.press('Escape')

  await page.goto('/audit')
  await expect(page.getByTestId('audit-table')).toContainText('post /notifications/channels')
  await page.getByTestId('audit-action').fill('post /notifications/rules')
  await expect(page.getByTestId('audit-table').locator('tbody tr').first()).toContainText('post /notifications/rules')
  await page.getByTestId('audit-action').fill('')
  await page.getByTestId('audit-failed').click()
  await expect(page.getByText('The result filter applies to the entries loaded so far')).toBeVisible()
  for (const t of await page.getByTestId('audit-table').locator('tbody tr').allTextContents()) if (!/No entries/.test(t)) expect(t).toContain('failed')
  await page.getByRole('group', { name: 'Result' }).getByRole('button', { name: 'All', exact: true }).click()
  await page.getByTestId('audit-table').locator('tbody tr').first().click()
  await expect(page.getByTestId('audit-detail')).toBeVisible()
  await page.keyboard.press('Escape')
})

test('clean up the e2e channel and rule', async () => {
  const rules = await call(page.request, 'GET', '/notifications/rules') as { id: string; name: string }[]
  for (const r of rules.filter((x) => x.name.startsWith('e2e-rule-'))) await call(page.request, 'DELETE', `/notifications/rules/${r.id}`)
  // channels of earlier, interrupted runs go through the API; this run's through the UI
  const chans = await call(page.request, 'GET', '/notifications/channels') as { id: string; name: string }[]
  for (const c of chans.filter((x) => x.name.startsWith('e2e-hook-') && x.name !== CHANNEL)) await call(page.request, 'DELETE', `/notifications/channels/${c.id}`)
  await page.goto('/notifications')
  await page.getByRole('button', { name: `Delete ${CHANNEL}` }).click()
  await page.getByTestId('confirm-input').fill(CHANNEL)
  await page.getByTestId('confirm-button').click()
  await expect(page.getByText('Channel deleted')).toBeVisible()
  con.assertClean()
  await page.close()
})

test('roles: a viewer sees alerts without actions and no notification pages', async ({ page: p }) => {
  await login(p, 'viewer')
  await p.goto('/alerts')
  await expect(p.getByTestId('alert-table')).toBeVisible()
  await expect(p.getByTestId('tick')).toHaveCount(0)
  const ack = p.getByTestId('row-ack').first()
  if (await ack.count()) await expect(ack).toBeDisabled()
  await expect(p.locator('[data-nav="notifications"]')).toHaveCount(0)
  await p.goto('/notifications')
  await expect(p.getByTestId('no-access')).toContainText('operator')
  await p.goto('/')
  await expect(p.getByTestId('headline')).toBeVisible()
  await expect(p.getByTestId('setup-checklist')).toHaveCount(0)
})

const PAGES: [string, string][] = [
  ['alerts', '/alerts'], ['silences', '/alerts/silences'], ['channels', '/notifications'], ['rules', '/notifications/rules'],
  ['deliveries', '/notifications/deliveries'], ['conditions', '/notifications/settings'], ['users', '/settings/users'],
  ['tokens', '/settings/tokens'], ['certificates', '/settings/certificates'], ['audit', '/audit'], ['server', '/settings'],
]
for (const theme of ['light', 'dark'] as const) {
  test(`screenshots, ${theme}`, async ({ browser }) => {
    test.setTimeout(240_000)
    for (const vp of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'mobile', width: 390, height: 844 }]) {
      const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, colorScheme: theme, deviceScaleFactor: vp.name === 'mobile' ? 2 : 1 })
      const p = await ctx.newPage()
      const c = watchConsole(p)
      await login(p, 'admin')
      for (const [name, path] of PAGES) {
        await p.goto(path)
        await expect(p.locator('.page-head h1')).toBeVisible()
        await p.waitForLoadState('networkidle')
        await shot(p, `cfg-${name}-${vp.name}-${theme}`, { fullPage: true })
        expect(await p.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${path} scrolls sideways at ${vp.width}px`).toBe(true)
        // the shell clips sideways overflow (overflow-x: clip), so also check that nothing is cut off
        expect(await p.evaluate(() => { const m = document.querySelector('.main-col') as HTMLElement; return m.scrollWidth - m.clientWidth }), `${path} is wider than the screen at ${vp.width}px`).toBeLessThanOrEqual(1)
      }
      await p.goto('/notifications')
      await p.getByTestId('channel-new').click()
      await shot(p, `cfg-channel-editor-${vp.name}-${theme}`)
      await p.keyboard.press('Escape')
      await p.goto('/notifications/rules')
      await p.getByTestId('rule-new').click()
      await p.getByTestId('rule-all-types').click()
      await shot(p, `cfg-rule-editor-${vp.name}-${theme}`)
      await p.keyboard.press('Escape')
      await p.goto('/alerts')
      await p.getByTestId('alert-table').locator('tbody tr').first().click()
      await expect(p.getByTestId('alert-detail')).toBeVisible()
      await shot(p, `cfg-alert-drawer-${vp.name}-${theme}`)
      await p.keyboard.press('Escape')
      await p.goto('/')
      await expect(p.getByTestId('headline')).toBeVisible()
      await p.waitForLoadState('networkidle')
      await shot(p, `cfg-overview-${vp.name}-${theme}`, { fullPage: true })
      c.assertClean()
      await ctx.close()
    }
  })
}
