// Against the seeded demo server (realistic data, four users with different roles): role-aware
// navigation, the repository selector, the command palette with live data, table behaviour, and the
// screenshots of the shell, login and style guide in both themes at desktop and phone size.
import { test, expect, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { DEMO_PASSWORD } from './backend'
import { shot, setTheme, watchConsole } from './helpers'

test.describe.configure({ mode: 'serial' })

async function login(page: Page, name: string) {
  await page.goto('/login')
  await page.getByLabel('User name').fill(name)
  await page.locator('input[name=password]').fill(DEMO_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByTestId('session-role')).not.toHaveText('')
}

for (const theme of ['light', 'dark'] as const) {
  test(`screenshots, ${theme}`, async ({ browser }) => {
    for (const vp of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'mobile', width: 390, height: 844 }]) {
      const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, colorScheme: theme, deviceScaleFactor: vp.name === 'mobile' ? 2 : 1, hasTouch: vp.name === 'mobile' })
      const page = await ctx.newPage()
      const con = watchConsole(page)
      await page.goto('/login')
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme) // system preference followed
      await shot(page, `login-${vp.name}-${theme}`)
      await login(page, 'admin')
      await expect(page.getByTestId('headline')).toContainText('objects protected')
      await expect(page.getByTestId('problem-count')).toContainText(/problem/)
      await page.waitForLoadState('networkidle')
      await shot(page, `shell-overview-${vp.name}-${theme}`)
      await shot(page, `shell-overview-full-${vp.name}-${theme}`, { fullPage: true })
      if (vp.name === 'mobile') {
        await page.getByTestId('nav-open').click()
        await expect(page.locator('.sidebar.open')).toBeVisible()
        await shot(page, `shell-nav-open-mobile-${theme}`)
        await page.locator('[data-nav="hosts"]').click()
        await expect(page.locator('.sidebar.open')).toHaveCount(0)
        // no horizontal page scroll at phone width
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
      } else {
        await page.locator('[data-nav="hosts"]').click()
      }
      await expect(page.getByTestId('host-table')).toBeVisible()
      await shot(page, `shell-placeholder-${vp.name}-${theme}`)
      await page.goto('/profile')
      await expect(page.getByRole('heading', { name: 'Profile' })).toBeVisible()
      await shot(page, `profile-${vp.name}-${theme}`)
      await page.goto('/_styleguide')
      await expect(page.getByTestId('styleguide')).toBeVisible()
      await page.waitForTimeout(300)
      await shot(page, `styleguide-${vp.name}-${theme}`)
      await shot(page, `styleguide-full-${vp.name}-${theme}`, { fullPage: true })
      if (vp.name === 'mobile') expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
      if (vp.name === 'desktop') {
        await page.keyboard.press('Control+k')
        await page.getByTestId('palette-input').fill('bos')
        await expect(page.getByRole('option').first()).toBeVisible()
        await shot(page, `palette-desktop-${theme}`)
        await page.keyboard.press('Escape')
      }
      con.assertClean()
      await ctx.close()
    }
  })
}

test('role-aware navigation: viewer sees no admin entries and gets a notice on admin pages', async ({ page }) => {
  await login(page, 'viewer')
  await expect(page.getByTestId('session-role')).toHaveText('readonly')
  await expect(page.locator('[data-nav="hosts"]')).toBeVisible()
  for (const hidden of ['users', 'settings', 'audit']) await expect(page.locator(`[data-nav="${hidden}"]`)).toHaveCount(0)
  await expect(page.locator('[data-nav="restore"]')).toHaveCount(0) // needs restore
  await page.goto('/settings/users')
  await expect(page.getByTestId('no-access')).toContainText('admin role')
  await page.goto('/hosts')
  await expect(page.getByTestId('host-table')).toBeVisible()
  // a policy that does not exist says so (the policy editor replaced its placeholder)
  await page.goto('/policies/whatever')
  await expect(page.getByText('No such policy')).toBeVisible()
})

test('restore role sees Restore; operator does not see Users', async ({ page }) => {
  await login(page, 'restorer')
  await expect(page.locator('[data-nav="restore"]')).toBeVisible()
  await expect(page.locator('[data-nav="users"]')).toHaveCount(0)
  await page.getByTestId('user-menu').click()
  await page.getByTestId('sign-out').click()
  await login(page, 'operator')
  await expect(page.getByTestId('session-role')).toHaveText('operator')
  await expect(page.locator('[data-nav="users"]')).toHaveCount(0)
  await expect(page.locator('[data-nav="policies"]')).toBeVisible()
})

test('repository selector scopes the requests; palette jumps to a host from live data', async ({ page }) => {
  await login(page, 'admin')
  const sel = page.getByTestId('repo-select')
  await expect(sel).toBeVisible()
  await expect(sel.locator('option')).toHaveCount(2)
  const req = page.waitForRequest((r) => r.url().includes('/api/v1/capacity/series') && r.url().includes('repository=offsite'))
  await sel.selectOption('offsite')
  await req
  await page.reload()
  await expect(page.getByTestId('repo-select')).toHaveValue('offsite') // remembered
  await sel.selectOption('custody')
  await page.keyboard.press('Control+k')
  await page.getByTestId('palette-input').fill('bosch')
  await expect(page.getByRole('option', { name: /bosch/ }).first()).toBeVisible()
  await page.getByRole('option', { name: /^bosch/ }).first().click()
  await expect(page).toHaveURL(/\/hosts\/bosch$/)
  await expect(page.getByRole('heading', { name: /bosch/ })).toBeVisible()
})

test('data table: 1500 rows render windowed, filter, sort, CSV export', async ({ page }) => {
  await login(page, 'admin')
  await page.goto('/_styleguide#sg-table')
  const table = page.locator('#sg-table')
  await expect(table.getByText('1,500 rows')).toBeVisible()
  const rendered = await table.locator('tbody tr:not(.pad)').count()
  expect(rendered).toBeLessThan(80) // windowed, not 1500 DOM rows
  await table.getByTestId('table-filter').fill('host:host-02 -failed')
  await expect(table.getByText(/of 1,500/)).toBeVisible()
  const hosts = await table.locator('tbody tr:not(.pad) td:nth-child(3)').allInnerTexts()
  expect(hosts.length).toBeGreaterThan(0)
  expect(hosts.every((h) => h.trim() === 'host-02')).toBe(true)
  await table.getByRole('button', { name: /^Snapshots/ }).click()
  const snaps = (await table.locator('tbody tr:not(.pad) td:nth-child(6)').allInnerTexts()).map(Number)
  expect(snaps).toEqual([...snaps].sort((a, b) => a - b))
  const dl = page.waitForEvent('download')
  await table.getByRole('button', { name: 'CSV' }).click()
  const file = await dl
  expect(file.suggestedFilename()).toBe('objects-demo.csv')
  const csv = readFileSync((await file.path())!, 'utf8')
  expect(csv.split('\r\n')[0]).toBe('Object,Host,Kind,Last run,Snapshots,Logical size,Last backup')
  expect(csv.split('\r\n').filter(Boolean).length - 1).toBe(hosts.length)
  // typed confirmation
  await page.goto('/_styleguide#sg-overlays')
  await page.getByRole('button', { name: 'Typed confirmation' }).click()
  await expect(page.getByTestId('confirm-button')).toBeDisabled()
  await page.getByTestId('confirm-input').fill('web01')
  await expect(page.getByTestId('confirm-button')).toBeEnabled()
  await page.keyboard.press('Escape')
  await setTheme(page, 'light')
})
