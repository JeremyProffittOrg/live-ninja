import { defineConfig, devices } from '@playwright/test';

/**
 * Targets a *deployed* origin rather than a locally booted server.
 *
 * The Fiber app needs DynamoDB, KMS and SSM to start, so a "just run the binary"
 * harness would either need those faked or would only ever exercise /healthz. The
 * gate is therefore wired after the deploy job (.github/workflows/deploy.yml) and
 * points at the real origin, which also means it catches CloudFront/API-Gateway
 * layer regressions that a local server cannot reproduce — the class of bug that
 * has actually bitten this project (see plan.md Gotchas on ConvID query strings).
 */
const baseURL = process.env.LN_BASE_URL || 'https://live.jeremy.ninja';

export default defineConfig({
  testDir: './specs',
  // Production is being hit over the network; give it room but never hang CI.
  timeout: 45_000,
  expect: { timeout: 10_000 },
  fullyParallel: true,
  // A gate that flakes gets ignored, and an ignored gate is worse than none.
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI
    ? [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]]
    : [['list']],
  use: {
    baseURL,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'desktop-chrome', use: { ...devices['Desktop Chrome'] } },
    // The house UI rules require the layout to hold at the smallest supported
    // width, so a mobile viewport is a first-class target, not an afterthought.
    { name: 'mobile-chrome', use: { ...devices['Pixel 7'] } },
  ],
});
