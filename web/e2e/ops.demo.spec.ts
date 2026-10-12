// Operations pages against the seeded demo server: tasks (a verify started from the repository Tasks
// tab is followed live and lands in the task list with its log), the top-bar indicator, the server
// status page and support report, the server log page, the update banner (mocked feed answer), and the
// guards of the danger zone and the object delete dialog (typed confirmation; nothing is deleted here,
// other demo specs use the seeded data). Screenshots light/dark x desktop/phone into e2e/screenshots/.
import { test, expect, type Page } from '@playwright/test'
import { DEMO_PASSWORD } from './backend'
import { shot, setTheme, watchConsole } from './helpers'

test.describe.configure({ mode: 'serial' })
test.setTimeout(120_000)

async function login(page: Page, name: string) {
  await page.context().clearCookies()
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(DEMO_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('session-role')).not.toHaveText('')
}

test('a verify started on the repository Tasks tab runs as a task and is followed live', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'operator')
  await page.goto('/repositories/custody?tab=tasks')
  await page.getByTestId('verify-mode').selectOption('new')
  await page.getByTestId('verify-run').click()
  const panel = page.locator('[data-testid=task-verify] [data-testid^=task-panel-]')
  await expect(panel).toBeVisible()
  await expect(panel).toContainText(/succeeded|failed/, { timeout: 60_000 })
  await expect(panel).toContainText('Log')
  await page.goto('/tasks')
  await expect(page.locator('tbody tr').first()).toContainText('Verify')
  await page.locator('tbody tr').first().click()
  await expect(page.locator('dialog[open]')).toContainText('operator')
  con.assertClean()
})

test('server status, log, support report and update banner', async ({ page }) => {
  const con = watchConsole(page)
  await login(page, 'admin')
  await page.goto('/settings/system')
  await expect(page.getByRole('heading', { name: 'Server status' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'PostgreSQL' })).toBeVisible()
  await page.getByTestId('support-report').click()
  const [dl] = await Promise.all([page.waitForEvent('download'), page.getByTestId('support-download').click()])
  expect(dl.suggestedFilename()).toMatch(/^incus-backup-support-.*\.tar\.gz$/)
  await page.goto('/settings/logs')
  await expect(page.getByRole('heading', { name: 'Server log' })).toBeVisible()
  await page.route('**/api/v1/system/update', (r) => r.fulfill({ json: { enabled: true, url: 'x', current: '0.3.0', latest: '0.4.0', update_available: true, checked_at: new Date().toISOString() } }))
  await page.goto('/')
  await expect(page.getByTestId('update-banner')).toContainText('0.4.0')
  await page.getByTestId('update-banner').getByRole('button', { name: /Dismiss/ }).click()
  await expect(page.getByTestId('update-banner')).toHaveCount(0)
  con.assertClean()
})

test('destroy and object delete need offline mode and the typed name', async ({ page }) => {
  await login(page, 'admin')
  await page.goto('/repositories/custody?tab=danger')
  await expect(page.getByTestId('repo-destroy')).toBeDisabled() // online
  await page.goto('/objects')
  await page.locator('tbody tr a').first().click()
  await page.getByRole('button', { name: 'More' }).click()
  await page.getByTestId('menu-delete').click()
  await expect(page.getByTestId('delete-submit')).toBeDisabled()
  await page.getByTestId('delete-confirm').fill('not the name')
  await expect(page.getByTestId('delete-submit')).toBeDisabled()
  await page.keyboard.press('Escape')
  await login(page, 'viewer')
  await page.goto('/tasks')
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
})

test('screenshots of the operations pages', async ({ page }) => {
  await login(page, 'admin')
  for (const theme of ['light', 'dark'] as const) {
    for (const [w, h, tag] of [[1440, 900, 'desktop'], [390, 844, 'phone']] as const) {
      await page.setViewportSize({ width: w, height: h })
      await setTheme(page, theme)
      for (const [path, name] of [['/tasks', 'tasks'], ['/settings/system', 'system'], ['/settings/logs', 'logs'], ['/repositories/custody?tab=danger', 'danger']] as const) {
        await page.goto(path)
        await page.waitForTimeout(800)
        await shot(page, `ops-${name}-${theme}-${tag}`)
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true)
      }
    }
  }
})
