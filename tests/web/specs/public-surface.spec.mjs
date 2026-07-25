import { test, expect } from '@playwright/test';

/**
 * Contract tests for the surface an unauthenticated visitor (or a crawler) can
 * reach. These are the checks that would have caught real regressions this
 * project has hit: a stale-HTML cache header, a broken PWA manifest, and an auth
 * gate that stops redirecting.
 */

test('healthz answers 200 with JSON', async ({ request }) => {
  const res = await request.get('/healthz');
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toContain('application/json');
});

test('HTML is served no-cache so a deploy is never served stale', async ({ request }) => {
  const res = await request.get('/');
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toContain('text/html');

  // The whole "hard-refresh required after deploy" class of bug starts with a
  // cacheable HTML document. Either no-cache or an explicitly short max-age is
  // acceptable; a long-lived or absent directive is not.
  const cc = res.headers()['cache-control'] || '';
  const shortMaxAge = /max-age=(\d+)/.exec(cc);
  const ok =
    /no-cache|no-store/.test(cc) || (shortMaxAge && Number(shortMaxAge[1]) <= 300);
  expect(ok, `HTML Cache-Control was "${cc}"`).toBeTruthy();
});

test('the service worker is served as JavaScript from the root scope', async ({ request }) => {
  const res = await request.get('/sw.js');
  expect(res.status()).toBe(200);
  // A service worker served from /static/ could only ever control /static/, so
  // the root path matters as much as the content type.
  expect(res.headers()['content-type']).toMatch(/javascript/);
  expect(await res.text()).toContain('addEventListener');
});

test('the PWA manifest is valid and installable', async ({ request }) => {
  const res = await request.get('/static/manifest.webmanifest');
  expect(res.status()).toBe(200);

  const manifest = JSON.parse(await res.text());
  expect(manifest.name || manifest.short_name).toBeTruthy();
  expect(manifest.start_url).toBeTruthy();
  expect(Array.isArray(manifest.icons) && manifest.icons.length).toBeTruthy();

  // Installability needs a 192px and a 512px icon; assert they actually resolve
  // rather than just that the manifest claims them.
  const sizes = manifest.icons.flatMap((i) => String(i.sizes || '').split(' '));
  expect(sizes).toContain('192x192');
  expect(sizes).toContain('512x512');

  for (const icon of manifest.icons) {
    const iconRes = await request.get(new URL(icon.src, 'https://x/').pathname);
    expect(iconRes.status(), `manifest icon ${icon.src} is not reachable`).toBe(200);
  }
});

test('the landing page references its manifest', async ({ page }) => {
  await page.goto('/', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('link[rel="manifest"]')).toHaveCount(1);
});

/**
 * The auth gate. These routes are deliberately on the API-Gateway authorizer's
 * public allowlist (cmd/authorizer/main.go) so that a signed-in browser gets HTML
 * rather than a bare 403 JSON — which means Fiber's own cookie check is the ONLY
 * thing standing between an anonymous visitor and the app shell. That makes it
 * worth an explicit test.
 */
for (const path of ['/conversation', '/history', '/memory', '/personas', '/downloads']) {
  test(`${path} redirects an anonymous visitor to sign-in`, async ({ request }) => {
    const res = await request.get(path, { maxRedirects: 0 });
    expect([301, 302, 303, 307, 308]).toContain(res.status());
    const location = res.headers()['location'] || '';
    expect(location === '/' || /\/$|\/\?|login/.test(location)).toBeTruthy();
  });
}

test('the landing page does not leak the authed app shell', async ({ page }) => {
  await page.goto('/conversation', { waitUntil: 'domcontentloaded' });
  // Followed the redirect: we must be on the sign-in page, not the transcript UI.
  await expect(page.locator('#costBadge')).toHaveCount(0);
  await expect(page.locator('h1')).toHaveCount(1);
});

test('layout reflows without horizontal scroll at 320px and at 200% zoom', async ({ page }) => {
  // WCAG 1.4.10 reflow, and the house rule that the page body must never scroll
  // horizontally. 320px is the narrowest supported width.
  await page.setViewportSize({ width: 320, height: 720 });
  await page.goto('/', { waitUntil: 'domcontentloaded' });

  const overflowAt320 = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflowAt320, 'body scrolls horizontally at 320px').toBeLessThanOrEqual(1);

  // 200% zoom is equivalent to halving the CSS viewport at the same device width.
  await page.setViewportSize({ width: 640, height: 512 });
  await page.evaluate(() => {
    document.documentElement.style.zoom = '200%';
  });
  const overflowZoomed = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflowZoomed, 'body scrolls horizontally at 200% zoom').toBeLessThanOrEqual(1);
});

test('no console errors on first load', async ({ page }) => {
  const errors = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') errors.push(msg.text());
  });
  page.on('pageerror', (err) => errors.push(String(err)));

  await page.goto('/', { waitUntil: 'load' });
  await page.waitForTimeout(1500); // let deferred module scripts run

  expect(errors, `console errors:\n${errors.join('\n')}`).toEqual([]);
});
