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

import { RealtimeSession, RealtimeError, acquireMicStream } from './realtime.mjs';

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
 *   error          {message, code, retryable}
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
  #els;
  #createSession;
  #getMicDeviceId;
  #getWakePhrase;

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
      endButtons: options.endButtons ?? Array.from(doc.querySelectorAll('[data-ln-end]')),
    };
    this.#createSession = options.createSession || (() => new RealtimeSession());
    this.#getMicDeviceId = options.getMicDeviceId || (() => null);
    this.#getWakePhrase = options.getWakePhrase || (() => '');

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

    this.#setState(MicState.REQUESTING);
    let stream;
    try {
      stream = await acquireMicStream({ deviceId: this.#getMicDeviceId() });
    } catch (err) {
      this.#handleMicError(err);
      return;
    }

    this.#setState(MicState.CONNECTING);
    const session = this.#createSession();
    this.#session = session;
    this.#attachSession(session);
    this.#emit('sessioncreated', { session });

    try {
      await session.connect({ stream });
    } catch (err) {
      for (const t of stream.getTracks()) t.stop();
      this.#session = null;
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
    const ms = handsFree ? GRACE_MS.handsFree : GRACE_MS.ptt;
    if (handsFree && this.#session) {
      // Hands-free: mute the session mic so only the wake word resumes the
      // conversation; the wake engine keeps its own audio pipeline.
      this.#session.setMicEnabled(false);
      this.#graceWaitingForWake = true;
      this.#setStatus(this.#idleHint(true));
    }
    this.#graceTimer = setTimeout(() => {
      this.#graceTimer = null;
      this.end();
    }, ms);
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

  #handleConnectError(err) {
    if (err instanceof RealtimeError) {
      switch (err.code) {
        case 'quota_exceeded': {
          const scope = err.kind === 'monthly_tokens' ? "this month's" : "today's";
          this.#fail({
            code: err.code,
            message: `You've reached ${scope} voice limit. It resets ${formatResetAt(err.resetAt)}. You can still type below.`,
            retryable: false,
            permanent: true,
          });
          return;
        }
        case 'rate_limited':
          this.#fail({
            code: err.code,
            message: 'Too many requests — try again in a few seconds.',
            retryable: true,
          });
          return;
        case 'broker_unavailable':
          this.#fail({
            code: err.code,
            message: "Couldn't reach the voice service. You can still type below.",
            retryable: true,
          });
          return;
        default:
          this.#fail({
            code: err.code,
            message: 'Connection to the voice service dropped.',
            retryable: true,
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

  #fail({ code, message, retryable, permanent = false }) {
    this.#clearGrace();
    this.#errorInfo = { message, retryable, permanent };
    this.#setState(MicState.ERROR);
    this.#emit('error', { message, code, retryable });
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
    if (!LIVE_STATES.has(next)) this.#graceWaitingForWake = false;
    this.#render();
    this.#emit('statechange', { state: next, prev });
  }

  #idleHint(forWakeResume = false) {
    if (this.handsFree) {
      const phrase = this.#getWakePhrase();
      if (forWakeResume) {
        return phrase ? `Say “${phrase}” or tap the mic to continue` : 'Say your wake phrase or tap the mic to continue';
      }
      return 'Tap the mic or say your wake phrase to start';
    }
    return 'Tap the mic to start';
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
