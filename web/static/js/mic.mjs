// mic.mjs — mic state machine + primary-control UI binding
// (docs/web-ui-spec.md §2.2, §2.3, §2.5, §2.7, §2.8).
//
// States: idle → requesting-mic → connecting → live-listening ⇄
// live-thinking ⇄ live-speaking → ending → idle, plus denied / error.
// The three live-* states share one connected RealtimeSession (one mint) —
// turns cycle inside it without re-minting.
//
// Input semantics (spec §2.2 mode table + §2.7 keyboard map, tap semantics
// with a press-and-hold superset):
//   - Tap / press Space while idle|denied|error  → start (or retry).
//   - Tap while live-listening                   → manual end-of-turn
//     (input_audio_buffer.commit + response.create) before VAD fires.
//   - Hold (≥400ms) started from idle            → release commits the turn
//     (walkie-talkie style push-to-talk).
//   - Tap while live-thinking                    → cancel the response.
//   - Tap while live-speaking                    → forced barge-in.
//   - Space is ignored while focus is in the composer/any editable control.
//   - Esc never ends a live call (deliberate-click rule); ending is the End
//     control or the keep-warm grace timeout.
//
// Keep-warm grace (spec §2.2): after a completed turn the session stays
// connected — 10s in push-to-talk mode, 60s in hands-free mode (mic track
// disabled so only the wake word resumes it) — then full teardown to idle.
//
// DOM contract (all optional — missing elements are skipped so the module
// never throws on a partial page): #pttBtn, #statePill, #statePillText,
// #statusText, #wakeToggle, #wakeHint, plus any [data-ln-end] end button.
// These ids match mockups/web/02-conversation.html and the M3 templates.

import { RealtimeSession, RealtimeError, acquireMicStream, prefetchSession } from './realtime.mjs';

export const MicState = Object.freeze({
  IDLE: 'idle',
  REQUESTING: 'requesting-mic',
  CONNECTING: 'connecting',
  LISTENING: 'live-listening',
  THINKING: 'live-thinking',
  SPEAKING: 'live-speaking',
  ENDING: 'ending',
  DENIED: 'denied',
  ERROR: 'error',
});

const LIVE_STATES = new Set([MicState.LISTENING, MicState.THINKING, MicState.SPEAKING]);

// Legacy keep-warm grace, used only when the keepListeningSeconds setting is
// unavailable (options.getKeepListeningSeconds not wired). The setting's 0
// default means "no client timeout — listen until the user or the provider
// ends the session" (owner decision 2026-07-19).
const GRACE_MS = { ptt: 10_000, handsFree: 60_000 };
const HOLD_MS = 400; // press shorter than this is a tap, longer is a hold
const HANDSFREE_STORAGE_KEY = 'ln.handsFree';

// Per-state presentation: state-pill text, button label, aria-label,
// aria-pressed ("mic currently open", spec §2.8), disabled during
// transitional states (no double-mint, spec §2.5).
const STATE_META = {
  [MicState.IDLE]: {
    pill: 'Idle',
    label: 'Start talking',
    aria: 'Push to talk, currently off — tap to start',
    pressed: false,
    disabled: false,
  },
  [MicState.REQUESTING]: {
    pill: 'Mic permission…',
    label: 'Allow microphone…',
    aria: 'Waiting for microphone permission',
    pressed: false,
    disabled: true,
  },
  [MicState.CONNECTING]: {
    pill: 'Connecting…',
    label: 'Connecting…',
    aria: 'Connecting to the voice service',
    pressed: false,
    disabled: true,
  },
  [MicState.LISTENING]: {
    pill: 'Listening',
    label: 'Stop',
    aria: 'Push to talk, currently on — listening. Tap to end your turn.',
    pressed: true,
    disabled: false,
  },
  [MicState.THINKING]: {
    pill: 'Thinking…',
    label: 'Cancel',
    aria: 'Assistant is thinking. Tap to cancel.',
    pressed: false,
    disabled: false,
  },
  [MicState.SPEAKING]: {
    pill: 'Speaking',
    label: 'Interrupt',
    aria: 'Assistant is speaking. Tap to interrupt.',
    pressed: false,
    disabled: false,
  },
  [MicState.ENDING]: {
    pill: 'Ending…',
    label: 'Ending…',
    aria: 'Ending the conversation',
    pressed: false,
    disabled: true,
  },
  [MicState.DENIED]: {
    pill: 'Mic blocked',
    label: 'Try again',
    aria: 'Microphone access is blocked. Tap to try again.',
    pressed: false,
    disabled: false,
  },
  [MicState.ERROR]: {
    pill: 'Error',
    label: 'Retry',
    aria: 'The voice connection failed. Tap to retry.',
    pressed: false,
    disabled: false,
  },
};

function isEditableTarget(t) {
  if (!t) return false;
  const tag = (t.tagName || '').toLowerCase();
  return tag === 'input' || tag === 'textarea' || tag === 'select' || t.isContentEditable === true;
}

function formatResetAt(resetAt) {
  const d = new Date(resetAt);
  if (Number.isNaN(d.getTime())) return 'soon';
  try {
    return new Intl.DateTimeFormat(undefined, {
      month: 'short',
      day: 'numeric',
      hour: 'numeric',
      minute: 'numeric',
    }).format(d);
  } catch {
    return d.toLocaleString();
  }
}

/**
 * Events (CustomEvent, payload in `detail`):
 *   statechange    {state, prev}
 *   sessioncreated {session}   — a new RealtimeSession exists; the page and
 *                                transcriptsink attach their listeners here
 *   ending         {session}   — flush point before teardown
 *   error          {message, code, retryable, txId, detail}
 *                              txId/detail are the backend transaction ref and
 *                              full backend message when the failure came from
 *                              the server (empty for client-side mic errors) —
 *                              the page surfaces them in the reportable banner.
 *   toast          {message}
 */
export class MicController extends EventTarget {
  #state = MicState.IDLE;
  #session = null;
  #graceTimer = null;
  #graceWaitingForWake = false; // hands-free grace: session warm, mic muted
  #pressAt = 0;
  #pressStartedSession = false;
  #lastHandledInputAt = 0;
  #errorInfo = null; // {message, retryable, permanent}
  #handsFreeAvailable = true;
  #permStatus = null; // PermissionStatus watched while in `denied` (auto-recovery)
  #els;
  #createSession;
  #getMicDeviceId;
  #getWakePhrase;
  #getKeepListeningSeconds;
  #prefetch;

  constructor(options = {}) {
    super();
    const doc = options.document || document;
    const byId = (id) => doc.getElementById(id);
    this.#els = {
      button: options.button ?? byId('pttBtn'),
      pill: options.pill ?? byId('statePill'),
      pillText: options.pillText ?? byId('statePillText'),
      status: options.status ?? byId('statusText'),
      wakeToggle: options.wakeToggle ?? byId('wakeToggle'),
      wakeHint: options.wakeHint ?? byId('wakeHint'),
      micHelp: options.micHelp ?? byId('micHelp'),
      endButtons: options.endButtons ?? Array.from(doc.querySelectorAll('[data-ln-end]')),
    };
    this.#createSession = options.createSession || (() => new RealtimeSession());
    this.#getMicDeviceId = options.getMicDeviceId || (() => null);
    this.#getWakePhrase = options.getWakePhrase || (() => '');
    this.#getKeepListeningSeconds = options.getKeepListeningSeconds || null;
    this.#prefetch = options.prefetchSession || prefetchSession;

    this.#bindUI(doc);
    this.#restoreHandsFree();
    this.#render();
  }

  get state() {
    return this.#state;
  }
  get session() {
    return this.#session;
  }
  get handsFree() {
    return this.#handsFreeAvailable && !!(this.#els.wakeToggle && this.#els.wakeToggle.checked);
  }

  #emit(type, detail = {}) {
    this.dispatchEvent(new CustomEvent(type, { detail }));
  }

  // ---- UI binding ----

  #bindUI(doc) {
    const btn = this.#els.button;
    if (btn) {
      btn.addEventListener('pointerdown', (e) => {
        if (e.button !== undefined && e.button !== 0) return;
        this.#lastHandledInputAt = Date.now();
        // Intent prefetch (latency plan #4.2): warm the session mint the
        // moment the finger lands, before the press/click resolves.
        this.#maybePrefetch();
        this.#press();
      });
      const release = () => {
        this.#lastHandledInputAt = Date.now();
        this.#release();
      };
      btn.addEventListener('pointerup', release);
      btn.addEventListener('pointercancel', () => {
        this.#pressAt = 0; // cancelled press: no release action
      });
      // Keyboard activation (Enter, or Space while the button is focused)
      // arrives as a click with no preceding pointerdown — treat as a tap,
      // guarded against double-firing after a real pointer press.
      btn.addEventListener('click', (e) => {
        if (Date.now() - this.#lastHandledInputAt < 500) {
          e.preventDefault();
          return;
        }
        this.#tapAction();
      });
    }

    // Spec §2.7: Space anywhere (except editable controls) mirrors the mic
    // button, with press/release for hold-to-talk.
    doc.addEventListener('keydown', (e) => {
      if (e.code !== 'Space' || e.repeat) return;
      if (isEditableTarget(e.target)) return;
      e.preventDefault();
      this.#lastHandledInputAt = Date.now();
      this.#press();
    });
    doc.addEventListener('keyup', (e) => {
      if (e.code !== 'Space') return;
      if (isEditableTarget(e.target)) return;
      e.preventDefault();
      this.#lastHandledInputAt = Date.now();
      this.#release();
    });

    const toggle = this.#els.wakeToggle;
    if (toggle) {
      toggle.addEventListener('change', () => {
        try {
          localStorage.setItem(HANDSFREE_STORAGE_KEY, toggle.checked ? '1' : '0');
        } catch {
          /* storage may be unavailable (private mode) — non-fatal */
        }
        this.#render();
      });
    }

    for (const el of this.#els.endButtons || []) {
      el.addEventListener('click', () => this.end());
    }
  }

  #restoreHandsFree() {
    const toggle = this.#els.wakeToggle;
    if (!toggle) return;
    try {
      // Spec §2.3: hands-free is a client-local preference, default OFF.
      toggle.checked = localStorage.getItem(HANDSFREE_STORAGE_KEY) === '1';
    } catch {
      toggle.checked = false;
    }
  }

  /** wakeword.mjs calls this with false when AudioWorklet/WASM support is
   * missing — the toggle hides and the guaranteed click-to-talk note shows
   * (spec §2.3 wake-word toggle row). */
  setHandsFreeAvailable(available) {
    this.#handsFreeAvailable = !!available;
    const toggle = this.#els.wakeToggle;
    if (toggle) {
      const wrapper = toggle.closest('label') || toggle;
      wrapper.hidden = !available;
      if (!available) toggle.checked = false;
    }
    if (!available && this.#els.wakeHint) {
      this.#els.wakeHint.hidden = false;
      this.#els.wakeHint.textContent =
        "Hands-free listening isn't available in this browser — use the mic button.";
    }
    this.#render();
  }

  // ---- press / release / tap dispatch ----

  /** Warm the session mint on an intent signal (mic-button pointerdown).
   * #press() already start()s synchronously for idle/denied/error, so on
   * the pointer path this is single-flight belt-and-braces (realtime.mjs
   * dedupes and connect() consumes the same in-flight mint); it exists so
   * the intent→mint wiring is uniform with the hands-free wake-arm
   * prefetch in conversation.mjs. COST: an unused mint holds a broker
   * concurrency slot + rate token until its ~60s server TTL lapses (no
   * release endpoint) — acceptable because it only fires on explicit
   * intent, never on page load (see prefetchSession in realtime.mjs). */
  #maybePrefetch() {
    const startable =
      this.#state === MicState.IDLE ||
      this.#state === MicState.DENIED ||
      this.#state === MicState.ERROR;
    if (!startable) return;
    if (this.#errorInfo && this.#errorInfo.permanent) return; // quota: no session will start
    try {
      this.#prefetch();
    } catch {
      /* prefetch is best-effort — start() mints fresh if it failed */
    }
  }

  #press() {
    this.#pressAt = Date.now();
    this.#pressStartedSession = false;
    switch (this.#state) {
      case MicState.IDLE:
      case MicState.DENIED:
      case MicState.ERROR:
        if (this.#errorInfo && this.#errorInfo.permanent) return; // quota: no retry
        this.#pressStartedSession = true;
        this.start();
        break;
      default:
        break; // live-state actions run on release/tap
    }
  }

  #release() {
    if (!this.#pressAt) return;
    const held = Date.now() - this.#pressAt >= HOLD_MS;
    this.#pressAt = 0;

    if (this.#pressStartedSession) {
      // Hold-to-talk: a hold that started this session commits on release.
      // A quick tap leaves the mic open for VAD to end the turn.
      if (held && this.#state === MicState.LISTENING) this.#commitTurn();
      this.#pressStartedSession = false;
      return;
    }
    this.#tapAction();
  }

  #tapAction() {
    switch (this.#state) {
      case MicState.IDLE:
      case MicState.DENIED:
      case MicState.ERROR:
        if (this.#errorInfo && this.#errorInfo.permanent) return;
        this.start();
        break;
      case MicState.LISTENING:
        if (this.#graceWaitingForWake) {
          this.#resumeFromGrace();
        } else {
          this.#commitTurn();
        }
        break;
      case MicState.THINKING:
        this.#cancelResponse();
        break;
      case MicState.SPEAKING:
        // Spec §2.2: mid-speech tap forces barge-in.
        if (this.#session) this.#session.bargeIn();
        break;
      default:
        break; // requesting/connecting/ending: control is disabled
    }
  }

  #commitTurn() {
    if (!this.#session || !this.#session.isConnected) return;
    try {
      this.#session.commitTurn();
      this.#setState(MicState.THINKING);
    } catch {
      /* datachannel raced closed — connectionlost handler owns recovery */
    }
  }

  #cancelResponse() {
    if (!this.#session || !this.#session.isConnected) return;
    try {
      this.#session.cancelResponse();
    } catch {
      return;
    }
    this.#setState(MicState.LISTENING);
    this.#armGrace();
  }

  // ---- session lifecycle ----

  /** Start a session: requesting-mic → connecting → live-listening.
   * Also the retry entry point from denied/error (spec §2.2). */
  async start() {
    if (
      this.#state !== MicState.IDLE &&
      this.#state !== MicState.DENIED &&
      this.#state !== MicState.ERROR
    ) {
      return;
    }
    this.#errorInfo = null;

    // Mic acquisition and the session mint run CONCURRENTLY (the promise is
    // handed to connect(), which mints while the permission/device settles) —
    // sequential awaits were adding the whole mint latency to every start.
    this.#setState(MicState.REQUESTING);
    const streamPromise = acquireMicStream({ deviceId: this.#getMicDeviceId() });

    this.#setState(MicState.CONNECTING);
    const session = this.#createSession();
    this.#session = session;
    this.#attachSession(session);
    this.#emit('sessioncreated', { session });

    try {
      await session.connect({ stream: streamPromise });
    } catch (err) {
      this.#session = null;
      // getUserMedia failures surface out of connect() now — route them to
      // the mic-specific error copy, everything else to connection errors.
      const name = err && err.name;
      if (
        name === 'NotAllowedError' ||
        name === 'SecurityError' ||
        name === 'NotFoundError' ||
        name === 'OverconstrainedError' ||
        name === 'NotReadableError' ||
        name === 'AbortError'
      ) {
        this.#handleMicError(err);
        return;
      }
      this.#handleConnectError(err);
      return;
    }
    // sessionready listener flips to live-listening; belt-and-braces here in
    // case the event fired before the listener attached.
    if (this.#state === MicState.CONNECTING) this.#setState(MicState.LISTENING);
  }

  /** Deliberate end (End control or grace timeout): flush point → teardown. */
  end() {
    if (!this.#session) {
      this.#clearGrace();
      this.#setState(MicState.IDLE);
      return;
    }
    if (this.#state === MicState.ENDING) return;
    this.#clearGrace();
    this.#setState(MicState.ENDING);
    this.#emit('ending', { session: this.#session });
    const s = this.#session;
    this.#session = null;
    s.close(); // 'closed' listener finishes the transition to idle
  }

  /** wakeword.mjs entry point: a local wake-word match (hands-free mode). */
  notifyWake() {
    if (!this.handsFree) return;
    if (this.#state === MicState.IDLE) {
      this.start();
      return;
    }
    if (this.#graceWaitingForWake) this.#resumeFromGrace();
  }

  #attachSession(session) {
    const on = (type, fn) => session.addEventListener(type, fn);

    on('sessionready', () => {
      if (this.#state === MicState.CONNECTING) this.#setState(MicState.LISTENING);
    });
    on('speechstarted', () => {
      this.#clearGrace();
      if (this.#state === MicState.LISTENING || this.#state === MicState.THINKING) {
        this.#setState(MicState.LISTENING);
      }
    });
    on('thinking', () => {
      if (LIVE_STATES.has(this.#state)) this.#setState(MicState.THINKING);
    });
    on('speaking', () => {
      this.#clearGrace();
      if (LIVE_STATES.has(this.#state)) this.#setState(MicState.SPEAKING);
    });
    on('speakingended', () => {
      if (this.#state === MicState.SPEAKING) {
        this.#setState(MicState.LISTENING);
        this.#armGrace();
      }
    });
    on('bargein', () => {
      this.#clearGrace();
      if (LIVE_STATES.has(this.#state)) this.#setState(MicState.LISTENING);
    });
    on('responsedone', () => {
      // Covers text-only responses that never enter `speaking`.
      if (this.#state === MicState.THINKING) {
        this.#setState(MicState.LISTENING);
        this.#armGrace();
      }
    });
    on('quotawarning', (e) => this.#emit('toast', { message: e.detail.message }));
    on('retrywait', (e) => {
      this.#setStatus(`Rate limited — retrying in ${e.detail.seconds}s…`);
    });
    on('connectionlost', () => {
      // Spec §2.5: transcript preserved; retry mints a fresh session.
      this.#session = null;
      this.#fail({
        code: 'connection_lost',
        message: 'Connection to the voice service dropped.',
        retryable: true,
      });
    });
    on('closed', () => {
      if (this.#state === MicState.ENDING) this.#setState(MicState.IDLE);
    });
  }

  // ---- keep-warm grace (spec §2.2 mode table) ----

  #armGrace() {
    this.#clearGrace();
    const handsFree = this.handsFree;
    const ms = this.#graceMs(handsFree);
    if (handsFree && this.#session) {
      // Hands-free: mute the session mic so only the wake word resumes the
      // conversation; the wake engine keeps its own audio pipeline.
      this.#session.setMicEnabled(false);
      this.#graceWaitingForWake = true;
      this.#setStatus(this.#idleHint(true));
    }
    // ms === 0 → no client-side timeout: the session stays live (mic
    // listening, or muted-awaiting-wake in hands-free) until the user ends
    // it or the provider closes the session server-side.
    if (ms > 0) {
      this.#graceTimer = setTimeout(() => {
        this.#graceTimer = null;
        this.end();
      }, ms);
    }
  }

  /** Post-reply session lifetime: the keepListeningSeconds setting when the
   * page wires it (0 = listen until the session ends — the default), else
   * the legacy per-mode constants. */
  #graceMs(handsFree) {
    if (this.#getKeepListeningSeconds) {
      const s = Number(this.#getKeepListeningSeconds());
      if (Number.isFinite(s) && s >= 0) return s * 1000;
    }
    return handsFree ? GRACE_MS.handsFree : GRACE_MS.ptt;
  }

  #clearGrace() {
    if (this.#graceTimer) {
      clearTimeout(this.#graceTimer);
      this.#graceTimer = null;
    }
    if (this.#graceWaitingForWake) {
      this.#graceWaitingForWake = false;
      if (this.#session) this.#session.setMicEnabled(true);
    }
  }

  #resumeFromGrace() {
    this.#clearGrace();
    this.#setState(MicState.LISTENING);
  }

  // ---- errors (spec §2.5 table) ----

  #handleMicError(err) {
    const name = err && err.name;
    if (name === 'NotAllowedError' || name === 'SecurityError') {
      this.#errorInfo = {
        message:
          "Microphone access is blocked. Enable it in your browser's site settings, then try again.",
        retryable: true,
        permanent: false,
      };
      this.#setState(MicState.DENIED);
      // The browser gives one NotAllowedError for two very different
      // situations — a dismissed prompt (retry WILL re-prompt) and a
      // standing site-level block (retry silently fails; the browser never
      // asks again until the user unblocks it themselves). Refine the copy
      // and, for a standing block, show step-by-step unblock help and watch
      // the Permissions API so flipping the toggle recovers automatically.
      void this.#refineDeniedState();
    } else if (name === 'NotFoundError' || name === 'OverconstrainedError') {
      this.#fail({
        code: 'no_mic',
        message: 'No microphone found. Connect one and try again.',
        retryable: true,
      });
      return;
    } else {
      this.#fail({
        code: 'mic_failed',
        message: 'Could not open the microphone. Try again.',
        retryable: true,
      });
      return;
    }
    this.#emit('error', { message: this.#errorInfo.message, code: 'mic_denied', retryable: true });
    this.#render();
  }

  /** Query the Permissions API to tell a dismissed prompt apart from a
   * standing block, adjust the denied copy, and arm auto-recovery. Fully
   * guarded: browsers without {name:'microphone'} support (e.g. older
   * Firefox throws) keep the generic copy and still get the help panel —
   * the user's browser demonstrably isn't going to prompt on its own. */
  async #refineDeniedState() {
    let status = null;
    try {
      status = await navigator.permissions.query({ name: 'microphone' });
    } catch {
      /* Permissions API unavailable for microphone — fall through */
    }
    if (this.#state !== MicState.DENIED) return; // user already moved on

    if (status && status.state === 'prompt') {
      // The prompt was dismissed (or auto-dismissed) — a retry re-prompts,
      // so guidance UI would only be noise.
      this.#errorInfo = {
        message: 'The microphone prompt was closed without an answer. Tap the mic and choose Allow when your browser asks.',
        retryable: true,
        permanent: false,
      };
      this.#render();
      return;
    }

    // Standing block (state === 'denied') or unknowable: the browser will
    // NOT ask again — say so plainly and show the unblock steps.
    this.#errorInfo = {
      message: 'Your browser has the microphone blocked for this site, so it won’t ask for permission again. Unblock it with the steps below.',
      retryable: true,
      permanent: false,
    };
    this.#render();
    this.#showMicHelp();
    if (status) this.#watchPermission(status);
  }

  /** Auto-recovery: the PermissionStatus `change` event fires when the user
   * flips the site's mic setting in browser UI. granted/prompt while we sit
   * in `denied` → clear the block state so the next tap just works (no
   * auto-start: a permission flip is not a user gesture, and Chrome usually
   * offers its own reload banner anyway). */
  #watchPermission(status) {
    this.#unwatchPermission();
    this.#permStatus = status;
    status.onchange = () => {
      if (this.#state !== MicState.DENIED) return;
      if (status.state === 'granted' || status.state === 'prompt') {
        this.#unwatchPermission();
        this.#hideMicHelp();
        this.#setState(MicState.IDLE);
        this.#setStatus('Microphone unblocked — tap the mic to talk.');
        this.#emit('toast', { message: 'Microphone unblocked — tap the mic to talk.' });
      }
    };
  }

  #unwatchPermission() {
    if (this.#permStatus) {
      this.#permStatus.onchange = null;
      this.#permStatus = null;
    }
  }

  /** Fill and reveal the #micHelp panel with unblock steps for the user's
   * browser. Built with DOM APIs (no innerHTML) from static strings. */
  #showMicHelp() {
    const el = this.#els.micHelp;
    if (!el) return;

    const ua = navigator.userAgent || '';
    const isEdge = ua.includes('Edg/');
    const isFirefox = ua.includes('Firefox/');
    const isSafari = !ua.includes('Chrome/') && ua.includes('Safari/');

    let steps;
    if (isFirefox) {
      steps = [
        'Click the microphone icon with a slash through it at the left of the address bar.',
        'Next to "Use the microphone", click the X to clear "Blocked" (or "Blocked Temporarily").',
        'Tap the mic below and choose Allow when Firefox asks again.',
      ];
    } else if (isSafari) {
      steps = [
        'Open the Safari menu → "Settings for This Website…".',
        'Set Microphone to "Allow".',
        'Tap the mic below to try again.',
      ];
    } else {
      const settingsUrl = isEdge ? 'edge://settings/content/microphone' : 'chrome://settings/content/microphone';
      steps = [
        'Click the tune (or lock) icon at the left of the address bar.',
        'Turn Microphone ON (or open "Site settings" and set Microphone to "Allow").',
        'Reload if the browser offers it, then tap the mic below.',
        `Still blocked? Paste ${settingsUrl} into the address bar and remove this site from the "Not allowed" list.`,
      ];
    }

    el.replaceChildren();
    const title = document.createElement('p');
    title.className = 'mic-help__title';
    title.textContent = 'To unblock the microphone:';
    el.appendChild(title);
    const ol = document.createElement('ol');
    ol.className = 'mic-help__steps';
    for (const s of steps) {
      const li = document.createElement('li');
      li.textContent = s;
      ol.appendChild(li);
    }
    el.appendChild(ol);
    const note = document.createElement('p');
    note.className = 'mic-help__note';
    note.textContent =
      'On Windows, also check Settings → Privacy & security → Microphone → microphone access is on.';
    el.appendChild(note);

    // Reuse the existing mic-check dialog when the page has it.
    const testBtn = document.getElementById('micTestBtn');
    if (testBtn) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'ln-btn ln-btn--ghost mic-help__test';
      b.textContent = 'Run the mic check';
      b.addEventListener('click', () => testBtn.click());
      el.appendChild(b);
    }

    el.hidden = false;
  }

  #hideMicHelp() {
    const el = this.#els.micHelp;
    if (el) {
      el.hidden = true;
      el.replaceChildren();
    }
  }

  #handleConnectError(err) {
    if (err instanceof RealtimeError) {
      // Backend-originated failures carry a txId + the raw backend message;
      // the friendly line goes to `message`, the detail/ref to the banner.
      const txId = err.txId || '';
      const detail = err.message || '';
      switch (err.code) {
        case 'quota_exceeded': {
          const scope = err.kind === 'monthly_tokens' ? "this month's" : "today's";
          this.#fail({
            code: err.code,
            message: `You've reached ${scope} voice limit. It resets ${formatResetAt(err.resetAt)}. You can still type below.`,
            retryable: false,
            permanent: true,
            txId,
            detail,
          });
          return;
        }
        case 'rate_limited':
          this.#fail({
            code: err.code,
            message: 'Too many requests — try again in a few seconds.',
            retryable: true,
            txId,
            detail,
          });
          return;
        case 'broker_unavailable':
          this.#fail({
            code: err.code,
            message: "Couldn't reach the voice service. You can still type below.",
            retryable: true,
            txId,
            detail,
          });
          return;
        default:
          this.#fail({
            code: err.code,
            message: 'Connection to the voice service dropped.',
            retryable: true,
            txId,
            detail,
          });
          return;
      }
    }
    if (err && err.name === 'AuthLostError') return; // toolclient redirects
    this.#fail({
      code: 'connect_failed',
      message: 'Connection to the voice service dropped.',
      retryable: true,
    });
  }

  #fail({ code, message, retryable, permanent = false, txId = '', detail = '' }) {
    this.#clearGrace();
    this.#errorInfo = { message, retryable, permanent };
    this.#setState(MicState.ERROR);
    this.#emit('error', { message, code, retryable, txId, detail });
  }

  // ---- rendering (state pill / status text / button, spec §2.8 ARIA) ----

  #setState(next) {
    const prev = this.#state;
    if (prev === next) {
      this.#render();
      return;
    }
    this.#state = next;
    if (next !== MicState.ERROR && next !== MicState.DENIED) this.#errorInfo = null;
    if (next !== MicState.DENIED) {
      this.#hideMicHelp();
      this.#unwatchPermission();
    }
    if (!LIVE_STATES.has(next)) this.#graceWaitingForWake = false;
    this.#render();
    this.#emit('statechange', { state: next, prev });
  }

  #idleHint(forWakeResume = false) {
    // The status line under the visualizer carries the how-to-talk copy
    // (owner spec 2026-07-18 — the old static ptt-hint element is gone):
    // wake ON tells the user the phrase, wake OFF tells them Space/mic.
    if (this.handsFree) {
      const phrase = this.#getWakePhrase();
      if (forWakeResume) {
        return phrase ? `Say “${phrase}” or tap the mic to continue` : 'Say your wake phrase or tap the mic to continue';
      }
      return phrase ? `Say “${phrase}” anytime` : 'Hands-free listening';
    }
    return 'Press SPACE or click mic to talk';
  }

  #statusFor(state) {
    if (this.#errorInfo) return this.#errorInfo.message;
    switch (state) {
      case MicState.IDLE:
        return this.#idleHint();
      case MicState.REQUESTING:
        return 'Waiting for microphone permission…';
      case MicState.CONNECTING:
        return 'Connecting…';
      case MicState.LISTENING:
        return this.#graceWaitingForWake ? this.#idleHint(true) : 'Listening — go ahead';
      case MicState.THINKING:
        return 'Thinking…';
      case MicState.SPEAKING:
        return 'Speaking — tap the mic to interrupt';
      case MicState.ENDING:
        return 'Ending the conversation…';
      default:
        return '';
    }
  }

  #setStatus(text) {
    if (this.#els.status) this.#els.status.textContent = text;
  }

  #render() {
    const state = this.#state;
    const meta = STATE_META[state];
    const { button, pill, pillText } = this.#els;

    if (button) {
      button.disabled = !!meta.disabled;
      button.setAttribute('aria-pressed', meta.pressed ? 'true' : 'false');
      button.setAttribute('aria-label', meta.aria);
      button.dataset.state = state;
      const labelEl = button.querySelector('[data-ln-label]');
      if (labelEl) labelEl.textContent = meta.label;
    }
    if (pill) {
      pill.dataset.state = state;
      if (!pillText) pill.textContent = meta.pill;
    }
    if (pillText) pillText.textContent = meta.pill;
    this.#setStatus(this.#statusFor(state));
  }
}

/** Convenience factory matching the other modules' style. */
export function createMicController(options) {
  return new MicController(options);
}
