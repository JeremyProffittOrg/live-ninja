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
// 'ending', and each created session's 'sessionready' / 'closed' /
// 'connectionlost' LIFECYCLE events. Turns themselves are fed by the
// page's transcript-rendering layer via addTurn (see attachSession's
// comment) so the sink stores exactly what the UI rendered.
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
  // Called synchronously right before the sink marks a session final
  // (End button / session closed / pagehide). The page uses it to drain
  // any turns it rendered whose finalizing event never arrived (the GA
  // user-transcription final routinely lags — or never lands before the
  // page dies), so they still make the final batch. Must be idempotent:
  // it can fire more than once per session.
  onBeforeFinal = null,
  // Called with the sessionId when a final flush is being built; returns
  // {usd, textTokens, audioTokens} (the page's per-session cost estimate,
  // see conversation.mjs attachCostBadge) or null when unknown. The server
  // persists it onto the conversation's history record.
  getSessionCost = null,
} = {}) {
  let enabled = true;
  let sessionId = '';
  let engine = '';
  // Sequence numbers are assigned at FLUSH time (not enqueue time) so a
  // late-arriving user turn can still be inserted before the assistant
  // reply it prompted (addTurn's `before`) — server rows keep transcript
  // order. Entries that have been attempted keep their seq forever (the
  // server dedupes on seq, so a retried batch must resend identical rows).
  let nextSeq = 0;
  let pending = []; // [{seq: number|null, role, text, engine}]
  let timer = null;
  let inFlight = false;
  let flushAgain = false;
  // Set when the session has ended: the next flush carries final:true so the
  // server releases the session's concurrency slot and kicks topic
  // extraction. Cleared only once a final flush succeeds.
  let finalPending = false;
  // Latched once a final flush for this session SUCCEEDS. The server kicks
  // topic extraction on every final:true it sees, so re-marking final (End
  // button, then pagehide 3s later) used to double-invoke the extractor and
  // write duplicate CONV rows — final is sent exactly once per session.
  let finalSent = false;

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
    if (sessionId && (pending.length || finalPending)) void flush();
    sessionId = id || '';
    engine = sessionEngine || '';
    nextSeq = 0;
    pending = [];
    finalPending = false;
    finalSent = false;
  }

  /** Queue one finalized turn. role: "user" | "assistant".
   * @param {'user'|'assistant'} role
   * @param {string} text
   * @param {{before?: object}} [opts] `before` is a handle previously
   *   returned by addTurn: if that entry is still queued and un-attempted,
   *   the new turn is inserted before it (a late user-transcription final
   *   lands above the assistant reply it prompted, mirroring the UI's
   *   anchor insert). An already-attempted/flushed anchor falls back to
   *   append — its seq was sent to the server and must not shift.
   * @returns {object|null} an opaque handle usable as a later `before`
   *   anchor, or null when the turn was dropped (disabled/empty). */
  function addTurn(role, text, { before = null } = {}) {
    if (!enabled || !sessionId) return null;
    const trimmed = (text || '').trim();
    if (!trimmed) return null; // server skips empty turns anyway — don't ship them
    const entry = { seq: null, role, text: trimmed, engine };
    const anchorIdx = before ? pending.indexOf(before) : -1;
    if (anchorIdx !== -1 && before.seq === null) {
      pending.splice(anchorIdx, 0, entry);
    } else {
      pending.push(entry);
    }
    if (pending.length > MAX_PENDING_TURNS) {
      pending = pending.slice(pending.length - MAX_PENDING_TURNS);
    }
    if (pending.length >= maxBatchTurns) {
      void flush();
    } else {
      armTimer();
    }
    return entry;
  }

  /** Send everything queued. `keepalive` marks the pagehide path (fetch
   * keepalive so the request survives the document going away). Failed
   * batches are re-queued at the front for the next flush. */
  async function flush({ keepalive = false } = {}) {
    if (!sessionId || (pending.length === 0 && !finalPending)) return;
    if (inFlight) {
      flushAgain = true;
      return;
    }
    clearTimer();

    const batch = pending;
    const isFinal = finalPending;
    pending = [];
    // Seqs are assigned at first attempt, in queue order (late inserts got
    // their slot via addTurn's `before`); an entry keeps its seq across
    // retries so a re-sent batch is a server-side no-op per row.
    for (const entry of batch) {
      if (entry.seq === null) entry.seq = nextSeq++;
    }
    inFlight = true;
    let ok = false;
    try {
      const json = {
        sessionId,
        turns: batch.map(({ seq, role, text, engine: turnEngine }) => ({ seq, role, text, engine: turnEngine })),
        final: isFinal,
      };
      if (isFinal && typeof getSessionCost === 'function') {
        // Ship the page's cost estimate with the one final flush so the
        // server can persist it on the conversation record. Best-effort:
        // a broken getter must never block the final flush.
        try {
          const cost = getSessionCost(sessionId);
          const usd = cost && Number.isFinite(cost.usd) ? cost.usd : 0;
          if (cost && (usd > 0 || cost.textTokens > 0 || cost.audioTokens > 0)) {
            json.cost = {
              usd,
              textTokens: Math.max(0, Math.round(cost.textTokens || 0)),
              audioTokens: Math.max(0, Math.round(cost.audioTokens || 0)),
            };
          }
        } catch {
          /* cost is optional */
        }
      }
      const resp = await authFetch(endpoint, {
        method: 'POST',
        json,
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
    if (isFinal) {
      finalPending = false;
      finalSent = true; // exactly one final per session — never re-marked
    }
    if (flushAgain || pending.length >= maxBatchTurns) {
      flushAgain = false;
      void flush();
    } else if (pending.length) {
      armTimer();
    }
  }

  /** Invokes the page's pre-final drain hook (idempotent, never throws
   * out of the sink). Runs only while the session hasn't sent its final. */
  function runBeforeFinal() {
    if (typeof onBeforeFinal !== 'function') return;
    try {
      onBeforeFinal();
    } catch {
      /* a drain failure must never block the final flush */
    }
  }

  /** Final flush for a finished session; keeps sessionId so a late retry
   * can still land, but stops the cadence timer. Marks the flush final so
   * the server releases the realtime concurrency slot and runs topic
   * extraction — a final-only flush with zero turns is valid. Idempotent:
   * once a final flush has SUCCEEDED, later calls (ending + closed, End
   * button + pagehide) only flush leftovers — they never re-mark final,
   * so the server's topic extraction runs exactly once per session. */
  function endSession() {
    clearTimer();
    if (!finalSent) {
      runBeforeFinal();
      finalPending = true;
    }
    void flush();
  }

  function setEnabled(on) {
    enabled = !!on;
    if (!enabled) {
      clearTimer();
      pending = [];
    }
  }

  // Session LIFECYCLE only. Turn capture deliberately does NOT live here
  // anymore: the page's transcript-rendering layer (conversation.mjs
  // attachTranscriptRendering) is the single place that knows every path a
  // turn can arrive by — streamed deltas, a late authoritative final, the
  // insert-before-anchor reorder — and it feeds addTurn so the sink saves
  // exactly what the UI rendered, exactly once. (Capturing 'userfinal'
  // here too would double-log every user turn.)
  function attachSession(session) {
    session.addEventListener('sessionready', (e) => {
      beginSession(e.detail.sessionId, e.detail.model || 'openai-realtime');
    });
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
  window.addEventListener('pagehide', () => {
    // The document is going away and any live session dies with it — mark
    // final so the server frees the concurrency slot instead of letting it
    // block new mints for the rest of the 10-minute cap. (visibilitychange
    // alone is NOT final: a backgrounded tab keeps its session.) If the
    // session's final already went out (End button pressed, then the user
    // navigated away), this only flushes stragglers — re-marking final
    // here was exactly the double-CONV bug.
    if (sessionId && !finalSent) {
      runBeforeFinal();
      finalPending = true;
    }
    void flush({ keepalive: true });
  });

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
