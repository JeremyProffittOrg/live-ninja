// conversation.mjs — page orchestrator for /conversation (integrator-owned
// glue; docs/web-ui-spec.md §2). Every capability lives in a focused module:
//
//   mic.mjs            — mic state machine + PTT/pill/status UI binding
//   realtime.mjs       — WebRTC transport (RealtimeSession)
//   toolclient.mjs     — auth'd fetch + tool dispatch (used via apiJSON here)
//   transcript.mjs     — XSS-safe incremental transcript renderer
//   transcriptsink.mjs — batched POST /api/v1/transcript logging
//   visualizer.mjs     — AnalyserNode canvas bars
//   wakeword.mjs       — WASM openWakeWord engine (hands-free mode)
//
// This file only wires them to the page DOM (ids from
// templates/pages/conversation.html) and to the settings document:
//   - persona/voice quick-switch selects populated from
//     GET /api/v1/realtime/personas|voices (never blind text/options);
//   - optimistic PUT /api/v1/settings with the §3.6 409 retry-once rule;
//   - composer → live session sendUserText, or POST /api/v1/fallback/turn
//     when no session is connected (spec §2.5 "you can still type below");
//   - wake-word toggle → WakeWordEngine lifecycle → mic.notifyWake().

import { apiJSON, ApiError } from './toolclient.mjs';
import { MicController, MicState } from './mic.mjs';
import { Transcript } from './transcript.mjs';
import { createTranscriptSink } from './transcriptsink.mjs';
import { Visualizer } from './visualizer.mjs';
import { createWakeWordEngine, isWakeWordSupported } from './wakeword.mjs';

const SETTINGS_PATH = '/api/v1/settings';
const VOICES_PATH = '/api/v1/realtime/voices';
const PERSONAS_PATH = '/api/v1/realtime/personas';
const FALLBACK_TURN_PATH = '/api/v1/fallback/turn';
const WAKE_CATALOG_PATH = '/static/wakewords/catalog.json';

const $ = (id) => document.getElementById(id);

// ---- toast (single #toast element on this page) --------------------------
//
// Plain toasts (settings applied, etc.) are a transient polite status line.
// Error toasts that carry a transaction ref (txId) and/or a backend message
// become a reportable error banner: role=alert, keyboard-focusable, with a
// "Details" affordance that reveals — on hover, on keyboard focus, or on tap
// — the full backend message plus "Ref: <txId>" and a Copy button that copies
// the txId (so the user can report it). Reportable errors persist (no
// auto-dismiss) and carry a close control; everything else auto-dismisses.

const toastEl = $('toast');
const DETAIL_PANEL_ID = 'toastDetailPanel';
let toastTimer = 0;
let copyResetTimer = 0;

function hideToast() {
  if (!toastEl) return;
  clearTimeout(toastTimer);
  toastEl.classList.remove('is-visible', 'is-open');
  toastEl.hidden = true;
}

async function copyText(text) {
  if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      /* fall through to the legacy path (permissions/focus edge cases) */
    }
  }
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    return ok;
  } catch {
    return false;
  }
}

/**
 * @param {string} message short, human-facing line
 * @param {{error?: boolean, txId?: string, detail?: string}} [opts]
 *   error  — style + assertive alert semantics
 *   txId   — transaction ref; drives the Copy button
 *   detail — full backend message shown under "Details"
 */
function toast(message, { error = false, txId = '', detail = '' } = {}) {
  if (!toastEl) return;
  clearTimeout(toastTimer);
  clearTimeout(copyResetTimer);

  const ref = (txId || '').trim();
  const backendMsg = (detail || '').trim();
  const reportable = !!error && (ref !== '' || backendMsg !== '');

  toastEl.replaceChildren();
  toastEl.classList.toggle('is-error', !!error);
  toastEl.classList.toggle('has-details', reportable);
  toastEl.classList.remove('is-open');
  // Errors announce assertively (role=alert); plain toasts stay polite.
  toastEl.setAttribute('role', error ? 'alert' : 'status');
  toastEl.setAttribute('aria-live', error ? 'assertive' : 'polite');
  // Reportable banners are keyboard-focusable so a screen-reader user can
  // land on them and reach the Details/Copy controls.
  if (reportable) toastEl.setAttribute('tabindex', '-1');
  else toastEl.removeAttribute('tabindex');

  const bodyRow = document.createElement('div');
  bodyRow.className = 'ln-toast__body';

  const msgEl = document.createElement('span');
  msgEl.className = 'ln-toast__msg';
  msgEl.textContent = message;
  bodyRow.appendChild(msgEl);

  if (reportable) {
    // Native tooltip mirrors the accessible expandable (title AND panel).
    const tooltipParts = [];
    if (backendMsg) tooltipParts.push(backendMsg);
    if (ref) tooltipParts.push(`Ref: ${ref}`);
    const tooltip = tooltipParts.join('\n');

    const detailsBtn = document.createElement('button');
    detailsBtn.type = 'button';
    detailsBtn.className = 'ln-toast__details';
    detailsBtn.textContent = 'Details';
    detailsBtn.title = tooltip;
    detailsBtn.setAttribute('aria-controls', DETAIL_PANEL_ID);
    detailsBtn.setAttribute('aria-expanded', 'false');
    detailsBtn.addEventListener('click', () => {
      const open = toastEl.classList.toggle('is-open');
      detailsBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    });
    bodyRow.appendChild(detailsBtn);

    const closeBtn = document.createElement('button');
    closeBtn.type = 'button';
    closeBtn.className = 'ln-toast__close';
    closeBtn.setAttribute('aria-label', 'Dismiss');
    closeBtn.textContent = '×'; // ×
    closeBtn.addEventListener('click', hideToast);
    bodyRow.appendChild(closeBtn);
  }

  toastEl.appendChild(bodyRow);

  if (reportable) {
    const panel = document.createElement('div');
    panel.className = 'ln-toast__panel';
    panel.id = DETAIL_PANEL_ID;

    if (backendMsg) {
      const detailMsg = document.createElement('p');
      detailMsg.className = 'ln-toast__detail-msg';
      detailMsg.textContent = backendMsg;
      panel.appendChild(detailMsg);
    }

    if (ref) {
      const refRow = document.createElement('div');
      refRow.className = 'ln-toast__ref';

      const refLabel = document.createElement('span');
      refLabel.className = 'ln-toast__ref-label';
      refLabel.append('Ref: ');
      const refVal = document.createElement('span');
      refVal.className = 'ln-toast__txid';
      refVal.textContent = ref;
      refLabel.appendChild(refVal);
      refRow.appendChild(refLabel);

      const copyBtn = document.createElement('button');
      copyBtn.type = 'button';
      copyBtn.className = 'ln-toast__copy';
      copyBtn.textContent = 'Copy';
      copyBtn.setAttribute('aria-label', 'Copy reference ID');
      copyBtn.addEventListener('click', async () => {
        const ok = await copyText(ref);
        copyBtn.textContent = ok ? 'Copied' : 'Press ⌘/Ctrl+C';
        copyBtn.classList.toggle('is-copied', ok);
        clearTimeout(copyResetTimer);
        copyResetTimer = setTimeout(() => {
          copyBtn.textContent = 'Copy';
          copyBtn.classList.remove('is-copied');
        }, 1600);
      });
      refRow.appendChild(copyBtn);
      panel.appendChild(refRow);
    }

    toastEl.appendChild(panel);
  }

  toastEl.hidden = false;
  requestAnimationFrame(() => toastEl.classList.add('is-visible'));

  // Reportable banners persist so the ref can be read/copied; everything
  // else auto-dismisses.
  if (!reportable) {
    toastTimer = setTimeout(hideToast, 6000);
  }
}

// Escape dismisses a focused/hovered banner (focus is inside it).
if (toastEl) {
  toastEl.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !toastEl.hidden) {
      e.stopPropagation();
      hideToast();
    }
  });
}

// ---- settings document (single source of truth for both quick-switches) --

let settingsDoc = null; // full canonical document, including `version`
let wakeCatalog = null; // {wakewords:[{id, phrase, ...}]}

function settingsVersion() {
  return Number(settingsDoc && settingsDoc.version) || 1;
}

function docCopyWithoutVersion() {
  const copy = structuredClone(settingsDoc);
  delete copy.version;
  return copy;
}

/**
 * Optimistic PUT with the spec §3.6 conflict rule: on 409 re-GET, re-apply
 * the same mutation on the fresh document, retry once; a second 409 means
 * remote wins — adopt it and tell the caller so the UI re-syncs.
 * @returns {Promise<{ok: boolean, conflict?: boolean}>}
 */
async function putSettings(mutate) {
  const attempt = async () => {
    const body = docCopyWithoutVersion();
    mutate(body);
    const resp = await apiJSON(SETTINGS_PATH, {
      method: 'PUT',
      json: { settings: body, version: settingsVersion() },
    });
    settingsDoc = resp.settings;
    settingsDoc.version = resp.version;
  };

  try {
    await attempt();
    return { ok: true };
  } catch (err) {
    if (!(err instanceof ApiError) || err.code !== 'version_conflict') throw err;
  }
  // Conflict: refresh, retry once with the same mutation on top.
  settingsDoc = await apiJSON(SETTINGS_PATH);
  try {
    await attempt();
    return { ok: true };
  } catch (err) {
    if (err instanceof ApiError && err.code === 'version_conflict') {
      settingsDoc = await apiJSON(SETTINGS_PATH);
      return { ok: false, conflict: true };
    }
    throw err;
  }
}

// ---- quick-switch selects (populated from the real catalogs, spec §2.3) --

const personaSelect = $('personaSelect');
const voiceSelect = $('voiceSelect');

function fillSelect(selectEl, rows, selectedId) {
  if (!selectEl) return;
  selectEl.replaceChildren();
  let found = false;
  for (const row of rows) {
    const opt = document.createElement('option');
    opt.value = row.id;
    opt.textContent = row.name || row.id;
    if (row.id === selectedId) {
      opt.selected = true;
      found = true;
    }
    selectEl.appendChild(opt);
  }
  if (!found && selectedId) {
    // Forward-compat: a stored value not in the catalog is kept, never
    // silently dropped (settings.schema.json rule).
    const opt = document.createElement('option');
    opt.value = selectedId;
    opt.textContent = `${selectedId} (kept as-is)`;
    opt.selected = true;
    selectEl.appendChild(opt);
  }
}

function currentPersonaId() {
  const p = settingsDoc && settingsDoc.persona;
  return (p && typeof p.presetId === 'string' && p.presetId) || 'default';
}

let personaCatalog = [];

function personaLabelFor(presetId) {
  if (presetId === 'default') return ''; // plain "Live Ninja" label
  if (presetId === 'custom') return 'Custom';
  const row = personaCatalog.find((p) => p.id === presetId);
  return (row && row.name) || presetId;
}

function syncQuickSwitchesFromDoc() {
  if (personaSelect) personaSelect.value = currentPersonaId();
  if (voiceSelect && typeof settingsDoc.voice === 'string') voiceSelect.value = settingsDoc.voice;
  transcript.setPersonaLabel(personaLabelFor(currentPersonaId()));
}

function isLive() {
  return !!(mic.session && mic.session.isConnected);
}

async function saveQuickSwitch({ mutate, revert, appliedToast }) {
  try {
    const res = await putSettings(mutate);
    if (res.conflict) {
      toast('Someone updated your settings from another device — refreshed.');
      syncQuickSwitchesFromDoc();
      return;
    }
    toast(appliedToast());
    syncQuickSwitchesFromDoc();
  } catch (err) {
    revert();
    if (err && err.name === 'AuthLostError') return; // toolclient redirects
    toast("Couldn't save your changes — check your connection and try again.", {
      error: true,
      txId: err instanceof ApiError ? err.txId : '',
      detail: err instanceof ApiError ? err.message : '',
    });
  }
}

// ---- transcript + per-session rendering ----------------------------------

const transcript = new Transcript($('transcriptScroll'), $('transcript'));

function attachTranscriptRendering(session) {
  const turnByItem = new Map(); // realtime itemId -> transcript turnId

  const beginOrAppend = (role, e) => {
    const { itemId, delta } = e.detail;
    let turnId = turnByItem.get(itemId);
    if (!turnId) {
      if (role === 'assistant') transcript.hideTypingIndicator();
      turnId = transcript.startTurn(role);
      turnByItem.set(itemId, turnId);
    }
    transcript.appendDelta(turnId, delta);
  };
  const finalize = (role, e) => {
    const { itemId, text } = e.detail;
    const turnId = turnByItem.get(itemId);
    if (turnId) {
      transcript.completeTurn(turnId);
      turnByItem.delete(itemId);
    } else if (text) {
      // Final arrived with no streamed deltas (possible for user
      // transcription) — render the whole turn at once.
      if (role === 'assistant') transcript.hideTypingIndicator();
      transcript.addMessage(role, text);
    }
  };

  session.addEventListener('assistantdelta', (e) => beginOrAppend('assistant', e));
  session.addEventListener('assistantfinal', (e) => finalize('assistant', e));
  session.addEventListener('userdelta', (e) => beginOrAppend('user', e));
  session.addEventListener('userfinal', (e) => finalize('user', e));
  session.addEventListener('thinking', () => transcript.showTypingIndicator());
  session.addEventListener('responsedone', () => transcript.hideTypingIndicator());
  session.addEventListener('bargein', () => transcript.hideTypingIndicator());
  session.addEventListener('connectionlost', () => transcript.hideTypingIndicator());
  session.addEventListener('closed', () => transcript.hideTypingIndicator());

  session.addEventListener('toolresult', (e) => {
    const { tool, result } = e.detail;
    transcript.appendToolResultCard({
      icon: '🛠',
      title: toolTitle(tool),
      badge: 'Done',
      badgeVariant: 'teal',
      fields: toolFields(result),
    });
  });
  session.addEventListener('toolerror', (e) => {
    transcript.appendToolResultCard({
      icon: '🛠',
      title: toolTitle(e.detail.tool),
      badge: 'Failed',
      badgeVariant: 'error',
      fields: [['Status', 'The tool call failed — the assistant was told.']],
    });
  });
}

function toolTitle(tool) {
  const name = String(tool || 'tool');
  return name.replace(/[_-]+/g, ' ').replace(/\b\w/g, (ch) => ch.toUpperCase());
}

/** Flatten a tool result object into [label, value] rows — scalars only,
 * nested values summarized, never a raw object dump (spec §2.8). */
function toolFields(result) {
  if (result === null || result === undefined) return [['Result', '—']];
  if (typeof result !== 'object') return [['Result', String(result)]];
  const rows = [];
  for (const [key, value] of Object.entries(result)) {
    if (rows.length >= 8) break;
    let text;
    if (value === null || value === undefined) text = '—';
    else if (typeof value === 'object') {
      text = Array.isArray(value) ? `${value.length} item${value.length === 1 ? '' : 's'}` : '(details)';
    } else text = String(value);
    rows.push([toolTitle(key), text]);
  }
  return rows.length ? rows : [['Result', 'OK']];
}

// ---- visualizer + orb ----------------------------------------------------

const viz = new Visualizer($('viz'));
const orbEl = $('orb');

function attachVisualizer(session) {
  session.addEventListener('sessionready', () => {
    viz.setLocalStream(session.localStream);
    viz.start();
  });
  session.addEventListener('speaking', () => {
    if (session.remoteStream) viz.setRemoteStream(session.remoteStream);
  });
  const clear = () => {
    viz.setActiveSource('none');
    viz.setLocalStream(null);
    viz.setRemoteStream(null);
    viz.stop();
  };
  session.addEventListener('closed', clear);
  session.addEventListener('connectionlost', clear);
}

function syncVisualToState(state) {
  if (orbEl) orbEl.classList.toggle('ln-orb--idle', !state.startsWith('live-'));
  switch (state) {
    case MicState.LISTENING:
      viz.setActiveSource('local');
      break;
    case MicState.SPEAKING:
      viz.setActiveSource('remote');
      break;
    default:
      viz.setActiveSource('none');
      break;
  }
}

// ---- mic controller + transcript sink ------------------------------------

// Declared before MicController: its constructor renders synchronously and
// calls getWakePhrase() -> wakePhraseText(), which reads wakeEngine. A `let`
// below this point would be in its temporal dead zone (ReferenceError).
let wakeEngine = null;
let wakeStarting = false;

const mic = new MicController({
  getMicDeviceId: () => (settingsDoc && typeof settingsDoc.micDeviceId === 'string' ? settingsDoc.micDeviceId : null),
  getWakePhrase: () => wakePhraseText(),
});

const sink = createTranscriptSink();
sink.observe(mic);

mic.addEventListener('sessioncreated', (e) => {
  const session = e.detail.session;
  attachTranscriptRendering(session);
  attachVisualizer(session);
});
mic.addEventListener('statechange', (e) => syncVisualToState(e.detail.state));
mic.addEventListener('error', (e) =>
  toast(e.detail.message, { error: true, txId: e.detail.txId, detail: e.detail.detail }),
);
mic.addEventListener('toast', (e) => toast(e.detail.message));

// ---- wake word (hands-free mode) -----------------------------------------

const wakeToggle = $('wakeToggle');
const wakeHint = $('wakeHint');
const wakePhraseEl = $('wakePhrase');

function wakePhraseText() {
  if (wakeEngine && wakeEngine.phrase) return wakeEngine.phrase;
  const id = settingsDoc && settingsDoc.wakeWord;
  if (wakeCatalog && id) {
    const row = wakeCatalog.wakewords.find((w) => w.id === id);
    if (row) return row.phrase;
  }
  return '';
}

function renderWakeUI() {
  const on = !!(wakeToggle && wakeToggle.checked);
  const phrase = wakePhraseText();
  if (wakePhraseEl) {
    wakePhraseEl.textContent = phrase ? `“${phrase}”` : '';
    wakePhraseEl.hidden = !(on && phrase);
  }
  if (wakeHint && isWakeWordSupported()) {
    wakeHint.textContent = on
      ? phrase
        ? `On — say “${phrase}” to start hands-free.`
        : 'On — listening for your wake phrase.'
      : 'Off — use the push-to-talk button to start a turn.';
  }
}

async function setWakeListening(on) {
  if (on) {
    if (wakeStarting) return;
    wakeStarting = true;
    if (!wakeEngine) {
      wakeEngine = createWakeWordEngine({
        wakeWordId: (settingsDoc && settingsDoc.wakeWord) || null,
        sensitivity: settingsDoc && typeof settingsDoc.sensitivity === 'number' ? settingsDoc.sensitivity : 0.5,
        onDetect: () => mic.notifyWake(),
      });
    }
    try {
      await wakeEngine.start();
      renderWakeUI();
    } catch (err) {
      if (wakeToggle) wakeToggle.checked = false;
      if (err && err.code === 'unsupported') {
        mic.setHandsFreeAvailable(false);
      } else {
        toast("Couldn't start hands-free listening — use the mic button.", { error: true });
        renderWakeUI();
      }
    } finally {
      wakeStarting = false;
    }
  } else if (wakeEngine) {
    await wakeEngine.stop();
    renderWakeUI();
  } else {
    renderWakeUI();
  }
}

if (!isWakeWordSupported()) {
  mic.setHandsFreeAvailable(false);
} else if (wakeToggle) {
  // mic.mjs already persists the toggle to localStorage; this listener owns
  // the engine lifecycle.
  wakeToggle.addEventListener('change', () => void setWakeListening(wakeToggle.checked));
}

// ---- composer (typed input; live session or fallback turn) ---------------

const composerForm = $('composerForm');
const composerInput = $('composerInput');
const composerSend = $('composerSend');
let fallbackInFlight = false;

if (composerInput && composerSend) {
  composerInput.addEventListener('input', () => {
    composerSend.disabled = composerInput.value.trim() === '' || fallbackInFlight;
  });
}

async function sendTyped(text) {
  transcript.addUserMessage(text);
  if (isLive()) {
    try {
      mic.session.sendUserText(text);
      return;
    } catch {
      // Datachannel raced closed — fall through to the HTTP fallback.
    }
  }
  fallbackInFlight = true;
  if (composerSend) composerSend.disabled = true;
  transcript.showTypingIndicator();
  try {
    const resp = await apiJSON(FALLBACK_TURN_PATH, {
      method: 'POST',
      json: { text, persona: currentPersonaId() },
    });
    transcript.hideTypingIndicator();
    transcript.addAssistantMessage((resp && resp.text) || '');
  } catch (err) {
    transcript.hideTypingIndicator();
    if (err && err.name === 'AuthLostError') return;
    // Short line stays friendly; the backend message + ref go under Details.
    toast("Couldn't send your message — check your connection and try again.", {
      error: true,
      txId: err instanceof ApiError ? err.txId : '',
      detail: err instanceof ApiError ? err.message : '',
    });
  } finally {
    fallbackInFlight = false;
    if (composerSend && composerInput) composerSend.disabled = composerInput.value.trim() === '';
  }
}

if (composerForm && composerInput) {
  composerForm.addEventListener('submit', (e) => {
    e.preventDefault();
    const text = composerInput.value.trim();
    if (!text || fallbackInFlight) return;
    composerInput.value = '';
    if (composerSend) composerSend.disabled = true;
    void sendTyped(text);
    composerInput.focus();
  });
}

// ---- quick-switch change handlers (spec §2.6) ----------------------------

if (personaSelect) {
  personaSelect.addEventListener('change', () => {
    const prev = currentPersonaId();
    const next = personaSelect.value;
    if (next === prev) return;
    const prevLabel = personaLabelFor(prev) || 'Live Ninja';
    void saveQuickSwitch({
      mutate: (doc) => {
        if (!doc.persona || typeof doc.persona !== 'object') doc.persona = {};
        doc.persona.presetId = next;
        // Server normalizes systemInstructions to null for non-custom
        // presets; nothing else to do client-side.
      },
      revert: () => {
        personaSelect.value = prev;
      },
      appliedToast: () =>
        isLive()
          ? `Applies to your next conversation — this one keeps ${prevLabel}.`
          : 'Persona updated.',
    });
  });
}

if (voiceSelect) {
  voiceSelect.addEventListener('change', () => {
    const prev = (settingsDoc && settingsDoc.voice) || 'cedar';
    const next = voiceSelect.value;
    if (next === prev) return;
    void saveQuickSwitch({
      mutate: (doc) => {
        doc.voice = next;
      },
      revert: () => {
        voiceSelect.value = prev;
      },
      appliedToast: () =>
        isLive() ? 'Applies to your next conversation — this one keeps the current voice.' : 'Voice updated.',
    });
  });
}

// ---- bootstrap -----------------------------------------------------------

async function bootstrap() {
  // Settings first (drives everything else); catalogs in parallel.
  const [settings, voices, personas, catalog] = await Promise.allSettled([
    apiJSON(SETTINGS_PATH),
    apiJSON(VOICES_PATH),
    apiJSON(PERSONAS_PATH),
    fetch(WAKE_CATALOG_PATH, { credentials: 'same-origin' }).then((r) => (r.ok ? r.json() : null)),
  ]);

  if (settings.status === 'fulfilled') {
    settingsDoc = settings.value;
  } else {
    if (settings.reason && settings.reason.name === 'AuthLostError') return; // redirecting
    // Defaults keep the page usable; writes will re-fetch on conflict.
    settingsDoc = {
      version: 1,
      voice: 'cedar',
      persona: { presetId: 'default', systemInstructions: null },
      wakeWord: 'hey-live-ninja',
      sensitivity: 0.5,
      privacy: { storeTranscripts: true },
    };
    toast("Couldn't load your settings — using defaults for now.", { error: true });
  }

  if (personas.status === 'fulfilled' && Array.isArray(personas.value.personas)) {
    personaCatalog = personas.value.personas.slice();
  }
  // The "custom" persona is a client-side concept (free-text instructions,
  // spec §3.3) — always offered last, matching the settings page.
  const personaRows = personaCatalog.concat([{ id: 'custom', name: 'Custom instructions' }]);
  fillSelect(personaSelect, personaRows, currentPersonaId());

  if (voices.status === 'fulfilled' && Array.isArray(voices.value.voices)) {
    fillSelect(voiceSelect, voices.value.voices, (settingsDoc && settingsDoc.voice) || 'cedar');
  }

  if (catalog.status === 'fulfilled' && catalog.value && Array.isArray(catalog.value.wakewords)) {
    wakeCatalog = catalog.value;
  }

  transcript.setPersonaLabel(personaLabelFor(currentPersonaId()));

  const privacy = settingsDoc.privacy;
  sink.setEnabled(!(privacy && privacy.storeTranscripts === false));

  renderWakeUI();
  // Hands-free restored ON from a previous visit (mic.mjs reads
  // localStorage in its constructor): bring the engine up now.
  if (mic.handsFree) void setWakeListening(true);
}

void bootstrap();
