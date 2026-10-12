// Infrastructure & Overview pages against the seeded demo server (real server stack, real engine
// backups): onboarding with a REAL client enrolment, hosts (filters, bulk disable/enable, detail tabs,
// back up now, restore test, cancel), objects (filters, grouping, CSV, detail: calendar, notes,
// retention change and preview, transfer), capacity (filesystem + S3), the overview widgets, role
// limits, and screenshots in both themes at desktop and phone size.
import { test, expect, type Page, type Browser } from '@playwright/test'
import { execFile, execFileSync } from 'node:child_process'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { DEMO_PASSWORD, ROOT, STATE } from './backend'
import { shot, watchConsole } from './helpers'

test.describe.configure({ mode: 'serial' })

// the demo directory survives between runs (no --reset): a fresh host name each run
const NODE = `e2e-${Date.now().toString(36)}`

async function login(page: Page, name: string) {
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(DEMO_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('session-role')).not.toHaveText('')
  await page.waitForURL((u) => !u.pathname.startsWith('/login'))
}
const toast = (page: Page, text: string | RegExp) => expect(page.getByTestId('toast').filter({ hasText: text }).first()).toBeVisible()
async function typeConfirm(page: Page, text: string) {
  await page.getByTestId('confirm-input').fill(text)
  await page.getByTestId('confirm-button').click()
}
async function newPage(browser: Browser, user: string, opts: Parameters<Browser['newContext']>[0] = {}) {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, ...opts })
  const page = await ctx.newPage()
  const con = watchConsole(page)
  await login(page, user)
  return { ctx, page, con }
}

// the real host client, built from this tree once
const CLIENT = join(STATE, 'incus-backup-client')
test.beforeAll(() => {
  mkdirSync(STATE, { recursive: true })
  execFileSync('go', ['build', '-o', CLIENT, './cmd/incus-backup'], { cwd: ROOT, env: { ...process.env, CGO_ENABLED: '0' }, stdio: 'pipe', timeout: 300_000 })
})

// The demo's recovery key (printed in its banner): a further host of a repository must use the
// repository's key (one key domain per repository), so the client imports it before init.
function demoRecoveryKey(): string {
  const m = /Recovery key\s+(IBR1-[A-Z0-9-]+)/.exec(readFileSync(join(STATE, 'demo.log'), 'utf8'))
  if (!m) throw new Error('no recovery key in the demo banner (e2e/.state/demo.log)')
  return m[1]
}

function run(args: string[], input?: string): Promise<{ code: number | null; out: string }> {
  return new Promise((resolve) => {
    const p = execFile(CLIENT, args, { timeout: 60_000 }, (err, stdout, stderr) => {
      resolve({ code: err ? (typeof err.code === 'number' ? err.code : 1) : 0, out: `${stdout}\n${stderr}` })
    })
    if (input !== undefined) { p.stdin?.write(input); p.stdin?.end() }
  })
}

async function runClient(command: string, host: string): Promise<{ code: number | null; out: string }> {
  const dir = join(STATE, `client-${host}`)
  mkdirSync(dir, { recursive: true })
  const cfg = join(dir, 'client.yaml')
  writeFileSync(cfg, `host: ${host}\nstate_dir: ${dir}/state\nspool_dir: ${dir}/spool\nkey_file: ${dir}/key.json\n`)
  const imp = await run(['-c', cfg, 'key', 'import'], `${demoRecoveryKey()}\n`)
  expect(imp.code, imp.out).toBe(0)
  const args = command.trim().split(/\s+/)
  expect(args[0]).toBe('incus-backup')
  return run(['-c', cfg, ...args.slice(1)])
}

test('onboarding: token, real enrolment turns the wait green, next steps, re-enrol and revoke', async ({ browser }) => {
  test.setTimeout(120_000)
  const { ctx, page, con } = await newPage(browser, 'admin')
  await page.goto('/hosts')
  await page.getByTestId('add-host').click()
  await expect(page).toHaveURL(/\/hosts\/onboard$/)
  await expect(page.getByTestId('onb-submit')).toBeDisabled()
  await page.getByTestId('onb-name').fill('bad name')
  await page.getByTestId('onb-name').blur()
  await expect(page.getByText('A host name cannot contain spaces or slashes.')).toBeVisible()
  await page.getByTestId('onb-name').fill(NODE)
  await page.getByTestId('onb-ttl').selectOption('1')
  const resp = page.waitForResponse((r) => r.url().includes('/api/v1/onboarding?') && r.request().method() === 'POST')
  await page.getByTestId('onb-submit').click()
  const ob = await (await resp).json() as { command: string; token: string; expires_in_hours: number }
  expect(ob.expires_in_hours).toBe(1)
  await expect(page.getByTestId('onb-command')).toBeVisible()
  await expect(page.getByTestId('onb-cmd')).toContainText(ob.command)
  await expect(page.getByTestId('onb-token')).toContainText('Shown only now')
  await expect(page.getByTestId('onb-token')).not.toContainText(ob.token) // masked until revealed
  await expect(page.getByTestId('onb-wait')).toHaveAttribute('data-state', 'waiting')
  await expect(page.getByTestId('onb-pending')).toContainText(NODE)
  await shot(page, 'infra-onboarding-waiting-desktop-light')

  // the repository already has a key domain: the page says the host must bring that key
  await expect(page.getByTestId('onb-domain')).toContainText('incus-backup key import')
  // the exact command, run by the real client binary (scratch config, repository key imported first)
  const res = await runClient(ob.command, NODE)
  expect(res.code, res.out).toBe(0)
  expect(res.out).toContain('Enrolled at')
  await expect(page.getByTestId('onb-wait')).toHaveAttribute('data-state', 'enrolled', { timeout: 15_000 })
  await expect(page.getByTestId('onb-wait')).toContainText(`${NODE} enrolled`)
  await toast(page, `${NODE} enrolled`)
  await shot(page, 'infra-onboarding-enrolled-desktop-light')

  await page.getByTestId('onb-next').click()
  await expect(page.getByTestId('onb-next-steps')).toBeVisible()
  await page.getByTestId('onb-assign').click()
  await toast(page, new RegExp(`Policy .* assigned to ${NODE}`))
  await page.getByTestId('onb-first-backup').click()
  await toast(page, `First backup queued on ${NODE}`)
  await page.getByTestId('onb-key-stored').check()
  await shot(page, 'infra-onboarding-next-desktop-light')
  await page.getByTestId('onb-open-host').click()
  await expect(page).toHaveURL(new RegExp(`/hosts/${NODE}$`))
  await expect(page.getByTestId('host-state')).toContainText('online')

  // re-enrol an existing host: needs an explicit acknowledgement, shows up as a pending re-enrolment
  await page.goto('/hosts/onboard?host=bosch&replace=1')
  await expect(page.getByRole('heading', { name: 'Re-enrol or replace a host' })).toBeVisible()
  await expect(page.getByTestId('onb-replace')).toContainText('bosch is already enrolled')
  await expect(page.getByTestId('onb-submit')).toBeDisabled()
  await page.getByTestId('onb-replace-ok').check()
  await page.getByTestId('onb-submit').click()
  await expect(page.getByTestId('onb-command')).toBeVisible()
  await page.goto('/hosts/onboard')
  const row = page.getByTestId('onb-pending').locator('tr', { hasText: 'bosch' })
  await expect(row).toContainText('re-enrolment')
  await row.locator('[data-revoke="bosch"]').click()
  await page.getByTestId('confirm-button').click()
  await toast(page, 'Token for bosch revoked')
  await expect(page.getByTestId('onb-pending').locator('tr', { hasText: 'bosch' })).toHaveCount(0)
  con.assertClean()
  await ctx.close()
})

test('hosts: filters, bulk disable/enable with typed confirmation, detail tabs, back up now, restore test, cancel', async ({ browser }) => {
  test.setTimeout(120_000)
  const { ctx, page, con } = await newPage(browser, 'operator')
  await page.goto('/hosts')
  const table = page.getByTestId('host-table')
  for (const h of ['benfica', 'bosch', 'uni-02', NODE]) await expect(table.locator(`[data-host="${h}"]`)).toBeVisible()
  await expect(page.getByTestId('host-tiles')).toContainText('Offline')
  // the offline chip shows only the host that went silent
  await page.locator('[data-chip="offline"]').click()
  await expect(table.locator('[data-host="uni-02"]')).toBeVisible()
  await expect(table.locator('[data-host="bosch"]')).toHaveCount(0)
  await page.locator('[data-chip="all"]').click()
  await page.getByTestId('scope-filter').selectOption('backup')
  await expect(table.locator('tbody tr', { hasText: 'sync-' })).toHaveCount(0)

  // bulk disable needs the typed confirmation, then enable again
  await page.getByLabel(`Select custody/${NODE}/backup`).check()
  await page.getByTestId('bulk-disable').click()
  await expect(page.getByTestId('confirm-button')).toBeDisabled()
  await typeConfirm(page, NODE)
  await toast(page, `${NODE} disabled`)
  await expect(table.locator('tr', { hasText: NODE })).toContainText('disabled')
  await page.getByLabel(`Select custody/${NODE}/backup`).check()
  await page.getByTestId('bulk-enable').click()
  await toast(page, `${NODE} enabled`)
  await expect(table.locator('tr', { hasText: NODE })).toContainText('online')

  // detail page of a host with problems
  await table.locator('[data-host="bosch"]').click()
  await expect(page).toHaveURL(/\/hosts\/bosch$/)
  await expect(page.getByRole('heading', { name: /bosch/ })).toBeVisible()
  const objs = page.getByTestId('tab-objects')
  await expect(objs).toContainText('instance/db-01/root')
  await expect(objs.locator('[data-freshness="never"]')).toHaveCount(1)
  await expect(objs.locator('[data-freshness="stale"]').first()).toBeVisible()

  // back up now: one job per project, then a restore test
  await page.getByTestId('host-backup-now').click()
  await expect(page.getByTestId('backup-now')).toContainText('one job per project')
  await page.getByTestId('backup-now-submit').click()
  await toast(page, /backup jobs queued on bosch|Backup queued on bosch/)
  await page.getByTestId('host-restore-test').click()
  const sel = page.getByTestId('restore-test').locator('select')
  await sel.selectOption(await sel.locator('option', { hasText: 'db-01/root' }).getAttribute('value') ?? '')
  await page.getByTestId('restore-test-submit').click()
  await toast(page, 'Restore test queued on bosch')
  await page.locator('[data-tab="jobs"]').click()
  const jobs = page.getByTestId('tab-jobs')
  await expect(jobs).toContainText('Backup now')
  await expect(jobs).toContainText('Restore test')
  await expect(jobs.locator('tr', { hasText: 'Restore test' }).first()).toContainText('queued')
  await jobs.locator('tr', { hasText: 'Restore test' }).first().getByRole('button', { name: 'Cancel' }).click()
  await page.getByTestId('confirm-button').click()
  await toast(page, 'Job cancelled')
  await expect(jobs.locator('tr', { hasText: 'Restore test' }).first()).toContainText('cancelled')

  await page.locator('[data-tab="runs"]').click()
  await expect(page.getByTestId('tab-runs').locator('tbody tr').first()).toBeVisible()
  await expect(page.getByTestId('tab-runs')).toContainText('partial')
  await page.locator('[data-tab="certificate"]').click()
  await expect(page.getByTestId('tab-certificate')).toContainText('days left')
  await page.locator('[data-tab="activity"]').click()
  await expect(page.getByTestId('tab-activity')).toContainText('queued by operator')
  await expect(page).toHaveURL(/tab=activity/)

  // danger zone with typed confirmation
  await page.goto(`/hosts/${NODE}?tab=danger`)
  await page.getByTestId('danger-disable').click()
  await typeConfirm(page, NODE)
  await toast(page, `${NODE} disabled`)
  await expect(page.getByTestId('host-state')).toContainText('disabled')
  await page.getByTestId('danger-enable').click()
  await toast(page, `${NODE} enabled`)
  await expect(page.getByTestId('host-state')).toContainText('online')
  con.assertClean()
  await ctx.close()
})

test('objects: freshness and kind filters, grouping, CSV; detail: calendar, snapshots, notes, retention change', async ({ browser }) => {
  test.setTimeout(90_000)
  const { ctx, page, con } = await newPage(browser, 'operator')
  await page.goto('/objects')
  const table = page.getByTestId('object-table')
  await expect(table.locator('[data-object="instance/db-01/root"]')).toBeVisible()
  await expect(table.locator('[data-object="instance/db-01"]')).toHaveCount(0) // instance records hidden by default
  await page.locator('[data-chip="stale"]').click()
  await expect(page).toHaveURL(/freshness=stale/)
  const stale = await table.locator('[data-freshness="stale"]').count()
  expect(stale).toBeGreaterThanOrEqual(3)
  await expect(table.locator('tbody tr')).toHaveCount(stale)
  await page.locator('[data-chip="all"]').click()
  await page.getByTestId('f-kind').selectOption('volume')
  await expect(table.locator('tbody tr')).toHaveCount(1)
  await expect(table).toContainText('volume/nextcloud-data')
  await page.getByTestId('f-kind').selectOption('data')
  await page.getByTestId('f-group').selectOption('host')
  await expect(page.locator('[data-group="bosch"]')).toBeVisible()
  await expect(page.locator('[data-group="uni-02"]')).toContainText('overdue')
  const dl = page.waitForEvent('download')
  await page.getByTestId('export-csv').click()
  const csv = readFileSync(await (await dl).path(), 'utf8')
  expect(csv.split('\r\n')[0]).toBe('Object,Kind,Host,Project,Policy,Retention,Snapshots,Latest,Size,Freshness,Protected,Notes,Group id')
  expect(csv).toContain('instance/db-01/root')
  await page.getByTestId('f-group').selectOption('none')

  await table.locator('[data-object="instance/db-01/root"]').click()
  await expect(page.getByRole('heading', { name: /instance\/db-01\/root/ })).toBeVisible()
  expect(await page.getByTestId('obj-calendar').locator('.cell[data-count]:not([data-count="0"])').count()).toBeGreaterThanOrEqual(4)
  await expect(page.getByTestId('obj-calendar')).toContainText('failed verification')
  await expect(page.getByTestId('obj-snapshots')).toContainText('protected')
  await expect(page.getByTestId('obj-retention')).toContainText('needs the admin role')
  // notes
  const note = `checked by e2e ${Date.now()}`
  await page.getByTestId('obj-notes-input').fill(note)
  await page.getByTestId('obj-notes-save').click()
  await toast(page, 'Notes saved')
  await page.reload()
  await expect(page.getByTestId('obj-notes-input')).toHaveValue(note)
  // retention change, typed, then back
  await page.getByRole('button', { name: 'More' }).click()
  await page.getByTestId('menu-retention').click()
  await page.getByTestId('ret-select').selectOption('weekly-offsite')
  await page.getByTestId('ret-apply').click()
  await typeConfirm(page, 'instance/db-01/root')
  await toast(page, 'Retention of instance/db-01/root set to weekly-offsite')
  await expect(page.getByTestId('obj-retention')).toContainText('weekly-offsite')
  await page.getByTestId('obj-retention').getByRole('button', { name: 'Change' }).click()
  await page.getByTestId('ret-select').selectOption('nightly')
  await page.getByTestId('ret-apply').click()
  await typeConfirm(page, 'instance/db-01/root')
  await toast(page, 'set to nightly')
  con.assertClean()
  await ctx.close()
})

test('object detail as admin: retention preview is the real plan; transfer to another host and back', async ({ browser }) => {
  const { ctx, page, con } = await newPage(browser, 'admin')
  await page.goto('/objects')
  await page.getByTestId('object-table').locator('[data-object="instance/redis/root"]').click()
  await page.getByTestId('obj-preview').click()
  const plan = page.getByTestId('obj-plan')
  await expect(plan).toContainText('kept')
  expect(await plan.locator('li').count()).toBeGreaterThan(0)
  await page.getByTestId('obj-transfer').click()
  await page.getByTestId('tr-select').selectOption('benfica')
  await page.getByTestId('tr-apply').click()
  await typeConfirm(page, 'instance/redis/root')
  await toast(page, 'instance/redis/root now belongs to benfica')
  await expect(page.locator('.lede')).toContainText('benfica')
  await page.getByTestId('obj-transfer').click()
  await page.getByTestId('tr-select').selectOption('bosch')
  await page.getByTestId('tr-apply').click()
  await typeConfirm(page, 'instance/redis/root')
  await toast(page, 'now belongs to bosch')
  con.assertClean()
  await ctx.close()
})

test('viewer: capacity (filesystem and S3), overview widgets, and no operator controls', async ({ browser }) => {
  const { ctx, page, con } = await newPage(browser, 'viewer')
  // overview
  await page.goto('/')
  await expect(page.getByTestId('host-strip').locator('[data-host="uni-02"]')).toHaveAttribute('data-status', 'critical')
  await expect(page.getByTestId('host-strip').locator('[data-host="benfica"]')).toHaveAttribute('data-status', 'ok')
  await expect(page.locator('[data-repo="custody"] [data-testid="repo-health"]')).toContainText('Critical')
  await expect(page.getByTestId('dash-jobs')).toContainText('Backup now')
  await expect(page.getByTestId('dash-alerts')).toBeVisible()
  // capacity
  await page.goto('/capacity')
  await expect(page.getByTestId('cap-tiles')).toContainText('Stored')
  await expect(page.getByTestId('cap-forecast')).toContainText('80 % full')
  expect(await page.getByTestId('cap-top').locator('tbody tr').count()).toBeGreaterThan(5)
  await expect(page.getByTestId('cap-hosts')).toContainText('bosch')
  const req = page.waitForRequest((r) => r.url().includes('/api/v1/capacity?') && r.url().includes('repository=offsite'))
  const offsite = page.getByTestId('cap-cards').locator('[data-repo="offsite"]')
  await offsite.click()
  await req
  // the e2e demo runs "offsite" on S3 only when it was given MinIO; each backend has its own forecast text
  if (/S3/.test(await offsite.innerText())) await expect(page.getByTestId('cap-forecast')).toContainText('No free-space forecast for object storage')
  else await expect(page.getByTestId('cap-forecast')).toContainText('80 % full')
  await expect(offsite).toHaveAttribute('aria-pressed', 'true')
  // no operator controls
  await page.goto('/hosts')
  await expect(page.getByTestId('host-table').locator('[data-host="bosch"]')).toBeVisible()
  await expect(page.getByLabel('Select all rows shown')).toHaveCount(0)
  await expect(page.getByTestId('row-backup-now')).toHaveCount(0)
  await expect(page.getByTestId('add-host')).toHaveCount(0)
  await page.goto('/hosts/bosch')
  await expect(page.getByTestId('host-backup-now')).toBeDisabled()
  await expect(page.locator('[data-tab="danger"]')).toHaveCount(0)
  await page.goto('/hosts/onboard')
  await expect(page.getByTestId('no-access')).toContainText('admin role')
  await page.goto('/objects')
  await page.getByTestId('object-table').locator('[data-object="instance/db-01/root"]').click()
  await expect(page.getByTestId('obj-notes-input')).toBeDisabled()
  await expect(page.getByTestId('obj-backup-now')).toBeDisabled()
  con.assertClean()
  await ctx.close()
})

const PAGES: Array<[string, string, (p: Page) => Promise<void>]> = [
  ['overview', '/', async (p) => { await expect(p.getByTestId('host-strip')).toBeVisible() }],
  ['hosts', '/hosts', async (p) => { await expect(p.getByTestId('host-table').locator('[data-host="bosch"]')).toBeVisible() }],
  ['host-objects', '/hosts/bosch', async (p) => { await expect(p.getByTestId('tab-objects')).toContainText('instance/db-01/root') }],
  ['host-runs', '/hosts/bosch?tab=runs', async (p) => { await expect(p.getByTestId('tab-runs').locator('tbody tr').first()).toBeVisible() }],
  ['host-certificate', '/hosts/bosch?tab=certificate', async (p) => { await expect(p.getByTestId('tab-certificate')).toContainText('days left') }],
  ['host-jobs', '/hosts/bosch?tab=jobs', async (p) => { await expect(p.getByTestId('tab-jobs')).toContainText('Backup now') }],
  ['host-activity', '/hosts/bosch?tab=activity', async (p) => { await expect(p.getByTestId('tab-activity')).toContainText('Backup run') }],
  ['host-danger', '/hosts/uni-02?tab=danger', async (p) => { await expect(p.getByTestId('tab-danger')).toBeVisible() }],
  ['onboarding', '/hosts/onboard', async (p) => { await expect(p.getByTestId('onb-form')).toBeVisible() }],
  ['objects', '/objects', async (p) => { await expect(p.getByTestId('object-table').locator('[data-object="instance/db-01/root"]')).toBeVisible() }],
  ['objects-grouped', '/objects?group=host', async (p) => { await expect(p.locator('[data-group="bosch"]')).toBeVisible() }],
  ['capacity', '/capacity', async (p) => { await expect(p.getByTestId('cap-forecast')).toContainText('Growth') }],
]

for (const theme of ['light', 'dark'] as const) {
  test(`infrastructure screenshots, ${theme}`, async ({ browser }) => {
    test.setTimeout(180_000)
    const { ctx, page, con } = await newPage(browser, 'admin', { colorScheme: theme })
    for (const vp of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'mobile', width: 390, height: 844 }]) {
      await page.setViewportSize({ width: vp.width, height: vp.height })
      for (const [name, path, ready] of PAGES) {
        await page.goto(path)
        await ready(page)
        await page.waitForLoadState('networkidle')
        await shot(page, `infra-${name}-${vp.name}-${theme}`)
        if (vp.name === 'desktop' && ['hosts', 'host-objects', 'objects', 'capacity', 'overview'].includes(name)) await shot(page, `infra-${name}-full-${vp.name}-${theme}`, { fullPage: true })
        if (vp.name === 'mobile') expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${name}: no horizontal page scroll`).toBe(true)
        if (vp.name === 'desktop' && ['hosts', 'host-objects', 'objects'].includes(name)) {
          const over = await page.evaluate(() => [...document.querySelectorAll('.dt .scroll')].map((e) => e.scrollWidth - e.clientWidth).filter((d) => d > 1))
          expect(over, `${name}: every column, including the actions, fits at 1440 px`).toEqual([])
        }
      }
      // the object detail and the two dialogs
      await page.goto('/objects')
      await page.getByTestId('object-table').locator('[data-object="instance/db-01/root"]').click()
      await expect(page.getByTestId('obj-calendar')).toBeVisible()
      await page.waitForLoadState('networkidle')
      await shot(page, `infra-object-${vp.name}-${theme}`)
      if (vp.name === 'desktop') await shot(page, `infra-object-full-${vp.name}-${theme}`, { fullPage: true })
      if (vp.name === 'mobile') expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), 'object: no horizontal page scroll').toBe(true)
      await page.goto('/hosts/bosch')
      await page.getByTestId('host-backup-now').click()
      await expect(page.getByTestId('backup-now')).toBeVisible()
      await shot(page, `infra-dialog-backup-now-${vp.name}-${theme}`)
      await page.keyboard.press('Escape')
      await page.getByTestId('host-restore-test').click()
      await expect(page.getByTestId('restore-test')).toBeVisible()
      await shot(page, `infra-dialog-restore-test-${vp.name}-${theme}`)
      await page.keyboard.press('Escape')
    }
    con.assertClean()
    await ctx.close()
  })
}
