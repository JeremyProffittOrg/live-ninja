// settings.mjs — settings-panel controller for the conversation page's
// docked drawer (WS-D settings workstream, docs/web-ui-spec.md §3).
//
// Owner 2026-07-19: the standalone /settings page is gone — every control
// this module drives now lives inline inside conversation.html's
// #settingsDrawer. conversation.mjs imports initSettingsPanel() and calls it
// once; there is no more SSR data island (#settings-data/#catalogs-data) —
// the settings document and the persona catalog are both fetched client-side
// here, exactly like conversation.mjs's own settingsDoc bootstrap. This
// module keeps its own optimistic-concurrency loop (doc/version/baseline/
// pendingKeys below), independent of conversation.mjs. Both writers use the
// current named device's section PATCH API, so unrelated sections and other
// device overrides cannot be overwritten by an autosave.
//
// Network access goes through toolclient.mjs (authFetch/apiJSON): the
// in-memory access JWT, refresh-once-on-401, and the X-LN-CSRF header all
// live there — this module never calls fetch() for /api/v1 or /auth
// routes directly. The only raw fetch is the public static wake-word
// catalog (/static/wakewords/catalog.json).

import { apiJSON, ApiError } from './toolclient.mjs';
import {
  SETTINGS_SECTIONS,
  ensureCurrentDeviceRegistered,
  initDeviceSettingsControls,
  sectionForKey,
  sectionSettings,
  withSettingsOperation,
} from './device-settings.mjs';

const $ = (id) => document.getElementById(id);

const clone = (v) => (v === undefined ? v : JSON.parse(JSON.stringify(v)));

function stable(v) {
  if (Array.isArray(v)) return v.map(stable);
  if (v && typeof v === 'object') {
    const o = {};
    for (const k of Object.keys(v).sort()) o[k] = stable(v[k]);
    return o;
  }
  return v;
}
const deepEq = (a, b) => JSON.stringify(stable(a)) === JSON.stringify(stable(b));

/** Wires every control inside #settingsDrawer. Called once by
 * conversation.mjs after the drawer's markup exists (the dialog need not be
 * open — elements are in the DOM regardless of <dialog> open state). */
export async function initSettingsPanel() {
// ---- state --------------------------------------------------------------

await ensureCurrentDeviceRegistered();
const doc = await apiJSON('/api/v1/settings?effective=true');
let version = Number(doc.version) || 1;
let baseline = clone(doc); // last server-confirmed document
const pendingKeys = new Set(); // top-level keys edited since last confirm

// Defensive: the server always fills these, but a malformed response must
// not take down every control in the drawer.
if (!doc.persona || typeof doc.persona !== 'object') doc.persona = { presetId: 'default', systemInstructions: null };
if (!doc.privacy || typeof doc.privacy !== 'object') doc.privacy = { storeAudio: false, storeTranscripts: true, retentionDays: 30 };
if (!doc.voiceEngine || typeof doc.voiceEngine !== 'object') doc.voiceEngine = { default: 'openai-realtime', devices: {} };

// ---- save-status bar + toast --------------------------------------------
// Drawer-scoped toast (#drawerToast/...) — deliberately NOT the page's own
// #toast (conversation.mjs owns that one for session/error banners with a
// different, richer DOM shape; reusing it would collide two incompatible
// renderers on one element).

const statusEl = $('saveStatus');
const statusTextEl = $('saveStatusText');
const retryBtn = $('saveRetryBtn');

function setStatus(state) {
  statusEl.classList.toggle('is-saving', state === 'saving');
  statusEl.classList.toggle('is-error', state === 'error');
  retryBtn.hidden = state !== 'error';
  if (state === 'saving') {
    statusTextEl.textContent = 'Saving…';
  } else if (state === 'error') {
    statusTextEl.textContent = "Couldn't save — retry";
  } else {
    const t = new Date().toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
    statusTextEl.textContent = `All changes saved · ${t}`;
  }
}

const toastEl = $('drawerToast');
const toastMsgEl = $('drawerToastMsg');
const toastActionBtn = $('drawerToastActionBtn');
let toastTimer = 0;
let toastAction = null;

function showToast(msg, { label, onClick, error = false } = {}) {
  clearTimeout(toastTimer);
  toastMsgEl.textContent = msg;
  toastEl.classList.toggle('is-error', !!error);
  if (label && onClick) {
    toastAction = onClick;
    toastActionBtn.textContent = label;
    toastActionBtn.hidden = false;
  } else {
    toastAction = null;
    toastActionBtn.hidden = true;
  }
  toastEl.hidden = false;
  requestAnimationFrame(() => toastEl.classList.add('is-visible'));
  toastTimer = setTimeout(hideToast, label ? 10000 : 6000);
}

function hideToast() {
  clearTimeout(toastTimer);
  toastEl.classList.remove('is-visible');
  toastEl.hidden = true;
}

toastActionBtn.addEventListener('click', () => {
  const fn = toastAction;
  hideToast();
  if (fn) fn();
});

// ---- autosave engine (spec §3.6) --------------------------------------

let saveTimer = 0;
let inFlight = false;
let queuedFlush = false;
let scopeWriteBarrier = false;

function markChanged(key, { debounce = 0 } = {}) {
  pendingKeys.add(key);
  setStatus('saving');
  clearTimeout(saveTimer);
  saveTimer = setTimeout(flush, debounce);
}

/** Cross-tab ping on the shared version key. Every path that adopts a new
 * document version calls this — autosave, the 409 reconcile, and the
 * M16 undo (which is written by the SERVER, so this tab's own doc is stale
 * until it re-reads). Storage being blocked (private mode) degrades cross-tab
 * sync, never the write. */
function pingSettingsVersion() {
  try {
    localStorage.setItem('ln.settings.version', String(version));
  } catch {
    /* storage blocked — cross-tab sync degrades gracefully */
  }
  // The settings drawer is embedded in /conversation, and `storage` never
  // fires in the document that made the write. Notify that document too;
  // conversation.mjs re-GETs the canonical settings before applying them.
  window.dispatchEvent(
    new CustomEvent('ln:settings-changed', { detail: { version } }),
  );
}

async function flush() {
  if (scopeWriteBarrier) {
    queuedFlush = true;
    return;
  }
  if (inFlight) {
    queuedFlush = true;
    return;
  }
  if (pendingKeys.size === 0) {
    setStatus('saved');
    return;
  }
  inFlight = true;
  const sent = clone(doc);
  const sentKeys = new Set(pendingKeys);
  try {
    await withSettingsOperation(async () => {
      try {
        const sections = [...new Set([...sentKeys].map(sectionForKey).filter(Boolean))];
        for (const section of sections) {
          const sectionKeys = new Set(
            [...sentKeys].filter((key) => sectionForKey(key) === section),
          );
          const resp = await apiJSON(
            `/api/v1/settings/sections/${encodeURIComponent(section)}`,
            {
              method: 'PATCH',
              json: {
                version,
                operation: 'set',
                target: { mode: 'current', deviceIds: [] },
                settings: sectionSettings(sent, section),
              },
            },
          );
          const responseVersion = Number(resp.version) || version;
          if (responseVersion < version) {
            queuedFlush = true;
            continue;
          }
          version = responseVersion;
          const current = (resp.devices || []).find(
            (device) => device.isCurrent || device.deviceId === resp.currentDeviceId,
          );
          const confirmed = current?.settings || sectionSettings(sent, section);
          for (const key of SETTINGS_SECTIONS[section]) {
            if (Object.prototype.hasOwnProperty.call(confirmed, key)) {
              baseline[key] = clone(confirmed[key]);
            }
          }
          // A field is confirmed unless the user changed it again mid-flight.
          for (const key of sectionKeys) {
            if (deepEq(doc[key], sent[key])) pendingKeys.delete(key);
          }
          // Adopt server normalizations after clearing fields whose sent value
          // was confirmed, while preserving edits made during the request.
          for (const key of SETTINGS_SECTIONS[section]) {
            if (
              Object.prototype.hasOwnProperty.call(confirmed, key)
              && !pendingKeys.has(key)
              && !deepEq(doc[key], confirmed[key])
            ) {
              doc[key] = clone(confirmed[key]);
              renderField(key);
            }
          }
        }
        doc.version = version;
        baseline.version = version;
        // Cross-tab ping: an open /conversation tab listens for 'storage' on
        // this key and re-GETs the effective doc so Mic pickup / turn detection changes
        // apply to its LIVE session (conversation.mjs, SETTINGS_PING_KEY).
        pingSettingsVersion();
        if (pendingKeys.size > 0) queuedFlush = true;
        else setStatus('saved');
      } catch (err) {
        if (!(err instanceof ApiError) || err.status !== 409) throw err;
        await reconcile409();
      }
    });
  } catch {
    failSave(sentKeys, sent);
  } finally {
    inFlight = false;
    if (queuedFlush) {
      queuedFlush = false;
      clearTimeout(saveTimer);
      saveTimer = setTimeout(flush, 50);
    }
  }
}

// 409: another surface wrote first. Re-read, let the remote win any field
// we both touched, re-apply unrelated local edits, retry once (§3.6).
async function reconcile409() {
  const fresh = await apiJSON('/api/v1/settings?effective=true');
  let remoteWon = false;
  for (const k of [...pendingKeys]) {
    if (!deepEq(fresh[k], baseline[k])) {
      // Same field changed remotely too — remote wins (documented rule).
      pendingKeys.delete(k);
      doc[k] = clone(fresh[k]);
      renderField(k);
      remoteWon = true;
    }
  }
  for (const k of Object.keys(fresh)) {
    if (k === 'version' || pendingKeys.has(k)) continue;
    if (!deepEq(doc[k], fresh[k])) {
      doc[k] = clone(fresh[k]);
      renderField(k);
    }
  }
  version = Number(fresh.version);
  doc.version = version;
  baseline = clone(fresh);
  // The adopted version may have come from another DEVICE (no shared
  // localStorage) — ping local tabs so they re-sync too (see flush()).
  pingSettingsVersion();
  if (remoteWon) {
    showToast('Someone updated your settings from another device — refreshed.');
  }
  if (pendingKeys.size > 0) queuedFlush = true; // automatic single retry
  else setStatus('saved');
}

// Network/5xx failure: revert the optimistic values to last-confirmed and
// offer a retry that re-applies exactly what failed (§3.5).
function failSave(sentKeys, sent) {
  const failedValues = {};
  for (const k of sentKeys) failedValues[k] = clone(sent[k]);
  for (const k of sentKeys) {
    if (deepEq(doc[k], sent[k])) {
      doc[k] = clone(baseline[k]);
      pendingKeys.delete(k);
      renderField(k);
    }
  }
  setStatus('error');
  const retry = () => {
    for (const k of Object.keys(failedValues)) {
      doc[k] = clone(failedValues[k]);
      pendingKeys.add(k);
      renderField(k);
    }
    setStatus('saving');
    clearTimeout(saveTimer);
    saveTimer = setTimeout(flush, 0);
  };
  retryBtn.onclick = retry;
  showToast("Couldn't save your changes — check your connection and try again.", {
    label: 'Retry',
    onClick: retry,
    error: true,
  });
}

// Best-effort flush of anything still pending when the tab goes away.
window.addEventListener('pagehide', () => {
  if (pendingKeys.size === 0 || inFlight || scopeWriteBarrier) return;
  const sections = [...new Set([...pendingKeys].map(sectionForKey).filter(Boolean))];
  void (async () => {
    let pageVersion = version;
    for (const section of sections) {
      try {
        const resp = await apiJSON(
          `/api/v1/settings/sections/${encodeURIComponent(section)}`,
          {
            method: 'PATCH',
            json: {
              version: pageVersion,
              operation: 'set',
              target: { mode: 'current', deviceIds: [] },
              settings: sectionSettings(doc, section),
            },
            keepalive: true,
          },
        );
        pageVersion = Number(resp.version) || pageVersion;
      } catch {
        break;
      }
    }
  })();
});

// ---- per-field re-render (used by reconcile/revert paths) -------------

function renderField(key) {
  switch (key) {
    case 'wakeWord':
      syncWakeWordDisplay();
      break;
    case 'wakeEngine': {
      const r = document.querySelector(`input[name="wakeEngine"][value="${CSS.escape(doc.wakeEngine)}"]`);
      if (r) r.checked = true;
      break;
    }
    case 'sensitivity':
      syncSensitivity(Math.round((Number(doc.sensitivity) || 0) * 100));
      break;
    case 'persona': {
      const preset = doc.persona?.presetId || 'default';
      const sel = $('personaPreset');
      sel.value = [...sel.options].some((o) => o.value === preset) ? preset : 'default';
      const custom = sel.value === 'custom';
      $('customInstructionsField').hidden = !custom;
      if (custom) {
        $('systemInstructions').value = doc.persona?.systemInstructions || '';
      }
      syncInstructionsCount();
      break;
    }
    case 'voice':
    case 'voiceAccent':
    case 'personaPrefs':
      // Personas are the unit of voice identity: voice/accent are edited
      // per persona in the conversation page's persona editor
      // (personaeditor.mjs) and stored under personaPrefs; the top-level
      // voice/voiceAccent remain in the doc purely as the fallback default.
      // No controls in the drawer — the values ride the write-back untouched.
      break;
    case 'profile':
      renderProfile();
      break;
    case 'turnDetection': {
      const r = document.querySelector(`input[name="turnDetection"][value="${CSS.escape(doc.turnDetection)}"]`);
      if (r) r.checked = true;
      break;
    }
    case 'keepListeningSeconds': {
      const v = Number.isFinite(Number(doc.keepListeningSeconds)) ? Number(doc.keepListeningSeconds) : 0;
      const r = document.querySelector(`input[name="keepListening"][value="${v}"]`);
      if (r) r.checked = true;
      break;
    }
    case 'micEagerness': {
      const r = document.querySelector(`input[name="micEagerness"][value="${CSS.escape(doc.micEagerness || 'auto')}"]`);
      if (r) r.checked = true;
      break;
    }
    case 'appearance': {
      const ap = appearanceDoc();
      const live = document.querySelector(`input[name="liveStyle"][value="${CSS.escape(ap.liveStyle || 'hal9000')}"]`);
      if (live) live.checked = true;
      const app = document.querySelector(`input[name="appStyle"][value="${CSS.escape(ap.appStyle || 'ninja')}"]`);
      if (app) app.checked = true;
      const custom = document.getElementById('accentCustom');
      if (custom && /^#[0-9a-fA-F]{6}$/.test(ap.accentColor || '')) custom.value = ap.accentColor;
      if (window.__lnApplyAppearance) window.__lnApplyAppearance(ap);
      syncAccentSwatches();
      break;
    }
    case 'voiceEngine': {
      // Reflect voiceEngine.default; an unknown value leaves all radios
      // unchecked (the stored value is still preserved on write-back).
      const val = (doc.voiceEngine && doc.voiceEngine.default) || 'openai-realtime';
      const r = document.querySelector(`input[name="voiceEngine"][value="${CSS.escape(val)}"]`);
      for (const el of document.querySelectorAll('input[name="voiceEngine"]')) el.checked = false;
      if (r) r.checked = true;
      syncGeminiVoiceVisibility(); // M13: the Gemini voice picker is engine-scoped
      break;
    }
    case 'geminiVoice':
      // M13: the Gemini-engine voice override (voice-engine-section block).
      syncGeminiVoiceValue();
      break;
    case 'theme': {
      const r = document.querySelector(`input[name="theme"][value="${CSS.escape(doc.theme)}"]`);
      if (r) r.checked = true;
      applyTheme(doc.theme);
      break;
    }
    case 'micDeviceId': {
      const sel = $('micDevice');
      const want = doc.micDeviceId || '';
      sel.value = [...sel.options].some((o) => o.value === want) ? want : '';
      break;
    }
    case 'privacy': {
      $('storeAudio').checked = !!doc.privacy?.storeAudio;
      $('storeAudioNote').hidden = !doc.privacy?.storeAudio;
      $('storeTranscripts').checked = doc.privacy?.storeTranscripts !== false;
      const days = Number(doc.privacy?.retentionDays ?? 30);
      const r = document.querySelector(`input[name="retentionDays"][value="${days}"]`);
      if (r) r.checked = true;
      break;
    }
    default:
      break; // unknown/forward-compat fields have no UI — preserved as data
  }
}

// ---- wake-word combobox (searchable, selection-only) ------------------

const wakeInput = $('wakeWordInput');
const wakeList = $('wakeWordListbox');
let wakeCatalog = []; // [{id, phrase, default}]
let wakeCatalogFailed = false;
let comboOpen = false;
let comboActive = -1;
let comboFiltered = [];

function wakePhraseFor(id) {
  const hit = wakeCatalog.find((w) => w.id === id);
  return hit ? hit.phrase : id;
}

function syncWakeWordDisplay() {
  wakeInput.value = wakePhraseFor(doc.wakeWord);
  wakeInput.dataset.selectedId = doc.wakeWord;
}

async function loadWakeCatalog() {
  try {
    const resp = await fetch('/static/wakewords/catalog.json', { credentials: 'same-origin' });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = await resp.json();
    wakeCatalog = Array.isArray(data.wakewords) ? data.wakewords : [];
    if (wakeCatalog.length === 0) throw new Error('empty catalog');
    syncWakeWordDisplay();
  } catch {
    wakeCatalogFailed = true;
    wakeInput.readOnly = true;
    $('wakeWordHint').textContent =
      "Couldn't load the wake-phrase list — reload the page to try again. Your current phrase is unchanged.";
  }
}

function comboRender(filterText) {
  const q = (filterText || '').trim().toLowerCase();
  comboFiltered = q === '' || q === wakePhraseFor(doc.wakeWord).toLowerCase()
    ? [...wakeCatalog]
    : wakeCatalog.filter((w) => w.phrase.toLowerCase().includes(q) || w.id.includes(q));

  wakeList.textContent = '';
  const unknownSelected = !wakeCatalog.some((w) => w.id === doc.wakeWord);
  if (unknownSelected && doc.wakeWord) {
    const li = document.createElement('li');
    li.className = 'ln-combobox-option';
    li.setAttribute('role', 'option');
    li.setAttribute('aria-disabled', 'true');
    li.setAttribute('aria-selected', 'true');
    li.id = 'wakeopt-current-unknown';
    li.textContent = `Current: ${doc.wakeWord} (kept as-is)`;
    wakeList.appendChild(li);
  }
  if (comboFiltered.length === 0) {
    const li = document.createElement('li');
    li.className = 'ln-combobox-empty';
    li.textContent = 'No matching phrases';
    wakeList.appendChild(li);
  }
  comboFiltered.forEach((w, i) => {
    const li = document.createElement('li');
    li.className = 'ln-combobox-option';
    li.setAttribute('role', 'option');
    li.id = `wakeopt-${w.id}`;
    li.setAttribute('aria-selected', w.id === doc.wakeWord ? 'true' : 'false');
    li.textContent = w.phrase + (w.default ? ' (default)' : '');
    // pointerdown fires before the input's blur, so selection wins.
    li.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      comboSelect(w.id);
    });
    li.dataset.index = String(i);
    wakeList.appendChild(li);
  });
  comboSetActive(comboFiltered.length > 0 ? 0 : -1);
}

function comboSetActive(i) {
  comboActive = i;
  [...wakeList.querySelectorAll('.ln-combobox-option')].forEach((el) => el.classList.remove('is-active'));
  if (i >= 0 && i < comboFiltered.length) {
    const el = $(`wakeopt-${comboFiltered[i].id}`);
    if (el) {
      el.classList.add('is-active');
      wakeInput.setAttribute('aria-activedescendant', el.id);
      el.scrollIntoView({ block: 'nearest' });
    }
  } else {
    wakeInput.removeAttribute('aria-activedescendant');
  }
}

function comboOpenPanel(filterText) {
  if (wakeCatalogFailed || wakeCatalog.length === 0) return;
  comboRender(filterText);
  wakeList.hidden = false;
  comboOpen = true;
  wakeInput.setAttribute('aria-expanded', 'true');
}

function comboClose({ revert = true } = {}) {
  wakeList.hidden = true;
  comboOpen = false;
  wakeInput.setAttribute('aria-expanded', 'false');
  wakeInput.removeAttribute('aria-activedescendant');
  if (revert) syncWakeWordDisplay();
}

function comboSelect(id) {
  doc.wakeWord = id;
  comboClose({ revert: false });
  syncWakeWordDisplay();
  markChanged('wakeWord');
}

wakeInput.addEventListener('focus', () => comboOpenPanel(''));
wakeInput.addEventListener('click', () => {
  if (!comboOpen) comboOpenPanel('');
});
wakeInput.addEventListener('input', () => comboOpenPanel(wakeInput.value));
wakeInput.addEventListener('keydown', (e) => {
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    if (!comboOpen) comboOpenPanel('');
    else comboSetActive(Math.min(comboActive + 1, comboFiltered.length - 1));
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    if (comboOpen) comboSetActive(Math.max(comboActive - 1, 0));
  } else if (e.key === 'Home' && comboOpen) {
    e.preventDefault();
    comboSetActive(0);
  } else if (e.key === 'End' && comboOpen) {
    e.preventDefault();
    comboSetActive(comboFiltered.length - 1);
  } else if (e.key === 'Enter') {
    if (comboOpen && comboActive >= 0 && comboActive < comboFiltered.length) {
      e.preventDefault();
      comboSelect(comboFiltered[comboActive].id);
    }
  } else if (e.key === 'Escape') {
    if (comboOpen) {
      e.preventDefault();
      comboClose();
    }
  } else if (e.key === 'Tab') {
    if (comboOpen) comboClose();
  }
});
wakeInput.addEventListener('blur', () => {
  // pointerdown selection already ran; anything else is an abandon.
  if (comboOpen) comboClose();
});

// ==================== wake-word-section:BEGIN ====================
// M6 custom wake-word studio (FR-K02/K03), owned by the M6 web-UI
// workstream — edit only inside these markers.
//
// Backend contract (contracts/api.md "Wake-word", M6 locked decisions;
// concrete shapes from internal/wakeword/catalog.go + wakeword_routes.go):
//   - GET  /api/v1/wakewords       — live authed catalog
//     {engines:[{id, trainable, reason?}], entries:[{id, phrase, engine,
//     source:"builtin"|"custom", status, platforms,...}],
//     esp32CustomSupported}. Customs are the source=="custom" entries;
//     the studio form only shows when the openwakeword engine reports
//     trainable (honest capability flag — locked decision). While the
//     training backend isn't deployed this route 404/503s and the studio
//     stays hidden (progressive disclosure).
//   - POST /api/v1/wakewords       — {phrase, engine} → 202 flat item
//     {id, phrase, engine, status, createdAt}. Validation 400, phrase
//     collision 409 {error:"phrase_conflict"}, ≤3/day + queue-full 429.
//     openwakeword is the only training engine (Porcupine needs a
//     Picovoice account — deferred, locked decision).
//   - GET  /api/v1/wakewords/{id}  — poll one entry's status
//     (pending|training|ready|failed); CatalogEntry shape (failed
//     entries carry failureReason).
//   - DELETE /api/v1/wakewords/{id} — 204; purges the model + item.
//     409 training_in_progress while actively training (jobs hard-cap
//     at 20 min, so the wait is bounded).
//   - POST /api/v1/wakewords/{id}/retry — failed items only → 202 flat
//     item (same shape as create). A retry consumes a ≤3/day training
//     slot exactly like a fresh train (429 daily_limit when spent);
//     409 not_retryable from any non-failed status.
// Ready models are merged into the combobox catalog above (hot-swap: the
// wakeword.mjs engine picks the model up through the standard
// wakeword-manifest.md flow once the id is selected + settings sync).

const wwStudio = $('wwStudio');
const wwPhraseInput = $('wwPhraseInput');
const wwTrainBtn = $('wwTrainBtn');
const wwPhraseError = $('wwPhraseError');
const wwChipsWrap = $('wwChipsWrap');
const wwChips = $('wwChips');

let userWakewords = []; // [{id, phrase, status, engine}]
const wwPollTimers = new Map(); // id → timeout handle
const WW_POLL_MS = 12000;
const WW_POLL_DEADLINE_MS = 30 * 60 * 1000; // jobs hard-cap at 20 min

function wwNormalize(w) {
  if (!w || typeof w !== 'object' || !w.id) return null;
  return {
    id: String(w.id),
    phrase: String(w.phrase || w.id),
    status: String(w.status || 'pending'),
    engine: String(w.engine || 'openwakeword'),
    failureReason: String(w.failureReason || ''),
  };
}

// Merge a ready custom phrase into the combobox catalog (selection-only
// invariant intact: users still pick ids, never free-type the value).
function wwMergeIntoCatalog(w) {
  if (w.status !== 'ready') return;
  if (!wakeCatalog.some((c) => c.id === w.id)) {
    wakeCatalog.push({ id: w.id, phrase: w.phrase, default: false, custom: true });
  }
  syncWakeWordDisplay(); // stored id may be this custom phrase
}

// loadWakeCatalog (combobox module above, outside these markers) REPLACES
// the wakeCatalog array when the static /static/wakewords/catalog.json
// fetch resolves — if wwInit's customs merged first they'd be lost. Both
// loads run concurrently at init, so after the customs arrive we re-merge
// once the static catalog has settled (either loaded or failed), polling
// briefly instead of touching the combobox module's code.
function wwRemergeAll() {
  for (const w of userWakewords) wwMergeIntoCatalog(w);
}

function wwSyncWithCatalog(attempt = 0) {
  if (wakeCatalog.length > 0 || wakeCatalogFailed || attempt >= 40) {
    wwRemergeAll();
    return;
  }
  setTimeout(() => wwSyncWithCatalog(attempt + 1), 250);
}

function wwStatusBadge(status) {
  const b = document.createElement('span');
  b.className = 'ln-badge ln-badge--dot-none';
  if (status === 'ready') {
    b.classList.add('ln-badge--teal');
    b.textContent = 'Ready';
  } else if (status === 'failed') {
    b.classList.add('ln-badge--error');
    b.textContent = 'Failed';
  } else {
    b.classList.add('ww-badge--warn');
    b.textContent = status === 'training' ? 'Training…' : 'Pending';
  }
  return b;
}

function wwRenderChips() {
  wwChips.textContent = '';
  wwChipsWrap.hidden = userWakewords.length === 0;
  for (const w of userWakewords) {
    const chip = document.createElement('span');
    chip.className = 'ww-chip';
    chip.setAttribute('role', 'listitem');

    const phrase = document.createElement('span');
    phrase.textContent = w.phrase;
    chip.appendChild(phrase);
    chip.appendChild(wwStatusBadge(w.status));

    if (w.status === 'failed' && w.failureReason) {
      const reason = document.createElement('span');
      reason.className = 'ln-hint';
      reason.style.marginTop = '0';
      reason.textContent = w.failureReason;
      reason.title = w.failureReason;
      chip.appendChild(reason);
    }

    if (w.status === 'ready' && doc.wakeWord !== w.id) {
      const use = document.createElement('button');
      use.type = 'button';
      use.className = 'ln-btn ln-btn--ghost';
      use.textContent = 'Use';
      use.setAttribute('aria-label', `Use ${w.phrase} as your wake phrase`);
      use.addEventListener('click', () => {
        comboSelect(w.id);
        wwRenderChips(); // drop this chip's Use button
      });
      chip.appendChild(use);
    }

    if (w.status === 'failed') {
      const retry = document.createElement('button');
      retry.type = 'button';
      retry.className = 'ln-btn ln-btn--ghost';
      retry.textContent = 'Retry';
      retry.setAttribute('aria-label', `Retry training ${w.phrase}`);
      retry.addEventListener('click', () => wwRetry(w, retry));
      chip.appendChild(retry);
    }

    if (w.status === 'failed' || w.status === 'ready') {
      const del = document.createElement('button');
      del.type = 'button';
      del.className = 'ln-btn ln-btn--ghost';
      del.textContent = 'Delete';
      del.setAttribute('aria-label', `Delete wake phrase ${w.phrase}`);
      del.addEventListener('click', () => wwDelete(w, del));
      chip.appendChild(del);
    }
    wwChips.appendChild(chip);
  }
}

// Retry a failed training run. The server re-submits through the normal
// create path, so a retry spends one of the 3 daily training slots —
// the toast says so explicitly.
async function wwRetry(w, btn) {
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    const resp = await apiJSON(`/api/v1/wakewords/${encodeURIComponent(w.id)}/retry`, { method: 'POST' });
    const nw = wwNormalize(resp.wakeword || resp)
      || { ...w, status: 'pending', failureReason: '' };
    wwUpsert(nw); // re-renders chips (this button is replaced by the badge)
    wwStartPoll(nw.id);
    showToast(`Retraining “${nw.phrase}” — a retry uses one of your 3 daily training runs. We'll email you when it's ready.`);
  } catch (err) {
    const serverMsg = err instanceof ApiError && err.body && typeof err.body.message === 'string'
      ? err.body.message : '';
    if (err instanceof ApiError && err.status === 429) {
      showToast(serverMsg || 'Daily training limit reached — up to 3 runs per day (retries count). Try again tomorrow.', { error: true });
    } else {
      showToast(serverMsg || "Couldn't restart training — check your connection and try again.", { error: true });
    }
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

// Delete a custom wake phrase (failed or ready) behind a native confirm.
// Deleting the phrase a device currently uses is safe: clients fall back
// to the bundled built-in phrase when the custom model disappears, and
// we re-point the selection at the catalog default here.
async function wwDelete(w, btn) {
  const msg = w.status === 'ready'
    ? `Delete “${w.phrase}”? Any device using it falls back to the bundled built-in wake phrase.`
    : `Delete the failed wake phrase “${w.phrase}”? This clears its training record so you can start fresh.`;
  if (!window.confirm(msg)) return;
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    await apiJSON(`/api/v1/wakewords/${encodeURIComponent(w.id)}`, { method: 'DELETE' });
    wwStopPoll(w.id);
    userWakewords = userWakewords.filter((x) => x.id !== w.id);
    const ci = wakeCatalog.findIndex((c) => c.id === w.id && c.custom);
    if (ci >= 0) wakeCatalog.splice(ci, 1);
    if (doc.wakeWord === w.id) {
      const fallback = wakeCatalog.find((c) => c.default) || wakeCatalog[0];
      if (fallback) comboSelect(fallback.id); // marks changed + autosaves
    }
    wwRenderChips();
    syncWakeWordDisplay();
    showToast(`Deleted “${w.phrase}”.`);
  } catch (err) {
    const serverMsg = err instanceof ApiError && err.body && typeof err.body.message === 'string'
      ? err.body.message : '';
    showToast(serverMsg || "Couldn't delete that wake phrase — try again.", { error: true });
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

function wwUpsert(w) {
  const i = userWakewords.findIndex((x) => x.id === w.id);
  if (i >= 0) userWakewords[i] = w;
  else userWakewords.push(w);
  wwMergeIntoCatalog(w);
  wwRenderChips();
}

function wwStopPoll(id) {
  const t = wwPollTimers.get(id);
  if (t) clearTimeout(t);
  wwPollTimers.delete(id);
}

function wwStartPoll(id, startedAt = Date.now()) {
  wwStopPoll(id);
  const tick = async () => {
    wwPollTimers.delete(id);
    if (Date.now() - startedAt > WW_POLL_DEADLINE_MS) return; // SES email covers the rest
    let w;
    try {
      const resp = await apiJSON(`/api/v1/wakewords/${encodeURIComponent(id)}`);
      w = wwNormalize(resp.wakeword || resp);
    } catch {
      // transient — keep polling until the deadline
    }
    if (w) {
      wwUpsert(w);
      if (w.status === 'ready') {
        showToast(`Your wake phrase “${w.phrase}” is ready.`, {
          label: 'Use it now',
          onClick: () => {
            comboSelect(w.id);
            wwRenderChips();
          },
        });
        return;
      }
      if (w.status === 'failed') {
        showToast(`Training “${w.phrase}” failed — retry it or delete it from your phrase list.`, { error: true });
        return;
      }
    }
    wwPollTimers.set(id, setTimeout(tick, WW_POLL_MS));
  };
  wwPollTimers.set(id, setTimeout(tick, WW_POLL_MS));
}

function wwSetError(msg) {
  wwPhraseError.textContent = msg || '';
  wwPhraseError.hidden = !msg;
  wwPhraseInput.setAttribute('aria-invalid', msg ? 'true' : 'false');
}

// Client-side validation mirrors the server's cheap checks (length/word
// count/charset) with specific, actionable copy; phoneme/profanity/
// collision depth stays server-side (FR-K03).
function wwValidate(raw) {
  const phrase = raw.trim().replace(/\s+/g, ' ');
  if (!phrase) return { error: 'Enter a phrase to train — e.g. “Hey Computer”.' };
  if (!/^[A-Za-z][A-Za-z' -]*$/.test(phrase)) {
    return { error: 'Letters, spaces, apostrophes and hyphens only — no digits or symbols.' };
  }
  const words = phrase.split(' ');
  if (words.length < 2 || words.length > 4) {
    return { error: 'Use 2–4 words — short phrases wake reliably, single words false-trigger.' };
  }
  if (phrase.length < 6) return { error: 'That phrase is too short — use at least 6 letters.' };
  const lower = phrase.toLowerCase();
  // Block only phrases that already have a WORKING model (client-bundled
  // builtin like "Hey Jarvis", or an existing custom). Catalog entries with
  // modelAvailable.web === false (e.g. "Hey Live Ninja" pre-training) are
  // exactly the ones training exists to fill — the server resolves the
  // catalog id to the trained model by phrase slug.
  const hit = wakeCatalog.find((c) => c.phrase.toLowerCase() === lower);
  if (hit && (hit.custom || !hit.modelAvailable || hit.modelAvailable.web !== false)) {
    return { error: 'That phrase already exists — pick it from the list above instead.' };
  }
  return { phrase };
}

async function wwSubmit() {
  const { phrase, error } = wwValidate(wwPhraseInput.value);
  if (error) {
    wwSetError(error);
    wwPhraseInput.focus();
    return;
  }
  wwSetError('');
  wwTrainBtn.disabled = true;
  wwTrainBtn.setAttribute('aria-busy', 'true');
  try {
    const resp = await apiJSON('/api/v1/wakewords', {
      method: 'POST',
      json: { phrase, engine: 'openwakeword' },
    });
    const w = wwNormalize(resp.wakeword || resp) || { id: '', phrase, status: 'pending', engine: 'openwakeword' };
    if (w.id) {
      wwUpsert(w);
      if (w.status === 'pending' || w.status === 'training') wwStartPoll(w.id);
    }
    wwPhraseInput.value = '';
    showToast(`Training “${phrase}” — usually takes a few minutes. We’ll email you when it’s ready.`);
  } catch (err) {
    // Server copy is specific (validation detail, phrase_conflict 409,
    // daily_limit vs queue_full 429 — wakeword_routes.go) — prefer it.
    const serverMsg = err instanceof ApiError && err.body && typeof err.body.message === 'string'
      ? err.body.message : '';
    if (serverMsg && err.status >= 400 && err.status < 500) {
      wwSetError(serverMsg);
    } else if (err instanceof ApiError && err.status === 429) {
      wwSetError('Daily limit reached — up to 3 custom phrases per day. Try again tomorrow.');
    } else if (err instanceof ApiError && err.status === 409) {
      wwSetError('That phrase already exists — pick it from the list above instead.');
    } else {
      wwSetError("Couldn't start training — check your connection and try again.");
    }
  } finally {
    wwTrainBtn.disabled = false;
    wwTrainBtn.removeAttribute('aria-busy');
  }
}

wwTrainBtn.addEventListener('click', wwSubmit);
wwPhraseInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    e.preventDefault();
    wwSubmit();
  }
});
wwPhraseInput.addEventListener('input', () => wwSetError(''));

// Feature probe + initial load. While the M6 training backend isn't
// deployed, GET /api/v1/wakewords 404/503s: the studio stays hidden and
// the combobox hint reverts to the built-ins-only copy — no fake
// capability. On success the response is the live catalog
// {engines, entries, esp32CustomSupported} (internal/wakeword/catalog.go):
// source=="custom" entries become the user's status chips; builtins stay
// sourced from the static combobox catalog per contracts/api.md.
async function wwInit() {
  let resp;
  try {
    resp = await apiJSON('/api/v1/wakewords');
  } catch {
    $('wakeWordHint').textContent =
      'Pick from the built-in phrases. Training your own phrase arrives with the wake-word studio.';
    return;
  }

  // Honest capability gate: only reveal the training form when the
  // server says openwakeword can actually train (EngineInfo.trainable).
  const engines = Array.isArray(resp.engines) ? resp.engines : [];
  const oww = engines.find((e) => e && e.id === 'openwakeword');
  const trainable = oww ? !!oww.trainable : true; // absent list = legacy OK
  wwStudio.hidden = !trainable;
  if (!trainable) {
    $('wakeWordHint').textContent =
      'Pick from the built-in phrases. Custom phrase training is unavailable right now.';
  }

  const list = Array.isArray(resp.entries) ? resp.entries
    : Array.isArray(resp.wakewords) ? resp.wakewords : [];
  for (const raw of list) {
    if (raw && raw.source && raw.source !== 'custom') continue; // builtins: static catalog owns them
    const w = wwNormalize(raw);
    if (!w) continue;
    wwUpsert(w);
    if (w.status === 'pending' || w.status === 'training') wwStartPoll(w.id);
  }
  if (userWakewords.length > 0) wwSyncWithCatalog();
}

wwInit();
// ==================== wake-word-section:END ====================

// ---- wake engine / sensitivity ----------------------------------------

for (const r of document.querySelectorAll('input[name="wakeEngine"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.wakeEngine = r.value;
    markChanged('wakeEngine');
  });
}

const sensSlider = $('sensitivity');
const sensValue = $('sensitivityValue');

function syncSensitivity(pct) {
  sensSlider.value = String(pct);
  sensSlider.style.setProperty('--val', `${pct}%`);
  sensSlider.setAttribute('aria-valuetext', `${pct}%`);
  sensValue.textContent = `${pct}%`;
}

sensSlider.addEventListener('input', () => syncSensitivity(Number(sensSlider.value)));
sensSlider.addEventListener('change', () => {
  doc.sensitivity = Number(sensSlider.value) / 100;
  markChanged('sensitivity', { debounce: 400 });
});

// ---- persona -------------------------------------------------------------
// The persona <select>'s options used to be SSR'd (server-rendered
// {{range .Personas}}); now that this panel lives in the drawer with no SSR
// data island, loadPersonaCatalog() fetches GET /api/v1/realtime/personas
// (id/name/description only — instruction text never leaves the server)
// and builds the options client-side, same catalog handleSettingsPage used.

const personaSel = $('personaPreset');
const instructionsField = $('customInstructionsField');
const instructionsArea = $('systemInstructions');
const instructionsCount = $('instructionsCharCount');

function syncInstructionsCount() {
  instructionsCount.textContent = `${instructionsArea.value.length} / 4000`;
}

async function loadPersonaCatalog() {
  let list = [];
  try {
    const resp = await apiJSON('/api/v1/realtime/personas');
    list = Array.isArray(resp.personas) ? resp.personas : [];
  } catch {
    /* fall through to the single-entry fallback below */
  }
  personaSel.textContent = '';
  if (list.length === 0) {
    const fallback = document.createElement('option');
    fallback.value = 'default';
    fallback.textContent = 'Live Ninja (default)';
    personaSel.appendChild(fallback);
  } else {
    for (const p of list) {
      const opt = document.createElement('option');
      opt.value = p.id;
      opt.textContent = p.description ? `${p.name} — ${p.description}` : p.name;
      personaSel.appendChild(opt);
    }
  }
  const customOpt = document.createElement('option');
  customOpt.value = 'custom';
  customOpt.textContent = 'Custom — write your own instructions';
  personaSel.appendChild(customOpt);
  renderPersonaVisibility(list);
  renderField('persona');
}

// ---- persona visibility (owner 2026-08-01) --------------------------------
// Twenty-eight built-ins is more than most people want to scroll past, so each
// one gets an off-switch. Stored as persona.hidden, an OPT-OUT list: only the
// ids that are switched off are written, so a persona added in a future deploy
// appears on its own instead of needing to be enabled. An allow-list would get
// that backwards and silently hide everything new.
//
// This is presentation only. The server's ResolvePersona never reads it, so a
// persona switched off here still mints a working session if another device or
// a stored document still names it — which is exactly why the default persona
// has no switch at all: it is the fallback that keeps the picker non-empty.
const personaVisibility = $('personaVisibility');
const PERSONA_GROUP_ORDER = ['General', 'PDLC', 'ESP32', 'Fun'];

function hiddenPersonaSet() {
  const raw = doc.persona && doc.persona.hidden;
  return new Set(Array.isArray(raw) ? raw.filter((v) => typeof v === 'string') : []);
}

function writeHiddenPersonas(next) {
  if (!doc.persona || typeof doc.persona !== 'object') doc.persona = { presetId: 'default' };
  // Sorted so two devices that switch the same personas off converge on the
  // same array and the document stops looking changed when it is not.
  doc.persona = { ...doc.persona, hidden: [...next].sort() };
  markChanged('persona');
}

function renderPersonaVisibility(list) {
  if (!personaVisibility) return;
  const hidden = hiddenPersonaSet();
  personaVisibility.replaceChildren();

  const groups = new Map();
  for (const p of list) {
    if (p.id === 'default') continue; // no switch: it is the fallback
    const g = p.group || 'General';
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(p);
  }
  const order = [
    ...PERSONA_GROUP_ORDER.filter((g) => groups.has(g)),
    ...[...groups.keys()].filter((g) => !PERSONA_GROUP_ORDER.includes(g)),
  ];

  for (const groupName of order) {
    const rows = groups.get(groupName);
    const fs = document.createElement('fieldset');
    fs.className = 'persona-visibility__group';

    const legend = document.createElement('legend');
    legend.className = 'persona-visibility__legend';
    legend.textContent = groupName;
    fs.appendChild(legend);

    // Whole-group switch. Turning a group of nine off one checkbox at a time
    // is the reason this control exists at all.
    const all = document.createElement('button');
    all.type = 'button';
    all.className = 'ln-btn ln-btn--ghost ln-btn--compact persona-visibility__all';
    const anyShown = () => rows.some((p) => !hiddenPersonaSet().has(p.id));
    all.textContent = anyShown() ? `Hide all ${groupName}` : `Show all ${groupName}`;
    all.addEventListener('click', () => {
      const next = hiddenPersonaSet();
      if (anyShown()) {
        for (const p of rows) next.add(p.id);
      } else {
        for (const p of rows) next.delete(p.id);
      }
      writeHiddenPersonas(next);
      renderPersonaVisibility(list);
    });
    fs.appendChild(all);

    for (const p of rows) {
      const label = document.createElement('label');
      label.className = 'ln-toggle persona-visibility__row';
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = !hidden.has(p.id);
      cb.addEventListener('change', () => {
        const next = hiddenPersonaSet();
        if (cb.checked) next.delete(p.id);
        else next.add(p.id);
        writeHiddenPersonas(next);
        // Re-render so the group button's label follows the new state.
        renderPersonaVisibility(list);
      });
      const track = document.createElement('span');
      track.className = 'ln-toggle-track';
      track.setAttribute('aria-hidden', 'true');
      const thumb = document.createElement('span');
      thumb.className = 'ln-toggle-thumb';
      track.appendChild(thumb);
      const text = document.createElement('span');
      text.textContent = p.name || p.id;
      label.append(cb, track, text);
      fs.appendChild(label);
    }
    personaVisibility.appendChild(fs);
  }
}

personaSel.addEventListener('change', () => {
  const v = personaSel.value;
  doc.persona = { ...doc.persona, presetId: v };
  if (v === 'custom') {
    instructionsField.hidden = false;
    doc.persona.systemInstructions = instructionsArea.value || null;
    instructionsArea.focus();
  } else {
    // Progressive disclosure: instructions only exist for "custom".
    instructionsField.hidden = true;
    doc.persona.systemInstructions = null;
  }
  markChanged('persona');
});

instructionsArea.addEventListener('input', () => {
  syncInstructionsCount();
  doc.persona = { ...doc.persona, systemInstructions: instructionsArea.value || null };
  markChanged('persona', { debounce: 400 });
});

instructionsArea.addEventListener('paste', (e) => {
  const incoming = e.clipboardData ? e.clipboardData.getData('text') : '';
  const projected =
    instructionsArea.value.length -
    (instructionsArea.selectionEnd - instructionsArea.selectionStart) +
    incoming.length;
  if (projected > 4000) {
    // maxlength already trimmed the paste — just be honest about it.
    showToast('Instructions were shortened to fit the 4000-character limit.');
  }
});

// ---- voice + accent: moved to the persona editor -----------------------
// The standalone Voice/Accent section is gone from this panel: personas are
// the unit of voice identity (settings.schema.json personaPrefs), edited in
// the conversation page's persona editor (personaeditor.mjs). The doc's
// top-level voice/voiceAccent fields survive as the fallback default and
// are preserved untouched until the persona section is explicitly saved.

// ---- turn detection ----------------------------------------------------

for (const r of document.querySelectorAll('input[name="turnDetection"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.turnDetection = r.value;
    markChanged('turnDetection');
  });
}

// ---- mic pickup eagerness ----------------------------------------------

for (const r of document.querySelectorAll('input[name="micEagerness"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.micEagerness = r.value;
    markChanged('micEagerness');
  });
}

// ---- keep listening after replies ---------------------------------------
// 0 = no client timeout: the mic listens until the user or the voice
// provider ends the session (default; owner decision 2026-07-19).

for (const r of document.querySelectorAll('input[name="keepListening"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.keepListeningSeconds = Number(r.value);
    markChanged('keepListeningSeconds');
  });
}

// ---- appearance: two style zones + accent color -------------------------
// appStyle themes everything outside the live panel (<html>); liveStyle
// themes the conversation page's orb/mic rail (#livePanel). The server
// migrates legacy {themeStyle} docs on read, but a stale cached bundle may
// still carry one — migrate it here too so the pickers and write-backs
// always use the two-zone shape.

function appearanceDoc() {
  if (!doc.appearance || typeof doc.appearance !== 'object') {
    doc.appearance = { appStyle: 'ninja', liveStyle: 'hal9000', accentColor: '' };
  }
  const ap = doc.appearance;
  if (typeof ap.themeStyle === 'string' && ap.themeStyle && !ap.liveStyle) {
    ap.liveStyle = ap.themeStyle;
  }
  delete ap.themeStyle;
  if (!ap.liveStyle) ap.liveStyle = 'hal9000';
  if (!ap.appStyle) ap.appStyle = 'ninja';
  return ap;
}

function applyAppearanceLive() {
  if (window.__lnApplyAppearance) window.__lnApplyAppearance(appearanceDoc());
  syncAccentSwatches();
}

function syncAccentSwatches() {
  const current = appearanceDoc().accentColor || '';
  for (const b of document.querySelectorAll('.ln-swatch')) {
    const active = (b.dataset.accent || '') === current;
    b.classList.toggle('is-active', active);
    b.setAttribute('aria-checked', active ? 'true' : 'false');
  }
}

for (const r of document.querySelectorAll('input[name="liveStyle"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    appearanceDoc().liveStyle = r.value;
    applyAppearanceLive();
    markChanged('appearance');
  });
}

for (const r of document.querySelectorAll('input[name="appStyle"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    appearanceDoc().appStyle = r.value;
    applyAppearanceLive();
    markChanged('appearance');
  });
}

for (const b of document.querySelectorAll('.ln-swatch')) {
  b.addEventListener('click', () => {
    appearanceDoc().accentColor = b.dataset.accent || '';
    applyAppearanceLive();
    markChanged('appearance');
  });
}

const accentCustom = document.getElementById('accentCustom');
if (accentCustom) {
  accentCustom.addEventListener('input', () => {
    appearanceDoc().accentColor = accentCustom.value;
    applyAppearanceLive();
  });
  accentCustom.addEventListener('change', () => {
    appearanceDoc().accentColor = accentCustom.value;
    applyAppearanceLive();
    markChanged('appearance');
  });
}

// ==================== voice-engine-section:BEGIN ====================
// M12 secondary-voice-engine picker (FR-VE-04) + M13 Gemini voice picker,
// owned by the voice-engine workstream — edit only inside these markers.
// Bound to voiceEngine.default inside this browser's effective voice-engine
// section.
// The segmented radios render without a checked attribute — the current
// value is hydrated from the fetched doc via renderField('voiceEngine') at
// the bottom of init(). Unknown forward-compat fields (e.g.
// voiceEngine.devices) are preserved untouched by the spread + the autosave
// engine's section PATCH. The radio wiring is value-generic: the M13
// gemini-flash-live radio needs no per-value code, only the engine-scoped
// Gemini voice picker below.
for (const r of document.querySelectorAll('input[name="voiceEngine"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.voiceEngine = { ...doc.voiceEngine, default: r.value };
    syncGeminiVoiceVisibility();
    markChanged('voiceEngine');
  });
}

// M13 Gemini voice (gemini-plan.md B4, D4): a per-engine voice picker fed
// by the additive `geminiVoices` catalog on GET /api/v1/realtime/voices,
// shown only while the engine selection is gemini-flash-live (the OpenAI
// voice picker stays persona-scoped in the persona editor). Writes the
// top-level `geminiVoice` settings key; "" = unset, which lets the broker's
// chain resolve persona geminiVoice ?? Kore instead.

const geminiVoiceField = $('geminiVoiceField');
const geminiVoiceSel = $('geminiVoice');

function syncGeminiVoiceVisibility() {
  const engine = (doc.voiceEngine && doc.voiceEngine.default) || 'openai-realtime';
  geminiVoiceField.hidden = engine !== 'gemini-flash-live';
}

function syncGeminiVoiceValue() {
  const want = typeof doc.geminiVoice === 'string' ? doc.geminiVoice : '';
  if ([...geminiVoiceSel.options].some((o) => o.value === want)) {
    geminiVoiceSel.value = want;
    return;
  }
  // Forward-compat: an unknown stored voice is kept selectable, never
  // silently dropped (same rule as the persona editor's fillCatalogSelect).
  const opt = document.createElement('option');
  opt.value = want;
  opt.textContent = `${want} (kept as-is)`;
  geminiVoiceSel.appendChild(opt);
  geminiVoiceSel.value = want;
}

async function loadGeminiVoices() {
  let rows = [];
  try {
    const resp = await apiJSON('/api/v1/realtime/voices');
    rows = Array.isArray(resp.geminiVoices) ? resp.geminiVoices : [];
  } catch {
    /* catalog fetch failed — keep the SSR "Persona default" option; the
       stored value still round-trips via syncGeminiVoiceValue below */
  }
  const auto = document.createElement('option');
  auto.value = '';
  auto.textContent = 'Persona default (Kore fallback)';
  geminiVoiceSel.replaceChildren(auto);
  for (const v of rows) {
    if (!v || !v.id) continue;
    const opt = document.createElement('option');
    opt.value = v.id;
    // Same label recipe as the persona editor's voice select.
    opt.textContent = `${v.name || v.id}${v.gender ? ` (${v.gender})` : ''}${v.description ? ` — ${v.description}` : ''}`;
    geminiVoiceSel.appendChild(opt);
  }
  syncGeminiVoiceValue();
}

geminiVoiceSel.addEventListener('change', () => {
  doc.geminiVoice = geminiVoiceSel.value;
  markChanged('geminiVoice');
});

loadGeminiVoices();
// ==================== voice-engine-section:END ====================

// ---- theme -------------------------------------------------------------

function applyTheme(v) {
  if (v === 'light' || v === 'dark') {
    document.documentElement.setAttribute('data-theme', v);
  } else {
    document.documentElement.removeAttribute('data-theme');
  }
  try {
    localStorage.setItem('ln-theme', v); // theme.js reads this pre-paint
  } catch {
    /* storage blocked — attribute alone still themes this page */
  }
}

for (const r of document.querySelectorAll('input[name="theme"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.theme = r.value;
    applyTheme(r.value); // instant (spec §3.3), then persist
    markChanged('theme');
  });
}

// ---- microphone device -------------------------------------------------

const micSel = $('micDevice');
const micGrantBtn = $('micGrant');
let warnedMissingMic = false;

async function refreshMicDevices() {
  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) {
    micSel.disabled = true;
    return;
  }
  let devices = [];
  try {
    devices = await navigator.mediaDevices.enumerateDevices();
  } catch {
    return; // keep whatever the select currently shows
  }
  const inputs = devices.filter((d) => d.kind === 'audioinput');
  const haveLabels = inputs.some((d) => d.label);

  micSel.textContent = '';
  const sysOpt = document.createElement('option');
  sysOpt.value = '';
  sysOpt.textContent = 'System default';
  micSel.appendChild(sysOpt);

  if (haveLabels) {
    inputs.forEach((d, i) => {
      if (!d.deviceId || d.deviceId === 'default') return; // "default" dupes the row above
      const o = document.createElement('option');
      o.value = d.deviceId;
      o.textContent = d.label || `Microphone ${i + 1}`;
      micSel.appendChild(o);
    });
    micGrantBtn.hidden = true;
  } else {
    // Labels are blank until mic permission is granted once — offer the
    // grant action instead of a dead dropdown of "Microphone 1/2/3".
    if (doc.micDeviceId) {
      const o = document.createElement('option');
      o.value = doc.micDeviceId;
      o.textContent = 'Saved microphone';
      micSel.appendChild(o);
    }
    // No devices at all → nothing a grant could reveal.
    micGrantBtn.hidden = inputs.length === 0;
  }

  const want = doc.micDeviceId || '';
  if ([...micSel.options].some((o) => o.value === want)) {
    micSel.value = want;
  } else {
    micSel.value = '';
    if (haveLabels && want && !warnedMissingMic) {
      warnedMissingMic = true;
      showToast("Your saved microphone isn't connected — using the system default.");
    }
  }
}

micGrantBtn.addEventListener('click', async () => {
  micGrantBtn.disabled = true;
  try {
    // One-shot grant purely to unlock device labels; released immediately.
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    for (const t of stream.getTracks()) t.stop();
    await refreshMicDevices();
  } catch {
    showToast(
      "Microphone access is blocked. Enable it in your browser's site settings, then try again.",
      { error: true },
    );
  } finally {
    micGrantBtn.disabled = false;
  }
});

micSel.addEventListener('change', () => {
  doc.micDeviceId = micSel.value === '' ? null : micSel.value;
  markChanged('micDeviceId');
});

if (navigator.mediaDevices && navigator.mediaDevices.addEventListener) {
  navigator.mediaDevices.addEventListener('devicechange', () => refreshMicDevices());
}

// ---- privacy -----------------------------------------------------------

$('storeAudio').addEventListener('change', () => {
  doc.privacy = { ...doc.privacy, storeAudio: $('storeAudio').checked };
  $('storeAudioNote').hidden = !$('storeAudio').checked;
  markChanged('privacy');
});

$('storeTranscripts').addEventListener('change', () => {
  doc.privacy = { ...doc.privacy, storeTranscripts: $('storeTranscripts').checked };
  markChanged('privacy');
});

for (const r of document.querySelectorAll('input[name="retentionDays"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.privacy = { ...doc.privacy, retentionDays: Number(r.value) };
    markChanged('privacy');
  });
}

// ---- account -----------------------------------------------------------

$('signOut').addEventListener('click', async () => {
  $('signOut').disabled = true;
  try {
    await apiJSON('/auth/logout', { method: 'POST' });
  } catch {
    /* session may already be gone — landing page is correct either way */
  }
  window.location.assign('/');
});

const signOutAllBtn = $('signOutAll');
const signOutAllPanel = $('signOutAllConfirm');

function setSignOutAllOpen(open) {
  signOutAllPanel.hidden = !open;
  signOutAllBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) $('signOutAllConfirmBtn').focus();
  else signOutAllBtn.focus();
}

signOutAllBtn.addEventListener('click', () => setSignOutAllOpen(signOutAllPanel.hidden));
$('signOutAllCancelBtn').addEventListener('click', () => setSignOutAllOpen(false));
signOutAllPanel.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    e.stopPropagation();
    setSignOutAllOpen(false);
  }
});
$('signOutAllConfirmBtn').addEventListener('click', async () => {
  const btn = $('signOutAllConfirmBtn');
  btn.disabled = true;
  try {
    await apiJSON('/api/v1/auth/logout-all', { method: 'POST' });
    window.location.assign('/');
  } catch {
    btn.disabled = false;
    showToast("Couldn't sign out everywhere — try again.", { error: true });
  }
});

// ---- About you / Base Knowledge (M15) ------------------------------------
// The profile is injected into every session's instructions server-side, so
// what is stored has to be trustworthy: locations are SELECTED from
// GET /api/v1/geocode and saved with their resolved lat/lon + IANA timezone,
// never as free text. That is what lets get_weather skip geocoding entirely.

function profileDoc() {
  if (!doc.profile || typeof doc.profile !== 'object') {
    doc.profile = {
      displayName: '', pronouns: '', homeLocation: null, workLocation: null,
      units: 'imperial', locale: '', contactEmail: '', quietHours: null, notes: [],
    };
  }
  if (!Array.isArray(doc.profile.notes)) doc.profile.notes = [];
  return doc.profile;
}

function setProfileField(key, value) {
  const p = profileDoc();
  if (deepEq(p[key], value)) return;
  p[key] = value;
  markChanged('profile', { debounce: 600 });
}

// Reflect the stored profile into every control (init + after a 409
// remote-wins refresh, same contract as the other renderField cases).
function renderProfile() {
  const p = profileDoc();
  const name = document.getElementById('profileDisplayName');
  if (name && document.activeElement !== name) name.value = p.displayName || '';
  const pronouns = document.getElementById('profilePronouns');
  if (pronouns && document.activeElement !== pronouns) pronouns.value = p.pronouns || '';
  const email = document.getElementById('profileContactEmail');
  if (email && document.activeElement !== email) email.value = p.contactEmail || '';
  const units = document.querySelector(`input[name="profileUnits"][value="${CSS.escape(p.units || 'imperial')}"]`);
  if (units) units.checked = true;
  renderLocation('home');
  renderLocation('work');
  renderNotes();
}

const LOCATION_FIELDS = {
  home: {
    key: 'homeLocation', input: 'profileHomeInput', list: 'profileHomeListbox',
    resolved: 'profileHomeResolved', clear: 'profileHomeClear',
  },
  work: {
    key: 'workLocation', input: 'profileWorkInput', list: 'profileWorkListbox',
    resolved: 'profileWorkResolved', clear: 'profileWorkClear',
  },
};

function renderLocation(which) {
  const f = LOCATION_FIELDS[which];
  const loc = profileDoc()[f.key];
  const input = document.getElementById(f.input);
  const resolved = document.getElementById(f.resolved);
  const clear = document.getElementById(f.clear);
  if (!input || !resolved || !clear) return;

  if (loc && loc.label) {
    if (document.activeElement !== input) input.value = loc.label;
    resolved.textContent = '';
    resolved.append(document.createTextNode(loc.label));
    if (loc.timezone) {
      const tz = document.createElement('span');
      tz.className = 'ln-resolved__tz';
      tz.textContent = ` · ${loc.timezone}`;
      resolved.appendChild(tz);
    }
    resolved.hidden = false;
    clear.hidden = false;
  } else {
    if (document.activeElement !== input) input.value = '';
    resolved.hidden = true;
    resolved.textContent = '';
    clear.hidden = true;
  }
}

// One typeahead per location field. Debounced at 300ms, with a slow response
// discarded when a newer keystroke has already fired — per the house rules for
// large/queried option sets.
function wireLocationPicker(which) {
  const f = LOCATION_FIELDS[which];
  const input = document.getElementById(f.input);
  const list = document.getElementById(f.list);
  const clear = document.getElementById(f.clear);
  if (!input || !list) return;

  let timer = 0;
  let seq = 0;
  let results = [];
  let active = -1;

  const close = () => {
    list.hidden = true;
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
    active = -1;
  };

  const setActive = (i) => {
    active = i;
    [...list.querySelectorAll('.ln-combobox-option')].forEach((el) => el.classList.remove('is-active'));
    const el = list.querySelector(`[data-index="${i}"]`);
    if (el) {
      el.classList.add('is-active');
      input.setAttribute('aria-activedescendant', el.id);
      el.scrollIntoView({ block: 'nearest' });
    }
  };

  const choose = (r) => {
    setProfileField(f.key, {
      label: r.label,
      city: r.city || '',
      admin1: r.admin1 || '',
      country: r.country || '',
      postalCode: r.postalCode || '',
      lat: r.lat,
      lon: r.lon,
      timezone: r.timezone || '',
    });
    close();
    renderLocation(which);
    input.blur();
    // A pick is the only way a location suggestion can be approved (M16) — it
    // is what attaches the lat/lon the geocode-free weather path needs.
    locationSuggestionPicked(which);
  };

  const render = () => {
    list.textContent = '';
    if (results.length === 0) {
      const li = document.createElement('li');
      li.className = 'ln-combobox-empty';
      li.textContent = 'No matching places';
      list.appendChild(li);
    }
    results.forEach((r, i) => {
      const li = document.createElement('li');
      li.className = 'ln-combobox-option';
      li.setAttribute('role', 'option');
      li.setAttribute('aria-selected', 'false');
      li.id = `${f.input}-opt-${i}`;
      li.dataset.index = String(i);
      li.textContent = r.label;
      // pointerdown fires before the input's blur, so the click still selects.
      li.addEventListener('pointerdown', (e) => { e.preventDefault(); choose(r); });
      list.appendChild(li);
    });
    list.hidden = false;
    input.setAttribute('aria-expanded', 'true');
    setActive(results.length > 0 ? 0 : -1);
  };

  const search = async (q) => {
    const mine = ++seq;
    try {
      const resp = await apiJSON(`/api/v1/geocode?q=${encodeURIComponent(q)}`);
      if (mine !== seq) return;
      results = Array.isArray(resp.results) ? resp.results : [];
      render();
    } catch {
      if (mine !== seq) return;
      results = [];
      list.textContent = '';
      const li = document.createElement('li');
      li.className = 'ln-combobox-empty';
      li.textContent = "Couldn't reach the place lookup — try again in a moment.";
      list.appendChild(li);
      list.hidden = false;
    }
  };

  input.addEventListener('input', () => {
    const q = input.value.trim();
    clearTimeout(timer);
    if (q.length < 2) { close(); return; }
    timer = setTimeout(() => search(q), 300);
  });

  input.addEventListener('keydown', (e) => {
    if (list.hidden) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActive(Math.min(active + 1, results.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive(Math.max(active - 1, 0));
    } else if (e.key === 'Enter') {
      if (active >= 0 && results[active]) { e.preventDefault(); choose(results[active]); }
    } else if (e.key === 'Escape') {
      e.preventDefault();
      close();
      renderLocation(which);
    }
  });

  input.addEventListener('blur', () => {
    // Typing without picking must not look like it saved: snap the box back to
    // whatever is actually stored.
    setTimeout(() => { close(); renderLocation(which); }, 120);
  });

  if (clear) {
    clear.addEventListener('click', () => {
      setProfileField(f.key, null);
      renderLocation(which);
      input.focus();
    });
  }
}

function renderNotes() {
  const list = document.getElementById('profileNotesList');
  if (!list) return;
  const notes = profileDoc().notes;
  list.textContent = '';
  notes.forEach((note, i) => {
    const li = document.createElement('li');
    li.className = 'ww-chip ln-notechip';
    const text = document.createElement('span');
    text.className = 'ln-notechip__text';
    text.textContent = note;
    const rm = document.createElement('button');
    rm.type = 'button';
    rm.className = 'ln-btn ln-btn--ghost';
    rm.textContent = 'Remove';
    rm.setAttribute('aria-label', `Remove fact: ${note}`);
    rm.addEventListener('click', () => {
      const next = profileDoc().notes.slice();
      next.splice(i, 1);
      setProfileField('notes', next);
      renderNotes();
    });
    li.append(text, rm);
    list.appendChild(li);
  });
}

function wireProfile() {
  const name = document.getElementById('profileDisplayName');
  if (name) name.addEventListener('input', () => setProfileField('displayName', name.value.trim()));
  const pronouns = document.getElementById('profilePronouns');
  if (pronouns) pronouns.addEventListener('input', () => setProfileField('pronouns', pronouns.value.trim()));
  const email = document.getElementById('profileContactEmail');
  if (email) {
    email.addEventListener('input', () => {
      const v = email.value.trim();
      // Don't push an obviously-incomplete address on every keystroke — the
      // server rejects an address with no @, which would surface as a save
      // error mid-typing.
      if (v === '' || v.includes('@')) setProfileField('contactEmail', v);
    });
  }
  document.querySelectorAll('input[name="profileUnits"]').forEach((r) => {
    r.addEventListener('change', () => { if (r.checked) setProfileField('units', r.value); });
  });

  wireLocationPicker('home');
  wireLocationPicker('work');

  const noteInput = document.getElementById('profileNoteInput');
  const noteAdd = document.getElementById('profileNoteAdd');
  const addNote = () => {
    const v = (noteInput?.value || '').trim();
    if (!v) return;
    const notes = profileDoc().notes;
    if (notes.length >= 20) {
      showToast('That is the 20-fact limit — remove one first.', { error: true });
      return;
    }
    if (notes.includes(v)) { noteInput.value = ''; return; }
    setProfileField('notes', notes.concat(v));
    noteInput.value = '';
    renderNotes();
  };
  if (noteAdd) noteAdd.addEventListener('click', addNote);
  if (noteInput) {
    noteInput.addEventListener('keydown', (e) => {
      // Enter adds the fact rather than submitting anything.
      if (e.key === 'Enter') { e.preventDefault(); addNote(); }
    });
  }

  const suggestBtn = document.getElementById('profileSuggestBtn');
  const suggestStatus = document.getElementById('profileSuggestStatus');
  if (suggestBtn) {
    suggestBtn.addEventListener('click', async () => {
      suggestBtn.disabled = true;
      suggestStatus.textContent = 'Looking through your memory…';
      try {
        const resp = await apiJSON('/api/v1/profile/suggest', { method: 'POST' });
        const s = resp.suggestions || {};
        const filled = [];
        // Plain-text fields are pre-filled; a LOCATION is never auto-applied
        // from prose. It has to be picked so it carries real coordinates — a
        // silently wrong home poisons every weather and time answer after it.
        if (s.displayName && !profileDoc().displayName) {
          document.getElementById('profileDisplayName').value = s.displayName;
          setProfileField('displayName', s.displayName);
          filled.push('name');
        }
        if (s.contactEmail && !profileDoc().contactEmail && s.contactEmail.includes('@')) {
          document.getElementById('profileContactEmail').value = s.contactEmail;
          setProfileField('contactEmail', s.contactEmail);
          filled.push('email');
        }
        if (s.homeLocation && !profileDoc().homeLocation) {
          const home = document.getElementById('profileHomeInput');
          if (home) {
            home.value = s.homeLocation;
            home.focus();
            filled.push('a home candidate (search and pick it to save coordinates)');
          }
        }
        suggestStatus.textContent = filled.length
          ? `Filled in ${filled.join(', ')} from memory — check each one, then it saves automatically.`
          : 'Nothing new found in memory for these fields.';
      } catch (err) {
        suggestStatus.textContent = String(err?.message || '').includes('503')
          ? 'The memory layer is not available right now.'
          : "Couldn't read your memory — try again in a moment.";
      } finally {
        suggestBtn.disabled = false;
      }
    });
  }
}

// ---- Suggestion queue (M16 Knowledge Refinement Loop) --------------------
//
// Two producers write PROFSUGG# rows server-side: the assistant's
// profile_suggest tool, and the tool-failure RCA analyzer (internal/rca). This
// renders whatever is still awaiting a decision and turns each decision into
// the RIGHT kind of write:
//
//   pending + plain field  -> put the value into `doc` and let the normal
//                             versioned autosave PUT it (no second write path,
//                             so an approval passes the same server validation
//                             a manual edit does), then record the decision;
//   pending + location     -> no direct apply at all: hand the user to the
//                             geocode picker, because only a picked result
//                             carries the lat/lon that makes the geocode-free
//                             weather path trustworthy. The row resolves when
//                             the pick lands;
//   auto-applied           -> already written by the server under the locked
//                             policy (units / a spoken always-known fact);
//                             offer Keep or Undo, and the Undo is a server
//                             route because this tab never saw the old value.

const SUGGESTIONS_PATH = '/api/v1/profile/suggestions';
const AUTOAPPLY_TOAST_KEY = 'ln.profile.autoApplyToastShown';

let suggestions = [];
let suggestionsInFlight = false;
// The location suggestion whose picker is currently open, if any: the row is
// resolved by the PICK, not by the button that opened the picker.
let awaitingLocationPick = null;

const suggestionsBox = $('profileSuggestions');
const suggestionsList = $('profileSuggestionsList');
const suggestionsCount = $('profileSuggestionsCount');
const suggestionsStatus = $('profileSuggestionsStatus');
const settingsTabBadge = $('settingsTabBadge');
const settingsTabBtn = $('settingsDrawerBtn');

function announceSuggestion(msg) {
  if (suggestionsStatus) suggestionsStatus.textContent = msg;
}

/** Drawer-tab badge. The count is the accessible signal too — the aria-label
 * carries it, so nothing here depends on seeing the pill. */
function renderSuggestionBadge() {
  const n = suggestions.length;
  if (settingsTabBadge) {
    settingsTabBadge.textContent = n > 9 ? '9+' : String(n);
    settingsTabBadge.hidden = n === 0;
  }
  if (settingsTabBtn) {
    settingsTabBtn.setAttribute(
      'aria-label',
      n === 0 ? 'Open settings' : `Open settings — ${n} suggestion${n === 1 ? '' : 's'} to review`,
    );
  }
}

async function refreshSuggestions() {
  if (suggestionsInFlight) return;
  suggestionsInFlight = true;
  try {
    const resp = await apiJSON(SUGGESTIONS_PATH);
    suggestions = Array.isArray(resp.suggestions) ? resp.suggestions : [];
    renderSuggestions();
    maybeToastAutoApplied();
  } catch {
    // A failed queue read must never break the rest of the drawer: the
    // suggestions block simply stays as it was (empty on first load).
  } finally {
    suggestionsInFlight = false;
  }
}

/** An auto-applied change happened without the owner asking, so it gets a
 * toast with an Undo even if they never open the drawer — the toast element
 * lives at page level for exactly this. Shown once per suggestion id.
 *
 * With the drawer OPEN the toast is skipped deliberately: showModal() puts the
 * drawer in the top layer, above any normal-flow element regardless of
 * z-index, so a page-level toast would be painted behind it. In that state the
 * row itself — sitting first in "About you", carrying the same Undo — is the
 * better affordance, so this scrolls to it and announces instead. */
function maybeToastAutoApplied() {
  const applied = suggestions.filter((s) => s.autoApplied);
  if (applied.length === 0) return;
  let shown = [];
  try {
    shown = JSON.parse(localStorage.getItem(AUTOAPPLY_TOAST_KEY) || '[]');
  } catch {
    shown = [];
  }
  if (!Array.isArray(shown)) shown = [];
  const fresh = applied.filter((s) => !shown.includes(s.id));
  if (fresh.length === 0) return;
  const sg = fresh[0];
  const drawer = $('settingsDrawer');
  if (drawer && drawer.open) {
    announceSuggestion(`${autoAppliedSentence(sg)} Undo it in the suggestions list.`);
    if (suggestionsBox) suggestionsBox.scrollIntoView({ block: 'nearest' });
  } else {
    showToast(autoAppliedSentence(sg), {
      label: 'Undo',
      onClick: () => void decideSuggestion(sg.id, 'undo'),
    });
  }
  try {
    // Keep only ids that are still live so this list can't grow forever.
    const live = suggestions.map((s) => s.id);
    localStorage.setItem(
      AUTOAPPLY_TOAST_KEY,
      JSON.stringify([...shown, ...fresh.map((s) => s.id)].filter((id) => live.includes(id))),
    );
  } catch {
    /* storage blocked — the toast may repeat next load; harmless */
  }
}

function autoAppliedSentence(sg) {
  if (sg.field === 'profile.notes[]') {
    return `Live Ninja added an always-known fact: “${sg.proposedValue}”.`;
  }
  return `Live Ninja set your ${(sg.fieldLabel || sg.field).toLowerCase()} to ${sg.proposedValue}.`;
}

function renderSuggestions() {
  renderSuggestionBadge();
  if (!suggestionsBox || !suggestionsList) return;
  suggestionsBox.hidden = suggestions.length === 0;
  if (suggestionsCount) suggestionsCount.textContent = String(suggestions.length);
  suggestionsList.textContent = '';
  for (const sg of suggestions) suggestionsList.appendChild(suggestionRow(sg));
}

/** One row. Heterogeneous fields of a single proposal, so the values render as
 * a labelled definition list rather than a table row — every value has a
 * visible label, per the house presentation rules. */
function suggestionRow(sg) {
  const li = document.createElement('li');
  li.className = 'ln-suggestion';
  li.dataset.id = sg.id;

  const head = document.createElement('div');
  head.className = 'ln-suggestion__head';
  const fieldEl = document.createElement('span');
  fieldEl.className = 'ln-suggestion__field';
  fieldEl.textContent = sg.fieldLabel || sg.field;
  head.appendChild(fieldEl);
  if (sg.autoApplied) {
    const badge = document.createElement('span');
    badge.className = 'ln-badge ln-badge--teal ln-badge--dot-none';
    badge.textContent = 'Applied automatically';
    head.appendChild(badge);
  }
  li.appendChild(head);

  const dl = document.createElement('dl');
  dl.className = 'ln-suggestion__values';
  const pair = (label, value) => {
    const dt = document.createElement('dt');
    dt.textContent = label;
    const dd = document.createElement('dd');
    dd.textContent = value;
    dl.append(dt, dd);
  };
  // notes[] is an ADDITION, so there is no "now" value to show — printing the
  // existing facts there would imply approving replaces them.
  if (sg.field !== 'profile.notes[]') {
    pair(sg.autoApplied ? 'Was' : 'Now', sg.currentValue || 'not set');
  }
  pair(sg.autoApplied ? 'Now' : 'Suggested', sg.proposedValue);
  li.appendChild(dl);

  if (sg.reason) {
    const why = document.createElement('p');
    why.className = 'ln-suggestion__reason';
    why.textContent = sg.reason;
    li.appendChild(why);
  }

  const src = document.createElement('p');
  src.className = 'ln-suggestion__src';
  src.textContent = sg.source === 'rca'
    ? 'Found while analysing a failed tool call'
    : 'Proposed by Live Ninja during a conversation';
  li.appendChild(src);

  const err = document.createElement('p');
  err.className = 'ln-suggestion__error';
  err.id = `suggestionError-${sg.id}`;
  err.setAttribute('role', 'alert');
  err.hidden = true;
  li.appendChild(err);

  const actions = document.createElement('div');
  actions.className = 'ln-suggestion__actions';
  const button = (label, variant, accessibleName, onClick) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = `ln-btn ${variant}`;
    b.textContent = label;
    b.setAttribute('aria-label', accessibleName);
    b.setAttribute('aria-describedby', err.id);
    b.addEventListener('click', () => onClick(b));
    return b;
  };
  const what = `${(sg.fieldLabel || sg.field).toLowerCase()}: ${sg.proposedValue}`;

  if (sg.autoApplied) {
    actions.append(
      button('Keep', 'ln-btn--primary', `Keep ${what}`, () => void decideSuggestion(sg.id, 'keep')),
      button('Undo', 'ln-btn--danger', `Undo ${what}`, () => void decideSuggestion(sg.id, 'undo')),
    );
  } else if (sg.needsPick) {
    // A location can only be approved by picking a real place, so the button
    // says what it does instead of promising an approval it cannot perform.
    actions.append(
      button('Find this place', 'ln-btn--primary', `Find ${what} in the location search`,
        () => beginLocationPick(sg)),
      button('Reject', 'ln-btn--ghost', `Reject ${what}`, () => void decideSuggestion(sg.id, 'reject')),
    );
  } else {
    actions.append(
      button('Approve', 'ln-btn--primary', `Approve ${what}`, () => void approveSuggestion(sg)),
      button('Reject', 'ln-btn--ghost', `Reject ${what}`, () => void decideSuggestion(sg.id, 'reject')),
    );
  }
  li.appendChild(actions);
  return li;
}

function suggestionRowError(id, message) {
  const el = document.getElementById(`suggestionError-${id}`);
  if (!el) return;
  el.textContent = message || '';
  el.hidden = !message;
}

function setRowBusy(id, busy) {
  const row = suggestionsList && suggestionsList.querySelector(`[data-id="${CSS.escape(id)}"]`);
  if (!row) return;
  for (const b of row.querySelectorAll('button')) b.disabled = busy;
}

/** Why this proposal cannot be approved as-is, or '' when it can. The server
 * validates too — this exists so the reason is stated next to the control
 * instead of arriving as a generic save failure. */
function approveBlockedReason(sg) {
  const v = sg.proposedValue || '';
  switch (sg.field) {
    case 'profile.units':
      return v === 'imperial' || v === 'metric'
        ? ''
        : 'That is not a unit system Live Ninja can store — reject it.';
    case 'profile.notes[]': {
      const notes = profileDoc().notes;
      if (notes.includes(v)) return '';
      return notes.length >= 20
        ? 'You already have 20 always-known facts. Remove one below, then approve this.'
        : '';
    }
    case 'profile.contactEmail':
      return v.includes('@')
        ? ''
        : 'That is not a valid email address — reject it, and tell Live Ninja the right one.';
    default:
      return v.trim() === '' ? 'That suggestion has no value — reject it.' : '';
  }
}

/** Put the proposed value into `doc` so the normal autosave writes it. */
function applySuggestionLocally(sg) {
  const v = sg.proposedValue;
  switch (sg.field) {
    case 'profile.units':
      setProfileField('units', v);
      break;
    case 'profile.notes[]': {
      const notes = profileDoc().notes;
      if (!notes.includes(v)) setProfileField('notes', notes.concat(v));
      break;
    }
    case 'profile.displayName':
      setProfileField('displayName', v);
      break;
    case 'profile.pronouns':
      setProfileField('pronouns', v);
      break;
    case 'profile.contactEmail':
      setProfileField('contactEmail', v);
      break;
    case 'profile.locale':
      setProfileField('locale', v);
      break;
    default:
      return false;
  }
  renderProfile();
  return true;
}

/** Did the approved value actually survive the write? A 409 reconcile lets the
 * REMOTE document win any field both sides touched (the documented rule), so a
 * successful flush is not by itself proof that this approval landed — and
 * recording an approval whose value was discarded would lose the suggestion
 * silently, which is the one outcome worse than an error. */
function suggestionValueLanded(sg) {
  const p = profileDoc();
  const v = sg.proposedValue;
  switch (sg.field) {
    case 'profile.units': return p.units === v;
    case 'profile.notes[]': return Array.isArray(p.notes) && p.notes.includes(v);
    case 'profile.displayName': return p.displayName === v;
    case 'profile.pronouns': return p.pronouns === v;
    case 'profile.contactEmail': return p.contactEmail === v;
    case 'profile.locale': return p.locale === v;
    case 'profile.homeLocation': return !!(p.homeLocation && p.homeLocation.lat);
    case 'profile.workLocation': return !!(p.workLocation && p.workLocation.lat);
    default: return false;
  }
}

/** Drive the autosave loop to completion for the profile key and report
 * whether the document actually committed. flush() already handles the 409
 * reconcile and re-queues; the second call is that queued retry. */
async function flushProfileNow() {
  clearTimeout(saveTimer);
  await flush();
  if (pendingKeys.has('profile')) await flush();
  return !pendingKeys.has('profile');
}

async function approveSuggestion(sg) {
  const blocked = approveBlockedReason(sg);
  if (blocked) {
    suggestionRowError(sg.id, blocked);
    return;
  }
  suggestionRowError(sg.id, '');
  setRowBusy(sg.id, true);
  if (!applySuggestionLocally(sg)) {
    suggestionRowError(sg.id, 'Live Ninja cannot apply that field here — reject it and edit the field below.');
    setRowBusy(sg.id, false);
    return;
  }
  if (!(await flushProfileNow())) {
    // The autosave path already reverted the optimistic value and toasted.
    suggestionRowError(sg.id, "Couldn't save that — check your connection and try again.");
    setRowBusy(sg.id, false);
    return;
  }
  if (!suggestionValueLanded(sg)) {
    suggestionRowError(sg.id,
      'Your settings changed on another device, so this was not applied. Try approving it again.');
    setRowBusy(sg.id, false);
    return;
  }
  await decideSuggestion(sg.id, 'approve', `Approved. ${sg.fieldLabel} is now ${sg.proposedValue}.`);
}

/** Record a decision server-side and drop the row. Also the only path for
 * keep/undo, whose write happens entirely on the server. */
async function decideSuggestion(id, action, announcement) {
  setRowBusy(id, true);
  suggestionRowError(id, '');
  try {
    await withSettingsOperation(async () => {
      const resp = await apiJSON(`${SUGGESTIONS_PATH}/${encodeURIComponent(id)}/resolve`, {
        method: 'POST',
        json: { action },
      });
      if (resp && resp.version) {
        // An undo wrote the settings document server-side, so this tab's copy
        // is stale — re-read before its next PATCH can 409 against the revert.
        await adoptServerSettingsRaw(Number(resp.version));
      }
    });
    suggestions = suggestions.filter((s) => s.id !== id);
    renderSuggestions();
    announceSuggestion(announcement || decisionAnnouncement(action));
    if (action === 'undo') showToast('Undone — put back the way it was.');
  } catch (err) {
    const status = err && err.status;
    if (status === 404 || status === 409) {
      // Already handled elsewhere (another tab, a double click). Re-sync
      // rather than telling the owner something went wrong.
      suggestions = suggestions.filter((s) => s.id !== id);
      renderSuggestions();
      void refreshSuggestions();
      return;
    }
    suggestionRowError(id, "Couldn't save that decision — check your connection and try again.");
    setRowBusy(id, false);
  }
}

function decisionAnnouncement(action) {
  switch (action) {
    case 'approve': return 'Approved.';
    case 'reject': return 'Suggestion dismissed.';
    case 'keep': return 'Kept.';
    case 'undo': return 'Undone.';
    default: return '';
  }
}

/** Re-read the settings document after a server-side write and adopt it,
 * keeping any field still pending locally. Same rule as the 409 reconcile:
 * the server's copy is authoritative for everything we are not mid-edit on. */
async function adoptServerSettingsRaw(newVersion) {
  try {
    const fresh = await apiJSON('/api/v1/settings?effective=true');
    const fetchedVersion = Number(fresh.version) || 0;
    const requiredVersion = Math.max(Number(newVersion) || 0, version);
    if (fetchedVersion < requiredVersion) return false;
    version = fetchedVersion || newVersion || version;
    for (const k of Object.keys(fresh)) {
      if (k === 'version' || pendingKeys.has(k)) continue;
      if (!deepEq(doc[k], fresh[k])) {
        doc[k] = clone(fresh[k]);
        renderField(k);
      }
    }
    doc.version = version;
    baseline = clone(fresh);
    baseline.version = version;
    pingSettingsVersion();
    return true;
  } catch {
    // Do not advance the shared version without the corresponding effective
    // document. A later write keeps the old version and must 409/reconcile
    // instead of writing stale sibling fields at a newer version.
    return false;
  }
}

/** Approving a location means picking it: prefill the matching combobox with
 * the proposed name, open the results, and let the pick resolve the row. */
function beginLocationPick(sg) {
  const which = sg.field === 'profile.workLocation' ? 'work' : 'home';
  const input = document.getElementById(LOCATION_FIELDS[which].input);
  if (!input) {
    suggestionRowError(sg.id, 'That location field is not available — reject the suggestion.');
    return;
  }
  awaitingLocationPick = { id: sg.id, which, field: sg.field, fieldLabel: sg.fieldLabel || sg.field };
  suggestionRowError(sg.id, '');
  input.value = sg.proposedValue;
  input.focus();
  input.dispatchEvent(new Event('input', { bubbles: true }));
  announceSuggestion(`Searching for ${sg.proposedValue}. Pick the right result to save it with real coordinates.`);
}

/** Called by wireLocationPicker's choose() once a real geocoder result has
 * been stored, which is the moment a location suggestion is genuinely
 * approved. A pick for the OTHER field, or one made with no pending
 * suggestion, resolves nothing. */
function locationSuggestionPicked(which) {
  const claim = awaitingLocationPick;
  if (!claim || claim.which !== which) return;
  awaitingLocationPick = null;
  void (async () => {
    if (!(await flushProfileNow()) || !suggestionValueLanded({ field: claim.field, proposedValue: '' })) {
      suggestionRowError(claim.id, "Couldn't save that location — check your connection and try again.");
      return;
    }
    await decideSuggestion(claim.id, 'approve', `Approved. ${claim.fieldLabel} saved with its coordinates.`);
  })();
}

// The drawer can be opened long after this module initialised, so
// conversation.mjs re-reads the queue on open through this hook (the same
// window.__ln* seam as the appearance applier).
window.__lnRefreshProfileSuggestions = () => void refreshSuggestions();

// A settings version bump in another tab often IS an auto-apply landing:
// re-read the queue so the toast and the badge appear without a reload.
window.addEventListener('storage', (e) => {
  if (e.key === 'ln.settings.version' && e.newValue !== null) void refreshSuggestions();
});
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') void refreshSuggestions();
});

// ---- init ----------------------------------------------------------------
// Every field below was previously SSR'd with a checked/value attribute
// baked in by the Go template; with no more SSR data island, each one needs
// an explicit renderField() pass once the fetched doc is available.

await loadPersonaCatalog(); // populates #personaPreset options, then renderField('persona')
renderField('wakeEngine');
renderField('sensitivity');
renderField('turnDetection');
renderField('micEagerness');
renderField('keepListeningSeconds');
renderField('theme'); // also applies the theme attribute + localStorage
renderField('privacy');
renderField('appearance');
renderField('voiceEngine');
wireProfile();
renderField('profile');
initDeviceSettingsControls({
  getDocument: () => doc,
  getVersion: () => version,
  prepareWrite: async () => {
    clearTimeout(saveTimer);
    await flush();
    if (pendingKeys.size > 0) await flush();
    return pendingKeys.size === 0 && !statusEl.classList.contains('is-error');
  },
  setWriteBarrier: (blocked) => {
    scopeWriteBarrier = blocked;
    if (!blocked && (queuedFlush || pendingKeys.size > 0)) {
      queuedFlush = false;
      clearTimeout(saveTimer);
      saveTimer = setTimeout(flush, 0);
    }
  },
  // Device-scoped Apply already owns the shared settings operation lane.
  reconcileEffective: adoptServerSettingsRaw,
});
void refreshSuggestions(); // M16 queue + drawer-tab badge; a failed read is silent
loadWakeCatalog();
refreshMicDevices();
setStatus('saved');
statusTextEl.textContent = 'All changes saved'; // no misleading timestamp yet
}
