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
//   settings.mjs       — settings-panel controller for the docked drawer
//     (owner 2026-07-19: the standalone /settings page is gone; its own
//     optimistic-concurrency section-PATCH loop is independent of this file's
//     settingsDoc/putSettings below — see settings.mjs's header comment)
//
// This file only wires them to the page DOM (ids from
// templates/pages/conversation.html) and to the settings document:
//   - persona quick-switch select populated from GET /api/v1/personas
//     (never blind text/options); the voice quick-switch is GONE (owner
//     shell redesign 2026-07-18 v2: voice is persona-embedded) — the
//     voices catalog is still fetched for human-readable banner labels;
//   - persona Edit button → openPersonaEditor(personaId) seam
//     (personaeditor.mjs, lazily imported; Author B owns that module);
//   - mic sensitivity chips (Low/Medium/High) ↔ settings micEagerness,
//     live-applied via session.updateAudioInput;
//   - docked settings drawer (native <dialog>: focus trap + Escape free),
//     now full-screen and hosting the settings panel (settings.mjs) inline;
//   - optimistic current-device section PATCH with the §3.6 409 retry-once rule;
//   - composer → live session sendUserText, or POST /api/v1/fallback/turn
//     when no session is connected (spec §2.5 "you can still type below");
//   - wake-word toggle → WakeWordEngine lifecycle → mic.notifyWake().

import { apiJSON, authFetch, ApiError } from './toolclient.mjs';
import { prefetchSession } from './realtime.mjs';
import { MicController, MicState } from './mic.mjs';
import { createMicTest } from './mictest.mjs';
import { Transcript } from './transcript.mjs';
import { createTranscriptSink } from './transcriptsink.mjs';
import { Visualizer } from './visualizer.mjs';
import {
  applyWakeWordSettings,
  createWakeWordEngine,
  isWakeWordSupported,
} from './wakeword.mjs';
import { initSettingsPanel } from './settings.mjs';
import { initSettingsAccordion } from './settings-accordion.mjs';
import {
  applySectionSettings,
  ensureCurrentDeviceRegistered,
  sectionSettings,
  withSettingsOperation,
} from './device-settings.mjs';
import { createDeferredDeviceActionGate } from './deviceactions.mjs';
import { startLiveEvents, presenceStateFor } from './liveevents.mjs';
import { openToolDetails } from './tooldetails.mjs';

const SETTINGS_PATH = '/api/v1/settings?effective=true';
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

let settingsDoc = null; // current device's effective document, including `version`
let wakeCatalog = null; // {wakewords:[{id, phrase, ...}]}

function settingsVersion() {
  return Number(settingsDoc && settingsDoc.version) || 1;
}

/**
 * Optimistic section PATCH with the spec §3.6 conflict rule: on 409 re-GET, re-apply
 * the same mutation on the fresh document, retry once; a second 409 means
 * remote wins — adopt it and tell the caller so the UI re-syncs.
 * @returns {Promise<{ok: boolean, conflict?: boolean}>}
 */
async function putSettings(section, mutate) {
  return withSettingsOperation(async () => {
    const attempt = async () => {
      const body = structuredClone(settingsDoc);
      mutate(body);
      const resp = await apiJSON(
        `/api/v1/settings/sections/${encodeURIComponent(section)}`,
        {
          method: 'PATCH',
          json: {
            version: settingsVersion(),
            operation: 'set',
            target: { mode: 'current', deviceIds: [] },
            settings: sectionSettings(body, section),
          },
        },
      );
      const responseVersion = Number(resp.version) || 0;
      if (responseVersion >= settingsVersion()) {
        const current = (resp.devices || []).find(
          (device) => device.isCurrent || device.deviceId === resp.currentDeviceId,
        );
        applySectionSettings(
          settingsDoc,
          section,
          current?.settings || sectionSettings(body, section),
        );
        settingsDoc.version = responseVersion || settingsVersion();
      }
      pingSettingsChanged(); // cross-tab channel (see the storage section below)
    };

    const adoptFreshDocument = (fresh) => {
      if (
        !settingsDoc
        || (Number(fresh?.version) || 0) >= settingsVersion()
      ) {
        settingsDoc = fresh;
      }
    };

    try {
      await attempt();
      return { ok: true };
    } catch (err) {
      if (!(err instanceof ApiError) || err.code !== 'version_conflict') throw err;
    }
    // Conflict: refresh, retry once with the same mutation on top.
    adoptFreshDocument(await apiJSON(SETTINGS_PATH));
    try {
      await attempt();
      return { ok: true };
    } catch (err) {
      if (err instanceof ApiError && err.code === 'version_conflict') {
        adoptFreshDocument(await apiJSON(SETTINGS_PATH));
        return { ok: false, conflict: true };
      }
      throw err;
    }
  });
}

// ---- persona quick-switch select (populated from the real catalog) -------
//
// The voice quick-switch select is gone (owner shell redesign 2026-07-18 v2:
// voice is embedded in the persona, edited via the persona editor). The
// voices catalog is still fetched so banner copy can show a human-readable
// voice name — never a raw id.

const personaSelect = $('personaSelect');
const personaGroupLabel = $('personaGroupLabel');

let voiceCatalog = []; // [{id, name, ...}] from GET /api/v1/realtime/voices

// Built-in persona picker sections, in render order. Mirrors
// realtime.GroupOrder (internal/realtime/personas.go) — the server tags each
// built-in with its group and this list decides where the sections sit.
const PERSONA_GROUP_ORDER = ['General', 'PDLC', 'ESP32', 'Fun'];

/* Persona ids switched off in Settings (persona.hidden). Presentation only —
 * ResolvePersona on the server never reads it, so a hidden persona still
 * mints a working session if a stored document still names it. */
function hiddenPersonaSet() {
  const raw = settingsDoc && settingsDoc.persona && settingsDoc.persona.hidden;
  return new Set(Array.isArray(raw) ? raw.filter((v) => typeof v === 'string') : []);
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
  // Built-ins are split into their picker sections (owner 2026-08-01):
  // General / PDLC / ESP32 / Fun, in that order, each rendered as its own
  // <optgroup> so the group name is visible while the list is open. The
  // order is fixed here rather than taken from the response so a new group
  // cannot reorder the picker on its own; anything carrying an unrecognised
  // group still renders, in a trailing section named after it, because a
  // persona that exists on the server and is invisible in the picker is the
  // one failure mode worth ruling out.
  // Personas switched off in Settings (persona.hidden) are dropped here —
  // this picker is the only thing that list affects. Two entries always
  // survive it regardless: the persona currently selected (otherwise the
  // select would show a value it does not contain, and switching away from a
  // persona you just hid would be impossible) and "default" (which the
  // settings route refuses to hide at all, so the picker can never empty).
  const hidden = hiddenPersonaSet();
  const visible = (row) =>
    row.id === selectedId || row.id === 'default' || !hidden.has(row.id);
  const builtins = ((groups && groups.builtin) || []).filter(visible);
  const seen = new Set();
  for (const label of PERSONA_GROUP_ORDER) {
    addGroup(label, builtins.filter((r) => (r.group || 'General') === label));
    seen.add(label);
  }
  for (const row of builtins) {
    const label = row.group || 'General';
    if (seen.has(label)) continue;
    seen.add(label);
    addGroup(label, builtins.filter((r) => (r.group || 'General') === label));
  }
  addGroup('Mine', ((groups && groups.mine) || []).filter(visible));
  addGroup('Shared', ((groups && groups.shared) || []).filter(visible));
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

/** Human-readable voice name from the fetched voices catalog — never show
 * a raw id in banner copy. */
function voiceLabelFor(voiceId) {
  const row = voiceCatalog.find((v) => v.id === voiceId);
  return (row && row.name) || voiceId;
}

/* Group · Persona caption under the picker (owner 2026-08-01: "show the
 * group and persona"). A collapsed <select> shows only the option text, so
 * the <optgroup> heading the user picked from disappears the moment the list
 * closes; this line is where it survives. Blank for anything without a group
 * (own/shared personas and "custom"), rather than inventing one. */
function updatePersonaGroupLabel() {
  if (!personaGroupLabel) return;
  const id = currentPersonaId();
  const row = personaCatalog.find((p) => p.id === id);
  const group = row && row.group;
  personaGroupLabel.textContent = group ? `${group} · ${personaLabelFor(id)}` : '';
  personaGroupLabel.hidden = !group;
}

function syncQuickSwitchesFromDoc() {
  if (personaSelect) personaSelect.value = currentPersonaId();
  syncMicChips();
  updatePersonaGroupLabel();
  transcript.setPersonaLabel(personaLabelFor(currentPersonaId()));
  // The persona is half of what the other devices show for this one, so any
  // path that changes it — including a settings adoption from another device —
  // has to put the new label on the wire.
  liveEvents.publishPresence();
}

function isLive() {
  return !!(mic.session && mic.session.isConnected);
}

async function saveQuickSwitch({ section, mutate, revert, appliedToast, appliedBanner }) {
  try {
    const res = await putSettings(section, mutate);
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

// Set by attachTranscriptRendering for the newest session; invoked by the
// transcript sink's onBeforeFinal hook (see the sink construction below)
// to sweep rendered-but-unfinalized turns into the final batch.
let drainLiveTurnsToSink = null;

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

  // ---- transcript sink capture (exactly-once, all arrival paths) ----
  //
  // This rendering layer is the ONLY feeder of sink.addTurn (the sink
  // stopped listening to 'userfinal'/'assistantfinal' itself): it is the
  // one place that sees every path a turn can arrive by — streamed deltas,
  // the late authoritative final, a final with no deltas — so what gets
  // SAVED is exactly what got RENDERED. Per-item text accumulates from the
  // delta events (e.detail.text is the full running text), the final
  // captures once (guarded by capturedItems), and drainToSink() sweeps
  // anything still un-finalized into the sink right before the final
  // flush (registered via the sink's onBeforeFinal — covers the prod loss
  // where the user's transcription final never landed before End/pagehide
  // killed the page, leaving the turn rendered but never saved).
  const liveTextByItem = new Map(); // itemId -> {role, text} still awaiting its final
  const capturedItems = new Set(); // itemIds already handed to the sink
  let sinkAnchor = null; // sink handle of the first assistant turn queued ahead of a pending user turn

  const captureToSink = (role, itemId, text) => {
    if (capturedItems.has(itemId)) return;
    capturedItems.add(itemId);
    liveTextByItem.delete(itemId);
    if (role === 'user') {
      // A late user final lands BEFORE the assistant reply it prompted,
      // mirroring the UI's anchor insert (transcriptsink honors `before`
      // only while the anchor batch hasn't been attempted).
      sink.addTurn('user', text, { before: sinkAnchor || undefined });
      sinkAnchor = null;
    } else {
      const handle = sink.addTurn('assistant', text);
      // An un-transcribed/un-captured user utterance is still ahead of us —
      // remember this assistant entry so that user turn can slot in above.
      const userStillPending =
        userSpeechPending || [...liveTextByItem.values()].some((v) => v.role === 'user');
      if (handle && userStillPending && !sinkAnchor) sinkAnchor = handle;
    }
  };

  const drainToSink = () => {
    // Users first so a drained user turn can still use the anchor slot.
    for (const [itemId, v] of [...liveTextByItem]) {
      if (v.role === 'user') captureToSink('user', itemId, v.text);
    }
    for (const [itemId, v] of [...liveTextByItem]) {
      captureToSink(v.role, itemId, v.text);
    }
  };
  drainLiveTurnsToSink = drainToSink; // sink.onBeforeFinal calls the newest session's drain

  const beginOrAppend = (role, e) => {
    const { itemId, delta } = e.detail;
    if (!capturedItems.has(itemId)) {
      liveTextByItem.set(itemId, { role, text: e.detail.text || '' });
    }
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
    // Save the turn: the final text is authoritative; fall back to the
    // accumulated delta text when the final arrived empty.
    const live = liveTextByItem.get(itemId);
    captureToSink(role, itemId, text || (live ? live.text : ''));
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
    // Same for the sink's anchor: a later utterance must never insert
    // itself above a previous exchange's assistant reply.
    userSpeechPending = true;
    anchorTurnId = null;
    sinkAnchor = null;
  });
  session.addEventListener('usertranscriptfailed', (e) => {
    // Transcription failed server-side, but any streamed deltas are still
    // on screen — save what was rendered (empty accumulations are dropped
    // by the sink itself).
    const itemId = e.detail && e.detail.itemId;
    if (itemId) {
      const live = liveTextByItem.get(itemId);
      if (live) captureToSink('user', itemId, live.text);
    }
    userTurnPlaced();
  });
  session.addEventListener('thinking', () => transcript.showTypingIndicator());
  session.addEventListener('responsedone', () => {
    transcript.hideTypingIndicator();
    // If this device won the turn-taking lock to announce a change, its turn
    // is over — free the lock before anything else, so a device that deferred
    // can win the next one instead of waiting out the 30-second expiry.
    liveEvents.releaseSpeakingTurn();
    // A cross-device change that arrived mid-turn was held rather than spoken
    // over the assistant; this is the moment it is safe to deliver.
    flushPendingNudge();
  });
  session.addEventListener('bargein', () => transcript.hideTypingIndicator());
  session.addEventListener('connectionlost', () => transcript.hideTypingIndicator());
  session.addEventListener('closed', () => transcript.hideTypingIndicator());

  // Every tool event is buffered UNCONDITIONALLY (bufferToolCall/Result/
  // Error below) regardless of the "Show tool calls" toggle, so turning it
  // on mid-conversation can retroactively reveal cards for calls that
  // already finished while it was off (replayBufferedToolCalls) — the
  // toggle only gates whether a card renders NOW, never whether the call
  // is remembered.
  session.addEventListener('toolcall', (e) => {
    toolActivityStart();
    bufferToolCall(e.detail);
  });
  session.addEventListener('toolresult', (e) => {
    toolActivityEnd();
    const entry = bufferToolResult(e.detail);
    if (!showToolCalls()) return;
    renderToolCard(entry);
  });
  session.addEventListener('toolerror', (e) => {
    toolActivityEnd();
    const entry = bufferToolError(e.detail);
    // M17 phase 2: best-effort breadcrumb for failures that may have happened
    // before /tools/invoke reached the registry. The server re-derives the
    // identity; failure to report diagnostics is intentionally silent.
    const detail = e.detail || {};
    void authFetch('/api/v1/rca/client-event', {
      method: 'POST',
      json: {
        tool: detail.tool || '',
        callId: detail.callId || '',
        sessionId: session.sessionId || '',
        engine: session.engine || '',
        args: detail.args || {},
        error: detail.error || { code: 'client_tool_error', message: 'The tool call failed.' },
      },
    }).catch(() => {});
    if (!showToolCalls()) return;
    renderToolCard(entry);
  });
  const deferredDeviceAction = createDeferredDeviceActionGate((action) => {
    if (action === 'stop_listening') {
      if (wakeToggle) {
        wakeToggle.checked = false;
        wakeToggle.dispatchEvent(new Event('change'));
      }
      mic.end();
    } else if (action === 'start_new_conversation') {
      session.addEventListener('closed', () => void mic.start(), { once: true });
      startNewConversation();
    }
  });
  session.addEventListener('devicetool', (e) => {
    deferredDeviceAction.queue((e.detail && e.detail.tool) || '');
  });
  session.addEventListener('responsedone', () => deferredDeviceAction.responseDone());
  session.addEventListener('closed', () => {
    deferredDeviceAction.clear();
    toolActivityReset();
  });
  session.addEventListener('connectionlost', () => {
    deferredDeviceAction.clear();
    toolActivityReset();
  });
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
    // Retroactive reveal: cards for calls that already finished while the
    // toggle was off never got rendered — replay them now, in order.
    if (showToolsToggle.checked) replayBufferedToolCalls();
  });
}

const toolActivityEl = $('toolActivity');
let toolsInFlight = 0;
let toolActivityLinger = 0;

function toolActivityStart() {
  toolsInFlight++;
  clearTimeout(toolActivityLinger);
  if (toolActivityEl) toolActivityEl.hidden = false;
  if (orbEl) orbEl.classList.add('ln-orb--toolcall');
}

function toolActivityEnd() {
  if (toolsInFlight > 0) toolsInFlight--;
  if (toolsInFlight === 0 && toolActivityEl) {
    // Brief linger so even instant tools visibly flash the badge.
    clearTimeout(toolActivityLinger);
    toolActivityLinger = setTimeout(() => {
      toolActivityEl.hidden = true;
      if (orbEl) orbEl.classList.remove('ln-orb--toolcall');
    }, 800);
  }
}

function toolActivityReset() {
  toolsInFlight = 0;
  clearTimeout(toolActivityLinger);
  if (toolActivityEl) toolActivityEl.hidden = true;
  if (orbEl) orbEl.classList.remove('ln-orb--toolcall');
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

/** args may arrive as an already-parsed object (fallback path never has
 * args at all — tools.Result carries no Args field server-side) or a JSON
 * string (realtime tool-call args, history's stored audit line) — parsed
 * defensively, falling back to the raw string when it isn't valid JSON. */
function parseArgsLoose(args) {
  if (typeof args !== 'string') return args;
  const trimmed = args.trim();
  if (!trimmed) return undefined;
  try {
    return JSON.parse(trimmed);
  } catch {
    return args;
  }
}

/** Flatten tool-call INPUT into [label, value] rows, mirroring toolFields()
 * but prefixed "In · " so input and output rows read as one group inside
 * the same <dl class="kv"> without a second heading element. */
function toolInputFields(args) {
  const parsed = parseArgsLoose(args);
  if (parsed === undefined || parsed === null) return [];
  if (typeof parsed !== 'object') return [['Input', String(parsed)]];
  const rows = [];
  for (const [key, value] of Object.entries(parsed)) {
    if (rows.length >= 8) break;
    let text;
    if (value === null || value === undefined) text = '—';
    else if (typeof value === 'object') {
      text = Array.isArray(value) ? `${value.length} item${value.length === 1 ? '' : 's'}` : '(details)';
    } else text = String(value);
    rows.push([`In · ${toolTitle(key)}`, text]);
  }
  return rows;
}

/** POST /api/v1/tools/invoke's response (and each fallback-turn executed
 * call) IS the whole tools.Result envelope ({tool, callId, ok, output,
 * error, ...}, internal/tools/registry.go) — a card/Details "Output" means
 * the TOOL's own output, so unwrap `.output` when present rather than
 * flattening envelope metadata (tool/callId/ok/duplicate) as if it were
 * the result. */
function unwrapToolOutput(result) {
  if (result && typeof result === 'object' && !Array.isArray(result) && 'output' in result) {
    return result.output;
  }
  return result;
}

function toolErrorMessage(error) {
  if (!error) return '';
  if (typeof error === 'string') return error;
  return (error && error.message) || '';
}

// ---- tool-call buffer (Feature: retroactive "Show tool calls" reveal) ----
//
// Every toolcall/toolresult/toolerror — live AND fallback-turn — is
// buffered here regardless of the showToolCalls() gate, keyed by callId so
// the call and its eventual result/error correlate into one entry. This is
// what lets flipping "Show tool calls" ON mid-conversation retroactively
// render cards for calls that already completed while it was off
// (replayBufferedToolCalls), instead of only affecting calls from that
// point forward. Bounded so a very long/tool-heavy conversation can't grow
// this unboundedly; only startNewConversation() clears it — a dropped
// connection ('closed'/'connectionlost') must NOT, since the transcript
// (and its buffered tool history) stays on screen through a reconnect.

const TOOL_BUFFER_MAX = 200;
const toolCallBuffer = []; // ordered [{tool, callId, args, result, error, failed, done, rendered, ts}]
const toolEntryByCallId = new Map();
let fallbackCallSeq = 0;

function trimToolBuffer() {
  while (toolCallBuffer.length > TOOL_BUFFER_MAX) {
    const dropped = toolCallBuffer.shift();
    if (dropped) toolEntryByCallId.delete(dropped.callId);
  }
}

function clearToolBuffer() {
  toolCallBuffer.length = 0;
  toolEntryByCallId.clear();
}

function bufferToolCall({ tool, callId, args }) {
  const entry = {
    tool,
    callId,
    args,
    result: undefined,
    error: undefined,
    failed: false,
    done: false,
    rendered: false,
    ts: Date.now(),
  };
  if (callId) toolEntryByCallId.set(callId, entry);
  toolCallBuffer.push(entry);
  trimToolBuffer();
  return entry;
}

function bufferToolResult({ tool, callId, result }) {
  const entry = (callId && toolEntryByCallId.get(callId)) || bufferToolCall({ tool, callId, args: undefined });
  entry.result = result;
  entry.done = true;
  entry.failed = false;
  return entry;
}

function bufferToolError({ tool, callId, error }) {
  const entry = (callId && toolEntryByCallId.get(callId)) || bufferToolCall({ tool, callId, args: undefined });
  entry.error = error;
  entry.done = true;
  entry.failed = true;
  return entry;
}

/** Fallback-turn calls arrive already complete (no separate call/result
 * events) — buffer the whole executed entry in one shot. `call` is one
 * tools.Result-shaped object; it never carries the original arguments
 * (server-side tools.Result has no Args field), so `args` stays undefined
 * and the Details popup shows "(no input recorded)" for these rather than
 * inventing data. */
function bufferFallbackCall(call) {
  const failed = !(call && call.ok);
  const callId = (call && call.callId) || `fallback-${Date.now().toString(36)}-${++fallbackCallSeq}`;
  const entry = {
    tool: call && call.tool,
    callId,
    args: undefined,
    result: failed ? undefined : call,
    error: failed ? call && call.error : undefined,
    failed,
    done: true,
    rendered: false,
    ts: Date.now(),
  };
  toolEntryByCallId.set(callId, entry);
  toolCallBuffer.push(entry);
  trimToolBuffer();
  return entry;
}

/** Renders one buffered tool-call entry as a transcript card. Shared by
 * the live toolresult/toolerror listeners, the fallback-turn path, and the
 * toggle's retroactive replay — so all three produce byte-identical cards.
 * Idempotent (checks/sets entry.rendered) so replay can never double-render
 * a card that already went up live. */
function renderToolCard(entry) {
  if (!entry || entry.rendered) return;
  entry.rendered = true;
  const failed = !!entry.failed;
  transcript.appendToolResultCard({
    icon: '🛠',
    title: toolTitle(entry.tool),
    badge: failed ? 'Failed' : 'Done',
    badgeVariant: failed ? 'error' : 'teal',
    fields: failed
      ? [['Status', toolErrorMessage(entry.error) || 'The tool call failed — the assistant was told.']]
      : [...toolInputFields(entry.args), ...toolFields(unwrapToolOutput(entry.result))],
    onDetails: () =>
      openToolDetails({
        tool: entry.tool,
        callId: entry.callId,
        args: entry.args,
        result: failed ? undefined : unwrapToolOutput(entry.result),
        error: failed ? entry.error : undefined,
        ts: entry.ts,
      }),
  });
}

/** Toggle turned ON: surface cards for every buffered call that finished
 * while it was off, in the order they happened — never just future ones. */
function replayBufferedToolCalls() {
  for (const entry of toolCallBuffer) {
    if (entry.done && !entry.rendered) renderToolCard(entry);
  }
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
  if (orbEl) {
    orbEl.classList.toggle('ln-orb--idle', !state.startsWith('live-'));
    // Rings pulse only while the assistant is actually talking; the core
    // spins faster while "thinking" (owner spec 2026-07-19). ln-orb--toolcall
    // is driven separately by toolActivityStart/End below.
    orbEl.classList.toggle('ln-orb--listening', state === MicState.LISTENING);
    orbEl.classList.toggle('ln-orb--thinking', state === MicState.THINKING);
    orbEl.classList.toggle('ln-orb--speaking', state === MicState.SPEAKING);
  }
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

// Per-session cost, keyed by sessionId. The badge above deliberately
// accumulates across reconnects; History persists each SESSION's own share
// (the transcript sink ships it on the final flush — see getSessionCost in
// the sink wiring below), so the split is tracked here separately.
const sessionCosts = new Map();

function trackSessionCost(sid) {
  if (!sid) return null;
  const entry = { usd: 0, textTokens: 0, audioTokens: 0 };
  sessionCosts.set(sid, entry);
  // A page lives through a handful of sessions at most — keep the map tiny.
  while (sessionCosts.size > 8) {
    sessionCosts.delete(sessionCosts.keys().next().value);
  }
  return entry;
}

// Most recent session id seen on this page. Used by "Tag for review" below
// to say WHICH conversation a note is about; it deliberately survives the
// session ending, because the natural moment to tag a conversation is right
// after it goes wrong, when the session is already closed.
let lastSessionId = '';

function attachCostBadge(session) {
  let sessCost = null;
  session.addEventListener('sessionready', (e) => {
    const sid = (e.detail && e.detail.sessionId) || '';
    if (sid) lastSessionId = sid;
    sessCost = trackSessionCost(sid);
    if (!costBadgeEl) return;
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

    const deltaUSD =
      (inText * rates.textInPer1M +
        inTextCached * rates.cachedTextInPer1M +
        inAudio * rates.audioInPer1M +
        inAudioCached * rates.cachedAudioInPer1M +
        outText * rates.textOutPer1M +
        outAudio * rates.audioOutPer1M) /
      1e6;
    const deltaText = inText + inTextCached + outText;
    const deltaAudio = inAudio + inAudioCached + outAudio;
    costTotalUSD += deltaUSD;
    costTextTokens += deltaText;
    costAudioTokens += deltaAudio;
    if (sessCost) {
      sessCost.usd += deltaUSD;
      sessCost.textTokens += deltaText;
      sessCost.audioTokens += deltaAudio;
    }

    if (!costBadgeEl) return;
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
  // Post-reply session lifetime (0 = keep listening until the user or the
  // provider ends the session — the default; owner decision 2026-07-19).
  getKeepListeningSeconds: () => {
    const v = settingsDoc && Number(settingsDoc.keepListeningSeconds);
    return Number.isFinite(v) && v >= 0 ? v : 0;
  },
});

// onBeforeFinal: right before the sink sends a session's one-and-only
// final flush, drain any rendered-but-never-finalized turns (set per
// session by attachTranscriptRendering) so they make the final batch.
const sink = createTranscriptSink({
  onBeforeFinal: () => {
    if (typeof drainLiveTurnsToSink === 'function') drainLiveTurnsToSink();
  },
  // Per-session cost estimate (attachCostBadge tracks it) — persisted on
  // the conversation's history record by the final flush.
  getSessionCost: (sid) => sessionCosts.get(sid) || null,
});
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
  // A dropped/reconnected session must NOT clear this (the transcript and
  // its tool history stay on screen) — only an explicit new conversation.
  clearToolBuffer();
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
// Second listener rather than a second job for the one above: the other
// devices' roster is only as fresh as this republish, and a visual sync that
// starts throwing must not take presence down with it.
mic.addEventListener('statechange', () => liveEvents.publishPresence());
mic.addEventListener('error', (e) =>
  toast(e.detail.message, { error: true, txId: e.detail.txId, detail: e.detail.detail }),
);
mic.addEventListener('toast', (e) => toast(e.detail.message));

// ---- wake word (hands-free mode) -----------------------------------------

const wakeToggle = $('wakeToggle');
const wakeToggleLabel = $('wakeToggleLabel');
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
  if (wakeToggleLabel) wakeToggleLabel.textContent = `Always listening: ${on ? 'On' : 'Off'}`;
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

function handleWakeEngineFailure(err) {
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
      handleWakeEngineFailure(err);
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
  wakeToggle.addEventListener('change', () => {
    // Intent prefetch (latency plan #4.2): the user arming hands-free means
    // a wake — and a session start — is likely soon, so warm the mint now
    // and the wake→"Listening" path skips the 0.7-1.2s bootstrap.
    // Deliberately wired to the user's toggle GESTURE, not inside
    // setWakeListening(): the page-load restore path (bootstrap() below)
    // must never mint speculatively — an unused mint burns a broker
    // concurrency slot + rate token for its ~60s server TTL, and there is
    // no release call (see prefetchSession in realtime.mjs).
    if (wakeToggle.checked) prefetchSession();
    void setWakeListening(wakeToggle.checked);
  });
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
 * shape the live dispatcher hands to the toolresult listener. Buffered
 * unconditionally (same rule as the live path) so toggling "Show tool
 * calls" on later still reveals these retroactively. */
function renderFallbackToolCalls(calls) {
  if (!Array.isArray(calls) || calls.length === 0) return;
  for (const call of calls) {
    const entry = bufferFallbackCall(call);
    if (showToolCalls()) renderToolCard(entry);
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

// ---- cross-device change notifications (§6 WS-3 M3.3/M3.4) ----------------
// Another device changed a shared document, memory entity or plan. The
// assistant says so unprompted (owner decision: auto-nudge). Three guards,
// because an unprompted voice is the most intrusive thing this app can do:
//
//   - never while the assistant is mid-turn (thinking or speaking) — cutting
//     into its own sentence is worse than saying it a moment later;
//   - never when no session is live — with nothing to speak through, the
//     change surfaces as a quiet toast instead;
//   - never for this device's own edits, which liveevents.mjs already filters
//     using the actor id the server told it to expect.
//
// Since §6 WS-5 there is a fourth: when several devices are live, only ONE of
// them says it out loud. The others hold what they heard and fold it into the
// end of a turn the user starts (§5 defer-and-merge), so three signed-in
// surfaces do not answer one edit three times.
//
// What is held is a QUEUE, not a slot. A single slot silently dropped the
// first change when a second arrived — invisible with two devices, routine
// with three.
const NUDGE_QUEUE_MAX = 5;
/** A held change that is never spoken must still arrive. This is the deadline. */
const NUDGE_QUIET_MS = 60_000;
let pendingNudges = [];
let nudgeQuietTimer = null;

function describeChange(ev) {
  const who = ev.actorPersona ? `The ${ev.actorPersona}` : 'Another device';
  return `${who} just ${ev.summary || 'changed something shared'}`;
}

/**
 * Collapses what has been held into the changes actually worth naming: one
 * entry per thing, latest wins (a file edited three times is one mention, not
 * three), and no more than three named.
 */
function mergeNudges(evs) {
  const byKey = new Map();
  for (const ev of evs) byKey.set(`${ev.type || 'change'}:${ev.id || ev.summary || ''}`, ev);
  return Array.from(byKey.values());
}

function nudgeText(evs) {
  const subjects = mergeNudges(evs);
  const named = subjects.slice(0, 3);
  const extra = subjects.length - named.length;
  const one = subjects.length === 1;
  const sentences = named.map((ev) => `${describeChange(ev)}.`).join(' ') +
    (extra > 0 ? ` There are ${extra} other change${extra === 1 ? '' : 's'} as well.` : '');
  // Phrased as a system aside rather than as the user talking, and it asks for
  // one sentence: this arrives mid-conversation and should not derail it.
  return `[Automatic update] ${sentences} Mention ${one ? 'this' : 'these'} to the user in one short ` +
    `sentence${one ? '' : ' covering all of them'}, then carry on with what you were doing. Re-read the ` +
    `shared file or memory before saying anything about its contents.`;
}

function quietNudgeText(evs) {
  const subjects = mergeNudges(evs);
  const first = `${subjects[0].actorPersona || 'Another device'} ${subjects[0].summary || 'changed something shared'}`;
  const extra = subjects.length - 1;
  return extra > 0
    ? `${first}, and ${extra} other change${extra === 1 ? '' : 's'}.`
    : `${first}.`;
}

function holdNudge(ev) {
  pendingNudges.push(ev);
  // Drop-oldest: if the user has been away long enough to stack six changes,
  // the newest five are the ones still worth saying.
  while (pendingNudges.length > NUDGE_QUEUE_MAX) pendingNudges.shift();
  if (!nudgeQuietTimer) {
    nudgeQuietTimer = setTimeout(() => {
      nudgeQuietTimer = null;
      const held = takePendingNudges();
      // No voice on this path: the user stopped talking to this device, so the
      // change surfaces the quiet way rather than being lost.
      if (held.length) toast(quietNudgeText(held));
    }, NUDGE_QUIET_MS);
  }
}

function takePendingNudges() {
  const held = pendingNudges;
  pendingNudges = [];
  clearTimeout(nudgeQuietTimer);
  nudgeQuietTimer = null;
  return held;
}

/**
 * The one path on which this device speaks without being asked.
 *
 * Everything above it is a guard; this is where the turn-taking lock is
 * claimed. Nothing may reach sendUserText unprompted without winning it, or
 * every live device answers the same edit.
 */
async function speakUnprompted(evs) {
  if (!evs.length) return;
  const won = await liveEvents.claimSpeakingTurn();
  if (!won) {
    // Another device is saying it. Hold ours and merge it into the next turn
    // the user starts here — no release to publish, this device never held it.
    for (const ev of evs) holdNudge(ev);
    return;
  }
  // The settle window is 400ms of real time, and the assistant may have
  // started a turn inside it.
  if (!isLive() || mic.state === MicState.THINKING || mic.state === MicState.SPEAKING) {
    liveEvents.releaseSpeakingTurn();
    for (const ev of evs) holdNudge(ev);
    return;
  }
  try {
    mic.session.sendUserText(nudgeText(evs));
  } catch {
    // Datachannel raced closed — a missed notification is not worth surfacing,
    // but the lock must not be left held over a turn that never happened.
    liveEvents.releaseSpeakingTurn();
    for (const ev of evs) holdNudge(ev);
  }
}

function deliverNudge(ev) {
  if (!isLive()) {
    toast(`${ev.actorPersona || 'Another device'} ${ev.summary || 'changed something shared'}.`);
    return;
  }
  // Mid-turn: hold it until the assistant finishes rather than talking over it.
  const state = mic.state;
  if (state === MicState.THINKING || state === MicState.SPEAKING) {
    holdNudge(ev);
    return;
  }
  void speakUnprompted([ev]);
}

function flushPendingNudge() {
  const held = takePendingNudges();
  if (!held.length) return;
  void speakUnprompted(held);
}

// ---- who else is live (§6 WS-5 M5.1) --------------------------------------
//
// Built here rather than in the template because it has no meaningful
// server-rendered form: every row of it comes from a retained MQTT message.
// It rides in the rail's status stack, under the state pill and cost badge —
// the bottom bar is an overflow-x:auto row of flex:1 items, and a fifth one
// pushes the existing labels into horizontal scroll.
//
// aria-live is OFF on purpose: this changes on every state transition of every
// other device, which would flood a screen reader. #statePill already
// announces THIS device politely, which is the part the user is acting on.
const PRESENCE_STATE_LABELS = {
  idle: 'Idle',
  connecting: 'Connecting…',
  listening: 'Listening',
  thinking: 'Thinking',
  speaking: 'Speaking',
};

const peersEl = (() => {
  const host = document.querySelector('.conv-rail__status');
  if (!host) return null;
  const el = document.createElement('div');
  el.className = 'conv-peers';
  el.id = 'devicePresence';
  el.setAttribute('role', 'status');
  el.setAttribute('aria-live', 'off');
  el.hidden = true;
  host.appendChild(el);
  return el;
})();

function renderPeers(peers) {
  if (!peersEl) return;
  const self = liveEvents.deviceId();
  const rows = [];
  for (const [deviceId, info] of peers) {
    if (deviceId === self) continue; // this device is the state pill above
    rows.push({
      // An empty persona is the plain "Live Ninja" label by design
      // (personaLabelFor returns '' for the default preset) — substituted here
      // rather than on the wire, so the payload stays a faithful echo.
      persona: (info && info.persona) || 'Live Ninja',
      state: (info && info.state) || 'idle',
    });
  }
  peersEl.textContent = '';
  peersEl.hidden = rows.length === 0;
  for (const row of rows) {
    const el = document.createElement('span');
    el.className = 'conv-peer';
    el.dataset.state = row.state;
    const dot = document.createElement('span');
    dot.className = 'conv-peer__dot';
    dot.setAttribute('aria-hidden', 'true');
    el.appendChild(dot);
    el.appendChild(
      document.createTextNode(`${row.persona} · ${PRESENCE_STATE_LABELS[row.state] || 'Idle'}`),
    );
    peersEl.appendChild(el);
  }
}

const liveEvents = startLiveEvents({
  onChange: deliverNudge,
  onPresence: renderPeers,
  persona: () => personaLabelFor(currentPersonaId()),
  state: () => presenceStateFor(mic.state),
});
// A clean exit clears this device's presence deliberately, so the other
// devices see it leave rather than time it out as a crash.
globalThis.addEventListener('pagehide', () => liveEvents.stop(), { once: true });

// ---- quick-switch change handlers (spec §2.6) ----------------------------

if (personaSelect) {
  personaSelect.addEventListener('change', () => {
    const prev = currentPersonaId();
    const next = personaSelect.value;
    if (next === prev) return;
    void saveQuickSwitch({
      section: 'persona',
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

// ---- persona editor seam (owner shell redesign 2026-07-18 v2) ------------
//
// The Edit button opens the persona editor dialog. The editor itself —
// partials/persona_editor.html + personaeditor.mjs (exports
// openPersonaEditor(personaId)) — is Author B's module; it is imported
// lazily on first click so this page never pays its cost up front.

const personaEditBtn = $('personaEditBtn');
if (personaEditBtn) {
  personaEditBtn.addEventListener('click', async () => {
    try {
      const mod = await import('./personaeditor.mjs');
      mod.openPersonaEditor(currentPersonaId());
    } catch (err) {
      toast("Couldn't open the persona editor.", {
        error: true,
        detail: (err && (err.message || String(err))) || 'unknown error',
      });
    }
  });
}

// ---- mic sensitivity chips (settings micEagerness; task #5) --------------
//
// Three real buttons (aria-pressed) under "Test my mic" bound to
// settings.micEagerness: low | medium | high; the schema default 'auto'
// shows none pressed until the user picks. Saved through the same
// optimistic quick-switch path as the persona select, and live-applied to
// a connected session via RealtimeSession.updateAudioInput (no-ops on
// nova-bridge / closed datachannel — the change still lands at next mint).

const micSensChips = [...document.querySelectorAll('#micSensGroup .ln-chip')];

function currentEagerness() {
  const v = settingsDoc && settingsDoc.micEagerness;
  return v === 'low' || v === 'medium' || v === 'high' ? v : 'auto';
}

function syncMicChips() {
  const cur = currentEagerness();
  for (const chip of micSensChips) {
    chip.setAttribute('aria-pressed', chip.dataset.eagerness === cur ? 'true' : 'false');
  }
  // The bottom bar's Audio select is the same setting in another shape — one
  // sync point so the two can never disagree (see the mobile shell block).
  syncAudioSelect();
}

for (const chip of micSensChips) {
  chip.addEventListener('click', () => {
    const next = chip.dataset.eagerness;
    const prev = currentEagerness();
    if (next === prev) return;
    // Optimistic press; revert re-syncs from the (unchanged) doc.
    for (const c of micSensChips) c.setAttribute('aria-pressed', c === chip ? 'true' : 'false');
    void saveQuickSwitch({
      section: 'turnDetection',
      mutate: (doc) => {
        doc.micEagerness = next;
      },
      revert: () => syncMicChips(),
      appliedToast: () => {
        if (isLive() && mic.session.updateAudioInput({ eagerness: next })) {
          return 'Mic sensitivity updated — applied to this conversation.';
        }
        return 'Mic sensitivity updated.';
      },
    });
  });
}

// ==========================================================================
// MOBILE CONVERSATION SHELL (2026-08-01)
//
// Everything from here to the "docked settings drawer" block below belongs to
// the phone/tablet layout added on 2026-08-01. The CSS half lives in the
// matching "MOBILE CONVERSATION SHELL" section at the end of app.css.
//
//   - Audio select in the bottom bar  -> settings turnDetection.micEagerness
//     (the same value the rail's Low/Med/High chips write) + a "Mic test…"
//     action that opens the existing #micTestDialog.
//   - Conversation overlay            -> a modal <dialog> mirroring
//     #transcript, so the conversation can be read over the voice panel.
//   - Copy / Screenshot / Tag         -> bound by data-conv-action, so the
//     row over the transcript and the row inside the overlay share one
//     handler each.
//   - Scroll hint                     -> scrolls the snap container to the
//     transcript panel, giving the swipe-up reveal a keyboard/tap route.
// ==========================================================================

// ---- Audio pickup select (mobile bottom bar) -----------------------------

const audioQualitySelect = $('audioQualitySelect');

function syncAudioSelect() {
  if (!audioQualitySelect) return;
  audioQualitySelect.value = currentEagerness();
}

if (audioQualitySelect) {
  audioQualitySelect.addEventListener('change', () => {
    const choice = audioQualitySelect.value;
    // "Mic test…" is an ACTION parked in a value list: run it and put the
    // select straight back to the setting it actually reflects, so it never
    // sits displaying a state that isn't stored anywhere.
    if (choice === 'mictest') {
      syncAudioSelect();
      void micTest.open();
      return;
    }
    const prev = currentEagerness();
    if (choice === prev) return;
    void saveQuickSwitch({
      section: 'turnDetection',
      mutate: (doc) => {
        doc.micEagerness = choice;
      },
      revert: () => syncMicChips(),
      appliedToast: () => {
        if (choice !== 'auto' && isLive() && mic.session.updateAudioInput({ eagerness: choice })) {
          return 'Audio pickup updated — applied to this conversation.';
        }
        return 'Audio pickup updated.';
      },
    });
  });
}

// ---- Conversation as text / as an image ----------------------------------

const transcriptRoot = $('transcript');

/** Flattens the live transcript into ordered {role, body, at} rows. Reads the
 * rendered DOM rather than keeping a parallel model: transcript.mjs owns that
 * subtree and is the only writer, so the DOM is the one copy that is always
 * current (including turns restored by a fallback reply). */
function conversationRows() {
  if (!transcriptRoot) return [];
  const rows = [];
  for (const bubble of transcriptRoot.querySelectorAll('.ln-bubble')) {
    const role = (bubble.querySelector('.ln-bubble__role') || {}).textContent || '';
    const body = (bubble.querySelector('.ln-bubble__content') || {}).textContent || '';
    if (!body.trim()) continue; // in-progress/typing bubbles have no text yet
    const wrap = bubble.parentElement;
    const tsEl = wrap ? wrap.querySelector('.conv-timestamp') : null;
    const at = tsEl && !tsEl.hidden ? (tsEl.textContent || '').trim() : '';
    rows.push({ role: role.trim() || 'Speaker', body: body.trim(), at });
  }
  return rows;
}

function conversationText() {
  return conversationRows()
    .map((r) => (r.at ? `${r.role} (${r.at}): ${r.body}` : `${r.role}: ${r.body}`))
    .join('\n\n');
}

async function copyConversation() {
  const text = conversationText();
  if (!text) {
    toast('Nothing to copy yet — this conversation is empty.');
    return;
  }
  if (await copyText(text)) toast('Conversation copied.');
  else toast("Couldn't reach the clipboard — try Screenshot instead.", { error: true });
}

/** Reads a CSS custom property off the document root, with a literal fallback
 * for the case where the token has been renamed out from under us. Canvas
 * needs concrete colours; the tokens keep the image matching the user's
 * chosen theme instead of hard-coding one palette. */
function cssColor(name, fallback) {
  try {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  } catch {
    return fallback;
  }
}

function wrapCanvasText(ctx, text, maxWidth) {
  const lines = [];
  for (const paragraph of String(text).split('\n')) {
    if (paragraph === '') {
      lines.push('');
      continue;
    }
    let line = '';
    for (const word of paragraph.split(/\s+/)) {
      const next = line ? `${line} ${word}` : word;
      if (ctx.measureText(next).width <= maxWidth || line === '') line = next;
      else {
        lines.push(line);
        line = word;
      }
    }
    lines.push(line);
  }
  return lines;
}

/** Paints the conversation to a canvas and returns a PNG Blob.
 *
 * Deliberately NOT html2canvas or any other CDN library: this page ships as
 * static assets from our own origin with no bundler and no third-party
 * script tags, and the transcript is plain text + role labels, so a direct
 * 2D-canvas render is both smaller and exact. Nothing leaves the device. */
async function conversationPNG() {
  const rows = conversationRows();
  if (!rows.length) return null;

  const WIDTH = 760;
  const PAD = 28;
  const GAP = 18;
  const BODY_LH = 22;
  const ROLE_LH = 18;
  const inner = WIDTH - PAD * 2;

  const bg = cssColor('--ln-surface', '#0e1420');
  const fg = cssColor('--ln-text', '#e8eef7');
  const dim = cssColor('--ln-text-muted', '#9fb0c6');
  const rule = cssColor('--ln-border', '#26364b');

  const bodyFont = '15px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif';
  const roleFont = '700 13px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif';
  const titleFont = '700 20px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif';

  // Measure pass on a throwaway context — the wrap depends on the same font
  // metrics the draw pass uses, so it has to be a real canvas context.
  const measure = document.createElement('canvas').getContext('2d');
  if (!measure) return null;
  const blocks = [];
  let height = PAD + 30 + GAP; // title + rule
  for (const row of rows) {
    measure.font = bodyFont;
    const lines = wrapCanvasText(measure, row.body, inner);
    blocks.push({ row, lines });
    height += ROLE_LH + lines.length * BODY_LH + GAP;
  }
  height += PAD;

  const scale = Math.min(2, Math.max(1, window.devicePixelRatio || 1));
  const canvas = document.createElement('canvas');
  canvas.width = Math.round(WIDTH * scale);
  canvas.height = Math.round(height * scale);
  const ctx = canvas.getContext('2d');
  if (!ctx) return null;
  ctx.scale(scale, scale);

  ctx.fillStyle = bg;
  ctx.fillRect(0, 0, WIDTH, height);

  let y = PAD + 20;
  ctx.fillStyle = fg;
  ctx.font = titleFont;
  ctx.fillText('Live Ninja conversation', PAD, y);
  ctx.font = roleFont;
  ctx.fillStyle = dim;
  const stamp = new Date().toLocaleString();
  ctx.fillText(stamp, WIDTH - PAD - ctx.measureText(stamp).width, y);
  y += 12;
  ctx.strokeStyle = rule;
  ctx.beginPath();
  ctx.moveTo(PAD, y);
  ctx.lineTo(WIDTH - PAD, y);
  ctx.stroke();
  y += GAP;

  for (const block of blocks) {
    ctx.font = roleFont;
    ctx.fillStyle = dim;
    const head = block.row.at ? `${block.row.role} · ${block.row.at}` : block.row.role;
    ctx.fillText(head, PAD, y + 12);
    y += ROLE_LH;
    ctx.font = bodyFont;
    ctx.fillStyle = fg;
    for (const line of block.lines) {
      ctx.fillText(line, PAD, y + 15);
      y += BODY_LH;
    }
    y += GAP;
  }

  return await new Promise((resolve) => {
    if (typeof canvas.toBlob === 'function') canvas.toBlob(resolve, 'image/png');
    else resolve(null);
  });
}

function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoked late so slow mobile download managers still resolve the URL.
  setTimeout(() => URL.revokeObjectURL(url), 30_000);
}

async function screenshotConversation() {
  let blob = null;
  try {
    blob = await conversationPNG();
  } catch (err) {
    toast("Couldn't build the screenshot.", {
      error: true,
      detail: (err && (err.message || String(err))) || 'canvas render failed',
    });
    return;
  }
  if (!blob) {
    toast('Nothing to capture yet — this conversation is empty.');
    return;
  }
  const filename = `live-ninja-conversation-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.png`;

  // Deliberately a plain download rather than navigator.share(): several
  // platforms report canShare({files}) === true and then leave share()
  // pending forever without a user-activation gesture, which would strand
  // the button with no image and no error. A download lands in the same
  // place on desktop and on Android, and the OS share sheet is one tap away
  // from there.
  downloadBlob(blob, filename);
  toast('Screenshot saved.');
}

// ---- Tag for review ------------------------------------------------------
//
// Stored on THIS DEVICE (localStorage), newest first, capped. There is no
// server-side review queue to post to, and a POST that quietly went nowhere
// would be worse than an honest local record — the Help panel says where the
// note lives, and Copy/Screenshot are what carry it off the device.

const REVIEW_TAGS_KEY = 'ln.reviewTags';
const REVIEW_TAGS_MAX = 50;

const reviewTagDialog = $('reviewTagDialog');
const reviewTagForm = $('reviewTagForm');
const reviewTagNote = $('reviewTagNote');
const reviewTagError = $('reviewTagError');
const reviewTagCancel = $('reviewTagCancel');

function currentConversationId() {
  return (mic.session && mic.session.sessionId) || lastSessionId || '';
}

function readReviewTags() {
  try {
    const parsed = JSON.parse(localStorage.getItem(REVIEW_TAGS_KEY) || '[]');
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

function writeReviewTag(note) {
  const tags = readReviewTags();
  tags.unshift({
    conversationId: currentConversationId(),
    note,
    at: new Date().toISOString(),
  });
  localStorage.setItem(REVIEW_TAGS_KEY, JSON.stringify(tags.slice(0, REVIEW_TAGS_MAX)));
  return tags.length;
}

function openReviewTag() {
  if (!reviewTagDialog || typeof reviewTagDialog.showModal !== 'function') return;
  if (reviewTagNote) reviewTagNote.value = '';
  if (reviewTagError) {
    reviewTagError.hidden = true;
    reviewTagError.textContent = '';
  }
  if (!reviewTagDialog.open) reviewTagDialog.showModal();
  if (reviewTagNote) reviewTagNote.focus({ preventScroll: true });
}

if (reviewTagForm && reviewTagDialog) {
  reviewTagForm.addEventListener('submit', (e) => {
    // method="dialog" would close on submit; take the event so a rejected
    // save can keep the form open with its message.
    e.preventDefault();
    const note = ((reviewTagNote && reviewTagNote.value) || '').trim();
    if (!note) {
      if (reviewTagError) {
        reviewTagError.textContent = 'Add a line about what to look at.';
        reviewTagError.hidden = false;
      }
      if (reviewTagNote) reviewTagNote.focus({ preventScroll: true });
      return;
    }
    try {
      writeReviewTag(note);
    } catch (err) {
      if (reviewTagError) {
        reviewTagError.textContent =
          "This browser wouldn't save the note — copy the conversation instead.";
        reviewTagError.hidden = false;
      }
      toast("Couldn't save the review tag.", {
        error: true,
        detail: (err && (err.message || String(err))) || 'localStorage write failed',
      });
      return;
    }
    reviewTagDialog.close();
    toast('Tagged for review — the note is saved on this device.');
  });
}
if (reviewTagCancel && reviewTagDialog) {
  reviewTagCancel.addEventListener('click', () => reviewTagDialog.close());
}
if (reviewTagDialog) {
  reviewTagDialog.addEventListener('click', (e) => {
    if (e.target === reviewTagDialog) reviewTagDialog.close();
  });
}

// ---- Conversation overlay ------------------------------------------------

const convOverlay = $('conversationOverlay');
const convOverlayOpenBtn = $('convOverlayOpen');
const convOverlayCloseBtn = $('conversationOverlayClose');
const convOverlayHideBtn = $('conversationOverlayHide');
const convOverlayBody = $('conversationOverlayBody');
const convOverlayEmpty = $('conversationOverlayEmpty');

let convMirrorObserver = null;
let convMirrorFrame = 0;

/** Re-clones #transcript into the overlay. A clone, not a move: transcript.mjs
 * owns the real subtree and appends to it on every delta, so handing that
 * element to the dialog would break live rendering the moment the overlay
 * closed. Interactive controls inside cloned tool cards are stripped — a
 * duplicate "Details" button would be a dead control. */
function renderConvMirror() {
  if (!convOverlayBody || !transcriptRoot) return;
  const clone = transcriptRoot.cloneNode(true);
  clone.removeAttribute('id');
  clone.removeAttribute('role');
  clone.removeAttribute('aria-live');
  clone.removeAttribute('aria-relevant');
  for (const btn of clone.querySelectorAll('button')) btn.remove();
  const pinned =
    convOverlayBody.scrollTop + convOverlayBody.clientHeight >= convOverlayBody.scrollHeight - 48;
  convOverlayBody.replaceChildren(clone);
  if (convOverlayEmpty) convOverlayEmpty.hidden = clone.childElementCount > 0;
  if (pinned) convOverlayBody.scrollTop = convOverlayBody.scrollHeight;
}

/** Coalesces a burst of transcript mutations (one per streamed token) into a
 * single re-clone per frame. */
function scheduleConvMirror() {
  if (convMirrorFrame) return;
  convMirrorFrame = requestAnimationFrame(() => {
    convMirrorFrame = 0;
    renderConvMirror();
  });
}

// Owner 2026-08-01: the overlay is the whole screen above the bottom bar, and
// the bar's own button is the toggle — so it opens NON-MODALLY (.show()).
// showModal() would inert the bar and make "Hide Conversation" unpressable.
// The two things showModal() gave us for free are re-supplied here: Escape
// (below) and focus handling; app.css hides .conv-body while it is open, which
// is what replaces the modal's inertness and keeps the page to one scrollbar.
const convAppEl = document.querySelector('.conv-app');
const convOverlayToggleLabel = $('convOverlayToggleLabel');

function setConvOverlayToggle(open) {
  if (convOverlayOpenBtn) convOverlayOpenBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (convOverlayToggleLabel) {
    convOverlayToggleLabel.textContent = open ? 'Hide Conversation' : 'Show Conversation';
  }
  if (convAppEl) convAppEl.classList.toggle('is-overlay-open', open);
}

function openConvOverlay() {
  if (!convOverlay || typeof convOverlay.show !== 'function') return;
  renderConvMirror();
  if (!convOverlay.open) convOverlay.show();
  setConvOverlayToggle(true);
  if (convOverlayBody) convOverlayBody.scrollTop = convOverlayBody.scrollHeight;
  if (convOverlayCloseBtn) convOverlayCloseBtn.focus({ preventScroll: true });
  if (transcriptRoot && typeof MutationObserver === 'function') {
    if (!convMirrorObserver) convMirrorObserver = new MutationObserver(scheduleConvMirror);
    convMirrorObserver.observe(transcriptRoot, {
      childList: true,
      subtree: true,
      characterData: true,
    });
  }
}

if (convOverlay && convOverlayOpenBtn) {
  convOverlayOpenBtn.addEventListener('click', () => {
    if (convOverlay.open) convOverlay.close();
    else openConvOverlay();
  });
  // A non-modal dialog does not close itself on Escape.
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && convOverlay.open) {
      e.preventDefault();
      convOverlay.close();
    }
  });
}
if (convOverlayCloseBtn && convOverlay) {
  convOverlayCloseBtn.addEventListener('click', () => convOverlay.close());
}
if (convOverlayHideBtn && convOverlay) {
  convOverlayHideBtn.addEventListener('click', () => convOverlay.close());
}
if (convOverlay) {
  convOverlay.addEventListener('close', () => {
    if (convMirrorObserver) convMirrorObserver.disconnect();
    if (convMirrorFrame) {
      cancelAnimationFrame(convMirrorFrame);
      convMirrorFrame = 0;
    }
    setConvOverlayToggle(false);
    if (convOverlayOpenBtn) convOverlayOpenBtn.focus({ preventScroll: true });
  });
}

// ---- Conversation tool buttons (transcript row + overlay row) ------------

for (const btn of document.querySelectorAll('[data-conv-action]')) {
  btn.addEventListener('click', () => {
    switch (btn.dataset.convAction) {
      case 'copy':
        void copyConversation();
        break;
      case 'screenshot':
        void screenshotConversation();
        break;
      case 'tag':
        openReviewTag();
        break;
      default:
        break;
    }
  });
}

// ---- docked settings drawer (site nav; replaces the removed header) ------
//
// Native <dialog>.showModal() supplies the focus trap, Escape-to-close,
// and inerting of the page behind; the scrim (::backdrop) click closes via
// the e.target === dialog check — the drawer's padding lives on an inner
// wrapper, so a click that reaches the dialog element itself is always on
// the backdrop.

// Initialize these before the drawer's deep-link path below. A
// ?openSettings=1 load opens the drawer during module evaluation and calls
// loadDrawerCost() synchronously, so later declarations would still be in
// their temporal dead zone.
const drawerCostEl = $('drawerCost');
const drawerCostValue = $('drawerCostValue');
const drawerCostSub = $('drawerCostSub');
let drawerCostFetchedAt = 0;

const settingsDrawer = $('settingsDrawer');
const settingsDrawerBtn = $('settingsDrawerBtn');
const settingsDrawerClose = $('settingsDrawerClose');
const settingsTabBadge = $('settingsTabBadge');
const settingsAccordion = settingsDrawer
  ? initSettingsAccordion(settingsDrawer)
  : null;

if (settingsDrawer && settingsDrawerBtn && typeof settingsDrawer.showModal === 'function') {
  const openSettingsDrawer = () => {
    // The badge counts work in About you. Reveal that work instead of opening
    // whichever panel happened to be used last.
    if (settingsAccordion && settingsTabBadge && !settingsTabBadge.hidden) {
      settingsAccordion.open('sectionAboutTrigger');
    }
    if (!settingsDrawer.open) settingsDrawer.showModal();
    settingsDrawerBtn.setAttribute('aria-expanded', 'true');
    if (settingsDrawerClose) settingsDrawerClose.focus({ preventScroll: true });
    void loadDrawerCost();
    // M16: the drawer may have been open for hours, or the badge may have
    // appeared from a background auto-apply — re-read the suggestion queue so
    // "About you" shows what the badge is counting (settings.mjs owns the
    // fetch; this is only the "you opened it" trigger).
    if (window.__lnRefreshProfileSuggestions) window.__lnRefreshProfileSuggestions();
  };

  settingsDrawerBtn.addEventListener('click', openSettingsDrawer);
  if (settingsDrawerClose) {
    settingsDrawerClose.addEventListener('click', () => settingsDrawer.close());
  }
  settingsDrawer.addEventListener('click', (e) => {
    if (e.target === settingsDrawer) settingsDrawer.close();
  });
  settingsDrawer.addEventListener('close', () => {
    settingsDrawerBtn.setAttribute('aria-expanded', 'false');
    settingsDrawerBtn.focus({ preventScroll: true });
  });
  // Owner 2026-07-19: settings moved inline into this drawer (the
  // standalone /settings page is gone) — wire every control once. The
  // panel's elements are always in the DOM regardless of <dialog> open
  // state, so this doesn't need to wait for a first open.
  initSettingsPanel().catch((err) => {
    console.error('settings panel failed to initialize', err);
  });
  // A Settings link elsewhere in the app (nav.html, history.html) can't
  // deep-link into this dialog directly, so it points here with
  // ?openSettings=1 and this opens the drawer on load.
  if (new URLSearchParams(window.location.search).get('openSettings') === '1') {
    openSettingsDrawer();
  }
}

// ---- docked help drawer --------------------------------------------------
//
// Same mechanics as the settings drawer directly above: native
// <dialog>.showModal() for the focus trap, Escape, and inerting; scrim click
// via the e.target === dialog check (padding lives on .conv-drawer__inner, so
// a click landing on the dialog element itself is always the backdrop); focus
// returns to the opening tab on close.
//
// The panel's content is static markup in conversation.html — there is no
// state to hydrate and nothing to fetch, so this block is open/close only.
// Keep the help copy current with the app: see the "Help section maintenance"
// section of CLAUDE.md / agents.md.

const helpDrawer = $('helpDrawer');
const helpDrawerBtn = $('helpDrawerBtn');
const helpDrawerClose = $('helpDrawerClose');

if (helpDrawer && helpDrawerBtn && typeof helpDrawer.showModal === 'function') {
  helpDrawerBtn.addEventListener('click', () => {
    if (!helpDrawer.open) helpDrawer.showModal();
    helpDrawerBtn.setAttribute('aria-expanded', 'true');
    // Long panel: always start at the top, even on a re-open after scrolling.
    const inner = helpDrawer.querySelector('.conv-drawer__inner');
    if (inner) inner.scrollTop = 0;
    if (helpDrawerClose) helpDrawerClose.focus({ preventScroll: true });
  });
  if (helpDrawerClose) {
    helpDrawerClose.addEventListener('click', () => helpDrawer.close());
  }
  helpDrawer.addEventListener('click', (e) => {
    if (e.target === helpDrawer) helpDrawer.close();
  });
  helpDrawer.addEventListener('close', () => {
    helpDrawerBtn.setAttribute('aria-expanded', 'false');
    helpDrawerBtn.focus({ preventScroll: true });
  });
}

// Month-to-date cost line in the Menu drawer (GET /api/v1/costs — the sum
// of every saved conversation's persisted per-session estimate). Fetched
// on each drawer open, cached 60s so repeated opens stay free; a failed
// fetch just leaves the line hidden — the menu must never break on it.
async function loadDrawerCost() {
  if (!drawerCostEl || !drawerCostValue) return;
  if (Date.now() - drawerCostFetchedAt < 60_000) return;
  try {
    const resp = await apiJSON('/api/v1/costs');
    drawerCostFetchedAt = Date.now();
    const usd = Number(resp && resp.totalUsd);
    const n = Number(resp && resp.conversations) || 0;
    const costed = Number(resp && resp.costed) || 0;
    const openAiSpent = Number(resp && resp.openAiTotalUsd);
    const openAiBudget = Number(resp && resp.openAiBudgetUsd);
    const openAiRemaining = Number(resp && resp.openAiRemainingUsd);
    const budgetWarning = resp && resp.openAiBudgetWarning === true;
    // Until at least one conversation carries a persisted cost, "$0.000
    // across N conversations" would read as "free" — stay hidden instead.
    if (!Number.isFinite(usd) || n === 0 || (costed === 0 && usd <= 0)) {
      drawerCostEl.hidden = true;
      return;
    }
    drawerCostEl.classList.toggle('is-budget-warning', budgetWarning);
    if (
      budgetWarning &&
      Number.isFinite(openAiSpent) &&
      Number.isFinite(openAiBudget) &&
      Number.isFinite(openAiRemaining)
    ) {
      const remainingText = openAiRemaining < 0
        ? `-$${Math.abs(openAiRemaining).toFixed(2)}`
        : `$${openAiRemaining.toFixed(2)}`;
      drawerCostValue.textContent = `${remainingText} left`;
      if (drawerCostSub) {
        drawerCostSub.textContent =
          `${formatCostUSD(openAiSpent)} your OpenAI spend · $${openAiBudget.toFixed(2)} allowance`;
      }
      drawerCostEl.setAttribute(
        'aria-label',
        `OpenAI per-user allowance warning: estimated ${remainingText} remaining this month`,
      );
    } else {
      drawerCostValue.textContent = formatCostUSD(usd);
      if (drawerCostSub) {
        drawerCostSub.textContent = `${n} conversation${n === 1 ? '' : 's'} · estimate`;
      }
      drawerCostEl.setAttribute('aria-label', 'Monthly usage estimate');
    }
    drawerCostEl.hidden = false;
  } catch {
    drawerCostEl.classList.remove('is-budget-warning');
    drawerCostEl.hidden = true;
  }
}

// ---- cross-tab settings delivery + mid-session application ---------------
//
// There is NO server-side settings fan-out to the web client (the web
// WebSocket/settings.updated frame does not exist — only the device shadow
// path has push), so this is the documented minimal channel: every
// successful settings section save — here (quick-switches) and in settings.mjs
// (the inline drawer autosave) — writes the new document version to
// localStorage under 'ln.settings.version'. The browser fires 'storage'
// in every OTHER same-origin tab; the inline settings drawer also emits a
// same-tab event after a successful save. Both paths re-GET this device's
// effective document and apply the delta:
//   - Mic pickup (micEagerness) / turn detection → applied to the LIVE
//     session via RealtimeSession.updateAudioInput (session.update,
//     mirroring internal/realtime/mint.go) — owner request 2026-07-18;
//   - persona/voice → mint-bound, so a live session gets the persistent
//     "applies to your next conversation" banner instead;
//   - appearance/privacy/quick-switch selects re-sync as on bootstrap.

const SETTINGS_PING_KEY = 'ln.settings.version';
const SETTINGS_LOCAL_EVENT = 'ln:settings-changed';

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
async function applySettingsDelta(prev, fresh) {
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

  try {
    const wakeDelta = await applyWakeWordSettings(wakeEngine, prev, fresh);
    // Normally a checked toggle already has an engine. Cover the narrow
    // bootstrap/save race where the settings panel commits first.
    if (wakeDelta.wakeWordChanged && !wakeEngine && wakeToggle && wakeToggle.checked) {
      await setWakeListening(true);
    }
    if (wakeDelta.wakeWordChanged) renderWakeUI();
  } catch (err) {
    if (err && err.wakeWordPreserved && wakeEngine && wakeEngine.state === 'listening') {
      toast("Couldn't switch wake phrases — the previous phrase is still active.", {
        error: true,
        detail: (err && (err.message || String(err))) || 'unknown error',
      });
      renderWakeUI();
    } else {
      // With no proven detector to preserve, fail closed instead of leaving
      // the toggle checked while no model is listening.
      handleWakeEngineFailure(err);
    }
  }
}

let adoptInFlight = false;
let adoptQueued = false;

async function adoptRemoteSettings() {
  if (!settingsDoc) return; // bootstrap still owns the doc
  if (adoptInFlight) {
    // Model verification can make an adoption take several seconds. Do not
    // lose a newer settings save that lands while that work is in flight.
    adoptQueued = true;
    return;
  }
  adoptInFlight = true;
  try {
    let adoption = null;
    await withSettingsOperation(async () => {
      const fresh = await apiJSON(SETTINGS_PATH);
      if ((Number(fresh?.version) || 0) < settingsVersion()) {
        adoptQueued = true;
        return;
      }
      const prev = settingsDoc;
      settingsDoc = fresh;
      syncQuickSwitchesFromDoc();
      if (window.__lnApplyAppearance && settingsDoc.appearance) {
        window.__lnApplyAppearance(settingsDoc.appearance);
      }
      const privacy = settingsDoc.privacy;
      sink.setEnabled(!(privacy && privacy.storeTranscripts === false));
      adoption = { prev, fresh };
    });
    if (adoption) await applySettingsDelta(adoption.prev, adoption.fresh);
  } catch {
    /* offline or auth redirect — the next ping (or a reload) re-syncs */
  } finally {
    adoptInFlight = false;
    if (adoptQueued) {
      adoptQueued = false;
      void adoptRemoteSettings();
    }
  }
}

window.addEventListener('storage', (e) => {
  // Fires only in tabs that did NOT write the key. Ignore unrelated keys
  // and the removal that a localStorage.clear() produces.
  if (e.key !== SETTINGS_PING_KEY || e.newValue === null || e.newValue === e.oldValue) return;
  void adoptRemoteSettings();
});

// `storage` deliberately excludes the document that performed the write.
// settings.mjs emits this after an inline-drawer autosave commits.
window.addEventListener(SETTINGS_LOCAL_EVENT, () => {
  void adoptRemoteSettings();
});

// ---- bootstrap -----------------------------------------------------------

async function bootstrap() {
  try {
    await ensureCurrentDeviceRegistered();
  } catch (err) {
    if (err && err.name === 'AuthLostError') return;
    toast("Couldn't register this browser's device name yet.", { error: true });
  }
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
  updatePersonaGroupLabel();

  // Voices catalog kept for human-readable labels in banner copy only —
  // the voice quick-switch select no longer exists (voice is
  // persona-embedded; owner shell redesign 2026-07-18 v2).
  if (voices.status === 'fulfilled' && Array.isArray(voices.value.voices)) {
    voiceCatalog = voices.value.voices;
  }

  syncMicChips();

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

  // Presence again, now that settings have landed. The first publish happens
  // the moment the socket opens — module top level, long before this — so it
  // carries the hardcoded 'default' persona fallback. Without this the other
  // devices would show every peer as plain "Live Ninja" until its mic state
  // happened to change.
  liveEvents.publishPresence();
}

void bootstrap();

/* ==== persona-editor:BEGIN ==== */
// Persona editor glue (personas are the unit of voice identity — voice and
// accent are embedded per persona, personaeditor.mjs). The page-shell code
// owns the Edit button and lazy-imports personaeditor.mjs itself (so this
// region deliberately adds NO static import — the editor's cost stays off
// the first-paint path). This region owns the 'personachanged' refresh
// contract: after the editor saves (text edit, duplicate, or a
// personaPrefs voice/accent change) it dispatches 'personachanged' on
// window, and this listener re-pulls the persona library so the select
// labels/groups reflect renames and fresh copies immediately.

async function refreshPersonaLibrary() {
  try {
    const v = await apiJSON(PERSONAS_PATH);
    const groups = {
      builtin: Array.isArray(v.builtin) ? v.builtin : [],
      mine: Array.isArray(v.mine) ? v.mine : [],
      shared: Array.isArray(v.shared) ? v.shared : [],
    };
    personaCatalog = groups.builtin.concat(groups.mine, groups.shared);
    fillPersonaSelect(personaSelect, groups, currentPersonaId());
    updatePersonaGroupLabel();
    transcript.setPersonaLabel(personaLabelFor(currentPersonaId()));
  } catch {
    /* cosmetic refresh — the stale labels correct on the next bootstrap */
  }
}

window.addEventListener('personachanged', (e) => {
  void refreshPersonaLibrary();
  // Editing the persona currently in use is mint-bound (like the old voice
  // quick-switch): mid-session, surface the persistent banner so the user
  // knows the new sound arrives with the NEXT conversation.
  const detail = (e && e.detail) || {};
  if (isLive() && detail.personaId && detail.personaId === currentPersonaId()) {
    showPendingBanner(
      `${personaLabelFor(detail.personaId) || 'Live Ninja'}'s updated voice applies to your next conversation — tap New conversation to switch now.`,
    );
  }
});
/* ==== persona-editor:END ==== */
