import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

/**
 * WCAG 2.1 AA gate (plan.md WS-1 1.6, the M3 remainder).
 *
 * Scope is deliberately and explicitly the **unauthenticated** surface. Every
 * interesting authed screen (/conversation, /history, /memory, /personas) sits
 * behind a Login-with-Amazon session that CI has no credentials for — those routes
 * 302 to the landing page, which is asserted in public-surface.spec.mjs. Scanning
 * them here would produce a green result that means nothing, so they are left out
 * rather than faked; they stay owner-verified under WS-1 1.4.
 *
 * Both colour schemes are scanned because the house rule requires AA contrast in
 * light *and* dark, and a single-theme scan cannot see a dark-mode regression.
 */

/** Public, server-rendered pages worth holding to AA. */
const PUBLIC_PAGES = [
  { path: '/', name: 'landing / sign-in' },
];

const WCAG_AA_TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];

/** Renders an axe violation list into something reviewable in CI logs. */
function formatViolations(violations) {
  return violations
    .map((v) => {
      const where = v.nodes.map((n) => `      - ${n.target.join(' ')}`).join('\n');
      return `  [${v.impact}] ${v.id}: ${v.help}\n    ${v.helpUrl}\n${where}`;
    })
    .join('\n\n');
}

for (const { path, name } of PUBLIC_PAGES) {
  for (const colorScheme of /** @type {const} */ (['light', 'dark'])) {
    test(`${name} has no WCAG 2.1 AA violations (${colorScheme})`, async ({ page }) => {
      await page.emulateMedia({ colorScheme });
      await page.goto(path, { waitUntil: 'domcontentloaded' });

      const { violations } = await new AxeBuilder({ page })
        .withTags([...WCAG_AA_TAGS])
        .analyze();

      expect(
        violations,
        violations.length ? `\n${formatViolations(violations)}\n` : '',
      ).toEqual([]);
    });
  }
}

test('landing page carries the document-level semantics screen readers need', async ({ page }) => {
  await page.goto('/', { waitUntil: 'domcontentloaded' });

  // A missing lang attribute silently gives every screen reader the wrong
  // pronunciation rules for the whole document, and axe only flags it as a
  // violation when it is absent entirely — assert the value too.
  await expect(page.locator('html')).toHaveAttribute('lang', /^en(-|$)/);

  await expect(page).toHaveTitle(/Live Ninja/);

  // Exactly one h1: zero leaves the page without a landmark heading, more than
  // one makes the outline ambiguous for heading-based navigation.
  await expect(page.locator('h1')).toHaveCount(1);
});

test('every focusable control on the landing page shows a visible focus ring', async ({ page }) => {
  await page.goto('/', { waitUntil: 'domcontentloaded' });

  const focusables = page.locator(
    'a[href], button, input:not([type=hidden]), select, textarea, [tabindex]:not([tabindex="-1"])',
  );
  const count = await focusables.count();
  expect(count, 'landing page should expose at least one focusable control').toBeGreaterThan(0);

  for (let i = 0; i < count; i++) {
    const el = focusables.nth(i);
    if (!(await el.isVisible())) continue;
    await el.focus();
    // `outline: none` with nothing replacing it is the specific regression this
    // catches — a keyboard user then has no idea where they are on the page.
    const ring = await el.evaluate((node) => {
      const s = getComputedStyle(node);
      return {
        outlineStyle: s.outlineStyle,
        outlineWidth: parseFloat(s.outlineWidth) || 0,
        boxShadow: s.boxShadow,
      };
    });
    const hasRing =
      (ring.outlineStyle !== 'none' && ring.outlineWidth > 0) ||
      (ring.boxShadow && ring.boxShadow !== 'none');
    expect(hasRing, `focusable #${i} has no visible focus indicator`).toBeTruthy();
  }
});
