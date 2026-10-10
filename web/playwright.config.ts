import { defineConfig, devices } from '@playwright/test'
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { DEMO_BIN, DEMO_URL, REAL_URL } from './e2e/backend'
import { fileURLToPath as toPath } from 'node:url'
const HERE = toPath(new URL('.', import.meta.url))

// e2e: `npm run e2e`. Global setup builds the UI, builds the real server with it embedded and starts it
// on a fresh PostgreSQL (project "real": first-run setup, 2FA, session expiry), plus the demo server
// when /tmp/ibwork/demo-bin/ibdemo (IB_DEMO_BIN) exists (project "demo": seeded data, roles, screenshots).
const demo = existsSync(DEMO_BIN)

// Containers without Chromium's system libraries (no root, no apt): point IB_CHROMIUM_ROOT at a
// directory where the Debian packages were unpacked with `dpkg-deb -x` (usr/lib/x86_64-linux-gnu,
// usr/share/fonts, ...). The libraries and a fontconfig file for its fonts are then given to the browser.
const chromiumRoot = process.env.IB_CHROMIUM_ROOT ?? join(HERE, 'e2e', '.cache', 'chromium-libs', 'root')
if (existsSync(join(chromiumRoot, 'usr', 'lib', 'x86_64-linux-gnu'))) {
  const libs = [join(chromiumRoot, 'usr', 'lib', 'x86_64-linux-gnu'), join(chromiumRoot, 'lib', 'x86_64-linux-gnu'), process.env.LD_LIBRARY_PATH].filter(Boolean)
  process.env.LD_LIBRARY_PATH = libs.join(':')
  if (!process.env.FONTCONFIG_FILE && !existsSync('/etc/fonts/fonts.conf')) {
    const dir = join(HERE, 'e2e', '.state')
    mkdirSync(dir, { recursive: true })
    const conf = join(dir, 'fonts.conf')
    writeFileSync(conf, `<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "fonts.dtd"><fontconfig><dir>${join(chromiumRoot, 'usr', 'share', 'fonts')}</dir><cachedir>${join(dir, 'fc-cache')}</cachedir></fontconfig>\n`)
    process.env.FONTCONFIG_FILE = conf
  }
}

export default defineConfig({
  testDir: './e2e',
  globalSetup: './e2e/global-setup.ts',
  globalTeardown: './e2e/global-teardown.ts',
  fullyParallel: false,
  workers: 1, // one server, one sequence of logins (the server throttles 20 logins/min per address)
  // CI (.github/workflows/ci.yml, job e2e): one retry, so a timing flake is reported as "flaky" with
  // the trace of the failed attempt instead of failing the run; a test that fails twice fails it.
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: process.env.CI ? [['list'], ['github']] : [['list']],
  use: {
    ignoreHTTPSErrors: true, // the real server's self-signed web certificate
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    viewport: { width: 1440, height: 900 },
  },
  projects: [
    { name: 'real', testMatch: /real\.spec\.ts/, use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 }, baseURL: REAL_URL } },
    ...(demo ? [{ name: 'demo', testMatch: /demo\.spec\.ts/, use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 }, baseURL: DEMO_URL } }] : []),
  ],
})
