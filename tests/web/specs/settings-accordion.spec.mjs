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

test('settings open and close bars match at 40 percent of the viewport height', async ({ page }) => {
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

  const viewport = page.viewportSize();
  const openerBox = await page.locator('#openSettings').boundingBox();
  expect(openerBox).not.toBeNull();
  expect(openerBox.height).toBeCloseTo(viewport.height * 0.4, 0);

  // Which edge the pair lives on is width-dependent (mobile shell,
  // 2026-08-01): on a computer the opener is flush RIGHT and the in-drawer
  // close bar mirrors to the left, but at <=900px the whole page is one
  // column and the tabs move to the LEFT edge so they stay clear of the
  // right-hand thumb, with the close bar mirroring to the right. What the
  // test actually pins either way is that the two bars are the same size and
  // sit on OPPOSITE edges at the same height.
  const mobile = viewport.width <= 900;
  if (mobile) {
    expect(openerBox.x).toBeCloseTo(0, 0);
  } else {
    expect(openerBox.x + openerBox.width).toBeCloseTo(viewport.width, 0);
  }

  await page.locator('#drawer').evaluate((dialog) => dialog.showModal());
  const closeBox = await page.locator('#closeSettings').boundingBox();
  expect(closeBox).not.toBeNull();
  expect(closeBox.height).toBeCloseTo(openerBox.height, 0);
  expect(closeBox.width).toBeCloseTo(openerBox.width, 0);
  expect(closeBox.y).toBeCloseTo(openerBox.y, 0);
  if (mobile) {
    expect(closeBox.x + closeBox.width).toBeCloseTo(viewport.width, 0);
  } else {
    expect(closeBox.x).toBeCloseTo(0, 0);
  }
});
