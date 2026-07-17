// transcriptsink.mjs — batched turn logging to POST /api/v1/transcript
// (docs/web-ui-spec.md §2.4 step 5: "batched and flushed every ~5s or on
// state transition to ending", plus a 25-turn size trigger per the M3 task
// brief).
//
// Server contract (internal/webapp/api_routes.go handleTranscript):
//   POST /api/v1/transcript
//   { sessionId, turns: [{seq, role, text, engine}] }
// Writes are conditional puts keyed LOG#<sessionId>#<seq> — re-sending the
// same seq is a server-side no-op (ErrAlreadyExists tolerated), so retrying
// a failed batch can never duplicate turns.
//
// Wiring: `sink.observe(micController)` subscribes to 'sessioncreated' /
// 'ending', and each created session's 'sessionready' / 'userfinal' /
// 'assistantfinal' / 'closed' / 'connectionlost' events. Or drive it
// manually with beginSession/addTurn/flush.
//
// Privacy: `setEnabled(false)` (settings.privacy.storeTranscripts === false)
// drops turns client-side; the server's retention policy remains the
// canonical enforcement point.

import { authFetch } from './toolclient.mjs';

const ENDPOINT = '/api/v1/transcript';
const FLUSH_INTERVAL_MS = 5_000;
const MAX_BATCH_TURNS = 25;
// Bounded retry buffer: if the API is down, keep at most this many pending
// turns (oldest dropped first) so a long outage can't grow memory unbounded.
const MAX_PENDING_TURNS = 200;

export function createTranscriptSink({
  endpoint = ENDPOINT,
  flushIntervalMs = FLUSH_INTERVAL_MS,
  maxBatchTurns = MAX_BATCH_TURNS,
} = {}) {
  let enabled = true;
  let sessionId = '';
  let engine = '';
  let seq = 0;
  let pending = []; // [{seq, role, text, engine}]
  let timer = null;
  let inFlight = false;
  let flushAgain = false;

  function armTimer() {
    if (timer !== null) return;
    timer = setTimeout(() => {
      timer = null;
      void flush();
    }, flushIntervalMs);
  }

  function clearTimer() {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  }

  /** Start (or restart) logging for a session. Flushes anything left over
   * from a previous session first. */
  function beginSession(id, sessionEngine = '') {
    if (sessionId && pending.length) void flush();
    sessionId = id || '';
    engine = sessionEngine || '';
    seq = 0;
    pending = [];
  }

  /** Queue one finalized turn. role: "user" | "assistant". */
  function addTurn(role, text) {
    if (!enabled || !sessionId) return;
    const trimmed = (text || '').trim();
    if (!trimmed) return; // server skips empty turns anyway — don't ship them
    pending.push({ seq: seq++, role, text: trimmed, engine });
    if (pending.length > MAX_PENDING_TURNS) {
      pending = pending.slice(pending.length - MAX_PENDING_TURNS);
    }
    if (pending.length >= maxBatchTurns) {
      void flush();
    } else {
      armTimer();
    }
  }

  /** Send everything queued. `keepalive` marks the pagehide path (fetch
   * keepalive so the request survives the document going away). Failed
   * batches are re-queued at the front for the next flush. */
  async function flush({ keepalive = false } = {}) {
    if (!sessionId || pending.length === 0) return;
    if (inFlight) {
      flushAgain = true;
      return;
    }
    clearTimer();

    const batch = pending;
    pending = [];
    inFlight = true;
    let ok = false;
    try {
      const resp = await authFetch(endpoint, {
        method: 'POST',
        json: { sessionId, turns: batch },
        keepalive,
      });
      ok = resp.ok;
    } catch {
      ok = false;
    } finally {
      inFlight = false;
    }

    if (!ok) {
      // Same-seq resends are server-side no-ops, so requeueing is safe.
      pending = batch.concat(pending).slice(-MAX_PENDING_TURNS);
      if (!keepalive) armTimer(); // retry on the normal cadence
      return;
    }
    if (flushAgain || pending.length >= maxBatchTurns) {
      flushAgain = false;
      void flush();
    } else if (pending.length) {
      armTimer();
    }
  }

  /** Final flush for a finished session; keeps sessionId so a late retry
   * can still land, but stops the cadence timer. */
  function endSession() {
    clearTimer();
    void flush();
  }

  function setEnabled(on) {
    enabled = !!on;
    if (!enabled) {
      clearTimer();
      pending = [];
    }
  }

  function attachSession(session) {
    session.addEventListener('sessionready', (e) => {
      beginSession(e.detail.sessionId, e.detail.model || 'openai-realtime');
    });
    session.addEventListener('userfinal', (e) => addTurn('user', e.detail.text));
    session.addEventListener('assistantfinal', (e) => addTurn('assistant', e.detail.text));
    session.addEventListener('closed', () => endSession());
    session.addEventListener('connectionlost', () => endSession());
  }

  /** One-call wiring against a MicController (mic.mjs). */
  function observe(micController) {
    micController.addEventListener('sessioncreated', (e) => attachSession(e.detail.session));
    micController.addEventListener('ending', () => endSession());
  }

  // Last-chance flush when the tab is backgrounded or closed. pagehide +
  // visibilitychange(hidden) together cover desktop and mobile lifecycles;
  // fetch keepalive (not sendBeacon — it can't carry the Authorization
  // header) lets the request outlive the document.
  const onHidden = () => {
    if (document.visibilityState === 'hidden') void flush({ keepalive: true });
  };
  document.addEventListener('visibilitychange', onHidden);
  window.addEventListener('pagehide', () => void flush({ keepalive: true }));

  return {
    beginSession,
    addTurn,
    flush,
    endSession,
    setEnabled,
    attachSession,
    observe,
    /** test/diagnostic visibility */
    get pendingCount() {
      return pending.length;
    },
    get sessionId() {
      return sessionId;
    },
  };
}
