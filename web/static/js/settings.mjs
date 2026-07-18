// settings.mjs — /settings page controller (WS-D settings workstream,
// docs/web-ui-spec.md §3).
//
// Hydrates from the SSR data islands (#settings-data = the full canonical
// settings document incl. `version`, #catalogs-data = the static voice/
// persona catalogs) — no settings fetch on first paint (spec §3.2). Every
// control autosaves through the shared optimistic-concurrency engine
// below: PUT /api/v1/settings {settings, version}; a 409 runs the spec
// §3.6 reconcile (remote wins per-field on a true collision, unrelated
// local edits re-apply and retry once automatically).
//
// Network access goes through toolclient.mjs (authFetch/apiJSON): the
// in-memory access JWT, refresh-once-on-401, and the X-LN-CSRF header all
// live there — this module never calls fetch() for /api/v1 or /auth
// routes directly. The only raw fetch is the public static wake-word
// catalog (/static/wakewords/catalog.json).

import { apiJSON, authFetch, ApiError } from './toolclient.mjs';

const $ = (id) => document.getElementById(id);

function readIsland(id) {
  const el = $(id);
  if (!el) throw new Error(`settings: missing data island #${id}`);
  return JSON.parse(el.textContent);
}

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

// ---- state ------------------------------------------------------------

const doc = readIsland('settings-data'); // canonical settings document
const catalogs = readIsland('catalogs-data'); // {voices, personas}
let version = Number(doc.version) || 1;
let baseline = clone(doc); // last server-confirmed document
const pendingKeys = new Set(); // top-level keys edited since last confirm

// Defensive: the server always fills these, but a malformed island must
// not take down every control on the page.
if (!doc.persona || typeof doc.persona !== 'object') doc.persona = { presetId: 'default', systemInstructions: null };
if (!doc.privacy || typeof doc.privacy !== 'object') doc.privacy = { storeAudio: false, storeTranscripts: true, retentionDays: 30 };
if (!doc.voiceEngine || typeof doc.voiceEngine !== 'object') doc.voiceEngine = { default: 'openai-realtime', devices: {} };

// ---- save-status bar + toast ------------------------------------------

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

const toastEl = $('toast');
const toastMsgEl = $('toastMsg');
const toastActionBtn = $('toastActionBtn');
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

function markChanged(key, { debounce = 0 } = {}) {
  pendingKeys.add(key);
  setStatus('saving');
  clearTimeout(saveTimer);
  saveTimer = setTimeout(flush, debounce);
}

async function flush() {
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
    const resp = await apiJSON('/api/v1/settings', {
      method: 'PUT',
      json: { settings: sent, version },
    });
    version = Number(resp.version);
    baseline = clone(resp.settings);
    baseline.version = version;
    // A field is confirmed unless the user changed it again mid-flight.
    for (const k of sentKeys) {
      if (deepEq(doc[k], sent[k])) pendingKeys.delete(k);
    }
    // Adopt server normalizations (e.g. instructions nulled on a preset
    // switch) for everything not still locally pending.
    for (const k of Object.keys(resp.settings)) {
      if (!pendingKeys.has(k) && !deepEq(doc[k], resp.settings[k])) {
        doc[k] = clone(resp.settings[k]);
        renderField(k);
      }
    }
    doc.version = version;
    if (pendingKeys.size > 0) queuedFlush = true;
    else setStatus('saved');
  } catch (err) {
    if (err instanceof ApiError && err.status === 409) {
      try {
        await reconcile409();
      } catch {
        failSave(sentKeys, sent);
      }
    } else {
      failSave(sentKeys, sent);
    }
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
  const fresh = await apiJSON('/api/v1/settings');
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
  if (pendingKeys.size === 0 || inFlight) return;
  authFetch('/api/v1/settings', {
    method: 'PUT',
    json: { settings: doc, version },
    keepalive: true,
  }).catch(() => {});
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
    case 'voice': {
      const r = document.querySelector(`input[name="voice"][value="${CSS.escape(doc.voice)}"]`);
      if (r) r.checked = true;
      break;
    }
    case 'turnDetection': {
      const r = document.querySelector(`input[name="turnDetection"][value="${CSS.escape(doc.turnDetection)}"]`);
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
      break;
    }
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

// ---- persona -----------------------------------------------------------

const personaSel = $('personaPreset');
const instructionsField = $('customInstructionsField');
const instructionsArea = $('systemInstructions');
const instructionsCount = $('instructionsCharCount');

function syncInstructionsCount() {
  instructionsCount.textContent = `${instructionsArea.value.length} / 4000`;
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

// ---- voice + preview ---------------------------------------------------

for (const r of document.querySelectorAll('input[name="voice"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.voice = r.value;
    markChanged('voice');
  });
}

const PREVIEW_SAMPLE = "Hi, I'm Live Ninja. This is how I sound.";
let previewAudio = null;
let previewBtn = null;
let previewUrl = null;
let previewSeq = 0;

function stopPreview() {
  previewSeq += 1;
  if (previewAudio) {
    previewAudio.pause();
    previewAudio = null;
  }
  if (previewUrl) {
    URL.revokeObjectURL(previewUrl);
    previewUrl = null;
  }
  if (previewBtn) {
    previewBtn.classList.remove('is-playing');
    previewBtn.setAttribute('aria-pressed', 'false');
    previewBtn = null;
  }
}

async function togglePreview(btn, voiceId) {
  if (previewBtn === btn) {
    stopPreview();
    return;
  }
  stopPreview(); // only one sample at a time (spec §3.3)
  const seq = previewSeq;
  previewBtn = btn;
  btn.classList.add('is-playing');
  btn.setAttribute('aria-pressed', 'true');
  try {
    const resp = await authFetch('/api/v1/fallback/tts', {
      method: 'POST',
      json: { text: PREVIEW_SAMPLE, voice: voiceId },
    });
    if (!resp.ok) throw new ApiError(resp.status, await resp.json().catch(() => null));
    const blob = await resp.blob();
    if (seq !== previewSeq) return; // superseded by another click
    previewUrl = URL.createObjectURL(blob);
    previewAudio = new Audio(previewUrl);
    previewAudio.addEventListener('ended', stopPreview);
    await previewAudio.play();
  } catch {
    if (seq === previewSeq) {
      stopPreview();
      showToast("Couldn't play the voice sample — try again.", { error: true });
    }
  }
}

for (const btn of document.querySelectorAll('[data-voice-preview]')) {
  btn.disabled = false; // SSR ships them disabled; JS makes them live
  btn.addEventListener('click', (e) => {
    // The button sits inside the radio's <label>: stop the click from
    // also activating the row's radio.
    e.preventDefault();
    e.stopPropagation();
    togglePreview(btn, btn.dataset.voicePreview);
  });
}

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

// ---- appearance: two style zones + accent color -------------------------
// appStyle themes everything outside the live panel (<html>); liveStyle
// themes the conversation page's orb/mic rail (#livePanel). The server
// migrates legacy {themeStyle} docs on read, but a stale island/cached
// bundle may still carry one — migrate it here too so the pickers and
// write-backs always use the two-zone shape.

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

// Hydrate from the island on load: normalizes any legacy shape, marks the
// active swatch, and applies + caches the authoritative appearance (the
// island beats a stale ln.appearance cache from another device/session).
renderField('appearance');

// ==================== voice-engine-section:BEGIN ====================
// M12 secondary-voice-engine picker (FR-VE-04), owned by the M12 web-client
// workstream — edit only inside these markers. Bound to voiceEngine.default
// (the engine this browser session and any un-pinned device use; the broker
// resolves devices[deviceId] ?? default). The segmented radios are SSR'd
// without a checked attribute — the current value is hydrated from the
// settings island via renderField('voiceEngine') on init below. Unknown
// forward-compat fields (e.g. voiceEngine.devices) are preserved untouched
// by the spread + the autosave engine's whole-document PUT.
for (const r of document.querySelectorAll('input[name="voiceEngine"]')) {
  r.addEventListener('change', () => {
    if (!r.checked) return;
    doc.voiceEngine = { ...doc.voiceEngine, default: r.value };
    markChanged('voiceEngine');
  });
}
renderField('voiceEngine');
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

// ---- init --------------------------------------------------------------

applyTheme(doc.theme); // also seeds localStorage for theme.js on next load
loadWakeCatalog();
refreshMicDevices();
setStatus('saved');
statusTextEl.textContent = 'All changes saved'; // no misleading timestamp yet

// The catalogs island is currently informational (controls are fully
// SSR'd); exported for the console/tests and future dynamic re-renders.
export { doc, catalogs, renderField };
