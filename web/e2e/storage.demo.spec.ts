// Storage and protection configuration against the seeded demo server (the real server stack):
// repositories (list, detail tabs, create on a filesystem in a temp dir, S3 failure messaging and a
// real S3 repository on the in-process S3 test server (cmd/ibs3)), maintenance mode, schedules, tasks (verify, retention preview,
// GC dry run), policies (editor, preview, assignments, delete impact), sync (wizard with a real token,
// run, remove), role gating, and screenshots light/dark x desktop/phone into e2e/screenshots/.
// The demo is started with --reset for every run (backend.ts), so the repositories, policies and sync
// jobs created here never leak into the next run.
import { test, expect, type Page } from '@playwright/test'
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DEMO_PASSWORD } from './backend'
import { shot, setTheme, watchConsole } from './helpers'
import { startS3, type S3 } from './s3'

test.describe.configure({ mode: 'serial' })
test.setTimeout(120_000)

const RUN = Date.now().toString(36)
const FS_REPO = `e2e-fs-${RUN}`
const S3_REPO = `e2e-s3-${RUN}`

async function login(page: Page, name: string) {
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(DEMO_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('session-role')).not.toHaveText('')
}

/** JSON API call with the page's session (cookie + CSRF). */
async function apiCall(page: Page, method: string, path: string, data?: unknown) {
  const s = await (await page.request.get('/api/v1/session')).json()
  const r = await page.request.fetch(`/api/v1${path}`, { method, data, headers: { 'X-CSRF-Token': s.csrf, 'Content-Type': 'application/json' } })
  return { status: r.status(), body: r.status() === 204 ? null : await r.json().catch(() => null) }
}

async function noHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true)
}

test('repositories: cards with backend, mode, health and stats; every detail tab renders', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  await page.goto('/repositories')
  const cards = page.getByTestId('repo-cards')
  await expect(cards.locator('[data-repo=custody]')).toContainText('Filesystem')
  await expect(cards.locator('[data-repo=offsite]')).toBeVisible()
  await expect(cards.locator('[data-repo=custody]')).toContainText('Snapshots')
  await expect(cards.locator('[data-repo=custody] [data-testid=mode-pill]')).toContainText('Online')
  // the seeded failed verification makes custody "Needs action" with the reason listed
  await expect(cards.locator('[data-repo=custody]')).toContainText(/failed verification|corrupt chunk/)
  await cards.locator('[data-repo=custody]').getByRole('link', { name: 'custody' }).click()
  await expect(page).toHaveURL(/\/repositories\/custody$/)
  await expect(page.getByRole('heading', { name: /custody/ })).toBeVisible()
  for (const tab of ['Summary', 'Maintenance mode', 'Schedules', 'Tasks', 'Integrity', 'Hosts', 'Credentials', 'Escrow', 'Danger zone']) {
    await page.getByRole('tab', { name: new RegExp(`^${tab}`) }).click()
    await expect(page.getByRole('tab', { name: new RegExp(`^${tab}`) })).toHaveAttribute('aria-selected', 'true')
  }
  // integrity lists the seeded quarantined chunk and the failed snapshot
  await page.getByRole('tab', { name: /^Integrity/ }).click()
  await expect(page.getByText('quarantined').first()).toBeVisible()
  await expect(page.getByRole('cell', { name: 'prod/instance/db-01/root' }).first()).toBeVisible()
  // the identity: owner.json names this server
  await page.getByRole('tab', { name: /^Summary/ }).click()
  await expect(page.getByText('this server', { exact: true })).toBeVisible()
  await page.getByTestId('repo-check').click()
  await expect(page.getByTestId('checklist').locator('[data-check=open]')).toHaveAttribute('data-level', 'pass')
  con.assertClean()
})

test('create a filesystem repository: path checks, then the real create (temp dir)', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  const base = mkdtempSync(join(tmpdir(), 'ib-e2e-fs-'))
  const full = join(base, 'full')
  mkdirSync(full)
  writeFileSync(join(full, 'x'), 'x')
  await page.goto('/repositories/new')
  await page.getByTestId('repo-name').fill(FS_REPO)
  await page.getByTestId('next').click()
  await page.getByTestId('repo-path').fill('relative/dir')
  await expect(page.getByText('Use an absolute path on the server')).toBeVisible()
  await expect(page.getByTestId('test')).toBeDisabled()
  // a non-empty directory fails with the reason
  await page.getByTestId('repo-path').fill(full)
  await page.getByTestId('test').click()
  await expect(page.getByTestId('checklist').locator('[data-check=path]')).toHaveAttribute('data-level', 'fail')
  await expect(page.getByTestId('checklist')).toContainText('is not empty')
  await expect(page.getByTestId('next')).toBeDisabled()
  await shot(page, 'storage-repo-create-fail-desktop-light')
  // a directory that does not exist yet: will be created, free space reported
  await page.getByRole('button', { name: 'Back' }).click()
  await page.getByTestId('repo-path').fill(join(base, 'repo'))
  await page.getByTestId('test').click()
  await expect(page.getByTestId('ready')).toBeVisible()
  await expect(page.getByTestId('checklist').locator('[data-check=space]')).toBeVisible()
  await shot(page, 'storage-repo-create-ready-desktop-light')
  await page.getByTestId('next').click()
  await page.getByTestId('add').click()
  await expect(page).toHaveURL(new RegExp(`/repositories/${FS_REPO}\\?tab=schedules`))
  await expect(page.getByTestId('schedules')).toBeVisible()
  const repos = await apiCall(page, 'GET', '/repositories')
  expect(repos.body.map((r: { name: string }) => r.name)).toContain(FS_REPO)
  con.assertClean()
})

test('S3: probe failures are explained; a real S3 repository on the S3 test server; the secret never comes back', async ({ page }) => {
  const con = watchConsole(page)
  let s3: S3 | null = null
  try {
    await login(page, 'admin')
    await page.goto('/repositories/new')
    await page.getByTestId('repo-name').fill(S3_REPO)
    await page.getByTestId('kind-s3').check()
    await page.getByTestId('next').click()
    await page.getByTestId('s3-endpoint').fill('127.0.0.1:1')
    await page.getByTestId('s3-bucket').fill('backups')
    await page.getByRole('switch', { name: 'HTTPS' }).click() // plain HTTP
    await page.getByTestId('s3-access').fill('AKIAEXAMPLE')
    await page.getByTestId('s3-secret').locator('input').fill('not-a-real-secret-1')
    await page.getByTestId('test').click()
    const list = page.getByTestId('checklist')
    await expect(list.locator('[data-check=connect]')).toHaveAttribute('data-level', 'fail')
    await expect(list.locator('[data-check=connect]')).toContainText('never creates buckets') // the advice
    await expect(list.locator('[data-check=delete]')).toHaveAttribute('data-level', 'skip')
    await expect(page.getByTestId('not-ready')).toBeVisible()
    await shot(page, 'storage-s3-probe-fail-desktop-light')

    // now a real service
    s3 = await startS3('ib-e2e')
    await page.getByRole('button', { name: 'Back' }).click()
    await page.getByTestId('s3-endpoint').fill(s3.endpoint)
    await page.getByTestId('s3-bucket').fill('ib-e2e')
    await page.getByTestId('s3-access').fill(s3.access)
    await page.getByTestId('s3-secret').locator('input').fill(s3.secret)
    await page.getByTestId('test').click()
    await expect(page.getByTestId('ready')).toBeVisible()
    for (const id of ['connect', 'put', 'conditional', 'get', 'stat', 'list', 'delete', 'versioning', 'lifecycle', 'object-lock']) {
      await expect(list.locator(`[data-check=${id}]`), id).toHaveAttribute('data-level', 'pass')
    }
    await expect(list.locator('[data-check=tls]')).toHaveAttribute('data-level', 'warn') // plain HTTP
    await shot(page, 'storage-s3-probe-ready-desktop-light')
    await page.getByTestId('next').click()
    await page.getByTestId('add').click()
    await expect(page).toHaveURL(new RegExp(`/repositories/${S3_REPO}`))
    // the secret is gone from the page and never returned by the API
    expect(await page.content()).not.toContain(s3.secret)
    const creds = await apiCall(page, 'GET', '/credentials')
    expect(JSON.stringify(creds.body)).not.toContain(s3.secret)
    expect(creds.body.find((c: { name: string }) => c.name === `${S3_REPO}-s3`).used_by).toEqual([S3_REPO])
    // attaching the same prefix again is refused up front: this server already uses it
    await page.goto('/repositories/new')
    await page.getByTestId('repo-name').fill(`${S3_REPO}-again`)
    await page.getByTestId('purpose-attach').check()
    await page.getByTestId('kind-s3').check()
    await page.getByTestId('next').click()
    await page.getByTestId('s3-endpoint').fill(s3.endpoint)
    await page.getByTestId('s3-bucket').fill('ib-e2e')
    await page.getByRole('switch', { name: 'HTTPS' }).click()
    await page.getByLabel('Use a stored credential').check()
    await page.getByTestId('test').click()
    await expect(list.locator('[data-check=attached]')).toHaveAttribute('data-level', 'fail')
    await expect(list.locator('[data-check=attached]')).toContainText(S3_REPO)
    con.assertClean()
  } finally {
    s3?.stop()
  }
})

test('maintenance mode: read-only with a reason, offline needs the typed name, back online', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  await page.goto(`/repositories/${FS_REPO}?tab=mode`)
  await page.getByTestId('mode-read-only').check({ force: true })
  await page.getByTestId('mode-reason').fill('e2e: storage migration')
  await page.getByTestId('mode-apply').click()
  await page.getByTestId('confirm-button').click()
  await expect(page.getByTestId('detail-mode')).toContainText('Read-only')
  await expect(page.getByText('e2e: storage migration').first()).toBeVisible()
  await shot(page, 'storage-repo-mode-desktop-light')
  await page.getByTestId('mode-offline').check({ force: true })
  await page.getByTestId('mode-apply').click()
  await expect(page.getByTestId('confirm-button')).toBeDisabled()
  await page.getByTestId('confirm-input').fill(FS_REPO)
  await page.getByTestId('confirm-button').click()
  await expect(page.getByTestId('detail-mode')).toContainText('Offline')
  // the tasks refuse to start while offline
  await page.getByRole('tab', { name: /^Tasks/ }).click()
  await expect(page.getByTestId('verify-run')).toBeDisabled()
  await page.getByRole('tab', { name: /^Maintenance mode/ }).click()
  await page.getByTestId('mode-online').check({ force: true })
  await page.getByTestId('mode-apply').click()
  await page.getByTestId('confirm-button').click()
  await expect(page.getByTestId('detail-mode')).toContainText('Online')
  con.assertClean()
})

test('schedules: next runs from the server, GC grace saved as hours', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  await page.goto(`/repositories/${FS_REPO}?tab=schedules`)
  const gc = page.locator('[data-job=gc]')
  await expect(gc.getByTestId('schedule-next').locator('li')).toHaveCount(4)
  await gc.getByTestId('schedule-input').fill('every 30s')
  await expect(gc).toContainText('invalid interval')
  await expect(page.getByTestId('schedules-save')).toBeDisabled()
  await gc.getByTestId('schedule-input').fill('sun 04:30')
  await expect(gc).toContainText('Sundays at 04:30')
  await page.getByTestId('gc-grace').fill('6')
  await expect(page.getByText('At least 12 hours')).toBeVisible()
  await page.getByTestId('gc-grace').fill('36')
  await page.getByTestId('schedules-save').click()
  await expect(page.getByText('Schedules saved')).toBeVisible()
  const m = await apiCall(page, 'GET', `/maintenance?repository=${FS_REPO}`)
  expect(m.body.gc).toBe('sun 04:30')
  expect(m.body.gc_grace_hours).toBe(36)
  con.assertClean()
})

test('tasks: verify new, retention preview with the plan, GC dry run, history', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  await page.goto('/repositories/custody?tab=tasks')
  await page.getByTestId('verify-run').click()
  await expect(page.getByTestId('verify-result')).toBeVisible({ timeout: 60_000 })
  await page.getByTestId('prune-preview').click()
  await expect(page.getByTestId('prune-plan')).toBeVisible({ timeout: 60_000 })
  await expect(page.getByTestId('prune-apply')).toBeEnabled()
  await page.getByTestId('prune-plan').getByRole('button', { name: /^All/ }).click()
  await expect(page.getByTestId('prune-plan').locator('tbody tr').first()).toBeVisible()
  await page.getByTestId('gc-dry').click()
  await expect(page.getByTestId('gc-dry-result')).toContainText('Dry run')
  // the dry runs and the verify appear in the history; a row opens its details
  await expect(page.locator('.hist tbody tr').first()).toBeVisible()
  await shot(page, 'storage-repo-tasks-full-desktop-light', { fullPage: true })
  await page.locator('.hist tbody tr').first().click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.keyboard.press('Escape')
  con.assertClean()
})

test('policies: create with schedule and retention preview, assign, delete with impact', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  await page.goto('/policies')
  await expect(page.getByTestId('policy-cards').locator('[data-policy=nightly]')).toContainText('Keeps 14 daily, 8 weekly and 12 monthly.')
  await page.getByTestId('policy-new').click()
  const name = `e2e-${RUN}`
  await page.getByTestId('policy-name').fill(name)
  const sched = page.getByTestId('policy-form').getByTestId('schedule-input').first()
  await sched.fill('every 30s')
  await expect(page.getByTestId('policy-form')).toContainText('invalid interval')
  await expect(page.getByTestId('policy-save')).toBeDisabled()
  await sched.fill('sun 01:00')
  await page.getByTestId('keep-daily').fill('7')
  await page.getByTestId('keep-weekly').fill('4')
  await page.getByTestId('keep-monthly').fill('0')
  await page.getByTestId('tz-input').click()
  await page.getByTestId('tz-input').fill('reykj')
  await page.getByRole('option', { name: /Reykjavik/ }).click()
  await expect(page.getByTestId('retention-preview')).toContainText('this keeps')
  await shot(page, 'storage-policy-edit-desktop-light', { fullPage: true })
  await page.getByTestId('policy-save').click()
  await expect(page).toHaveURL(new RegExp(`/policies/${name}$`))
  const pols = await apiCall(page, 'GET', '/policies?repository=custody')
  const p = pols.body.find((x: { name: string }) => x.name === name)
  expect(p).toMatchObject({ schedule: 'sun 01:00', keep_daily: 7, keep_weekly: 4, timezone: 'Atlantic/Reykjavik' })

  // assign it to bosch / staging and see the server's resolution
  await page.getByRole('tab', { name: /^Assignments/ }).click()
  const expand = page.getByRole('button', { name: 'Expand bosch' })
  if (await expand.count()) await expand.click() // hosts are collapsed when several are enrolled
  const proj = page.getByLabel('Policy of project staging on bosch')
  await proj.selectOption(name)
  await expect(page.getByText('Assignment saved')).toBeVisible()
  await expect(page.locator('[data-owner="bosch/staging/ci-runner"]')).toContainText(name)
  await expect(page.locator('[data-owner="bosch/staging/ci-runner"]')).toContainText('project assignment')
  await page.getByRole('tab', { name: /^Objects/ }).click()
  await expect(page.locator('tbody tr').first()).toContainText('staging/')

  // delete: the confirmation states what governs those objects afterwards
  await page.getByTestId('policy-delete').click()
  await expect(page.getByRole('dialog')).toContainText(/nightly|no policy/)
  await page.getByTestId('confirm-input').fill(name)
  await page.getByTestId('confirm-button').click()
  await expect(page).toHaveURL(/\/policies$/)
  const after = await apiCall(page, 'GET', '/assignments?repository=custody')
  expect(after.body.some((a: { policy: string }) => a.policy === name)).toBe(false)
  con.assertClean()
})

test('sync: wizard with a real token (tested without spending it), run now, remove', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  const job = `e2e-${RUN}`
  // a sync-write token of the demo's own "offsite" repository, through the same API an admin uses
  const tok = await apiCall(page, 'POST', '/onboarding?repository=offsite', { host: `sync-custody-${job}`, scope: 'sync-write', ttl_hours: 1 })
  expect(tok.status).toBe(201)
  await page.goto('/sync')
  await expect(page.getByTestId('sync-topology')).toContainText('offsite-archive')
  await page.getByTestId('sync-add-open').click()
  await page.getByTestId('sync-name').fill(job)
  await page.getByTestId('sync-remote').fill(tok.body.server)
  await page.getByTestId('sync-next').click()
  await page.getByTestId('sync-token').locator('input').fill('ibt1.nonsense')
  await page.getByTestId('sync-test').click()
  await expect(page.getByTestId('sync-test-fail')).toBeVisible()
  await expect(page.getByTestId('checklist').locator('[data-check=token]')).toHaveAttribute('data-level', 'fail')
  await page.getByTestId('sync-token').locator('input').fill(tok.body.token)
  await page.getByTestId('sync-test').click()
  await expect(page.getByTestId('sync-test-ok')).toBeVisible()
  await expect(page.getByTestId('checklist').locator('[data-check=identity]')).toHaveAttribute('data-level', 'pass')
  await shot(page, 'storage-sync-wizard-desktop-light')
  await page.getByTestId('sync-next').click()
  await page.getByTestId('sync-next').click()
  await page.getByTestId('sync-add').click()
  await expect(page).toHaveURL(new RegExp(`/sync/${job}$`))
  await page.getByTestId('sync-run').click()
  await expect(page.getByTestId('sync-result')).toBeVisible({ timeout: 90_000 })
  await expect(page.locator('tbody tr').first()).toBeVisible()
  await page.getByTestId('sync-rate').fill('20MB')
  await page.getByTestId('sync-save').click()
  await expect(page.getByText('Sync job saved')).toBeVisible()
  const st = await apiCall(page, 'GET', `/sync/${job}?repository=custody`)
  expect(st.body.job.rate_limit_bps).toBe(20_000_000)
  await page.getByTestId('sync-remove').click()
  await page.getByTestId('confirm-input').fill(job)
  await page.getByTestId('confirm-button').click()
  await expect(page).toHaveURL(/\/sync$/)
  con.assertClean()
})

test('roles: a viewer sees state but no action; an operator can verify but not prune', async ({ page }) => {
  await login(page, 'viewer')
  await page.goto('/repositories')
  await expect(page.getByTestId('repo-add')).toHaveCount(0)
  await page.goto('/repositories/custody?tab=mode')
  await expect(page.getByTestId('mode-offline')).toBeDisabled()
  await expect(page.getByTestId('mode-apply')).toHaveCount(0)
  await page.goto('/repositories/custody?tab=tasks')
  await expect(page.getByTestId('verify-run')).toBeDisabled()
  await expect(page.getByRole('tab', { name: /^Credentials/ })).toHaveCount(0)
  await page.goto('/policies/nightly')
  await expect(page.getByTestId('policy-save')).toHaveCount(0)
  await page.getByTestId('user-menu').click()
  await page.getByTestId('sign-out').click()
  await login(page, 'operator')
  await page.goto('/repositories/custody?tab=tasks')
  await expect(page.getByTestId('verify-run')).toBeEnabled()
  await expect(page.getByTestId('prune-preview')).toBeDisabled()
  await expect(page.getByTestId('gc-run')).toBeDisabled()
})

for (const theme of ['light', 'dark'] as const) {
  test(`storage screenshots, ${theme}`, async ({ browser }) => {
    for (const vp of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'mobile', width: 390, height: 844 }]) {
      const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, colorScheme: theme, deviceScaleFactor: vp.name === 'mobile' ? 2 : 1, hasTouch: vp.name === 'mobile' })
      const page = await ctx.newPage()
      const con = watchConsole(page)
      await login(page, 'admin')
      await setTheme(page, theme)
      const pages: Array<[string, string, () => Promise<void>]> = [
        ['repos', '/repositories', async () => { await expect(page.getByTestId('repo-cards')).toBeVisible() }],
        ['repo-summary', '/repositories/custody', async () => { await expect(page.getByText('Identity')).toBeVisible() }],
        ['repo-mode', '/repositories/offsite?tab=mode', async () => { await expect(page.getByTestId('mode-online')).toBeAttached() }],
        ['repo-schedules', '/repositories/custody?tab=schedules', async () => { await expect(page.getByTestId('schedules')).toBeVisible() }],
        ['repo-tasks', '/repositories/custody?tab=tasks', async () => { await expect(page.getByTestId('task-verify')).toBeVisible() }],
        ['repo-integrity', '/repositories/custody?tab=integrity', async () => { await expect(page.getByText('How healing works')).toBeVisible() }],
        ['repo-hosts', '/repositories/custody?tab=hosts', async () => { await expect(page.getByText('bosch').first()).toBeVisible() }],
        ['repo-credentials', '/repositories/offsite?tab=credentials', async () => { await expect(page.getByRole('heading', { name: 'S3 credentials' })).toBeVisible() }],
        ['policies', '/policies', async () => { await expect(page.getByTestId('policy-cards')).toBeVisible() }],
        ['policy-assignments', '/policies?tab=assignments', async () => { await expect(page.getByTestId('assignments')).toBeVisible() }],
        ['policy-edit', '/policies/nightly', async () => { await expect(page.getByTestId('retention-preview')).toBeVisible() }],
        ['sync', '/sync', async () => { await expect(page.getByTestId('sync-topology')).toBeVisible() }],
        ['sync-job', '/sync/offsite-archive', async () => { await expect(page.getByTestId('sync-health')).toBeVisible() }],
      ]
      for (const [n, path, ready] of pages) {
        await page.goto(path)
        await ready()
        await page.waitForLoadState('networkidle')
        await shot(page, `storage-${n}-${vp.name}-${theme}`, { fullPage: vp.name === 'desktop' })
        if (vp.name === 'mobile') await noHorizontalScroll(page)
      }
      con.assertClean()
      await ctx.close()
    }
  })
}
