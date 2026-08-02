import { readFile } from 'node:fs/promises';
import { test, expect } from '@playwright/test';

const accordionSource = await readFile(
  new URL('../../../web/static/js/settings-accordion.mjs', import.meta.url),
  'utf8',
);
const appStyles = await readFile(
  new URL('../../../web/static/css/app.css', import.meta.url),
  'utf8',
);
const conversationTemplate = await readFile(
  new URL('../../../web/templates/pages/conversation.html', import.meta.url),
  'utf8',
);
const conversationSource = await readFile(
  new URL('../../../web/static/js/conversation.mjs', import.meta.url),
  'utf8',
);

const section = (name, expanded) => `
  <section>
    <h2>
      <button id="${name}Trigger" data-settings-accordion-trigger
              aria-expanded="${expanded}" aria-controls="${name}Panel">${name}</button>
    </h2>
    <div id="${name}Panel" role="region" aria-labelledby="${name}Trigger"
         ${expanded ? '' : 'hidden'}>
      <button>${name} control</button>
    </div>
  </section>`;

test.beforeEach(async ({ page }) => {
  await page.setContent(`
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <main id="settings">
      ${section('about', true)}
      ${section('voice', false)}
      ${section('privacy', false)}
    </main>
  `);
  await page.addScriptTag({
    type: 'module',
    content: `${accordionSource}
      window.__settingsAccordion = initSettingsAccordion(document.querySelector('#settings'));`,
  });
  await page.waitForFunction(() => Boolean(window.__settingsAccordion));
});

test('settings accordion keeps one panel open and allows all panels to collapse', async ({ page }) => {
  const about = page.locator('#aboutTrigger');
  const voice = page.locator('#voiceTrigger');

  await expect(about).toHaveAttribute('aria-expanded', 'true');
  await expect(page.locator('#aboutPanel')).toBeVisible();
  await expect(page.locator('#voicePanel')).toBeHidden();

  await voice.click();
  await expect(voice).toHaveAttribute('aria-expanded', 'true');
  await expect(page.locator('#voicePanel')).toBeVisible();
  await expect(about).toHaveAttribute('aria-expanded', 'false');
  await expect(page.locator('#aboutPanel')).toBeHidden();
  await expect(page.locator('[data-settings-accordion-trigger][aria-expanded="true"]')).toHaveCount(1);

  await voice.click();
  await expect(voice).toHaveAttribute('aria-expanded', 'false');
  await expect(page.locator('#voicePanel')).toBeHidden();
  await expect(page.locator('[data-settings-accordion-trigger][aria-expanded="true"]')).toHaveCount(0);
});

test('deep-linked settings initializes drawer cost state before opening', async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop-chrome',
    'source-order contract only needs one project',
  );

  const deepLink = conversationSource.indexOf(
    "new URLSearchParams(window.location.search).get('openSettings') === '1'",
  );
  const immediateOpen = conversationSource.indexOf('openSettingsDrawer();', deepLink);

  for (const declaration of [
    "const drawerCostEl = $('drawerCost');",
    "const drawerCostValue = $('drawerCostValue');",
    "const drawerCostSub = $('drawerCostSub');",
    'let drawerCostFetchedAt = 0;',
  ]) {
    const costState = conversationSource.indexOf(declaration);
    expect(costState, declaration).toBeGreaterThanOrEqual(0);
    expect(deepLink, declaration).toBeGreaterThan(costState);
  }
  expect(immediateOpen).toBeGreaterThan(deepLink);
});

test('shipped settings markup keeps every control inside its owned panel', async ({ page }) => {
  const drawerStart = conversationTemplate.indexOf('<button class="conv-settings-tab"');
  const drawerEnd = conversationTemplate.indexOf('</dialog>', drawerStart) + '</dialog>'.length;
  expect(drawerStart).toBeGreaterThanOrEqual(0);
  expect(drawerEnd).toBeGreaterThan(drawerStart);
  await page.setContent(conversationTemplate.slice(drawerStart, drawerEnd));

  const triggers = page.locator('[data-settings-accordion-trigger]');
  await expect(triggers).toHaveCount(9);
  await expect(page.locator('[data-settings-accordion-trigger][aria-expanded="true"]')).toHaveCount(1);

  for (let i = 0; i < await triggers.count(); i++) {
    const trigger = triggers.nth(i);
    const panelId = await trigger.getAttribute('aria-controls');
    const panel = page.locator(`#${panelId}`);
    await expect(panel).toHaveCount(1);
    expect(await panel.locator('input, select, textarea, button').count()).toBeGreaterThan(0);
    expect(await trigger.evaluate((node) => node.parentElement.tagName)).toBe('H2');
    const triggerSection = await trigger.evaluate(
      (node) => node.closest('section')?.getAttribute('aria-labelledby'),
    );
    const panelSection = await panel.evaluate(
      (node) => node.closest('section')?.getAttribute('aria-labelledby'),
    );
    expect(panelSection).toBe(triggerSection);
  }
});

test('settings accordion supports header navigation and native keyboard activation', async ({ page }) => {
  const about = page.locator('#aboutTrigger');
  const voice = page.locator('#voiceTrigger');
  const privacy = page.locator('#privacyTrigger');

  await about.focus();
  await about.press('ArrowUp');
  await expect(privacy).toBeFocused();
  await privacy.press('Home');
  await expect(about).toBeFocused();
  await about.press('End');
  await expect(privacy).toBeFocused();
  await privacy.press('ArrowDown');
  await expect(about).toBeFocused();

  await voice.focus();
  await voice.press('Enter');
  await expect(voice).toHaveAttribute('aria-expanded', 'true');
  await expect(page.locator('#voicePanel')).toBeVisible();
});

test('settings open and close tabs are matching corner squares', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.addStyleTag({ content: appStyles });
  await page.evaluate(() => {
    document.body.insertAdjacentHTML('beforeend', `
      <button class="conv-settings-tab" id="openSettings" aria-label="Open settings">
        <span class="conv-settings-tab__label">Settings</span>
      </button>
      <dialog class="conv-drawer" id="drawer">
        <button class="conv-settings-tab conv-settings-tab--close"
                id="closeSettings" aria-label="Close settings">
          <span class="conv-settings-tab__label">Close</span>
        </button>
      </dialog>
    `);
  });

  // The size is read from the CSS custom properties rather than hardcoded, so
  // this stays true if the owner retunes the tab. --ln-edge-tab is the 40px
  // height; --ln-touch is the 44px width, which is what keeps the tap target
  // past the WCAG 2.2 AA minimum even though the tab reads as a small square.
  const tokens = await page.evaluate(() => {
    const s = getComputedStyle(document.documentElement);
    return {
      edge: parseFloat(s.getPropertyValue('--ln-edge-tab')),
      touch: parseFloat(s.getPropertyValue('--ln-touch')),
    };
  });
  expect(tokens.edge).toBeGreaterThan(0);
  expect(tokens.touch).toBeGreaterThan(0);

  const viewport = page.viewportSize();
  const openerBox = await page.locator('#openSettings').boundingBox();
  expect(openerBox).not.toBeNull();
  expect(openerBox.height).toBeCloseTo(tokens.edge, 0);
  expect(openerBox.width).toBeCloseTo(tokens.touch, 0);

  // Corner tabs, at EVERY width (owner, 2026-08-01). The previous contract —
  // 40dvh bars, flush right on a computer and flush left on a phone — was
  // replaced wholesale: a bar tall enough to be found by feel was also tall
  // enough to paint over the transcript's right-hand bubbles at tablet
  // widths, which is what "the conversation is cut off" turned out to be.
  // A corner tab can only ever overlap one corner. There is deliberately no
  // width-dependent branch here any more; if one comes back, the tabs have
  // moved and this test should be the thing that says so.
  expect(openerBox.x).toBeCloseTo(0, 0);
  expect(openerBox.y).toBeGreaterThan(0);
  expect(openerBox.y).toBeLessThan(viewport.height / 2);

  await page.locator('#drawer').evaluate((dialog) => dialog.showModal());
  const closeBox = await page.locator('#closeSettings').boundingBox();
  expect(closeBox).not.toBeNull();
  // Same size, same height up the page, mirrored to the opposite corner so the
  // close tab can never be drawn on top of the opener it replaces.
  expect(closeBox.height).toBeCloseTo(openerBox.height, 0);
  expect(closeBox.width).toBeCloseTo(openerBox.width, 0);
  expect(closeBox.y).toBeCloseTo(openerBox.y, 0);
  expect(closeBox.x + closeBox.width).toBeCloseTo(viewport.width, 0);
});
