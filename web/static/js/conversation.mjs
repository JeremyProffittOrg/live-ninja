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
import { createMicTest } from './mictest.mjs';
import { Transcript } from './transcript.mjs';
import { createTranscriptSink } from './transcriptsink.mjs';
import { Visualizer } from './visualizer.mjs';
import { createWakeWordEngine, isWakeWordSupported } from './wakeword.mjs';

const SETTINGS_PATH = '/api/v1/settings';
const VOICES_PATH = '/api/v1/realtime/voices';
// Full grouped persona library (Built-in / Mine / Shared) — the quick-
// switch select renders it as <optgroup>s (personas platform feature).
const PERSONAS_PATH = '/api/v1/personas';
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
    pingSettingsChanged(); // cross-tab channel (see the storage section below)
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

// fillPersonaSelect renders the grouped persona library into the quick-
// switch select: Built-in / Mine / Shared <optgroup>s plus the trailing
// client-side "custom" option (settings.schema.json persona rule). Same
// forward-compat posture as fillSelect: an unknown stored value is kept,
// never silently dropped.
function fillPersonaSelect(selectEl, groups, selectedId) {
  if (!selectEl) return;
  selectEl.replaceChildren();
  let found = false;
  const addOption = (parent, id, name) => {
    const opt = document.createElement('option');
    opt.value = id;
    opt.textContent = name || id;
    if (id === selectedId) {
      opt.selected = true;
      found = true;
    }
    parent.appendChild(opt);
  };
  const addGroup = (label, rows) => {
    if (!rows || rows.length === 0) return;
    const og = document.createElement('optgroup');
    og.label = label;
    for (const row of rows) addOption(og, row.id, row.name);
    selectEl.appendChild(og);
  };
  addGroup('Built-in', groups && groups.builtin);
  addGroup('Mine', groups && groups.mine);
  addGroup('Shared', groups && groups.shared);
  addOption(selectEl, 'custom', 'Custom instructions');
  if (!found && selectedId) {
    addOption(selectEl, selectedId, `${selectedId} (kept as-is)`);
    selectEl.value = selectedId;
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

/** Human-readable voice name from the (catalog-populated) quick-switch
 * select — never show a raw id in banner copy. */
function voiceLabelFor(voiceId) {
  if (voiceSelect) {
    const opt = [...voiceSelect.options].find((o) => o.value === voiceId);
    if (opt) return opt.textContent;
  }
  return voiceId;
}

function syncQuickSwitchesFromDoc() {
  if (personaSelect) personaSelect.value = currentPersonaId();
  if (voiceSelect && typeof settingsDoc.voice === 'string') voiceSelect.value = settingsDoc.voice;
  transcript.setPersonaLabel(personaLabelFor(currentPersonaId()));
}

function isLive() {
  return !!(mic.session && mic.session.isConnected);
}

async function saveQuickSwitch({ mutate, revert, appliedToast, appliedBanner }) {
  try {
    const res = await putSettings(mutate);
    if (res.conflict) {
      toast('Someone updated your settings from another device — refreshed.');
      syncQuickSwitchesFromDoc();
      return;
    }
    // Mid-session persona/voice changes get the PERSISTENT banner (owner
    // 2026-07-18) instead of a transient toast; everything else toasts.
    const bannerMsg = appliedBanner ? appliedBanner() : '';
    if (bannerMsg) showPendingBanner(bannerMsg);
    else toast(appliedToast());
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

  // GA Realtime ordering quirk (verified in prod): the user transcription
  // final (conversation.item.input_audio_transcription.completed) routinely
  // lands AFTER the assistant response deltas have started rendering, which
  // used to paint the answer above the question. Track the assistant turn
  // that got ahead of a still-untranscribed user utterance so the late user
  // turn can be inserted BEFORE the response it prompted.
  let userSpeechPending = false; // user spoke; their transcript hasn't rendered yet
  let anchorTurnId = null; // first assistant turn rendered ahead of that user turn

  const userTurnPlaced = () => {
    userSpeechPending = false;
    anchorTurnId = null;
  };

  const beginOrAppend = (role, e) => {
    const { itemId, delta } = e.detail;
    let turnId = turnByItem.get(itemId);
    if (!turnId) {
      if (role === 'assistant') {
        transcript.hideTypingIndicator();
        turnId = transcript.startTurn(role);
        if (userSpeechPending && !anchorTurnId) anchorTurnId = turnId;
      } else {
        turnId = transcript.startTurn(role, { before: anchorTurnId || undefined });
        userTurnPlaced();
      }
      turnByItem.set(itemId, turnId);
    }
    transcript.appendDelta(turnId, delta);
  };
  const finalize = (role, e) => {
    const { itemId, text } = e.detail;
    const turnId = turnByItem.get(itemId);
    if (turnId) {
      // Pass the final text through: the completed transcript is
      // authoritative and updates the streamed bubble in place when they
      // differ (transcript.mjs replaces via textContent).
      transcript.completeTurn(turnId, { text });
      turnByItem.delete(itemId);
    } else if (text) {
      // Final arrived with no streamed deltas (the normal GA path for user
      // transcription) — render the whole turn at once, anchored before the
      // response it prompted if that response is already rendering.
      if (role === 'assistant') transcript.hideTypingIndicator();
      if (role === 'user') {
        transcript.addMessage(role, text, { before: anchorTurnId || undefined });
      } else {
        transcript.addMessage(role, text);
      }
    }
    // Any user final ends that utterance's transcription (even an empty
    // one) — drop the anchor so it can't misplace a later user turn.
    if (role === 'user') userTurnPlaced();
  };

  session.addEventListener('assistantdelta', (e) => beginOrAppend('assistant', e));
  session.addEventListener('assistantfinal', (e) => finalize('assistant', e));
  session.addEventListener('userdelta', (e) => beginOrAppend('user', e));
  session.addEventListener('userfinal', (e) => finalize('user', e));
  session.addEventListener('speechstarted', () => {
    // A new utterance begins: any anchor left from a previous exchange is
    // stale (its transcript was lost or absorbed) — never re-anchor to it.
    userSpeechPending = true;
    anchorTurnId = null;
  });
  session.addEventListener('usertranscriptfailed', () => userTurnPlaced());
  session.addEventListener('thinking', () => transcript.showTypingIndicator());
  session.addEventListener('responsedone', () => transcript.hideTypingIndicator());
  session.addEventListener('bargein', () => transcript.hideTypingIndicator());
  session.addEventListener('connectionlost', () => transcript.hideTypingIndicator());
  session.addEventListener('closed', () => transcript.hideTypingIndicator());

  session.addEventListener('toolcall', () => toolActivityStart());
  session.addEventListener('toolresult', (e) => {
    toolActivityEnd();
    if (!showToolCalls()) return;
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
    toolActivityEnd();
    if (!showToolCalls()) return;
    transcript.appendToolResultCard({
      icon: '🛠',
      title: toolTitle(e.detail.tool),
      badge: 'Failed',
      badgeVariant: 'error',
      fields: [['Status', 'The tool call failed — the assistant was told.']],
    });
  });
  session.addEventListener('closed', () => toolActivityReset());
  session.addEventListener('connectionlost', () => toolActivityReset());
}

// ---- tool-call visibility toggle + in-flight activity badge --------------

const SHOW_TOOLS_KEY = 'ln.showToolCalls';
const showToolsToggle = $('showToolsToggle');

function showToolCalls() {
  return !showToolsToggle || showToolsToggle.checked;
}

if (showToolsToggle) {
  try {
    showToolsToggle.checked = localStorage.getItem(SHOW_TOOLS_KEY) === '1';
  } catch {
    /* storage unavailable — default off (owner 2026-07-18) */
  }
  showToolsToggle.addEventListener('change', () => {
    try {
      localStorage.setItem(SHOW_TOOLS_KEY, showToolsToggle.checked ? '1' : '0');
    } catch {
      /* non-fatal */
    }
  });
}

const toolActivityEl = $('toolActivity');
let toolsInFlight = 0;
let toolActivityLinger = 0;

function toolActivityStart() {
  toolsInFlight++;
  clearTimeout(toolActivityLinger);
  if (toolActivityEl) toolActivityEl.hidden = false;
}

function toolActivityEnd() {
  if (toolsInFlight > 0) toolsInFlight--;
  if (toolsInFlight === 0 && toolActivityEl) {
    // Brief linger so even instant tools visibly flash the badge.
    clearTimeout(toolActivityLinger);
    toolActivityLinger = setTimeout(() => {
      toolActivityEl.hidden = true;
    }, 800);
  }
}

function toolActivityReset() {
  toolsInFlight = 0;
  clearTimeout(toolActivityLinger);
  if (toolActivityEl) toolActivityEl.hidden = true;
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

// ---- session cost badge (upper-right of the live panel) -------------------
//
// OpenAI Realtime reports token usage on each completed response
// (response.done -> realtime.mjs's 'usage' event, openai-direct only —
// nova-bridge doesn't surface usage, internal/voiceengine drops it). Rates
// come from the session bootstrap (internal/realtime/rates.go,
// session.rates) so pricing never lives in this file, only the arithmetic.
// Accumulates across reconnects within one displayed conversation; only
// "New conversation" (below) zeroes it — a dropped/retried session mid-call
// must not silently undercount the running total.

const costBadgeEl = $('costBadge');
let costTotalUSD = 0;
let costTextTokens = 0; // input + output text tokens, running total
let costAudioTokens = 0; // input + output audio tokens, running total

function formatCostUSD(usd) {
  return usd >= 1 ? `~$${usd.toFixed(2)}` : `~$${usd.toFixed(3)}`;
}

function renderCostBadge() {
  if (!costBadgeEl) return;
  costBadgeEl.textContent = formatCostUSD(costTotalUSD);
  costBadgeEl.title =
    `Session cost estimate (list price, not a bill)\n` +
    `Text tokens: ${costTextTokens.toLocaleString()}\n` +
    `Audio tokens: ${costAudioTokens.toLocaleString()}`;
}

function resetCostBadge() {
  costTotalUSD = 0;
  costTextTokens = 0;
  costAudioTokens = 0;
  if (costBadgeEl) costBadgeEl.hidden = true;
}

function attachCostBadge(session) {
  if (!costBadgeEl) return;
  session.addEventListener('sessionready', () => {
    costBadgeEl.hidden = false;
    renderCostBadge();
  });
  session.addEventListener('usage', (e) => {
    const rates = session.rates;
    if (!rates) return; // nova-bridge, or a bootstrap that omitted rates
    const usage = (e.detail && e.detail.usage) || {};
    const inDetails = usage.input_token_details || {};
    const outDetails = usage.output_token_details || {};
    const cachedDetails = inDetails.cached_tokens_details || {};

    const inTextCached = cachedDetails.text_tokens || 0;
    const inAudioCached = cachedDetails.audio_tokens || 0;
    const inText = Math.max(0, (inDetails.text_tokens || 0) - inTextCached);
    const inAudio = Math.max(0, (inDetails.audio_tokens || 0) - inAudioCached);
    const outText = outDetails.text_tokens || 0;
    const outAudio = outDetails.audio_tokens || 0;

    costTotalUSD +=
      (inText * rates.textInPer1M +
        inTextCached * rates.cachedTextInPer1M +
        inAudio * rates.audioInPer1M +
        inAudioCached * rates.cachedAudioInPer1M +
        outText * rates.textOutPer1M +
        outAudio * rates.audioOutPer1M) /
      1e6;
    costTextTokens += inText + inTextCached + outText;
    costAudioTokens += inAudio + inAudioCached + outAudio;

    costBadgeEl.hidden = false;
    renderCostBadge();
  });
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

// ---- mic test (self-serve diagnostics; button in the left rail) ----------

const micTest = createMicTest({
  getMicDeviceId: () => (settingsDoc && typeof settingsDoc.micDeviceId === 'string' ? settingsDoc.micDeviceId : null),
});
const micTestBtn = $('micTestBtn');
if (micTestBtn) micTestBtn.addEventListener('click', () => void micTest.open());

// ---- pending-change banner (persona/voice changed mid-session) -----------
//
// Owner 2026-07-18: a persona/voice quick-switch during a live session only
// takes effect at the NEXT mint, and the old transient toast was easy to
// miss. This persistent inline banner (templates/pages/conversation.html
// #pendingBanner, role=status) stays up until the session ends, a new
// conversation starts, or the user dismisses it — and carries its own
// "New conversation" action so the switch is one tap away.

const pendingBannerEl = $('pendingBanner');
const pendingBannerMsg = $('pendingBannerMsg');
const pendingBannerNew = $('pendingBannerNew');
const pendingBannerClose = $('pendingBannerClose');

function showPendingBanner(message) {
  if (!pendingBannerEl || !pendingBannerMsg) return;
  pendingBannerMsg.textContent = message;
  pendingBannerEl.hidden = false;
}

function hidePendingBanner() {
  if (!pendingBannerEl) return;
  pendingBannerEl.hidden = true;
  if (pendingBannerMsg) pendingBannerMsg.textContent = '';
}

if (pendingBannerClose) pendingBannerClose.addEventListener('click', hidePendingBanner);
if (pendingBannerNew) {
  pendingBannerNew.addEventListener('click', () => startNewConversation());
}

// ---- new conversation ----------------------------------------------------

function startNewConversation() {
  // End any live session (flushes the transcript sink with final:true so
  // the finished conversation lands in History), then present a clean
  // slate — the next mic tap mints a fresh session (which picks up any
  // pending persona/voice change, so the banner comes down too).
  mic.end();
  transcript.clear();
  toolActivityReset();
  resetCostBadge();
  hidePendingBanner();
  toast('New conversation — tap the mic when ready.');
}

const newConversationBtn = $('newConversationBtn');
if (newConversationBtn) {
  newConversationBtn.addEventListener('click', startNewConversation);
}

mic.addEventListener('sessioncreated', (e) => {
  const session = e.detail.session;
  attachTranscriptRendering(session);
  attachVisualizer(session);
  attachCostBadge(session);
  // The pending persona/voice change applies once this session is over —
  // whether it ended deliberately or dropped, the banner's job is done.
  session.addEventListener('closed', hidePendingBanner);
  session.addEventListener('connectionlost', hidePendingBanner);
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
        // Owner rule: never a bare "couldn't" — the underlying error goes in
        // the banner's Details so it's report-able.
        toast("Couldn't start hands-free listening — use the mic button.", {
          error: true,
          detail: (err && (err.message || String(err))) || 'unknown error',
        });
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

/** Render the tool calls the fallback turn executed server-side, using the
 * exact same card treatment as live-session tools (spec §2.3). Each entry is
 * the tool router's Result JSON ({tool, ok, output, error, ...}) — the same
 * shape the live dispatcher hands to the toolresult listener. */
function renderFallbackToolCalls(calls) {
  if (!Array.isArray(calls) || calls.length === 0 || !showToolCalls()) return;
  for (const call of calls) {
    const failed = !(call && call.ok);
    transcript.appendToolResultCard({
      icon: '🛠',
      title: toolTitle(call && call.tool),
      badge: failed ? 'Failed' : 'Done',
      badgeVariant: failed ? 'error' : 'teal',
      fields: failed
        ? [['Status', (call && call.error && call.error.message) || 'The tool call failed — the assistant was told.']]
        : toolFields(call),
    });
  }
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
    renderFallbackToolCalls(resp && resp.toolCalls);
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
      appliedToast: () => 'Persona updated.',
      appliedBanner: () =>
        isLive()
          ? `${personaLabelFor(next) || 'Live Ninja'} applies to your next conversation — tap New conversation to switch now.`
          : '',
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
      appliedToast: () => 'Voice updated.',
      appliedBanner: () =>
        isLive()
          ? `The ${voiceLabelFor(next)} voice applies to your next conversation — tap New conversation to switch now.`
          : '',
    });
  });
}

// ---- cross-tab settings delivery + mid-session application ---------------
//
// There is NO server-side settings fan-out to the web client (the web
// WebSocket/settings.updated frame does not exist — only the device shadow
// path has push), so this is the documented minimal channel: every
// successful settings PUT — here (quick-switches) and in settings.mjs
// (the /settings page autosave) — writes the new document version to
// localStorage under 'ln.settings.version'. The browser fires 'storage'
// in every OTHER same-origin tab (never the writer, so no self-loop);
// those tabs re-GET the canonical document and apply the delta:
//   - Mic pickup (micEagerness) / turn detection → applied to the LIVE
//     session via RealtimeSession.updateAudioInput (session.update,
//     mirroring internal/realtime/mint.go) — owner request 2026-07-18;
//   - persona/voice → mint-bound, so a live session gets the persistent
//     "applies to your next conversation" banner instead;
//   - appearance/privacy/quick-switch selects re-sync as on bootstrap.

const SETTINGS_PING_KEY = 'ln.settings.version';

function pingSettingsChanged() {
  try {
    localStorage.setItem(SETTINGS_PING_KEY, String(settingsVersion()));
  } catch {
    /* storage blocked (private mode) — cross-tab sync degrades gracefully */
  }
}

function personaIdOf(doc) {
  const p = doc && doc.persona;
  return (p && typeof p.presetId === 'string' && p.presetId) || 'default';
}

/** Apply what changed between the previous and freshly-fetched settings
 * docs to the current page/session (see the section comment above). */
function applySettingsDelta(prev, fresh) {
  const eagerness = (fresh && fresh.micEagerness) || 'auto';
  const audioChanged =
    ((prev && prev.micEagerness) || 'auto') !== eagerness ||
    ((prev && prev.turnDetection) || 'semantic_vad') !== ((fresh && fresh.turnDetection) || 'semantic_vad');
  if (audioChanged && isLive()) {
    // No-ops on nova-bridge / a closed datachannel (returns false) — the
    // change still lands at the next mint via the settings doc.
    if (mic.session.updateAudioInput({ eagerness })) {
      toast('Listening settings updated — applied to this conversation.');
    }
  }

  const personaChanged = personaIdOf(prev) !== personaIdOf(fresh);
  const voiceChanged = ((prev && prev.voice) || '') !== ((fresh && fresh.voice) || '');
  if ((personaChanged || voiceChanged) && isLive()) {
    showPendingBanner(
      personaChanged
        ? `${personaLabelFor(personaIdOf(fresh)) || 'Live Ninja'} applies to your next conversation — tap New conversation to switch now.`
        : `The ${voiceLabelFor(fresh.voice)} voice applies to your next conversation — tap New conversation to switch now.`,
    );
  }
}

let adoptInFlight = false;

async function adoptRemoteSettings() {
  if (adoptInFlight || !settingsDoc) return; // bootstrap still owns the doc
  adoptInFlight = true;
  try {
    const fresh = await apiJSON(SETTINGS_PATH);
    const prev = settingsDoc;
    settingsDoc = fresh;
    syncQuickSwitchesFromDoc();
    if (window.__lnApplyAppearance && settingsDoc.appearance) {
      window.__lnApplyAppearance(settingsDoc.appearance);
    }
    const privacy = settingsDoc.privacy;
    sink.setEnabled(!(privacy && privacy.storeTranscripts === false));
    applySettingsDelta(prev, fresh);
  } catch {
    /* offline or auth redirect — the next ping (or a reload) re-syncs */
  } finally {
    adoptInFlight = false;
  }
}

window.addEventListener('storage', (e) => {
  // Fires only in tabs that did NOT write the key. Ignore unrelated keys
  // and the removal that a localStorage.clear() produces.
  if (e.key !== SETTINGS_PING_KEY || e.newValue === null || e.newValue === e.oldValue) return;
  void adoptRemoteSettings();
});

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

  // Grouped persona library (Built-in / Mine / Shared). personaCatalog
  // stays the flattened list so personaLabelFor keeps working; the
  // "custom" persona remains a client-side concept (free-text
  // instructions, spec §3.3) appended by fillPersonaSelect.
  let personaGroups = null;
  if (personas.status === 'fulfilled' && personas.value && typeof personas.value === 'object') {
    const v = personas.value;
    personaGroups = {
      builtin: Array.isArray(v.builtin) ? v.builtin : [],
      mine: Array.isArray(v.mine) ? v.mine : [],
      shared: Array.isArray(v.shared) ? v.shared : [],
    };
    personaCatalog = personaGroups.builtin.concat(personaGroups.mine, personaGroups.shared);
  }
  fillPersonaSelect(personaSelect, personaGroups, currentPersonaId());

  if (voices.status === 'fulfilled' && Array.isArray(voices.value.voices)) {
    fillSelect(voiceSelect, voices.value.voices, (settingsDoc && settingsDoc.voice) || 'cedar');
  }

  if (catalog.status === 'fulfilled' && catalog.value && Array.isArray(catalog.value.wakewords)) {
    wakeCatalog = catalog.value;
  }

  transcript.setPersonaLabel(personaLabelFor(currentPersonaId()));

  const privacy = settingsDoc.privacy;
  sink.setEnabled(!(privacy && privacy.storeTranscripts === false));

  // Apply + cache the synced appearance (theme.js reads the cache pre-paint
  // on every page; this keeps other devices/pages in step with settings).
  if (window.__lnApplyAppearance && settingsDoc.appearance) {
    window.__lnApplyAppearance(settingsDoc.appearance);
  }

  renderWakeUI();
  // Hands-free restored ON from a previous visit (mic.mjs reads
  // localStorage in its constructor): bring the engine up now.
  if (mic.handsFree) void setWakeListening(true);
}

void bootstrap();
