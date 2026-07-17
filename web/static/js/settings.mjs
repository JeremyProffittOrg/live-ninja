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
