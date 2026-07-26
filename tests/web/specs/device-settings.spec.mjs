import { readFile } from 'node:fs/promises';
import { test, expect } from '@playwright/test';

const ROOT = new URL('../../../', import.meta.url);
const identity = await import(new URL('web/static/js/device-identity.mjs', ROOT));
const deviceSettings = await import(new URL('web/static/js/device-settings.mjs', ROOT));
const { ApiError } = await import(new URL('web/static/js/toolclient.mjs', ROOT));
const deviceSettingsSource = await readFile(
  new URL('web/static/js/device-settings.mjs', ROOT),
  'utf8',
);
const appStyles = await readFile(new URL('web/static/css/app.css', ROOT), 'utf8');

function desktopOnly(testInfo) {
  test.skip(testInfo.project.name !== 'desktop-chrome', 'local contract only needs one project');
}

test('browser device identity is stable and stores only normalized host information', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const values = new Map();
  const storage = {
    getItem: (key) => values.get(key) || null,
    setItem: (key, value) => values.set(key, value),
  };
  const id = '12345678-1234-4234-9234-123456789abc';
  const cryptoImpl = { randomUUID: () => id };

  expect(identity.getDeviceID({ storage, cryptoImpl })).toBe(id);
  expect(identity.getDeviceID({ storage, cryptoImpl })).toBe(id);
  expect(values.get(identity.DEVICE_ID_STORAGE_KEY)).toBe(id);

  const rawUserAgent = 'SECRET-RAW-UA Chrome/140.0 Windows NT 10.0';
  const inferred = identity.inferDeviceIdentity({
    userAgent: rawUserAgent,
    platform: 'Win32',
    userAgentData: {
      brands: [{ brand: 'Google Chrome', version: '140' }],
      platform: 'Windows',
      mobile: false,
    },
  });
  expect(inferred.suggestedName).toBe('Chrome on Windows');
  expect(inferred.metadata).toEqual({
    surface: 'web',
    browser: 'Chrome',
    platform: 'Windows',
    deviceClass: 'desktop',
  });
  expect(JSON.stringify(inferred)).not.toContain(rawUserAgent);
});

test('registration rotates a conflicted device ID and retries exactly once', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const oldID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const rotatedID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const values = new Map([[identity.DEVICE_ID_STORAGE_KEY, oldID]]);
  const storage = {
    getItem: (key) => values.get(key) || null,
    setItem: (key, value) => values.set(key, value),
  };
  const getID = () =>
    identity.getDeviceID({ storage, cryptoImpl: { randomUUID: () => oldID } });
  const rotateID = () =>
    identity.rotateDeviceID({
      storage,
      cryptoImpl: { randomUUID: () => rotatedID },
    });
  const requests = [];
  const response = await deviceSettings.registerCurrentDevice({
    getID,
    rotateID,
    identity: {
      suggestedName: 'Chrome on Windows',
      metadata: { browser: 'Chrome', platform: 'Windows' },
      capabilities: ['privacy'],
    },
    request: async (path, options) => {
      requests.push({ path, options });
      if (requests.length === 1) {
        throw new ApiError(409, {
          error: { code: 'device_conflict', message: 'already owned' },
        });
      }
      return { device: { deviceId: options.json.deviceId } };
    },
  });

  expect(requests).toHaveLength(2);
  expect(requests.map((entry) => entry.options.json.deviceId)).toEqual([
    oldID,
    rotatedID,
  ]);
  expect(response.device.deviceId).toBe(rotatedID);
  expect(values.get(identity.DEVICE_ID_STORAGE_KEY)).toBe(rotatedID);
  expect(getID()).toBe(rotatedID);
});

test('revoked persisted identity rotates and retries exactly once after reauthentication', async ({}, testInfo) => {
  desktopOnly(testInfo);
  let requests = 0;
  let revokedRotations = 0;
  let currentID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const response = await deviceSettings.registerCurrentDevice({
    getID: () => currentID,
    rotateID: () => {
      revokedRotations += 1;
      currentID = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
      return currentID;
    },
    identity: { suggestedName: 'Chrome on Windows' },
    request: async (_path, options) => {
      requests += 1;
      if (requests === 1) {
        throw new ApiError(409, {
          error: { code: 'device_revoked', message: 'revoked' },
        });
      }
      return { device: { deviceId: options.json.deviceId } };
    },
  });
  expect(requests).toBe(2);
  expect(revokedRotations).toBe(1);
  expect(response.device.deviceId).toBe(currentID);
});

test('registration recovery never retries more than once', async ({}, testInfo) => {
  desktopOnly(testInfo);
  let requests = 0;
  let rotations = 0;
  await expect(
    deviceSettings.registerCurrentDevice({
      getID: () => 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee',
      rotateID: () => {
        rotations += 1;
        return 'ffffffff-ffff-4fff-8fff-ffffffffffff';
      },
      identity: { suggestedName: 'Chrome on Windows' },
      request: async () => {
        requests += 1;
        throw new ApiError(409, {
          error: { code: 'device_revoked', message: 'still revoked' },
        });
      },
    }),
  ).rejects.toMatchObject({ code: 'device_revoked' });
  expect(requests).toBe(2);
  expect(rotations).toBe(1);
});

test('all eight configurable sections have host scope controls and account has naming only', async ({ page }, testInfo) => {
  desktopOnly(testInfo);
  const template = await readFile(
    new URL('web/templates/pages/conversation.html', ROOT),
    'utf8',
  );
  const sectionNames = Object.keys(deviceSettings.SETTINGS_SECTIONS);
  for (const section of sectionNames) {
    expect(template).toContain(`data-device-settings-root="${section}"`);
  }
  expect(sectionNames).toHaveLength(8);
  expect(template).toContain('data-account-devices-list');
  expect(template).not.toContain('data-device-settings-root="account"');

  const styles = await readFile(new URL('web/static/css/app.css', ROOT), 'utf8');
  expect(styles).toContain('.set-device-scope__actions');
  expect(styles).toContain('@media (max-width: 600px)');
  await page.setViewportSize({ width: 320, height: 700 });
});

test('section helpers preserve boundaries and reject copying named microphones', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const doc = {
    theme: 'dark',
    appearance: { appStyle: 'ninja' },
    privacy: { storeTranscripts: true },
    micDeviceId: 'browser-local-id',
  };
  expect(deviceSettings.sectionSettings(doc, 'appearance')).toEqual({
    theme: 'dark',
    appearance: { appStyle: 'ninja' },
  });
  expect(deviceSettings.sectionSettings(doc, 'appearance')).not.toHaveProperty('privacy');
  expect(deviceSettings.canCopyMicrophone({ micDeviceId: 'browser-local-id' })).toBeFalsy();
  expect(deviceSettings.canCopyMicrophone({ micDeviceId: null })).toBeTruthy();
});

test('authenticated requests carry the device header and settings writers use section PATCHes', async ({}, testInfo) => {
  desktopOnly(testInfo);
  const [toolclient, settings, conversation, editor] = await Promise.all([
    readFile(new URL('web/static/js/toolclient.mjs', ROOT), 'utf8'),
    readFile(new URL('web/static/js/settings.mjs', ROOT), 'utf8'),
    readFile(new URL('web/static/js/conversation.mjs', ROOT), 'utf8'),
    readFile(new URL('web/static/js/personaeditor.mjs', ROOT), 'utf8'),
  ]);
  expect(toolclient).toContain("'X-LN-Device-ID': getDeviceID()");
  for (const source of [settings, conversation, editor]) {
    expect(source).toContain('/api/v1/settings/sections/');
    expect(source).not.toMatch(
      /apiJSON\((?:SETTINGS_PATH|'\/api\/v1\/settings'),\s*\{\s*method:\s*'PUT'/,
    );
  }
  expect(settings).toContain('reconcileEffective: adoptServerSettings');
});

test('section GET is version-read-only and conflict retry reuses the clicked settings snapshot', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 700 });
  await page.setContent(`
    <section data-device-settings-root="privacy">
      <span class="ln-card__title">Privacy</span>
      <div class="set-accordion__panel"></div>
    </section>
    <div data-account-devices-list></div>
    <p data-account-devices-status></p>
  `);
  await page.addStyleTag({ content: appStyles });
  await page.evaluate(() => {
    window.__apiRequests = [];
    window.__reconciles = [];
    window.__barriers = [];
    window.__patchAttempts = 0;
    window.__sectionEnvelope = {
      section: 'privacy',
      version: 2,
      currentDeviceId: 'current-id',
      accountDefaults: { privacy: { storeTranscripts: true } },
      devices: [
        {
          deviceId: 'current-id',
          name: 'Chrome on Windows',
          isCurrent: true,
          inherited: false,
          capabilities: ['privacy'],
          metadata: { browser: 'Chrome', platform: 'Windows' },
          settings: { privacy: { storeTranscripts: false } },
        },
        {
          deviceId: 'tablet-id',
          name: 'Kitchen tablet',
          inherited: true,
          capabilities: ['privacy'],
          metadata: { browser: 'Chrome', platform: 'Android' },
          settings: { privacy: { storeTranscripts: true } },
        },
      ],
    };
    window.__apiJSON = async (path, options = {}) => {
      window.__apiRequests.push({ path, options });
      if (path === '/api/v1/devices') {
        return { devices: window.__sectionEnvelope.devices };
      }
      if (path === '/api/v1/settings/sections/privacy') {
        if (options.method === 'PATCH') {
          window.__patchAttempts += 1;
          if (window.__patchAttempts === 1) throw new window.__ApiError(409);
          return {
            ...window.__sectionEnvelope,
            version: window.__patchAttempts === 2 ? 100 : 101,
          };
        }
        return { ...window.__sectionEnvelope, version: 99 };
      }
      throw new Error(`unexpected path ${path}`);
    };
  });
  const runnable = deviceSettingsSource
    .replace(
      "import { apiJSON, ApiError } from './toolclient.mjs';",
      `const apiJSON = (...args) => window.__apiJSON(...args);
       class ApiError extends Error {
         constructor(status) { super('API error'); this.status = status; }
       }
       window.__ApiError = ApiError;`,
    )
    .replace(
      `import {
  getDeviceID,
  inferDeviceIdentity,
  rotateDeviceID,
} from './device-identity.mjs';`,
      "const getDeviceID = () => 'current-id'; const inferDeviceIdentity = () => ({}); const rotateDeviceID = () => 'rotated-id';",
    );
  await page.addScriptTag({
    type: 'module',
    content: `${runnable}
      window.__doc = { version: 2, privacy: { storeTranscripts: false } };
      window.__deviceController = initDeviceSettingsControls({
        getDocument: () => window.__doc,
        getVersion: () => window.__doc.version,
        reconcileEffective: async (version) => {
          window.__reconciles.push(version);
          window.__doc.version = version;
          if (version === 99) window.__doc.privacy.storeTranscripts = true;
          if (version === 100) window.__doc.privacy.storeTranscripts = false;
          if (version === 101) window.__doc.privacy.storeTranscripts = true;
          return true;
        },
        setWriteBarrier: (blocked) => window.__barriers.push(blocked),
      });`,
  });

  const trigger = page.getByRole('button', { name: /Device settings/ });
  await expect(trigger).toHaveAttribute('aria-expanded', 'false');
  await trigger.click();
  await expect(trigger).toHaveAttribute('aria-expanded', 'true');
  await expect.poll(() => page.evaluate(() => window.__apiRequests.length)).toBeGreaterThan(1);
  expect(await page.evaluate(() => window.__doc.version)).toBe(2);
  expect(await page.evaluate(() => window.__reconciles)).toEqual([]);
  await expect(page.getByText('Chrome on Windows', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Kitchen tablet', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Uses account default', { exact: true })).toBeVisible();

  const tablet = page.locator('[data-device-target][value="tablet-id"]');
  await tablet.check();
  const selectedAction = page.getByRole('button', {
    name: /Apply these settings to 1 selected device/,
  });
  const actionBox = await selectedAction.boundingBox();
  const panelBox = await page.locator('[data-device-settings-panel]').boundingBox();
  expect(actionBox.width).toBeLessThanOrEqual(panelBox.width);
  expect(actionBox.height).toBeGreaterThanOrEqual(44);
  await selectedAction.click();
  await expect.poll(async () =>
    page.evaluate(() =>
      window.__apiRequests.find((entry) => entry.options?.method === 'PATCH')?.options?.json,
    ),
  ).toEqual({
    version: 2,
    operation: 'set',
    target: { mode: 'selected', deviceIds: ['tablet-id'] },
    settings: { privacy: { storeTranscripts: false } },
  });
  await expect.poll(() => page.evaluate(() => window.__patchAttempts)).toBe(2);
  const patchBodies = await page.evaluate(() =>
    window.__apiRequests
      .filter((entry) => entry.options?.method === 'PATCH')
      .map((entry) => entry.options.json),
  );
  expect(patchBodies[1].version).toBe(99);
  expect(patchBodies[1].settings).toEqual({
    privacy: { storeTranscripts: false },
  });
  await expect.poll(() => page.evaluate(() => window.__reconciles)).toEqual([99, 100]);
  expect(await page.evaluate(() => window.__barriers)).toEqual([true, false]);
  expect(await page.evaluate(() => window.__doc.version)).toBe(100);

  const kitchenRow = page.locator('.set-device-row').filter({ hasText: 'Kitchen tablet' });
  await kitchenRow.getByText('View all privacy values').click();
  await expect(kitchenRow.getByText('Privacy · Store Transcripts')).toBeVisible();
  await expect(kitchenRow.getByText('On', { exact: true })).toBeVisible();
  await kitchenRow.getByRole('button', {
    name: 'Copy settings from Kitchen tablet',
  }).click();
  expect(await page.evaluate(() => window.__doc.privacy.storeTranscripts)).toBe(false);
  await page.locator('[data-device-target][value="current-id"]').check();
  await page.getByRole('button', {
    name: /Apply these settings to 1 selected device/,
  }).click();
  await expect.poll(() => page.evaluate(() => window.__patchAttempts)).toBe(3);
  const finalPatch = await page.evaluate(() =>
    window.__apiRequests.filter((entry) => entry.options?.method === 'PATCH').at(-1)
      .options.json,
  );
  expect(finalPatch.settings).toEqual({
    privacy: { storeTranscripts: true },
  });
  expect(await page.evaluate(() => window.__doc.privacy.storeTranscripts)).toBe(true);
});

test('a scope write disables every section so settings PATCHes cannot overlap', async ({ page }) => {
  await page.setContent(`
    <section data-device-settings-root="privacy">
      <span class="ln-card__title">Privacy</span>
      <div class="set-accordion__panel"></div>
    </section>
    <section data-device-settings-root="appearance">
      <span class="ln-card__title">Appearance</span>
      <div class="set-accordion__panel"></div>
    </section>
  `);
  await page.evaluate(() => {
    window.confirm = () => true;
    window.__patches = 0;
    window.__activePatches = 0;
    window.__maxActivePatches = 0;
    window.__version = 2;
    window.__envelope = (section) => ({
      section,
      version: window.__version,
      currentDeviceId: 'current-id',
      accountDefaults:
        section === 'privacy'
          ? { privacy: { storeTranscripts: true } }
          : { theme: 'light', appearance: { appStyle: 'ninja' } },
      devices: [
        {
          deviceId: 'current-id',
          name: 'Chrome on Windows',
          isCurrent: true,
          inherited: false,
          capabilities: ['privacy', 'appearance'],
          settings:
            section === 'privacy'
              ? { privacy: { storeTranscripts: false } }
              : { theme: 'dark', appearance: { appStyle: 'ninja' } },
        },
      ],
    });
    window.__apiJSON = async (path, options = {}) => {
      const section = path.split('/').at(-1);
      if (options.method !== 'PATCH') return window.__envelope(section);
      window.__patches += 1;
      window.__activePatches += 1;
      window.__maxActivePatches = Math.max(
        window.__maxActivePatches,
        window.__activePatches,
      );
      await new Promise((resolve) => setTimeout(resolve, 75));
      window.__activePatches -= 1;
      window.__version += 1;
      return window.__envelope(section);
    };
  });
  const runnable = deviceSettingsSource
    .replace(
      "import { apiJSON, ApiError } from './toolclient.mjs';",
      `const apiJSON = (...args) => window.__apiJSON(...args);
       class ApiError extends Error {
         constructor(status) { super('API error'); this.status = status; }
       }`,
    )
    .replace(
      `import {
  getDeviceID,
  inferDeviceIdentity,
  rotateDeviceID,
} from './device-identity.mjs';`,
      "const getDeviceID = () => 'current-id'; const inferDeviceIdentity = () => ({}); const rotateDeviceID = () => 'rotated-id';",
    );
  await page.addScriptTag({
    type: 'module',
    content: `${runnable}
      window.__doc = {
        version: 2,
        privacy: { storeTranscripts: false },
        theme: 'dark',
        appearance: { appStyle: 'ninja' },
      };
      initDeviceSettingsControls({
        getDocument: () => window.__doc,
        getVersion: () => window.__version,
        reconcileEffective: async () => true,
      });`,
  });

  const triggers = page.locator('[data-device-settings-button]');
  await triggers.nth(0).click();
  await triggers.nth(1).click();
  await expect(page.locator('[data-device-settings-all]')).toHaveCount(2);
  await expect(page.locator('[data-device-settings-all]').nth(0)).toBeEnabled();
  await expect(page.locator('[data-device-settings-all]').nth(1)).toBeEnabled();

  await page.evaluate(() => {
    const actions = [...document.querySelectorAll('[data-device-settings-all]')];
    actions[0].click();
    actions[1].click();
  });

  await expect.poll(() => page.evaluate(() => window.__patches)).toBe(1);
  await expect.poll(() => page.evaluate(() => window.__activePatches)).toBe(0);
  expect(await page.evaluate(() => window.__maxActivePatches)).toBe(1);
  await expect(page.locator('[data-device-settings-all]').nth(1)).toBeEnabled();
});
