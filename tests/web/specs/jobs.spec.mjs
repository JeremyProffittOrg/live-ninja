import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

// This suite writes jobs. Never inherit the repository's deployed-origin
// default. It only runs against the explicitly local FileStore preview.
const baseURL = process.env.LN_JOBS_BASE_URL || 'http://127.0.0.1:8793';
if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(baseURL).hostname)) throw new Error('Jobs browser tests require a loopback preview origin.');
test.use({ baseURL, serviceWorkers: 'block' });

const unique = (prefix) => `${prefix} ${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
async function open(page) { await page.goto('/jobs'); await expect(page.locator('#jobNew')).toBeEnabled(); }
async function create(page, title, kind = 'reminder', instructions = 'A real local job for the browser test.') {
  await page.getByRole('button', { name: 'New job', exact: true }).click();
  await page.getByLabel('Job title', { exact: true }).fill(title);
  await page.getByLabel('Notes', { exact: true }).fill(instructions);
  await page.getByLabel('Job type', { exact: true }).selectOption(kind);
  await page.getByLabel('Repeat', { exact: true }).selectOption('manual');
  await page.getByRole('button', { name: 'Create job', exact: true }).click();
  await expect(page.locator('#jobEditor')).not.toBeVisible();
  await expect(page.locator('#jobSelectedTitle')).toHaveText(title);
}
async function api(page, path, method = 'GET', payload) {
  return page.evaluate(async ({ path, method, payload }) => {
    const { apiJSON } = await import('/static/js/toolclient.mjs');
    return apiJSON(path, { method, ...(payload ? { json: payload } : {}) });
  }, { path, method, payload });
}
async function selectedID(page) { return page.locator('.jobs-card[aria-current="true"]').getAttribute('data-job-id'); }
async function allJobs(page) {
  const jobs = []; let cursor = '';
  do { const result = await api(page, `/api/v1/jobs${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`); jobs.push(...result.jobs); cursor = result.nextCursor || ''; } while (cursor);
  return jobs;
}

test('real jobs: create, edit recurrence, repeated run clicks, pause/resume, cancel and filter', async ({ page }) => {
  await open(page); const title = unique('Priorities'); await create(page, title);
  await page.getByRole('button', { name: 'Edit job', exact: true }).click();
  await page.getByLabel('Job title', { exact: true }).fill(`${title} edited`);
  await page.getByLabel('Repeat', { exact: true }).selectOption('weekdays');
  await page.getByLabel('Timezone', { exact: true }).fill('America/New_York');
  await page.getByLabel('Time', { exact: true }).fill('09:30');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await expect(page.locator('#jobSelectedTitle')).toHaveText(`${title} edited`);
  await expect(page.locator('#jobContent')).toContainText('Weekdays at 09:30');
  const id = await selectedID(page); let calls = 0;
  await page.route(`**/api/v1/jobs/${id}/run`, async (route) => { calls++; await new Promise((resolve) => setTimeout(resolve, 200)); await route.continue(); });
  await page.getByRole('button', { name: 'Run now', exact: true }).evaluate((button) => { button.click(); button.click(); button.click(); });
  await expect(page.locator('#jobsRuns .jobs-badge').first()).toHaveText('Completed');
  expect(calls).toBe(1);
  expect((await api(page, `/api/v1/jobs/${id}/runs`)).runs).toHaveLength(1);
  await page.getByRole('button', { name: 'Pause', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Resume', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Resume', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Run now', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Cancel job', exact: true }).click();
  await page.getByRole('button', { name: 'Confirm cancellation', exact: true }).click();
  await expect(page.locator('#jobConfirm')).not.toBeVisible();
  await expect(page.locator('#jobContent > .jobs-detail-head .jobs-badge')).toHaveText('Cancelled');
  await page.getByLabel('Show', { exact: true }).selectOption('cancelled');
  await expect(page.locator(`.jobs-card[data-job-id="${id}"]`)).toBeVisible();
});

test('real review checkpoints show exact content and support explicit approve and cancel-run', async ({ page }) => {
  await open(page); await create(page, unique('Review'), 'review', 'Review the exact weekly priorities; do not send email.');
  await page.getByRole('button', { name: 'Run now', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Review & approve', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Review & approve', exact: true }).click();
  await expect(page.locator('#jobConfirmDetails')).toContainText('Review the exact weekly priorities; do not send email.');
  await expect(page.locator('#jobConfirmText')).toContainText('does not send messages');
  await page.getByRole('button', { name: 'Approve this review', exact: true }).click();
  await expect(page.locator('#jobConfirm')).not.toBeVisible();
  await expect(page.locator('#jobsRuns .jobs-badge').first()).toHaveText('Completed');
  await page.getByRole('button', { name: 'Run now', exact: true }).click();
  await page.getByRole('button', { name: 'Cancel run', exact: true }).click();
  await page.getByRole('button', { name: 'Confirm cancellation', exact: true }).click();
  await expect(page.locator('#jobConfirm')).not.toBeVisible();
  await expect(page.locator('#jobsRuns .jobs-badge').first()).toHaveText('Cancelled');
  await expect(page.getByRole('button', { name: 'Run now', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Retry run', exact: true }).click();
  await expect(page.locator('#jobsRuns .jobs-badge').first()).toHaveText('Needs review');
  await expect(page.locator('#jobsRuns .jobs-run').first()).toContainText('attempt 2');
});

test('real stale edit conflict preserves input until the user explicitly loads latest', async ({ page }) => {
  await open(page); await create(page, unique('Versioned'));
  const id = await selectedID(page); const { job } = await api(page, `/api/v1/jobs/${id}`);
  await page.getByRole('button', { name: 'Edit job', exact: true }).click();
  await page.getByLabel('Job title', { exact: true }).fill('My unsaved edit');
  await api(page, `/api/v1/jobs/${id}`, 'PATCH', { title: 'Changed in another tab', instructions: job.instructions, kind: job.kind, schedule: job.schedule, expectedVersion: job.version, requestId: crypto.randomUUID() });
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await expect(page.locator('#jobFormError')).toContainText('Your edits are kept');
  await expect(page.getByLabel('Job title', { exact: true })).toHaveValue('My unsaved edit');
  await page.getByRole('button', { name: 'Load latest version (replaces your edits)', exact: true }).click();
  await expect(page.getByLabel('Job title', { exact: true })).toHaveValue('Changed in another tab');
});

test('load failure recovers and ambiguous save retry retains the same request identity', async ({ page }) => {
  let failList = true;
  await page.route('**/api/v1/jobs', async (route) => {
    if (route.request().method() === 'GET' && failList) { failList = false; await route.fulfill({ status: 503, json: { error: { code: 'unavailable', message: 'Temporarily unavailable' } } }); }
    else await route.continue();
  });
  await page.goto('/jobs'); await expect(page.locator('#jobsError')).toBeVisible();
  await page.getByRole('button', { name: 'Try again', exact: true }).click(); await expect(page.locator('#jobNew')).toBeEnabled();
  await page.unroute('**/api/v1/jobs');
  const ids = []; let attempts = 0;
  await page.route('**/api/v1/jobs', async (route) => {
    if (route.request().method() !== 'POST') return route.continue();
    ids.push(route.request().postDataJSON().requestId); attempts++;
    if (attempts === 1) {
      // The server actually accepts it, then the browser loses the response.
      await route.fetch(); await route.abort('connectionreset');
    } else await route.continue();
  });
  await page.getByRole('button', { name: 'New job', exact: true }).click();
  const title = unique('Uncertain delivery');
  await page.getByLabel('Job title', { exact: true }).fill(title); await page.getByLabel('Notes', { exact: true }).fill('Keep this input after a network problem.');
  await page.getByRole('button', { name: 'Create job', exact: true }).click();
  await expect(page.locator('#jobFormError')).toBeVisible();
  await expect(page.getByLabel('Job title', { exact: true })).toHaveValue(title);
  await page.getByRole('button', { name: 'Create job', exact: true }).click();
  await expect(page.locator('#jobEditor')).not.toBeVisible();
  expect(ids).toHaveLength(2); expect(ids[1]).toBe(ids[0]);
  expect((await allJobs(page)).filter((job) => job.title === title)).toHaveLength(1);
});

test('user content is inert, keyboard editor works and Jobs navigation is discoverable', async ({ page }) => {
  await open(page);
  await expect(page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: 'Jobs', exact: true })).toHaveAttribute('aria-current', 'page');
  const text = '<img src=x onerror="window.jobsXSS=true">';
  await create(page, text, 'reminder', '<script>window.jobsXSS=true</script>');
  expect(await page.evaluate(() => window.jobsXSS)).toBeUndefined();
  await expect(page.locator('#jobContent img,#jobContent script')).toHaveCount(0);
  await page.getByRole('button', { name: 'Edit job', exact: true }).click();
  await expect(page.getByLabel('Job title', { exact: true })).toBeFocused();
  await page.keyboard.press('Escape'); await expect(page.locator('#jobEditor')).not.toBeVisible();
  const a11y = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze();
  expect(a11y.violations).toEqual([]);
  await page.setViewportSize({ width: 320, height: 720 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(1);
});

test('unavailable scheduling is explicit and unsupported providers cannot be selected', async ({ page }) => {
  await page.route('**/api/v1/jobs', async (route) => {
    if (route.request().method() !== 'GET') return route.continue();
    const response = await route.fetch(); const body = await response.json();
    body.capabilities.scheduling = false; await route.fulfill({ response, json: body });
  });
  await open(page); await expect(page.locator('#jobsCapability')).toContainText('Automatic scheduling unavailable');
  await page.getByRole('button', { name: 'New job', exact: true }).click();
  await expect(page.locator('#jobSchedule option[value="daily"]')).toBeDisabled();
  await expect(page.getByLabel('Repeat', { exact: true })).toHaveValue('manual');
  expect(await page.locator('#jobKind option').allTextContents()).toEqual(['Reminder', 'Human review']);
});

test('real one-time schedule produces exactly one receipt through the background worker', async ({ page }) => {
  await open(page);
  const { job } = await api(page, '/api/v1/jobs', 'POST', { title: unique('Scheduled receipt'), instructions: 'A one-time in-app reminder from the real local worker.', kind: 'reminder', schedule: { kind: 'once', timezone: 'UTC', at: new Date(Date.now() + 2500).toISOString() }, requestId: crypto.randomUUID() });
  await expect.poll(async () => (await api(page, `/api/v1/jobs/${job.id}/runs`)).runs[0]?.status, { timeout: 10000 }).toBe('succeeded');
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await expect(page.locator('#jobsLoading')).not.toBeVisible();
  let card = page.locator(`.jobs-card[data-job-id="${job.id}"]`);
  // Query pages are intentionally bounded; follow real pagination as needed.
  for (let pages = 0; !(await card.count()) && pages < 10; pages++) {
    if (!(await page.locator('#jobsMore').isVisible())) break;
    await page.locator('#jobsMore').click();
    await expect(page.locator('#jobsLoading')).not.toBeVisible();
  }
  await card.click();
  await expect(page.locator('#jobsRuns .jobs-badge').first()).toHaveText('Completed');
  const history = await api(page, `/api/v1/jobs/${job.id}/runs`);
  expect(history.runs).toHaveLength(1);
  expect((await api(page, `/api/v1/jobs/${job.id}`)).job.nextRunAt || '').toBe('');
});

test('background refresh preserves expanded receipt and keyboard focus', async ({ page }) => {
  await open(page); await create(page, unique('Stable review'));
  await page.getByRole('button', { name: 'Run now', exact: true }).click();
  await expect(page.locator('#jobsRuns .jobs-badge').first()).toHaveText('Completed');
  const summary = page.locator('#jobsRuns summary').first();
  await summary.click(); await summary.focus();
  const refreshed = page.waitForResponse((response) => response.url().endsWith('/runs') && response.request().method() === 'GET');
  // Trigger the same refresh as the timer without deliberately moving focus
  // onto the Refresh button, then verify the unchanged detail isn't replaced.
  await page.locator('#jobsRefresh').evaluate((button) => button.click()); await refreshed;
  await expect(page.locator('#jobsRuns details').first()).toHaveAttribute('open', '');
  await expect(summary).toBeFocused();
});

test('loading, empty, failed receipt and retry error states remain clear', async ({ page }) => {
  let releaseList;
  const hold = new Promise((resolve) => { releaseList = resolve; });
  await page.route('**/api/v1/jobs', async (route) => {
    if (route.request().method() !== 'GET') return route.continue();
    await hold;
    const response = await route.fetch(); const body = await response.json();
    body.jobs = []; delete body.nextCursor; await route.fulfill({ response, json: body });
  });
  await page.goto('/jobs'); await expect(page.locator('#jobsLoading')).toBeVisible(); releaseList();
  await expect(page.locator('#jobsEmpty')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Create your first job', exact: true })).toBeEnabled();
  await page.unroute('**/api/v1/jobs');
  await create(page, unique('Failure rendering'));
  await page.getByRole('button', { name: 'Run now', exact: true }).click();
  await expect(page.locator('#jobsRuns .jobs-badge').first()).toHaveText('Completed');
  const id = await selectedID(page);
  // Inject an error response to verify the UI's failure path. The actual
  // reminder provider does not manufacture failures merely for a screenshot.
  await page.route(`**/api/v1/jobs/${id}/runs`, async (route) => {
    const response = await route.fetch(); const body = await response.json();
    body.runs[0].status = 'failed'; body.runs[0].result = ''; body.runs[0].error = 'Review expired. Check the saved contents before retrying.';
    await route.fulfill({ response, json: body });
  });
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await expect(page.locator('#jobsRuns')).toContainText('Review expired.');
  await page.getByRole('button', { name: 'Retry run', exact: true }).click();
  // The real API rejects retrying this already-successful run. The UI must
  // show that conflict, not manufacture success from its synthetic GET state.
  await expect(page.locator('#jobsNotice')).toContainText('latest version');
});

test('local preview rejects absent bearer, wrong bearer, cross-site origin and host', async ({ request }) => {
  expect((await request.get('/api/v1/jobs')).status()).toBe(401);
  expect((await request.get('/api/v1/jobs', { headers: { Authorization: 'Bearer invalid-preview-token' } })).status()).toBe(401);
  expect((await request.post('/api/v1/auth/refresh', { headers: { Origin: 'https://untrusted.example' } })).status()).toBe(403);
  expect((await request.get('/healthz', { headers: { Host: 'untrusted.example' } })).status()).toBe(403);
  expect((await request.get('/healthz', { headers: { 'Sec-Fetch-Site': 'cross-site' } })).status()).toBe(403);
});

test('schedule editor saves daily, weekly Sunday and one-time values without timezone drift', async ({ page }) => {
  await open(page); await create(page, unique('Schedule choices'));
  const id = await selectedID(page);
  for (const kind of ['daily', 'weekly', 'once']) {
    await page.getByRole('button', { name: 'Edit job', exact: true }).click();
    await page.getByLabel('Repeat', { exact: true }).selectOption(kind);
    await page.getByLabel('Timezone', { exact: true }).fill('Asia/Kathmandu');
    if (kind === 'once') await page.getByLabel('Date and time', { exact: true }).fill('2030-07-04T09:15');
    else await page.getByLabel('Time', { exact: true }).fill('09:15');
    if (kind === 'weekly') await page.getByLabel('Day of week', { exact: true }).selectOption('0');
    await page.getByRole('button', { name: 'Save changes', exact: true }).click();
    await expect(page.locator('#jobEditor')).not.toBeVisible();
    const { job } = await api(page, `/api/v1/jobs/${id}`);
    expect(job.schedule.kind).toBe(kind); expect(job.schedule.timezone).toBe('Asia/Kathmandu');
    if (kind === 'once') expect(Date.parse(job.schedule.at)).toBe(Date.parse('2030-07-04T03:30:00Z'));
    if (kind === 'weekly') {
      await expect(page.locator('#jobContent')).toContainText('Every Sunday at 09:15');
      await page.getByRole('button', { name: 'Edit job', exact: true }).click();
      await expect(page.getByLabel('Day of week', { exact: true })).toHaveValue('0');
      await page.getByRole('button', { name: 'Close job editor', exact: true }).click();
    }
  }
});

test('older run pages survive background refresh until an explicit refresh', async ({ page }) => {
  await open(page); await create(page, unique('Long history'));
  const id = await selectedID(page); let { job } = await api(page, `/api/v1/jobs/${id}`);
  for (let i = 0; i < 23; i++) {
    ({ job } = await api(page, `/api/v1/jobs/${id}/run`, 'POST', { expectedVersion: job.version, requestId: crypto.randomUUID() }));
  }
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await expect(page.locator('#jobsRuns .jobs-run')).toHaveCount(20);
  await page.getByRole('button', { name: 'Load older runs', exact: true }).click();
  await expect(page.locator('#jobsRuns .jobs-run')).toHaveCount(23);
  const refreshed = page.waitForResponse((response) => response.url().endsWith('/api/v1/jobs') && response.request().method() === 'GET');
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange'))); await refreshed;
  await expect(page.locator('#jobsRuns .jobs-run')).toHaveCount(23);
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await expect(page.locator('#jobsRuns .jobs-run')).toHaveCount(20);
});
