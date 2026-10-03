import deployed from './playwright.config.mjs';
import { defineConfig } from '@playwright/test';

// These suites pin and validate loopback origins themselves. They must never
// inherit the deployed origin or write to production. Start cmd/jobs-preview
// first; CI provides a fresh FileStore and explicit localhost environment.
const baseURL = process.env.LN_JOBS_BASE_URL || 'http://127.0.0.1:8795';
if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(baseURL).hostname)) throw new Error('Local workflow tests require loopback.');

export default defineConfig({
  ...deployed,
  use: { ...deployed.use, baseURL, serviceWorkers: 'block' },
  testMatch: ['**/jobs.spec.mjs', '**/approvals.spec.mjs', '**/ghost-work.spec.mjs'],
  testIgnore: [],
  reporter: process.env.CI
    ? [['list'], ['html', { open: 'never', outputFolder: 'playwright-local-report' }]]
    : [['list']],
});
