// The library in a real browser: builds dist/ and runs browser/run.mjs (headless Chromium via
// Playwright, a local static server, the fixture served at the HttpFetcher paths). Fails, never
// skips, when no browser can be launched.
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const here = fileURLToPath(new URL('..', import.meta.url));

describe('headless Chromium', () => {
  it('lists, extracts and unseals in the page, in the worker pool and inside a user Web Worker', () => {
    execFileSync('npx', ['tsc', '-p', 'tsconfig.json'], { cwd: here, stdio: 'inherit' });
    const out = execFileSync('node', ['browser/run.mjs'], { cwd: here, encoding: 'utf8', maxBuffer: 64 << 20 });
    expect(out).toMatch(/all browser checks passed/);
    expect(out).not.toMatch(/^FAIL/m);
  }, 600_000);
});
