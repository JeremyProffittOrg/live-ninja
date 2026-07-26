import { apiJSON, ApiError } from './toolclient.mjs';
import {
  getDeviceID,
  inferDeviceIdentity,
  rotateDeviceID,
} from './device-identity.mjs';

export const SETTINGS_SECTIONS = Object.freeze({
  aboutYou: ['profile'],
  wakeWord: ['wakeWord', 'wakeEngine', 'sensitivity'],
  persona: ['persona', 'voice', 'voiceAccent', 'personaPrefs'],
  voiceEngine: ['voiceEngine', 'geminiVoice'],
  turnDetection: ['turnDetection', 'micEagerness', 'keepListeningSeconds'],
  appearance: ['theme', 'appearance'],
  microphone: ['micDeviceId'],
  privacy: ['privacy'],
});

let registrationPromise = null;
let currentDevice = null;
let settingsOperationTail = Promise.resolve();

/**
 * Serialize settings-document reads that are coupled to a mutation, writes,
 * and authoritative adoptions across every module on the page. The backend
 * version is document-wide, so separate per-section lanes can still race.
 */
export function withSettingsOperation(operation) {
  const result = settingsOperationTail.then(operation, operation);
  settingsOperationTail = result.then(
    () => undefined,
    () => undefined,
  );
  return result;
}

export async function registerCurrentDevice({
  request = apiJSON,
  getID = getDeviceID,
  rotateID = rotateDeviceID,
  identity = inferDeviceIdentity(),
} = {}) {
  const attempt = () => {
    const deviceId = getID();
    return request('/api/v1/devices/current', {
      method: 'PUT',
      json: { deviceId, ...identity },
    });
  };
  try {
    return await attempt();
  } catch (error) {
    const retryable =
      error instanceof ApiError
      && error.status === 409
      && (error.code === 'device_conflict' || error.code === 'device_revoked');
    if (!retryable) throw error;
    rotateID();
    return attempt();
  }
}

export async function ensureCurrentDeviceRegistered() {
  if (currentDevice) return currentDevice;
  if (registrationPromise) return registrationPromise;
  registrationPromise = registerCurrentDevice()
    .then((body) => {
      currentDevice = body?.device || body;
      return currentDevice;
    })
    .finally(() => {
      registrationPromise = null;
    });
  return registrationPromise;
}

export function sectionForKey(key) {
  return Object.keys(SETTINGS_SECTIONS).find((section) =>
    SETTINGS_SECTIONS[section].includes(key),
  ) || '';
}

export function sectionSettings(documentValue, section) {
  const result = {};
  for (const key of SETTINGS_SECTIONS[section] || []) {
    if (Object.prototype.hasOwnProperty.call(documentValue || {}, key)) {
      result[key] = structuredClone(documentValue[key]);
    }
  }
  return result;
}

export function applySectionSettings(documentValue, section, settings) {
  for (const key of SETTINGS_SECTIONS[section] || []) {
    if (Object.prototype.hasOwnProperty.call(settings || {}, key)) {
      documentValue[key] = structuredClone(settings[key]);
    }
  }
}

function shortValue(value) {
  if (value == null || value === '') return 'System default';
  if (typeof value === 'boolean') return value ? 'On' : 'Off';
  if (typeof value === 'string' || typeof value === 'number') return String(value);
  if (Array.isArray(value)) return `${value.length} selected`;
  return Object.entries(value)
    .slice(0, 3)
    .map(([key, item]) => `${key}: ${shortValue(item)}`)
    .join(', ');
}

export function summarizeSection(section, settings) {
  if (!settings || typeof settings !== 'object') return 'No values';
  switch (section) {
    case 'aboutYou':
      return settings.profile?.displayName
        ? `Name: ${settings.profile.displayName}`
        : 'Profile has no display name';
    case 'wakeWord':
      return `Phrase: ${shortValue(settings.wakeWord)} · Engine: ${shortValue(settings.wakeEngine)}`;
    case 'persona':
      return `Persona: ${shortValue(settings.persona?.presetId)}`;
    case 'voiceEngine':
      return `Engine: ${shortValue(settings.voiceEngine?.default)}${
        settings.geminiVoice ? ` · Voice: ${settings.geminiVoice}` : ''
      }`;
    case 'turnDetection':
      return `Detection: ${shortValue(settings.turnDetection)} · Pickup: ${shortValue(settings.micEagerness)}`;
    case 'appearance':
      return `Theme: ${shortValue(settings.theme)} · App: ${shortValue(settings.appearance?.appStyle)}`;
    case 'microphone':
      return settings.micDeviceId ? 'A local microphone' : 'System default';
    case 'privacy':
      return `Transcripts: ${settings.privacy?.storeTranscripts === false ? 'not stored' : 'stored'} · Retention: ${shortValue(settings.privacy?.retentionDays)} days`;
    default:
      return 'No values';
  }
}

function settingLabel(parts) {
  return parts
    .map((part) =>
      String(part)
        .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
        .replace(/[-_]/g, ' ')
        .replace(/^./, (letter) => letter.toUpperCase()),
    )
    .join(' · ');
}

function detailedValue(parts, value) {
  const field = parts.at(-1);
  if (field === 'micDeviceId') {
    return value ? 'A local microphone is selected (identifier hidden)' : 'System default';
  }
  if (value == null || value === '') return 'Not set';
  if (typeof value === 'boolean') return value ? 'On' : 'Off';
  if (Array.isArray(value)) {
    return value.length === 0 ? 'None' : JSON.stringify(value);
  }
  return String(value);
}

function settingsDetailRows(value, parts = [], rows = []) {
  if (value && typeof value === 'object' && !Array.isArray(value)) {
    const entries = Object.entries(value);
    if (entries.length === 0 && parts.length) rows.push([settingLabel(parts), 'None']);
    for (const [key, child] of entries) {
      settingsDetailRows(child, [...parts, key], rows);
    }
    return rows;
  }
  rows.push([settingLabel(parts), detailedValue(parts, value)]);
  return rows;
}

function makeSettingsDetails(section, settings, deviceName) {
  const details = make('details', 'set-device-row__details');
  const summary = make('summary', '', `View all ${section} values`);
  const values = make('dl', 'set-device-values');
  const rows = settingsDetailRows(settings);
  if (rows.length === 0) {
    values.append(make('dt', '', 'Settings'), make('dd', '', 'No values'));
  } else {
    for (const [label, value] of rows) {
      values.append(make('dt', '', label), make('dd', '', value));
    }
  }
  const source = make(
    'button',
    'ln-btn ln-btn--ghost set-device-source',
    `Copy settings from ${deviceName}`,
  );
  source.type = 'button';
  source.dataset.deviceSource = '';
  source.dataset.deviceId = '';
  details.append(summary, values, source);
  return { details, source, values };
}

export function canCopyMicrophone(settings) {
  return !settings?.micDeviceId;
}

function make(tag, className, text) {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (text != null) element.textContent = text;
  return element;
}

function hostSummary(device) {
  const metadata = device?.metadata || {};
  if (metadata.browser && metadata.platform) return `${metadata.browser} on ${metadata.platform}`;
  if (metadata.model) return String(metadata.model);
  return device?.surface ? String(device.surface) : 'Live Ninja device';
}

function deviceNameCounts(devices) {
  const counts = new Map();
  for (const device of devices || []) {
    const name = String(device?.name || hostSummary(device));
    counts.set(name, (counts.get(name) || 0) + 1);
  }
  return counts;
}

function displayDeviceName(device, counts) {
  const name = String(device?.name || hostSummary(device));
  if ((counts.get(name) || 0) < 2) return name;
  return `${name} · ${String(device?.deviceId || '').slice(0, 8)}`;
}

async function patchSection(section, version, operation, target, settings) {
  return apiJSON(`/api/v1/settings/sections/${encodeURIComponent(section)}`, {
    method: 'PATCH',
    json: { version, operation, target, settings },
  });
}

export function initDeviceSettingsControls({
  getDocument,
  getVersion,
  prepareWrite = async () => true,
  setWriteBarrier = () => {},
  reconcileEffective,
}) {
  for (const root of document.querySelectorAll('[data-device-settings-root]')) {
    if (root.querySelector('[data-device-settings-button]')) continue;
    const section = root.dataset.deviceSettingsRoot;
    const sectionTitle =
      root.querySelector('.ln-card__title')?.textContent?.trim() || 'this section';
    const host = root.querySelector('.set-accordion__panel');
    if (!host) continue;

    const wrapper = make('div', 'set-device-scope');
    const button = make('button', 'ln-btn ln-btn--ghost set-device-scope__trigger');
    const buttonID = `deviceSettings-${section}-button`;
    const panelID = `deviceSettings-${section}-panel`;
    button.type = 'button';
    button.id = buttonID;
    button.dataset.deviceSettingsButton = '';
    button.setAttribute('aria-expanded', 'false');
    button.setAttribute('aria-controls', panelID);
    const name = make('span', '', 'This device');
    name.dataset.deviceSettingsName = '';
    button.append(document.createTextNode('Device settings · '), name);

    const panel = make('div', 'set-device-scope__panel');
    panel.id = panelID;
    panel.hidden = true;
    panel.dataset.deviceSettingsPanel = '';
    panel.setAttribute('role', 'region');
    panel.setAttribute('aria-labelledby', buttonID);
    panel.append(
      make(
        'p',
        'ln-hint',
        `View how ${sectionTitle.toLowerCase()} is configured on each named device. Viewing never changes this browser.`,
      ),
    );
    const status = make('p', 'set-device-scope__status');
    status.dataset.deviceSettingsStatus = '';
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    const content = make('div', 'set-device-scope__content');
    content.dataset.deviceSettingsContent = '';
    const actions = make('div', 'set-device-scope__actions');
    const selected = make(
      'button',
      'ln-btn ln-btn--ghost',
      'Apply these settings to selected devices',
    );
    selected.type = 'button';
    selected.dataset.deviceSettingsSelected = '';
    selected.disabled = true;
    const all = make('button', 'ln-btn ln-btn--ghost', 'Apply these settings to all devices');
    all.type = 'button';
    all.dataset.deviceSettingsAll = '';
    all.disabled = true;
    const inherit = make('button', 'ln-btn ln-btn--ghost', 'Use account default on this device');
    inherit.type = 'button';
    inherit.dataset.deviceSettingsInherit = '';
    inherit.disabled = true;
    actions.append(selected, all, inherit);
    panel.append(status, content, actions);
    wrapper.append(button, panel);
    host.prepend(wrapper);
  }

  const roots = [...document.querySelectorAll('[data-device-settings-root]')];
  const openPanels = new Map();
  const sourceSelections = new Map();
  let scopeWriteLocked = false;

  async function reconcileRequired(version, { afterWrite = false } = {}) {
    const reconciled = await reconcileEffective(version);
    if (reconciled === false) {
      const error = new Error('Effective settings could not be reconciled.');
      error.name = afterWrite ? 'SettingsReconcileError' : 'SettingsRefreshError';
      throw error;
    }
  }

  function setWriteBusy(root, busy) {
    for (const button of root.querySelectorAll(
      '[data-device-settings-selected], [data-device-settings-all], [data-device-settings-inherit]',
    )) {
      button.disabled = busy;
    }
  }

  function acquireScopeWrite() {
    if (scopeWriteLocked) return false;
    scopeWriteLocked = true;
    for (const root of roots) setWriteBusy(root, true);
    return true;
  }

  function releaseScopeWrite(owner) {
    scopeWriteLocked = false;
    for (const root of roots) {
      if (root === owner) continue;
      const envelope = openPanels.get(root.dataset.deviceSettingsRoot);
      if (!envelope) continue;
      if (!renderEnvelope(root, envelope)) void load(root);
    }
  }

  async function prepareScopeWrite() {
    if ((await prepareWrite()) === false) {
      const error = new Error('Current device settings are not saved.');
      error.name = 'SettingsPrepareError';
      throw error;
    }
  }

  function currentFromEnvelope(envelope) {
    return (envelope?.devices || []).find(
      (device) => device.isCurrent || device.deviceId === envelope.currentDeviceId,
    );
  }

  function selectedSource(root) {
    const section = root.dataset.deviceSettingsRoot;
    const envelope = openPanels.get(section);
    const sourceID = sourceSelections.get(section);
    return (envelope?.devices || []).find((device) => device.deviceId === sourceID)
      || currentFromEnvelope(envelope)
      || null;
  }

  function selectedSourceSettings(root) {
    const section = root.dataset.deviceSettingsRoot;
    const source = selectedSource(root);
    if (
      !source
      || source.isCurrent
      || source.deviceId === openPanels.get(section)?.currentDeviceId
    ) {
      return sectionSettings(getDocument(), section);
    }
    return structuredClone(source.settings || {});
  }

  function envelopeVersion(envelope) {
    return Number(envelope?.version) || 0;
  }

  function canRenderEnvelope(root, envelope) {
    const prior = openPanels.get(root.dataset.deviceSettingsRoot);
    return envelopeVersion(envelope) >= Math.max(
      Number(getVersion()) || 0,
      envelopeVersion(prior),
    );
  }

  async function patchWithConflictRetry(root, operation, target, settingsSnapshot) {
    const section = root.dataset.deviceSettingsRoot;
    const settings = structuredClone(settingsSnapshot);
    try {
      return await patchSection(section, getVersion(), operation, target, settings);
    } catch (error) {
      if (!(error instanceof ApiError) || error.status !== 409) throw error;
      const fresh = await apiJSON(
        `/api/v1/settings/sections/${encodeURIComponent(section)}`,
      );
      renderEnvelope(root, fresh);
      setWriteBusy(root, true);
      await reconcileRequired(Number(fresh.version) || getVersion());
      return patchSection(section, getVersion(), operation, target, settings);
    }
  }

  function renderEnvelope(root, envelope) {
    if (!canRenderEnvelope(root, envelope)) return false;
    const section = root.dataset.deviceSettingsRoot;
    openPanels.set(section, envelope);
    const content = root.querySelector('[data-device-settings-content]');
    const status = root.querySelector('[data-device-settings-status]');
    const selectedButton = root.querySelector('[data-device-settings-selected]');
    const allButton = root.querySelector('[data-device-settings-all]');
    const inheritButton = root.querySelector('[data-device-settings-inherit]');
    content.replaceChildren();

    const defaultCard = make('div', 'set-device-scope__default');
    defaultCard.append(
      make('strong', '', 'Account default'),
      make('p', 'ln-hint', summarizeSection(section, envelope.accountDefaults)),
    );
    content.appendChild(defaultCard);

    const list = make('fieldset', 'set-device-list');
    const legend = make('legend', 'ln-sr-only', `Devices for ${section}`);
    list.appendChild(legend);
    const nameCounts = deviceNameCounts(envelope.devices);
    const current = currentFromEnvelope(envelope);
    const isSupported = (device) => {
      const capabilities = Array.isArray(device.capabilities) ? device.capabilities : null;
      const active = !device.status || device.status === 'active';
      return active
        && (!capabilities || capabilities.length === 0 || capabilities.includes(section));
    };
    const priorSourceID = sourceSelections.get(section);
    const sourceDevice = (envelope.devices || []).find(
      (device) => device.deviceId === priorSourceID && isSupported(device),
    ) || (current && isSupported(current) ? current : null)
      || (envelope.devices || []).find(isSupported)
      || null;
    if (sourceDevice) sourceSelections.set(section, sourceDevice.deviceId);
    else sourceSelections.delete(section);
    const sourceBanner = make('p', 'set-device-source-banner');
    sourceBanner.dataset.deviceSourceName = '';
    content.appendChild(sourceBanner);
    const sourceButtons = [];
    let unsupportedCount = 0;
    for (const device of envelope.devices || []) {
      const capabilities = Array.isArray(device.capabilities) ? device.capabilities : null;
      const active = !device.status || device.status === 'active';
      const capable = !capabilities || capabilities.length === 0 || capabilities.includes(section);
      const supported = active && capable;
      if (active && !capable) unsupportedCount += 1;
      const row = make('div', 'set-device-row');
      const label = make('label', 'set-device-row__select');
      const check = document.createElement('input');
      check.type = 'checkbox';
      check.value = device.deviceId;
      check.dataset.deviceTarget = '';
      check.disabled = !supported;
      const body = make('span', 'set-device-row__body');
      const title = make('span', 'set-device-row__title', displayDeviceName(device, nameCounts));
      if (device.isCurrent) title.append(' ', make('span', 'ln-badge ln-badge--teal', 'This device'));
      body.append(
        title,
        make('span', 'set-device-row__meta', hostSummary(device)),
        make(
          'span',
          'set-device-row__state',
          !active
            ? 'Signed out on this device'
            : !capable
            ? 'Not supported on this device'
            : device.inherited
              ? 'Uses account default'
              : 'Customized for this device',
        ),
        make('span', 'set-device-row__summary', summarizeSection(section, device.settings)),
      );
      label.append(check, body);
      const deviceName = displayDeviceName(device, nameCounts);
      const detail = makeSettingsDetails(section, device.settings, deviceName);
      detail.source.dataset.deviceId = device.deviceId;
      detail.source.disabled = !supported;
      detail.source.setAttribute('aria-pressed', 'false');
      sourceButtons.push({ button: detail.source, device });
      row.append(label, detail.details);
      list.appendChild(row);
    }
    content.appendChild(list);

    const currentLabel = current ? displayDeviceName(current, nameCounts) : 'this device';
    root.querySelector('[data-device-settings-name]').textContent = currentLabel;
    inheritButton.textContent = `Use account default on ${currentLabel}`;
    inheritButton.disabled = false;

    const refreshActions = () => {
      const count = root.querySelectorAll('[data-device-target]:checked').length;
      const portableMic =
        section !== 'microphone'
        || canCopyMicrophone(selectedSourceSettings(root));
      selectedButton.disabled = count === 0 || !portableMic;
      allButton.disabled = !portableMic || unsupportedCount > 0;
      selectedButton.textContent = count
        ? `Apply these settings to ${count} selected ${count === 1 ? 'device' : 'devices'}`
        : 'Apply these settings to selected devices';
      if (!portableMic) {
        status.textContent =
          'A named microphone belongs to its device and cannot be copied. Use System default as the source first.';
      } else if (unsupportedCount > 0) {
        status.textContent = `${unsupportedCount} ${
          unsupportedCount === 1 ? 'device does' : 'devices do'
        } not support this section. Select supported devices individually.`;
      } else {
        status.textContent = '';
      }
    };
    const updateSourceControls = () => {
      const source = selectedSource(root);
      const sourceName = source
        ? displayDeviceName(source, nameCounts)
        : currentLabel;
      sourceBanner.textContent =
        `Copy source: ${sourceName}. Viewing or choosing a source does not change this browser.`;
      for (const entry of sourceButtons) {
        const chosen = entry.device.deviceId === source?.deviceId;
        entry.button.setAttribute('aria-pressed', chosen ? 'true' : 'false');
        entry.button.textContent = chosen
          ? `Using ${displayDeviceName(entry.device, nameCounts)} as source`
          : `Copy settings from ${displayDeviceName(entry.device, nameCounts)}`;
      }
      refreshActions();
    };
    for (const entry of sourceButtons) {
      entry.button.addEventListener('click', () => {
        sourceSelections.set(section, entry.device.deviceId);
        updateSourceControls();
      });
    }
    list.addEventListener('change', refreshActions);
    updateSourceControls();
    if (scopeWriteLocked) setWriteBusy(root, true);

    return true;
  }

  async function load(root, retried = false) {
    const section = root.dataset.deviceSettingsRoot;
    const status = root.querySelector('[data-device-settings-status]');
    setWriteBusy(root, true);
    status.textContent = 'Loading device settings…';
    try {
      const envelope = await apiJSON(
        `/api/v1/settings/sections/${encodeURIComponent(section)}`,
      );
      if (!renderEnvelope(root, envelope)) {
        if (!retried) return load(root, true);
        status.textContent = "Couldn't refresh device settings. Try again.";
        return false;
      }
      return true;
    } catch {
      status.textContent = "Couldn't load device settings. Try again.";
      return false;
    }
  }

  for (const root of roots) {
    const section = root.dataset.deviceSettingsRoot;
    const button = root.querySelector('[data-device-settings-button]');
    const panel = root.querySelector('[data-device-settings-panel]');
    const status = root.querySelector('[data-device-settings-status]');
    button.addEventListener('click', () => {
      const opening = panel.hidden;
      panel.hidden = !opening;
      button.setAttribute('aria-expanded', opening ? 'true' : 'false');
      if (opening) void load(root);
    });

    const runApply = async (target) => {
      if (!acquireScopeWrite()) return;
      status.textContent = 'Saving device settings…';
      let barrierHeld = false;
      let patchCommitted = false;
      try {
        await prepareScopeWrite();
        const settingsSnapshot = selectedSourceSettings(root);
        setWriteBarrier(true);
        barrierHeld = true;
        const envelope = await withSettingsOperation(async () => {
          const saved = await patchWithConflictRetry(
            root,
            'set',
            target,
            settingsSnapshot,
          );
          patchCommitted = true;
          await reconcileRequired(Number(saved.version) || getVersion(), {
            afterWrite: true,
          });
          return saved;
        });
        setWriteBarrier(false);
        barrierHeld = false;
        releaseScopeWrite(root);
        const refreshed = renderEnvelope(root, envelope) || await load(root);
        root.querySelector('[data-device-settings-status]').textContent = refreshed
          ? 'Device settings saved.'
          : 'Settings saved, but the device list could not refresh. Try again.';
      } catch (error) {
        const requiresReload =
          patchCommitted
          || error?.name === 'SettingsReconcileError'
          || error?.name === 'SettingsRefreshError';
        if (barrierHeld && !requiresReload) {
          setWriteBarrier(false);
          barrierHeld = false;
        }
        if (!requiresReload) releaseScopeWrite(root);
        if (error?.name === 'SettingsReconcileError') {
          status.textContent =
            'Settings saved, but this browser could not refresh. Reload before making another change.';
        } else if (error?.name === 'SettingsRefreshError') {
          status.textContent =
            'Settings changed elsewhere, but this browser could not refresh. Reload before making another change.';
        } else if (patchCommitted) {
          status.textContent =
            'Settings may be saved, but this browser could not refresh. Reload before making another change.';
        } else {
          const envelope = openPanels.get(section);
          if (!envelope || !renderEnvelope(root, envelope)) void load(root);
          root.querySelector('[data-device-settings-status]').textContent =
            error?.name === 'SettingsPrepareError'
              ? 'Save this device’s pending changes before applying them elsewhere.'
              : "Couldn't save device settings. Try again.";
        }
      }
    };

    root.querySelector('[data-device-settings-selected]').addEventListener('click', () => {
      const deviceIds = [...root.querySelectorAll('[data-device-target]:checked')].map(
        (input) => input.value,
      );
      if (deviceIds.length) void runApply({ mode: 'selected', deviceIds });
    });
    root.querySelector('[data-device-settings-all]').addEventListener('click', () => {
      const count = openPanels.get(section)?.devices?.length || 0;
      if (
        window.confirm(
          `Apply this section to all ${count || ''} devices? Existing device customizations for this section will be replaced.`,
        )
      ) {
        void runApply({ mode: 'all', deviceIds: [] });
      }
    });
    root.querySelector('[data-device-settings-inherit]').addEventListener('click', async () => {
      if (!acquireScopeWrite()) return;
      status.textContent = 'Using account default…';
      let barrierHeld = false;
      let patchCommitted = false;
      try {
        await prepareScopeWrite();
        setWriteBarrier(true);
        barrierHeld = true;
        const envelope = await withSettingsOperation(async () => {
          const saved = await patchWithConflictRetry(
            root,
            'inherit',
            { mode: 'current', deviceIds: [] },
            {},
          );
          patchCommitted = true;
          await reconcileRequired(Number(saved.version) || getVersion(), {
            afterWrite: true,
          });
          return saved;
        });
        setWriteBarrier(false);
        barrierHeld = false;
        releaseScopeWrite(root);
        const refreshed = renderEnvelope(root, envelope) || await load(root);
        root.querySelector('[data-device-settings-status]').textContent =
          refreshed
            ? 'This device now uses the account default.'
            : 'Account default applied, but the device list could not refresh. Try again.';
      } catch (error) {
        const requiresReload =
          patchCommitted
          || error?.name === 'SettingsReconcileError'
          || error?.name === 'SettingsRefreshError';
        if (barrierHeld && !requiresReload) {
          setWriteBarrier(false);
          barrierHeld = false;
        }
        if (!requiresReload) releaseScopeWrite(root);
        if (error?.name === 'SettingsReconcileError') {
          status.textContent =
            'Settings saved, but this browser could not refresh. Reload before making another change.';
        } else if (error?.name === 'SettingsRefreshError') {
          status.textContent =
            'Settings changed elsewhere, but this browser could not refresh. Reload before making another change.';
        } else if (patchCommitted) {
          status.textContent =
            'Settings may be saved, but this browser could not refresh. Reload before making another change.';
        } else {
          const envelope = openPanels.get(section);
          if (!envelope || !renderEnvelope(root, envelope)) void load(root);
          root.querySelector('[data-device-settings-status]').textContent =
            error?.name === 'SettingsPrepareError'
              ? 'Save this device’s pending changes before using the account default.'
              : "Couldn't change this device. Try again.";
        }
      }
    });
  }

  async function loadAccountDevices() {
    const list = document.querySelector('[data-account-devices-list]');
    const status = document.querySelector('[data-account-devices-status]');
    if (!list || !status) return;
    status.textContent = 'Loading devices…';
    try {
      const body = await apiJSON('/api/v1/devices');
      const nameCounts = deviceNameCounts(body.devices);
      list.replaceChildren();
      for (const device of body.devices || []) {
        const form = make('form', 'set-account-device');
        const text = make('div', 'set-account-device__copy');
        text.append(
          make('strong', '', displayDeviceName(device, nameCounts)),
          make(
            'span',
            'ln-hint',
            `${hostSummary(device)}${device.isCurrent ? ' · This device' : ''}${
              device.status === 'revoked' ? ' · Signed out' : ''
            }`,
          ),
        );
        const input = document.createElement('input');
        input.className = 'ln-input';
        input.name = 'name';
        input.value = device.name || '';
        input.maxLength = 80;
        input.required = true;
        input.setAttribute('aria-label', `Name for ${device.name || hostSummary(device)}`);
        const save = make('button', 'ln-btn ln-btn--ghost', 'Save name');
        save.type = 'submit';
        save.setAttribute(
          'aria-label',
          `Save name for ${device.name || hostSummary(device)}`,
        );
        form.append(text, input, save);
        form.addEventListener('submit', async (event) => {
          event.preventDefault();
          const name = input.value.trim();
          if (!name) return;
          save.disabled = true;
          status.textContent = `Saving ${name}…`;
          try {
            const result = await apiJSON(
              `/api/v1/devices/${encodeURIComponent(device.deviceId)}`,
              { method: 'PATCH', json: { name } },
            );
            device.name = result?.device?.name || name;
            text.querySelector('strong').textContent = device.name;
            save.setAttribute('aria-label', `Save name for ${device.name}`);
            status.textContent = `Saved device name ${device.name}.`;
          } catch {
            status.textContent = "Couldn't save that device name.";
          } finally {
            save.disabled = false;
          }
        });
        list.appendChild(form);
      }
      status.textContent = `${(body.devices || []).length} ${
        (body.devices || []).length === 1 ? 'device' : 'devices'
      } connected.`;
    } catch {
      status.textContent = "Couldn't load devices.";
    }
  }

  void loadAccountDevices();
}
