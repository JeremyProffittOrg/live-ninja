// realtime.mjs — OpenAI Realtime WebRTC transport (docs/web-ui-spec.md §2.4).
//
// Responsibilities (plan.md M3 / WS-D "realtime JS"):
//   - Session mint: `GET /api/v1/realtime/session` (via toolclient's
//     authFetch) with typed 402 quota_exceeded / 429 rate_limited / 502
//     broker_unavailable handling (spec §2.5 table). 429 auto-retries once
//     after `retryAfterSeconds` before surfacing an error. `prefetchSession`
//     lets intent signals (mic pointerdown, hands-free arm) start the mint
//     early; connect() consumes it and overlaps the WebRTC offer + ICE
//     gathering with whatever mint wait remains (latency plan #4).
//   - WebRTC: RTCPeerConnection + mic track (AEC/NS/AGC true), `oai-events`
//     datachannel, SDP offer → POST https://api.openai.com/v1/realtime/calls
//     (Authorization: Bearer <ephemeral>, Content-Type: application/sdp) →
//     answer SDP; remote audio via ontrack → hidden <audio> + GainNode.
//   - Event routing: transcript deltas, user transcription, tool calls
//     (delegated to toolclient's dispatcher), speaking/turn lifecycle.
//   - Barge-in: on `input_audio_buffer.speech_started` while assistant audio
//     is playing → 30ms gain ramp to silence + `response.cancel` +
//     `output_audio_buffer.clear`, then immediate return to listening (never
//     waits for the cancel ack, spec §2.2). In "Patient" mode (settings Mic
//     pickup = low → the minted session has turn_detection.interrupt_response
//     false, so the SERVER no longer truncates on VAD blips) the client gates
//     itself the same way: speech_started only soft-ducks the output and arms
//     a confirm window; sustained speech (or the server committing the user's
//     turn) escalates to the full barge-in, while a short ambient-noise blip
//     (speech_stopped inside the window) just restores the audio. This is the
//     false-barge-in fix: noise dips the assistant briefly instead of cutting
//     it off mid-sentence, and a real close-mic interjection still interrupts.
//
// The mic *state machine* + UI binding live in mic.mjs — this module is the
// transport and emits lifecycle events for it. Integration sketch:
//
//   import { RealtimeSession, acquireMicStream } from './realtime.mjs';
//   const stream = await acquireMicStream({ deviceId: settings.micDeviceId });
//   const session = new RealtimeSession();
//   session.addEventListener('assistantdelta', (e) => transcript.append(e.detail));
//   await session.connect({ stream });
//
// ---- M12 Nova Sonic bridge (FR-VE-01..03) --------------------------------
// `GET /api/v1/realtime/session` returns one of three shapes, resolved from
// the device's `voiceEngine` pin (contracts/settings.schema.json):
//   { mode:"openai-direct", clientSecret, model, voice, ... }  ← default path,
//     the WebRTC-to-OpenAI transport below (unchanged).
//   { mode:"nova-bridge",  wsUrl, token, model?, voice?, sessionId }  ← a
//     Nova-pinned device; audio flows device⇄backend-bridge⇄Bedrock Nova
//     Sonic. Bedrock's bidirectional stream is server-held (SigV4 + HTTP/2),
//     so it can't be client-direct — the browser instead opens a WebSocket to
//     the bridge and streams raw PCM16 frames both ways.
//   { mode:"gemini-direct", geminiEndpoint, accessToken:{value, expiresAt,
//     newSessionExpiresAt}, sessionConfig, model, voice, sessionId, rates }
//     ← a gemini-flash-live-pinned device (M13, gemini-plan.md §3.4): the
//     browser opens a WSS DIRECTLY to Google's Live API with the single-use
//     ephemeral token in the URL and sends {"setup": sessionConfig} verbatim
//     on open. Structurally a hybrid: the Nova WSS/PCM skeleton (16 kHz in,
//     24 kHz out) with client-direct auth like OpenAI — but uplink audio is
//     JSON+base64 frames, never raw binary (see #connectGemini).
//
// This class exposes ONE public surface (the same events, same methods) for
// all modes, so mic.mjs / the conversation page don't branch. A response
// with no `mode` (the pre-M12 shape) is treated as openai-direct.
//
// Nova bridge wire protocol (bridge ⇄ client; the bridge normalizes Nova
// Sonic and OpenAI events to the FR-VE-01 common schema before it reaches
// either surface — see internal/voiceengine):
//   * Binary WS frames  = raw little-endian mono PCM16 audio. Client→bridge
//     frames are mic input (`audio.in`); bridge→client frames are assistant
//     output (`audio.out`). Sample rates are announced in `session.start`.
//   * Text WS frames    = JSON control/lifecycle events:
//       {type:"session.start", input:{sampleRate}, output:{sampleRate},
//                              model?, voice?, sessionId?}
//       {type:"turn.start"|"turn.end", role:"user"|"assistant"}
//       {type:"transcript", role, delta?, text?, final?, itemId?}
//       {type:"tool.call", tool, callId, args}    (args: object or JSON string)
//       {type:"speaking.start"|"speaking.stop"}   (optional; also inferred
//                                                   from audio.out frames)
//       {type:"error", error:{message}}
//     Client→bridge control the RealtimeSession emits (translated from the
//     shared OpenAI-shaped calls in #novaSend):
//       {type:"tool.result", callId, result}
//       {type:"user.text", text}   {type:"turn.commit"}   {type:"barge-in"}
//
// CSP note: openai-direct reaches https://api.openai.com, which the page
// `connect-src` allowlist names explicitly (spec §0). nova-bridge is NOT on a
// subdomain — the session bootstrap hands back a same-origin wss URL
// (wss://<page-origin>/nova/session; the bridge ALB is a /nova/* behavior on
// the same CloudFront distribution), so it is already covered by `connect-src
// 'self'` and needs no extra allowlist entry. gemini-direct reaches
// wss://generativelanguage.googleapis.com, also named explicitly in
// `connect-src` (internal/webapp/pages_routes.go).

import { authFetch, createToolDispatcher, ApiError } from './toolclient.mjs';

const SESSION_PATH = '/api/v1/realtime/session';
const OPENAI_CALLS_URL = 'https://api.openai.com/v1/realtime/calls';
const DC_OPEN_TIMEOUT_MS = 10_000;
// Trickle-less ICE wait cap. Host/srflx candidates land well inside this;
// the old 2s cap added up to 1.5s of dead air to every connect (owner:
// "takes a bit to pick up").
const ICE_GATHER_TIMEOUT_MS = 600;
const RATE_LIMIT_MAX_WAIT_S = 15;
const DUCK_RAMP_S = 0.03; // spec §2.2: ~30ms ramp, not an abrupt cut
// Patient-mode (interrupt_response:false) barge-in gate: how long VAD speech
// must persist before a speech_started escalates to a full barge-in, and the
// gain the output ducks to while the gate is pending (audible dip, not
// silence — a false trigger should feel like a flicker, not a dropout).
const BARGE_CONFIRM_MS = 350;
const PENDING_DUCK_LEVEL = 0.15;

// Nova bridge: how long to wait for the bridge's `session.start` after the
// WebSocket opens, and the PCM sample rates Nova Sonic uses when the bridge
// doesn't override them in `session.start` (16 kHz in, 24 kHz out). The
// Gemini Live transport (M13) runs the exact same rates and shares this
// capture/playback plumbing.
const NOVA_START_TIMEOUT_MS = 12_000;
const NOVA_DEFAULT_IN_RATE = 16_000;
const NOVA_DEFAULT_OUT_RATE = 24_000;
// ScriptProcessor block size for mic capture (frames per PCM chunk sent).
const NOVA_CAPTURE_FRAMES = 2048;

// Gemini Live (M13): how long to wait for `setupComplete` after the socket
// opens, and how close to the ephemeral token's expiresAt a goAway
// reconnect still reuses the same token — inside the skew the client
// re-fetches the session bootstrap (fresh token) before resuming.
const GEMINI_SETUP_TIMEOUT_MS = 12_000;
const GEMINI_TOKEN_EXPIRY_SKEW_MS = 60_000;
// Gemini frames arrive as binary (ArrayBuffer) JSON — audio rides base64
// INSIDE the JSON, so every frame decodes through here (never raw PCM).
const GEMINI_TEXT_DECODER = new TextDecoder();

/** Typed error for session bootstrap / connection failures. `code` is the
 * server envelope's `error` (quota_exceeded, rate_limited,
 * broker_unavailable, ...) or a client-side code (sdp_failed). */
export class RealtimeError extends Error {
  constructor(code, message, extras = {}) {
    super(message);
    this.name = 'RealtimeError';
    this.code = code;
    // quota_exceeded: kind ("daily_minutes"|"monthly_tokens"), used, limit,
    // resetAt. rate_limited: retryAfterSeconds. Any backend-originated error
    // also carries `txId` (the transaction ref) for the reportable banner.
    Object.assign(this, extras);
  }

  static fromApiError(err) {
    if (!(err instanceof ApiError)) {
      return new RealtimeError('mint_failed', 'Could not start a voice session.');
    }
    const b = err.body || {};
    const txId = err.txId || '';
    switch (err.code) {
      case 'quota_exceeded':
        return new RealtimeError('quota_exceeded', err.message, {
          kind: b.kind || '',
          used: b.used,
          limit: b.limit,
          resetAt: b.resetAt || '',
          txId,
        });
      case 'rate_limited':
        return new RealtimeError('rate_limited', err.message, {
          retryAfterSeconds: Number(b.retryAfterSeconds) || 0,
          txId,
        });
      case 'broker_unavailable':
        return new RealtimeError('broker_unavailable', err.message, { txId });
      default:
        return new RealtimeError(err.code || 'mint_failed', err.message, { txId });
    }
  }
}

/**
 * Acquire the local mic with the mandated processing constraints
 * (echoCancellation/noiseSuppression/autoGainControl all true — AEC is
 * load-bearing for barge-in: without it the assistant's own audio would
 * trigger speech_started). `deviceId` comes from settings.micDeviceId; if
 * that saved device is gone (OverconstrainedError), falls back to the
 * system default per spec §3.3 — the caller can detect the fallback via the
 * returned stream's track settings if it wants to toast about it.
 */
export async function acquireMicStream({ deviceId = null } = {}) {
  const base = { echoCancellation: true, noiseSuppression: true, autoGainControl: true };
  if (deviceId) {
    try {
      return await navigator.mediaDevices.getUserMedia({
        audio: { ...base, deviceId: { exact: deviceId } },
      });
    } catch (err) {
      if (err && err.name === 'OverconstrainedError') {
        // Saved mic unplugged — retry on the system default.
        return navigator.mediaDevices.getUserMedia({ audio: base });
      }
      throw err;
    }
  }
  return navigator.mediaDevices.getUserMedia({ audio: base });
}

// ---- session mint (single attempt) + intent prefetch ---------------------

/** One mint attempt: GET the session bootstrap and validate its shape.
 * Returns {body, warning}; throws ApiError (HTTP error envelope),
 * AuthLostError (from authFetch), or RealtimeError (network / bad shape). */
async function mintOnce(sessionPath) {
  let resp;
  try {
    resp = await authFetch(sessionPath);
  } catch (err) {
    if (err && err.name === 'AuthLostError') throw err;
    throw new RealtimeError('broker_unavailable', 'Could not reach the voice service.');
  }
  const warning = resp.headers.get('X-LN-Quota-Warning') || '';
  let body = null;
  try {
    body = await resp.json();
  } catch {
    /* handled below via !resp.ok / missing clientSecret */
  }
  if (!resp.ok) {
    throw new ApiError(resp.status, body, undefined, resp.headers.get('X-LN-Txn') || '');
  }
  // Three valid success shapes (FR-VE-03 + M13). A missing `mode` is the
  // pre-M12 openai-direct shape.
  const mode = body && body.mode ? body.mode : 'openai-direct';
  if (mode === 'nova-bridge') {
    if (!body || !body.wsUrl) {
      throw new RealtimeError('mint_failed', 'The voice service returned an invalid Nova session.');
    }
  } else if (mode === 'gemini-direct') {
    // gemini-plan.md §3.4: the endpoint, the URL token, and the exact
    // `setup` frame body are all load-bearing — refuse a partial shape.
    if (
      !body ||
      !body.geminiEndpoint ||
      !body.accessToken ||
      !body.accessToken.value ||
      !body.sessionConfig
    ) {
      throw new RealtimeError('mint_failed', 'The voice service returned an invalid Gemini session.');
    }
  } else if (!body || !body.clientSecret || !body.clientSecret.value) {
    throw new RealtimeError('mint_failed', 'The voice service returned an invalid session.');
  }
  return { body, warning };
}

// Intent prefetch (latency plan #4.2): the session bootstrap is the longest
// serial leg of connect (~0.7-1.2s CloudFront → web fn → broker → OpenAI
// mint), so callers with a strong intent signal — mic-button pointerdown,
// hands-free wake-arm — start it before start()/connect() runs. It is
// single-flight and consumed at most once (one mint = one session).
//
// COST TRADEOFF: a mint consumes a broker concurrency slot + rate-limit
// token, and there is NO release endpoint — a prefetched mint that goes
// unused simply lapses when its ~60s server-side token TTL expires, holding
// that slot/token until then. That is why the client cache TTL is 45s (the
// ephemeral token is still comfortably valid when consumed) and why
// prefetch must NEVER fire on page load — only on explicit user intent.
const PREFETCH_TTL_MS = 45_000;
let prefetchEntry = null; // {promise, at, sessionPath}

/** Start (or reuse) a speculative session mint. Best-effort: the returned
 * promise never needs awaiting — failures self-evict so the next real
 * connect() falls back to a fresh mint with full retry handling. */
export function prefetchSession({ sessionPath = SESSION_PATH } = {}) {
  const now = Date.now();
  if (
    prefetchEntry &&
    prefetchEntry.sessionPath === sessionPath &&
    now - prefetchEntry.at < PREFETCH_TTL_MS
  ) {
    return prefetchEntry.promise; // single-flight: reuse the pending/fresh mint
  }
  const entry = { at: now, sessionPath, promise: mintOnce(sessionPath) };
  // A failed prefetch must not poison the next start: evict it so connect()
  // mints fresh (connect owns 429 retry/backoff + error surfacing).
  entry.promise.catch(() => {
    if (prefetchEntry === entry) prefetchEntry = null;
  });
  prefetchEntry = entry;
  return entry.promise;
}

/** Consume (at most once) a still-fresh prefetched mint for `sessionPath`. */
function takePrefetchedMint(sessionPath) {
  const entry = prefetchEntry;
  if (!entry || entry.sessionPath !== sessionPath) return null;
  prefetchEntry = null; // one mint = one session: never hand it out twice
  if (Date.now() - entry.at >= PREFETCH_TTL_MS) return null; // lapsed — server slot expires on its own
  return entry.promise;
}

function waitForIceGathering(pc, timeoutMs) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(done, timeoutMs); // trickle-less best effort
    function done() {
      clearTimeout(timer);
      pc.removeEventListener('icegatheringstatechange', check);
      pc.removeEventListener('icecandidate', onCandidate);
      resolve();
    }
    function check() {
      if (pc.iceGatheringState === 'complete') done();
    }
    function onCandidate(e) {
      // Send on the FIRST candidate — zero settle (latency plan #4.3).
      // OpenAI answers with its own public candidates and accepts
      // host/srflx, so one local candidate is enough to start connectivity
      // checks; the old +150ms settle bought extra candidates but taxed
      // every connect. Gathering also now OVERLAPS the session mint (see
      // connect()), so by the time the SDP is posted the local description
      // usually carries the full candidate set anyway.
      if (e.candidate) done();
    }
    pc.addEventListener('icegatheringstatechange', check);
    pc.addEventListener('icecandidate', onCandidate);
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * One realtime voice session: one mint, one RTCPeerConnection, one
 * datachannel. Turns cycle inside it (spec §2.2's "live" super-state).
 *
 * Events (CustomEvent, payload in `detail`):
 *   quotawarning   {message}                 — X-LN-Quota-Warning header
 *   retrywait      {seconds}                 — 429 auto-retry back-off started
 *   sessionready   {sessionId, model, voice} — datachannel open, turns may start
 *   speechstarted  {}                        — user speech detected (VAD)
 *   speechstopped  {}
 *   thinking       {}                        — response.created
 *   speaking       {}                        — assistant audio started
 *   speakingended  {}                        — assistant audio finished
 *   responsedone   {}                        — full response turn complete
 *   usage          {usage, responseId}      — token usage for a completed
 *                                              response (openai-direct:
 *                                              evt.response.usage verbatim;
 *                                              gemini-direct: usageMetadata
 *                                              mapped to the same shape —
 *                                              input/output, text/audio,
 *                                              cached breakdowns; never on
 *                                              nova-bridge)
 *   bargein        {}                        — barge-in executed
 *   assistantdelta {itemId, delta, text}     — streaming assistant transcript
 *   assistantfinal {itemId, text}
 *   userdelta      {itemId, delta, text}     — streaming user transcription
 *   userfinal      {itemId, text}
 *   usertranscriptfailed {itemId}
 *   toolcall       {tool, callId, args}
 *   toolresult     {tool, callId, result}
 *   toolerror      {tool, callId, error: {message, code}}
 *   servererror    {error}                   — realtime API error event
 *   connectionlost {reason}                  — ICE/datachannel drop mid-call
 *   closed         {}                        — deliberate close() finished
 */
export class RealtimeSession extends EventTarget {
  #pc = null;
  #dc = null;
  #dcOpen = false;
  #localStream = null;
  #remoteStream = null;
  #audioEl = null;
  #audioCtx = null;
  #gain = null;
  #speaking = false;
  // Whether the minted session config has turn_detection.interrupt_response
  // true (server truncates the response itself on speech_started). When the
  // server interrupts, the local hard cut mirrors it immediately (≤150ms,
  // PRD FR-V03); when it does not (Mic pickup = low / "Patient"), the client
  // owns the decision and gates it via #pendingBarge below.
  #serverInterrupts = true;
  #pendingBarge = null; // confirm-window timer id while a barge is pending
  #closing = false;
  #connected = false;
  #sessionId = '';
  #model = '';
  #voice = '';
  #engine = ''; // resolved voiceEngine pin from the bootstrap ("" pre-M12)
  #rates = null; // per-1M-token rate table from the mint response (rates.go); null on nova-bridge
  #tools = null;
  #assistantText = new Map(); // itemId -> accumulated transcript
  #userText = new Map();
  #finalizedItems = new Set();

  /** Connect-latency breakdown of the last successful connect (null until
   * then): {bootstrapMs, iceMs, sdpMs, totalMs} from performance.now()
   * marks. bootstrap/ICE/mic overlap, so parts don't sum to totalMs. Also
   * logged via console.debug('[realtime] connect timing', …). */
  connectTiming = null;

  // ---- M12 Nova bridge state (only populated in mode==='nova-bridge') ----
  #mode = 'openai-direct';
  #novaWs = null;
  #novaReady = null; // {resolve, reject} while awaiting session.start
  #inRate = NOVA_DEFAULT_IN_RATE;
  #outRate = NOVA_DEFAULT_OUT_RATE;
  #captureCtx = null;
  #captureSource = null;
  #captureProcessor = null;
  #captureSink = null;
  #playbackCtx = null;
  #playbackGain = null;
  #playbackCursor = 0;
  #novaSources = new Set();

  // ---- M13 Gemini Live state (only populated in mode==='gemini-direct') --
  #geminiWs = null;
  #geminiMinted = null; // {endpoint, token, expiresAtMs, sessionConfig}
  #geminiResumeHandle = ''; // latest sessionResumptionUpdate handle
  #geminiReconnecting = false; // a goAway/expiry reconnect is in flight
  #geminiReconnectPending = false; // goAway mid-speech: reconnect at turn end
  #geminiTurn = 0; // itemId counter (g-user-N / g-asst-N)
  #geminiUserItem = ''; // open user transcription itemId ('' = none)
  #geminiAsstItem = ''; // open assistant transcription itemId
  #geminiThinking = false; // 'thinking' emitted for the current turn
  #geminiCallNames = new Map(); // toolCall id → function name (toolResponse needs it)
  #geminiCancelled = new Set(); // toolCallCancellation ids: suppress late results
  #geminiUsage = null; // latest usageMetadata, surfaced at turnComplete

  constructor({ sessionPath = SESSION_PATH, callsUrl = OPENAI_CALLS_URL } = {}) {
    super();
    this.sessionPath = sessionPath;
    this.callsUrl = callsUrl;
    this.#tools = createToolDispatcher({
      sendEvent: (evt) => this.sendEvent(evt),
      onToolCall: (d) => this.#emit('toolcall', d),
      onToolResult: (d) => this.#emit('toolresult', d),
      // `d.error` is whatever createToolDispatcher's invoke caught — an
      // ApiError (message/code/txId) or a generic Error/AuthLostError.
      // Surfaced here (not just swallowed) so the tool-card Details popup
      // can show the real failure instead of a bare "it failed".
      onToolError: (d) => {
        const err = d.error;
        const message = (err && (err.message || String(err))) || 'The tool call failed.';
        const code = (err && err.code) || '';
        this.#emit('toolerror', { tool: d.tool, callId: d.callId, error: { message, code } });
      },
    });
  }

  get sessionId() {
    return this.#sessionId;
  }
  get model() {
    return this.#model;
  }
  get voice() {
    return this.#voice;
  }
  /** Resolved voiceEngine pin from the session bootstrap ("" on a pre-M12
   * shape) — e.g. "openai-realtime", "nova-sonic", "gemini-flash-live". */
  get engine() {
    return this.#engine;
  }
  /** Per-1M-token USD rate table for this session's model (session bootstrap,
   * internal/realtime/rates.go) — null on nova-bridge sessions. */
  get rates() {
    return this.#rates;
  }
  get isConnected() {
    return this.#connected && !this.#closing;
  }
  get isSpeaking() {
    return this.#speaking;
  }
  /** For visualizer.mjs (AnalyserNode taps). */
  get localStream() {
    return this.#localStream;
  }
  get remoteStream() {
    return this.#remoteStream;
  }

  #emit(type, detail = {}) {
    this.dispatchEvent(new CustomEvent(type, { detail }));
  }

  // ---- session mint (spec §2.5 error table) ----

  async #mint() {
    // A prefetched mint (pointerdown / wake-arm intent, prefetchSession
    // above) skips the whole bootstrap wait. If the prefetch failed, fall
    // through to a fresh mint so retry/backoff + error surfacing applies.
    const prefetched = takePrefetchedMint(this.sessionPath);
    if (prefetched) {
      try {
        const { body, warning } = await prefetched;
        if (warning) this.#emit('quotawarning', { message: warning });
        return body;
      } catch {
        /* fresh mint below owns the error */
      }
    }

    let attempt = 0;
    for (;;) {
      let minted;
      try {
        minted = await mintOnce(this.sessionPath);
      } catch (err) {
        if (err instanceof ApiError) {
          const rtErr = RealtimeError.fromApiError(err);
          // 429: auto-retry once after retryAfterSeconds (spec §2.5), then
          // surface for a manual Retry.
          if (rtErr.code === 'rate_limited' && attempt === 0) {
            attempt++;
            const seconds = Math.min(rtErr.retryAfterSeconds || 3, RATE_LIMIT_MAX_WAIT_S);
            this.#emit('retrywait', { seconds });
            await sleep(seconds * 1000);
            continue;
          }
          throw rtErr;
        }
        throw err; // RealtimeError / AuthLostError pass through
      }
      if (minted.warning) this.#emit('quotawarning', { message: minted.warning });
      return minted.body;
    }
  }

  // ---- connect / teardown ----

  /**
   * Full bootstrap: mint ∥ (peer connection + offer + ICE) → SDP exchange →
   * datachannel open. `stream` is a pre-acquired mic stream (mic.mjs
   * acquires it first so permission-denied is distinguishable from
   * connection errors); if omitted, acquires one here with `micDeviceId`.
   *
   * Latency plan #4.1: nothing in the WebRTC setup — peer connection,
   * datachannel, mic track, offer, ICE gathering — needs the minted token,
   * so it all runs CONCURRENTLY with the session bootstrap; the token's
   * only serial job is the SDP POST. The breakdown of each connect lands in
   * `connectTiming` and a console.debug line.
   */
  async connect({ stream = null, micDeviceId = null } = {}) {
    if (this.#pc || this.#novaWs || this.#geminiWs) {
      throw new RealtimeError('already_connected', 'Session is already connected.');
    }
    this.#closing = false;
    const t0 = performance.now();

    // The mic stream is load-bearing for both transports: a WebRTC audio
    // track for openai-direct, the PCM capture source for nova-bridge.
    // `stream` may be a MediaStream OR a promise of one — mic acquisition,
    // the session mint, and the WebRTC offer/ICE below all run CONCURRENTLY.
    const streamPromise = Promise.resolve(
      stream || acquireMicStream({ deviceId: micDeviceId }),
    );

    // Speculative WebRTC setup for the (default) openai-direct mode. If the
    // mint comes back nova-bridge the unused pc is simply closed — the only
    // cost is a local RTCPeerConnection and host-candidate gathering.
    const rtc = this.#beginOffer(streamPromise);

    let minted;
    try {
      minted = await this.#mint();
    } catch (err) {
      this.#abortRtc(rtc);
      // Don't leave a granted mic running if the mint failed.
      streamPromise
        .then((s) => {
          for (const t of s.getTracks()) t.stop();
        })
        .catch(() => {});
      throw err;
    }
    const bootstrapMs = performance.now() - t0;
    this.#mode = minted.mode || 'openai-direct';
    this.#sessionId = minted.sessionId || 'web-' + Date.now().toString(36);
    this.#model = minted.model || '';
    this.#voice = minted.voice || '';
    this.#engine = minted.engine || '';
    this.#rates = minted.rates || null;
    // Barge-in policy comes from the minted (server-authored) session
    // config — single source of truth, never a separate client setting.
    // Missing/legacy shapes default to server-driven interruption.
    const td =
      minted.sessionConfig &&
      minted.sessionConfig.audio &&
      minted.sessionConfig.audio.input &&
      minted.sessionConfig.audio.input.turn_detection;
    this.#serverInterrupts = !td || td.interrupt_response !== false;

    try {
      this.#localStream = await streamPromise;
    } catch (err) {
      this.#abortRtc(rtc);
      throw err; // getUserMedia errors keep their name for mic.mjs routing
    }

    if (this.#mode === 'nova-bridge') {
      this.#abortRtc(rtc); // Nova is WS+PCM — the speculative pc is unused
      await this.#connectNovaBridge(minted);
      this.#finishTiming(t0, bootstrapMs, 0, 0);
    } else if (this.#mode === 'gemini-direct') {
      this.#abortRtc(rtc); // Gemini is WS+PCM too — the speculative pc is unused
      await this.#connectGemini(minted);
      this.#finishTiming(t0, bootstrapMs, 0, 0);
    } else {
      await this.#connectOpenAI(minted, rtc, t0, bootstrapMs);
    }
  }

  /** Record + report the connect-latency breakdown. bootstrap, ICE, and the
   * mic all OVERLAP by design, so the parts deliberately don't sum to
   * totalMs — totalMs is the wall-clock connect() duration. */
  #finishTiming(t0, bootstrapMs, iceMs, sdpMs) {
    this.connectTiming = {
      bootstrapMs: Math.round(bootstrapMs),
      iceMs: Math.round(iceMs),
      sdpMs: Math.round(sdpMs),
      totalMs: Math.round(performance.now() - t0),
    };
    console.debug('[realtime] connect timing', this.connectTiming);
  }

  // ---- openai-direct: WebRTC to OpenAI Realtime ----

  /** Start the token-free half of the WebRTC connect (pc, datachannel, mic
   * track, offer, ICE gathering) so it overlaps the session mint. The mic
   * track MUST be in the offer, so the task awaits the (already in-flight)
   * stream first: in the common case everything here settles before the
   * mint returns; when the mic resolves late the ordering still holds
   * (add-track → offer → ICE), just with less overlap. `rtc.ready` never
   * rejects — it resolves {ok:false, err} so an abandoned attempt cannot
   * surface an unhandled rejection. */
  #beginOffer(streamPromise) {
    const pc = new RTCPeerConnection();
    this.#pc = pc;

    pc.ontrack = (e) => {
      this.#remoteStream = (e.streams && e.streams[0]) || new MediaStream([e.track]);
      this.#attachRemoteAudio();
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed') this.#handleDrop('ice');
    };
    pc.oniceconnectionstatechange = () => {
      if (pc.iceConnectionState === 'failed') this.#handleDrop('ice');
    };

    const dc = pc.createDataChannel('oai-events');
    this.#dc = dc;
    dc.onmessage = (e) => this.#onDcMessage(e);
    dc.onclose = () => {
      this.#dcOpen = false;
      this.#handleDrop('datachannel');
    };

    const rtc = { pc, dc, aborted: false, iceMs: 0, ready: null };
    rtc.ready = (async () => {
      const stream = await streamPromise;
      if (rtc.aborted) return { ok: false };
      for (const track of stream.getAudioTracks()) {
        pc.addTrack(track, stream);
      }
      const offer = await pc.createOffer();
      if (rtc.aborted) return { ok: false };
      await pc.setLocalDescription(offer);
      const iceStart = performance.now();
      await waitForIceGathering(pc, ICE_GATHER_TIMEOUT_MS);
      rtc.iceMs = performance.now() - iceStart;
      return { ok: true };
    })().catch((err) => ({ ok: false, err }));
    return rtc;
  }

  /** Abandon a speculative #beginOffer (mint failed, mic failed, or the
   * mint resolved to nova-bridge). Detaches handlers first so the close
   * can't masquerade as a mid-call drop. */
  #abortRtc(rtc) {
    if (!rtc || rtc.aborted) return;
    rtc.aborted = true;
    rtc.dc.onmessage = null;
    rtc.dc.onclose = null;
    rtc.dc.onopen = null;
    try {
      rtc.dc.close();
    } catch {
      /* already closed */
    }
    try {
      rtc.pc.close();
    } catch {
      /* already closed */
    }
    if (this.#dc === rtc.dc) this.#dc = null;
    if (this.#pc === rtc.pc) this.#pc = null;
    this.#dcOpen = false;
  }

  async #connectOpenAI(minted, rtc, t0, bootstrapMs) {
    const { pc, dc } = rtc;

    const dcOpen = new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => reject(new RealtimeError('sdp_failed', 'Connection to the voice service timed out.')),
        DC_OPEN_TIMEOUT_MS,
      );
      dc.onopen = () => {
        clearTimeout(timer);
        this.#dcOpen = true;
        resolve();
      };
    });
    // A failure below exits without ever awaiting dcOpen — pre-attach a
    // no-op handler so its eventual timeout rejection is never "unhandled".
    dcOpen.catch(() => {});

    try {
      // Usually already settled: the offer + ICE ran during the mint.
      const prep = await rtc.ready;
      if (!prep.ok) {
        throw prep.err instanceof RealtimeError
          ? prep.err
          : new RealtimeError('sdp_failed', 'Could not prepare the voice connection.');
      }

      const sdpStart = performance.now();
      const url = this.#model
        ? this.callsUrl + '?model=' + encodeURIComponent(this.#model)
        : this.callsUrl;
      let callResp;
      try {
        callResp = await fetch(url, {
          method: 'POST',
          headers: {
            Authorization: 'Bearer ' + minted.clientSecret.value,
            'Content-Type': 'application/sdp',
          },
          body: pc.localDescription.sdp,
        });
      } catch {
        throw new RealtimeError('sdp_failed', 'Connection to the voice service dropped.');
      }
      if (!callResp.ok) {
        throw new RealtimeError(
          'sdp_failed',
          `The voice service rejected the connection (HTTP ${callResp.status}).`,
        );
      }
      const answerSdp = await callResp.text();
      await pc.setRemoteDescription({ type: 'answer', sdp: answerSdp });

      // Perceived-latency note (latency plan #4.4): `sessionready` (which
      // mic.mjs renders as "Listening") already fires at datachannel OPEN,
      // not at OpenAI's session.created ack. Moving it earlier still — to
      // media-up (pc.connectionState 'connected') — was evaluated and NOT
      // done: the oai-events channel rides the same DTLS association as the
      // media, so dc open trails media-up by roughly one SCTP handshake RTT
      // (tens of ms), and before dc open commitTurn/bargeIn/sendEvent all
      // throw not_connected — "Listening" would advertise controls that
      // can't work yet, for a negligible win.
      await dcOpen;
      this.#finishTiming(t0, bootstrapMs, rtc.iceMs, performance.now() - sdpStart);
    } catch (err) {
      this.#teardown();
      throw err instanceof RealtimeError
        ? err
        : new RealtimeError('sdp_failed', 'Connection to the voice service dropped.');
    }

    this.#connected = true;
    this.#emit('sessionready', {
      sessionId: this.#sessionId,
      model: this.#model,
      voice: this.#voice,
      engine: this.#engine,
    });
  }

  // ---- nova-bridge: WebSocket + PCM to the backend Nova Sonic bridge ----

  #buildNovaUrl(minted) {
    let url = minted.wsUrl || '';
    // The bridge usually embeds the single-use token in the URL already
    // (WS upgrades can't carry a Bearer header on every client stack —
    // contracts/api.md). If it's returned as a separate field, append it.
    const token = minted.token || '';
    if (url && token && !/[?&]token=/.test(url)) {
      url += (url.includes('?') ? '&' : '?') + 'token=' + encodeURIComponent(token);
    }
    return url;
  }

  async #connectNovaBridge(minted) {
    const url = this.#buildNovaUrl(minted);
    if (!url) throw new RealtimeError('mint_failed', 'The voice bridge URL was missing.');

    let ws;
    try {
      ws = new WebSocket(url);
    } catch {
      throw new RealtimeError('bridge_failed', 'Could not open the voice bridge.');
    }
    ws.binaryType = 'arraybuffer';
    this.#novaWs = ws;

    // Resolve once the bridge announces the session (sample rates, model,
    // voice); reject on socket error/close/timeout before that.
    try {
      await new Promise((resolve, reject) => {
        const timer = setTimeout(
          () => reject(new RealtimeError('bridge_failed', 'The voice bridge did not start in time.')),
          NOVA_START_TIMEOUT_MS,
        );
        this.#novaReady = {
          resolve: () => {
            clearTimeout(timer);
            resolve();
          },
          reject: (err) => {
            clearTimeout(timer);
            reject(err);
          },
        };
        ws.onmessage = (e) => this.#onNovaMessage(e);
        ws.onerror = () => {
          if (this.#novaReady) this.#novaReady.reject(new RealtimeError('bridge_failed', 'The voice bridge connection failed.'));
        };
        ws.onclose = (e) => {
          // A pre-upgrade rejection (auth/session) surfaces here as close
          // 1006 — include the code so the error banner is diagnosable.
          const code = e && e.code ? ' (close ' + e.code + ')' : '';
          if (this.#novaReady) this.#novaReady.reject(new RealtimeError('bridge_failed', 'The voice bridge refused the connection' + code + '.'));
        };
      });
    } catch (err) {
      this.#teardown();
      throw err instanceof RealtimeError ? err : new RealtimeError('bridge_failed', 'Could not start the voice bridge.');
    }

    this.#novaReady = null;
    // Post-handshake: a close/error is now a mid-call drop, not a start failure.
    ws.onerror = () => this.#handleDrop('bridge');
    ws.onclose = () => this.#handleDrop('bridge');

    this.#startNovaCapture();

    this.#connected = true;
    this.#emit('sessionready', {
      sessionId: this.#sessionId,
      model: this.#model,
      voice: this.#voice,
      engine: this.#engine,
    });
  }

  #novaWsSend(obj) {
    const ws = this.#novaWs;
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    try {
      ws.send(JSON.stringify(obj));
    } catch {
      /* socket raced closed — drop handler owns recovery */
    }
  }

  // Translate the shared OpenAI-shaped control events (from the tool
  // dispatcher, commitTurn, sendUserText, barge-in) into the bridge's common
  // schema, so those callers stay engine-agnostic.
  #novaSend(obj) {
    const t = obj && obj.type;
    if (t === 'conversation.item.create' && obj.item) {
      if (obj.item.type === 'function_call_output') {
        let result = obj.item.output;
        try {
          result = JSON.parse(obj.item.output);
        } catch {
          /* keep as string */
        }
        this.#novaWsSend({ type: 'tool.result', callId: obj.item.call_id, result });
        return;
      }
      if (obj.item.type === 'message') {
        const parts = obj.item.content || [];
        const text = (parts[0] && parts[0].text) || '';
        if (text) this.#novaWsSend({ type: 'user.text', text });
        return;
      }
    }
    if (t === 'response.create') return; // Nova continues turns implicitly
    if (t === 'input_audio_buffer.commit') {
      this.#novaWsSend({ type: 'turn.commit' });
      return;
    }
    if (t === 'response.cancel' || t === 'output_audio_buffer.clear') {
      this.#novaWsSend({ type: 'barge-in' });
      return;
    }
    this.#novaWsSend(obj); // forward-compat: anything else passes through
  }

  #onNovaMessage(e) {
    // Binary frame = assistant PCM16 audio (audio.out).
    if (e.data instanceof ArrayBuffer) {
      this.#novaEnqueueAudio(e.data);
      return;
    }
    if (typeof e.data !== 'string') return;
    let evt;
    try {
      evt = JSON.parse(e.data);
    } catch {
      return;
    }
    if (!evt || typeof evt.type !== 'string') return;

    switch (evt.type) {
      case 'session.start': {
        const inRate = Number(evt.input && evt.input.sampleRate);
        const outRate = Number(evt.output && evt.output.sampleRate);
        if (inRate > 0) this.#inRate = inRate;
        if (outRate > 0) this.#outRate = outRate;
        if (evt.model) this.#model = evt.model;
        if (evt.voice) this.#voice = evt.voice;
        if (evt.sessionId) this.#sessionId = evt.sessionId;
        if (this.#novaReady) this.#novaReady.resolve();
        break;
      }

      case 'turn.start':
        if (evt.role === 'user') {
          // Nova server-VAD heard the user: barge-in if the assistant is
          // still speaking, then report the user turn (spec §2.2).
          if (this.#speaking) this.#novaBargeIn(false);
          this.#emit('speechstarted');
        } else if (evt.role === 'assistant') {
          this.#emit('thinking');
        }
        break;

      case 'turn.end':
        if (evt.role === 'user') {
          this.#emit('speechstopped');
        } else if (evt.role === 'assistant') {
          if (this.#speaking) {
            this.#speaking = false;
            this.#emit('speakingended');
          }
          this.#emit('responsedone');
        }
        break;

      case 'speaking.start':
        if (!this.#speaking) {
          this.#speaking = true;
          this.#emit('speaking');
        }
        break;

      case 'speaking.stop':
        if (this.#speaking) {
          this.#speaking = false;
          this.#emit('speakingended');
        }
        break;

      case 'transcript': {
        const role = evt.role === 'user' ? 'user' : 'assistant';
        const map = role === 'user' ? this.#userText : this.#assistantText;
        const itemId = evt.itemId || (role === 'user' ? 'current-user' : 'current');
        if (evt.final) {
          const text = evt.text ?? map.get(itemId) ?? '';
          map.delete(itemId);
          this.#emit(role === 'user' ? 'userfinal' : 'assistantfinal', { itemId, text });
        } else {
          const delta = evt.delta || '';
          const text = (map.get(itemId) || '') + delta;
          map.set(itemId, text);
          this.#emit(role === 'user' ? 'userdelta' : 'assistantdelta', { itemId, delta, text });
        }
        break;
      }

      case 'tool.call': {
        const argsJson =
          typeof evt.args === 'string' ? evt.args : JSON.stringify(evt.args || {});
        this.#tools.dispatch({ name: evt.tool, callId: evt.callId, argsJson });
        break;
      }

      case 'error':
        this.#emit('servererror', { error: evt.error || evt });
        break;

      default:
        break;
    }
  }

  // Assistant audio: schedule each PCM16 frame back-to-back on a playback
  // AudioContext (buffers are auto-resampled from #outRate to the context
  // rate on playback), tracking active sources so barge-in can flush them.
  #novaEnqueueAudio(arrayBuffer) {
    if (this.#closing) return;
    if (!this.#playbackCtx) {
      this.#playbackCtx = new (window.AudioContext || window.webkitAudioContext)();
      this.#playbackGain = this.#playbackCtx.createGain();
      this.#playbackGain.connect(this.#playbackCtx.destination);
      this.#playbackCursor = 0;
    }
    const ctx = this.#playbackCtx;
    ctx.resume().catch(() => {});

    const int16 = new Int16Array(arrayBuffer);
    if (int16.length === 0) return;
    const f32 = new Float32Array(int16.length);
    for (let i = 0; i < int16.length; i++) f32[i] = int16[i] / 0x8000;

    const buf = ctx.createBuffer(1, f32.length, this.#outRate);
    buf.copyToChannel(f32, 0);
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(this.#playbackGain);

    const now = ctx.currentTime;
    if (this.#playbackCursor < now) this.#playbackCursor = now;
    src.start(this.#playbackCursor);
    this.#playbackCursor += buf.duration;
    this.#novaSources.add(src);
    src.onended = () => this.#novaSources.delete(src);

    if (!this.#speaking) {
      this.#speaking = true;
      this.#emit('speaking');
    }
  }

  #novaStopPlayback() {
    for (const s of this.#novaSources) {
      try {
        s.stop();
      } catch {
        /* already stopped */
      }
    }
    this.#novaSources.clear();
    if (this.#playbackCtx) this.#playbackCursor = this.#playbackCtx.currentTime;
  }

  /** Nova barge-in: flush queued assistant audio locally and tell the bridge
   * to cancel the in-flight response. `emit` gates the bargein event so the
   * server-VAD path can suppress a duplicate. */
  #novaBargeIn(emit = true) {
    this.#novaStopPlayback();
    this.#novaWsSend({ type: 'barge-in' });
    this.#speaking = false;
    if (emit) this.#emit('bargein');
  }

  // Shared 16 kHz mic capture for the WSS transports (Nova bridge + Gemini
  // Live): one ScriptProcessor tap; only the uplink FRAMING branches per
  // mode — Nova sends raw binary PCM16, Gemini wraps the same PCM16 as
  // base64 inside a JSON realtimeInput frame.
  #startNovaCapture() {
    if (!this.#localStream) return;
    let ctx;
    try {
      ctx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: this.#inRate });
    } catch {
      ctx = new (window.AudioContext || window.webkitAudioContext)();
    }
    this.#captureCtx = ctx;
    ctx.resume().catch(() => {});
    const fromRate = ctx.sampleRate;
    const ratio = fromRate / this.#inRate; // >1 when the ctx couldn't honor #inRate

    const source = ctx.createMediaStreamSource(this.#localStream);
    const proc = ctx.createScriptProcessor(NOVA_CAPTURE_FRAMES, 1, 1);
    proc.onaudioprocess = (ev) => {
      // Respect the mic gate (setMicEnabled / keep-warm) and user mute.
      const tracks = this.#localStream ? this.#localStream.getAudioTracks() : [];
      if (tracks.length && !tracks[0].enabled) return;

      if (this.#mode === 'gemini-direct') {
        const pcm = this.#floatToPcm16(ev.inputBuffer.getChannelData(0), ratio);
        if (pcm.length > 0) this.#geminiSendAudio(pcm);
        return;
      }

      const ws = this.#novaWs;
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      const input = ev.inputBuffer.getChannelData(0);
      const pcm = this.#floatToPcm16(input, ratio);
      if (pcm.byteLength > 0) {
        try {
          ws.send(pcm.buffer);
        } catch {
          /* raced closed */
        }
      }
    };
    // ScriptProcessor only fires while connected to the graph; route it
    // through a muted gain so the mic never leaks into local playback.
    const sink = ctx.createGain();
    sink.gain.value = 0;
    source.connect(proc);
    proc.connect(sink);
    sink.connect(ctx.destination);
    this.#captureSource = source;
    this.#captureProcessor = proc;
    this.#captureSink = sink;
  }

  // Float32 [-1,1] → little-endian Int16 PCM, linearly resampled to #inRate
  // when the capture context couldn't be forced to that rate.
  #floatToPcm16(input, ratio) {
    const outLen = ratio === 1 ? input.length : Math.floor(input.length / ratio);
    const out = new Int16Array(outLen);
    for (let i = 0; i < outLen; i++) {
      const s = ratio === 1 ? input[i] : this.#sampleAt(input, i * ratio);
      const c = s < -1 ? -1 : s > 1 ? 1 : s;
      out[i] = c < 0 ? c * 0x8000 : c * 0x7fff;
    }
    return out;
  }

  #sampleAt(input, pos) {
    const i0 = Math.floor(pos);
    const i1 = Math.min(i0 + 1, input.length - 1);
    const frac = pos - i0;
    return input[i0] * (1 - frac) + input[i1] * frac;
  }

  #stopNovaCapture() {
    if (this.#captureProcessor) {
      this.#captureProcessor.onaudioprocess = null;
      try {
        this.#captureProcessor.disconnect();
      } catch {
        /* already detached */
      }
      this.#captureProcessor = null;
    }
    if (this.#captureSource) {
      try {
        this.#captureSource.disconnect();
      } catch {
        /* already detached */
      }
      this.#captureSource = null;
    }
    if (this.#captureSink) {
      try {
        this.#captureSink.disconnect();
      } catch {
        /* already detached */
      }
      this.#captureSink = null;
    }
    if (this.#captureCtx) {
      this.#captureCtx.close().catch(() => {});
      this.#captureCtx = null;
    }
  }

  // ---- gemini-direct: WSS + JSON/base64 PCM to Gemini Live (M13) ----------
  //
  // Fork of the Nova WSS skeleton (gemini-plan.md §3.5): the same 16 kHz
  // ScriptProcessor capture and 24 kHz scheduled playback
  // (#startNovaCapture / #novaEnqueueAudio), but client-direct to Google
  // with an ephemeral token in the URL, JSON+base64 uplink frames, and
  // Gemini's event vocabulary translated to the shared CustomEvents.
  // Lifecycle: Google recycles the connection ~every 10 minutes (goAway) —
  // the client reconnects with the latest sessionResumptionUpdate handle,
  // re-fetching a fresh bootstrap (new token) once the current token nears
  // expiresAt (the Nova-style per-reconnect re-mint, gemini-plan.md §3.2).

  /** Stash the parts of a gemini-direct bootstrap that reconnects need. */
  #adoptGeminiMint(minted) {
    this.#geminiMinted = {
      endpoint: minted.geminiEndpoint,
      token: minted.accessToken.value,
      expiresAtMs: Date.parse(minted.accessToken.expiresAt || '') || 0,
      sessionConfig: minted.sessionConfig,
    };
  }

  async #connectGemini(minted) {
    this.#adoptGeminiMint(minted);
    try {
      await this.#openGeminiSocket();
    } catch (err) {
      this.#teardown();
      throw err instanceof RealtimeError
        ? err
        : new RealtimeError('gemini_failed', 'Could not start the Gemini voice session.');
    }

    this.#startNovaCapture(); // shared 16k capture; uplink framing branches per mode

    this.#connected = true;
    this.#emit('sessionready', {
      sessionId: this.#sessionId,
      model: this.#model,
      voice: this.#voice,
      engine: this.#engine,
    });
  }

  /** Open one Gemini WSS connection and complete the setup handshake:
   * connect with the token in the URL (browsers can't set WS headers; the
   * Constrained method only accepts ?access_token= auth), send the
   * broker-authored `setup` frame on open — swapping in the stored
   * resumption handle on reconnects — and gate on `setupComplete`. On
   * success the socket is installed as the live one (make-before-break for
   * mid-call reconnects); rejects on error/close/timeout before the gate. */
  #openGeminiSocket() {
    const { endpoint, token, sessionConfig } = this.#geminiMinted;
    const url = endpoint + '?access_token=' + encodeURIComponent(token);
    let ws;
    try {
      ws = new WebSocket(url);
    } catch {
      return Promise.reject(
        new RealtimeError('gemini_failed', 'Could not open the Gemini voice connection.'),
      );
    }
    ws.binaryType = 'arraybuffer';

    return new Promise((resolve, reject) => {
      let settled = false;
      const fail = (err) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null;
        try {
          ws.close();
        } catch {
          /* already closed */
        }
        reject(err);
      };
      const timer = setTimeout(
        () => fail(new RealtimeError('gemini_failed', 'The Gemini session did not start in time.')),
        GEMINI_SETUP_TIMEOUT_MS,
      );

      ws.onopen = () => {
        // sessionConfig is the exact `setup` body the broker minted the
        // token against (belt-and-suspenders for the known Google bug where
        // constraints-only systemInstruction is intermittently ignored). A
        // resumption reconnect swaps in the stored handle — same server-side
        // session, new connection.
        const setup = this.#geminiResumeHandle
          ? { ...sessionConfig, sessionResumption: { handle: this.#geminiResumeHandle } }
          : sessionConfig;
        try {
          ws.send(JSON.stringify({ setup }));
        } catch {
          /* the close handler owns the failure */
        }
      };
      ws.onmessage = (e) => {
        const evt = this.#parseGeminiFrame(e.data);
        if (!evt || !('setupComplete' in evt)) return; // nothing else is expected pre-gate
        if (this.#closing) {
          fail(new RealtimeError('gemini_failed', 'The session was closed.'));
          return;
        }
        settled = true;
        clearTimeout(timer);
        this.#installGeminiSocket(ws);
        resolve();
      };
      ws.onerror = () =>
        fail(new RealtimeError('gemini_failed', 'The Gemini voice connection failed.'));
      ws.onclose = (e) => {
        // A pre-setup rejection (bad/expired token) surfaces here — include
        // the close code so the error banner is diagnosable.
        const code = e && e.code ? ' (close ' + e.code + ')' : '';
        fail(new RealtimeError('gemini_failed', 'Gemini refused the connection' + code + '.'));
      };
    });
  }

  /** Swap `ws` in as the live Gemini socket: detach + close any predecessor
   * (goAway reconnects are make-before-break) and attach the steady-state
   * handlers — from here on a close/error is a mid-call drop, except while
   * a reconnect is in flight (its own failure path owns the drop). */
  #installGeminiSocket(ws) {
    const old = this.#geminiWs;
    if (old && old !== ws) {
      old.onopen = old.onmessage = old.onerror = old.onclose = null;
      try {
        old.close();
      } catch {
        /* already closed */
      }
    }
    this.#geminiWs = ws;
    ws.onmessage = (e) => this.#onGeminiMessage(e);
    ws.onerror = () => {
      if (this.#geminiWs === ws && !this.#geminiReconnecting) this.#handleDrop('gemini');
    };
    ws.onclose = () => {
      if (this.#geminiWs === ws && !this.#geminiReconnecting) this.#handleDrop('gemini');
    };
  }

  /** Gemini frames are JSON in text OR binary frames (Google sends
   * ArrayBuffers; downlink audio is base64 inside the JSON). */
  #parseGeminiFrame(data) {
    let text;
    if (typeof data === 'string') text = data;
    else if (data instanceof ArrayBuffer) text = GEMINI_TEXT_DECODER.decode(data);
    else return null;
    let evt;
    try {
      evt = JSON.parse(text);
    } catch {
      return null;
    }
    return evt && typeof evt === 'object' ? evt : null;
  }

  #onGeminiMessage(e) {
    const evt = this.#parseGeminiFrame(e.data);
    if (!evt) return;

    // One frame can carry several fields (usageMetadata rides beside
    // serverContent) — check presence, never switch exclusively.
    if (evt.usageMetadata) this.#geminiUsage = evt.usageMetadata;

    if (evt.sessionResumptionUpdate) {
      const u = evt.sessionResumptionUpdate;
      if (u.resumable && u.newHandle) this.#geminiResumeHandle = u.newHandle;
    }

    if (evt.goAway) {
      // Connection recycle warning (~10-min lifetime). Reconnect
      // proactively with the stored handle; mid-utterance, defer to the
      // turn boundary so the recycle doesn't audibly clip the assistant.
      if (this.#speaking) this.#geminiReconnectPending = true;
      else void this.#geminiReconnect();
    }

    if (evt.toolCall && Array.isArray(evt.toolCall.functionCalls)) {
      // Same router flow as every engine: the dispatcher POSTs
      // /api/v1/tools/invoke and answers through sendEvent → #geminiSend,
      // which needs the call's name again — remember it per id.
      for (const fc of evt.toolCall.functionCalls) {
        if (!fc || !fc.id || !fc.name) continue;
        this.#geminiCallNames.set(fc.id, fc.name);
        this.#tools.dispatch({
          name: fc.name,
          callId: fc.id,
          argsJson: JSON.stringify(fc.args || {}),
        });
      }
    }

    if (evt.toolCallCancellation && Array.isArray(evt.toolCallCancellation.ids)) {
      // The server withdrew these calls (usually a barge-in): the invoke
      // may already be in flight, so suppress the eventual output instead —
      // no stale toolResponse goes back.
      for (const id of evt.toolCallCancellation.ids) {
        this.#geminiCancelled.add(id);
        this.#geminiCallNames.delete(id);
      }
    }

    if (evt.serverContent) this.#onGeminiServerContent(evt.serverContent);
  }

  #onGeminiServerContent(sc) {
    // Barge-in: automatic VAD heard the user over the assistant. There is
    // no client→server cancel primitive (gemini-plan.md §3.2) — the barge
    // is the local half of the Nova flow only: flush the playback queue.
    if (sc.interrupted === true && this.#speaking) this.#geminiBargeIn();

    // User transcription deltas (inputTranscription.text — deltas, not
    // snapshots). The first delta of an utterance opens a fresh itemId and
    // doubles as the "user is speaking" signal (no explicit VAD event
    // reaches the client in auto-VAD mode).
    if (sc.inputTranscription && typeof sc.inputTranscription.text === 'string' && sc.inputTranscription.text) {
      if (!this.#geminiUserItem) {
        this.#geminiUserItem = 'g-user-' + ++this.#geminiTurn;
        this.#emit('speechstarted');
      }
      const itemId = this.#geminiUserItem;
      const delta = sc.inputTranscription.text;
      const text = (this.#userText.get(itemId) || '') + delta;
      this.#userText.set(itemId, text);
      this.#emit('userdelta', { itemId, delta, text });
    }

    // Assistant transcription deltas (outputTranscription.text).
    if (sc.outputTranscription && typeof sc.outputTranscription.text === 'string' && sc.outputTranscription.text) {
      this.#geminiAssistantBegan();
      if (!this.#geminiAsstItem) this.#geminiAsstItem = 'g-asst-' + ++this.#geminiTurn;
      const itemId = this.#geminiAsstItem;
      const delta = sc.outputTranscription.text;
      const text = (this.#assistantText.get(itemId) || '') + delta;
      this.#assistantText.set(itemId, text);
      this.#emit('assistantdelta', { itemId, delta, text });
    }

    // Assistant audio: base64 PCM16 in modelTurn parts (24 kHz; the
    // mimeType announces the rate) → the shared scheduled-playback path.
    const parts = sc.modelTurn && Array.isArray(sc.modelTurn.parts) ? sc.modelTurn.parts : [];
    for (const part of parts) {
      const inline = part && part.inlineData;
      if (!inline || !inline.data) continue;
      this.#geminiAssistantBegan();
      const m = /rate=(\d+)/.exec(inline.mimeType || '');
      if (m) this.#outRate = Number(m[1]);
      this.#novaEnqueueAudio(this.#base64ToPcm(inline.data));
    }

    if (sc.turnComplete === true) this.#onGeminiTurnComplete();
  }

  /** First sign of the assistant's reply: the user's utterance is over —
   * finalize their transcription (mirrors the Nova user-turn.end →
   * assistant-turn.start ordering) and report thinking once per turn. */
  #geminiAssistantBegan() {
    if (this.#geminiUserItem) {
      this.#emit('speechstopped');
      this.#geminiFinalizeUser();
    }
    if (!this.#geminiThinking) {
      this.#geminiThinking = true;
      this.#emit('thinking');
    }
  }

  /** Emit userfinal for the open user transcription item, if any. */
  #geminiFinalizeUser() {
    const itemId = this.#geminiUserItem;
    if (!itemId) return;
    this.#geminiUserItem = '';
    const text = this.#userText.get(itemId) ?? '';
    this.#userText.delete(itemId);
    this.#emit('userfinal', { itemId, text });
  }

  /** Emit assistantfinal for the open assistant transcription item, if any
   * (also called on interruption so the partial reply stays rendered). */
  #geminiFinalizeAssistant() {
    const itemId = this.#geminiAsstItem;
    if (!itemId) return;
    this.#geminiAsstItem = '';
    const text = this.#assistantText.get(itemId) ?? '';
    this.#assistantText.delete(itemId);
    this.#emit('assistantfinal', { itemId, text });
  }

  /** serverContent.turnComplete ends the assistant turn: finalize
   * transcripts, settle the speaking state, surface the turn's usage, and
   * run any reconnect deferred to this boundary. */
  #onGeminiTurnComplete() {
    this.#geminiFinalizeAssistant();
    this.#geminiFinalizeUser(); // e.g. a turn with no audible reply
    this.#geminiThinking = false;
    if (this.#speaking) {
      this.#speaking = false;
      this.#emit('speakingended');
    }
    this.#emitGeminiUsage();
    this.#emit('responsedone');
    if (this.#geminiReconnectPending) {
      this.#geminiReconnectPending = false;
      void this.#geminiReconnect();
    }
  }

  /** usageMetadata → the OpenAI-shaped usage payload the page's cost badge
   * already prices (modality breakdowns → text/audio in/out). Gemini has no
   * input caching, so the cached breakdown stays empty — rates.go prices
   * Gemini's cached == uncached, so the arithmetic stays honest either way. */
  #emitGeminiUsage() {
    const u = this.#geminiUsage;
    if (!u) return;
    this.#geminiUsage = null;
    const split = (rows) => {
      const acc = { text: 0, audio: 0 };
      for (const r of Array.isArray(rows) ? rows : []) {
        if (!r) continue;
        const n = Number(r.tokenCount) || 0;
        if (String(r.modality || '').toUpperCase() === 'AUDIO') acc.audio += n;
        else acc.text += n; // TEXT + any future modality prices as text
      }
      return acc;
    };
    const inTok = split(u.promptTokensDetails);
    const outTok = split(u.responseTokensDetails);
    this.#emit('usage', {
      usage: {
        total_tokens: Number(u.totalTokenCount) || 0,
        input_token_details: {
          text_tokens: inTok.text,
          audio_tokens: inTok.audio,
          cached_tokens_details: {},
        },
        output_token_details: {
          text_tokens: outTok.text,
          audio_tokens: outTok.audio,
        },
      },
      responseId: '',
    });
  }

  /** Gemini barge-in: flush local playback and keep the partial transcript.
   * Nothing is sent — automatic VAD truncates server-side and there is no
   * cancel primitive (never send response.cancel here). */
  #geminiBargeIn() {
    this.#novaStopPlayback();
    this.#geminiFinalizeAssistant();
    this.#speaking = false;
    this.#emit('bargein');
  }

  /** goAway/expiry reconnect: open a replacement connection resuming the
   * same server-side session via the stored handle (make-before-break —
   * #installGeminiSocket retires the old socket once the new one is ready).
   * Within the token's window the same token reopens (a uses:1 token
   * permits resumption reconnects); near/past expiresAt a fresh bootstrap
   * is fetched first — the Nova-style per-reconnect re-mint. */
  async #geminiReconnect() {
    if (this.#closing || !this.#connected || this.#geminiReconnecting) return;
    this.#geminiReconnecting = true;
    try {
      if (Date.now() >= this.#geminiMinted.expiresAtMs - GEMINI_TOKEN_EXPIRY_SKEW_MS) {
        const { body, warning } = await mintOnce(this.sessionPath);
        if (warning) this.#emit('quotawarning', { message: warning });
        if (!body || body.mode !== 'gemini-direct') {
          // The pin changed mid-session — treat it as a drop; the next
          // conversation picks up the new engine.
          throw new RealtimeError('gemini_failed', 'The voice service no longer offers a Gemini session.');
        }
        // Keep the original sessionId: the conversation (and its transcript
        // log) continues across the re-mint; the resumption handle carries
        // the model-side context into the fresh token's session.
        this.#adoptGeminiMint(body);
      }
      await this.#openGeminiSocket();
    } catch {
      this.#geminiReconnecting = false;
      this.#handleDrop('gemini-reconnect');
      return;
    }
    this.#geminiReconnecting = false;
  }

  #geminiWsSend(obj) {
    const ws = this.#geminiWs;
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    try {
      ws.send(JSON.stringify(obj));
    } catch {
      /* socket raced closed — drop handler owns recovery */
    }
  }

  // Uplink audio framing: base64 PCM16 inside a JSON realtimeInput frame —
  // NEVER raw binary like the Nova bridge (16 kHz mono, mimeType announces
  // the rate).
  #geminiSendAudio(pcm) {
    const ws = this.#geminiWs;
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    try {
      ws.send(
        JSON.stringify({
          realtimeInput: {
            audio: {
              data: this.#pcmToBase64(pcm),
              mimeType: 'audio/pcm;rate=' + this.#inRate,
            },
          },
        }),
      );
    } catch {
      /* socket raced closed — drop handler owns recovery */
    }
  }

  #pcmToBase64(pcm) {
    const bytes = new Uint8Array(pcm.buffer, pcm.byteOffset, pcm.byteLength);
    let bin = '';
    const STRIDE = 0x2000; // stay under fromCharCode argument-count limits
    for (let i = 0; i < bytes.length; i += STRIDE) {
      bin += String.fromCharCode.apply(null, bytes.subarray(i, i + STRIDE));
    }
    return btoa(bin);
  }

  #base64ToPcm(b64) {
    const bin = atob(b64);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    return bytes.buffer;
  }

  // Translate the shared OpenAI-shaped control events into Gemini Live wire
  // messages so the tool dispatcher / composer / turn controls stay
  // engine-agnostic (the Gemini sibling of #novaSend).
  #geminiSend(obj) {
    const t = obj && obj.type;
    if (t === 'conversation.item.create' && obj.item) {
      if (obj.item.type === 'function_call_output') {
        const callId = obj.item.call_id;
        if (this.#geminiCancelled.delete(callId)) return; // server withdrew this call
        const name = this.#geminiCallNames.get(callId) || '';
        this.#geminiCallNames.delete(callId);
        let result = obj.item.output;
        try {
          result = JSON.parse(obj.item.output);
        } catch {
          /* keep as string */
        }
        this.#geminiWsSend({
          toolResponse: {
            functionResponses: [{ id: callId, name, response: { result } }],
          },
        });
        return;
      }
      if (obj.item.type === 'message') {
        const parts = obj.item.content || [];
        const text = (parts[0] && parts[0].text) || '';
        if (text) {
          this.#geminiWsSend({
            clientContent: {
              turns: [{ role: 'user', parts: [{ text }] }],
              turnComplete: true,
            },
          });
        }
        return;
      }
    }
    // Turn boundaries belong to Gemini's automatic VAD (and clientContent's
    // own turnComplete) — the OpenAI-shaped turn nudges have no wire sibling.
    if (t === 'response.create' || t === 'input_audio_buffer.commit') return;
    if (t === 'response.cancel' || t === 'output_audio_buffer.clear') {
      // No server cancel primitive exists — the barge is purely local.
      this.#novaStopPlayback();
      this.#speaking = false;
      return;
    }
    // Anything else is OpenAI-dialect with no Gemini equivalent: dropping
    // it beats sending a frame Google would close the connection over.
  }

  /** Deliberate end (spec `ending` state): close everything, stop tracks. */
  close() {
    if (this.#closing) return;
    this.#closing = true;
    this.#teardown();
    this.#emit('closed');
  }

  #handleDrop(reason) {
    if (this.#closing || !this.#connected) return;
    this.#closing = true;
    this.#teardown();
    this.#emit('connectionlost', { reason });
  }

  #teardown() {
    this.#connected = false;
    this.#dcOpen = false;
    this.#speaking = false;
    this.#disarmBargeConfirm();

    // Nova bridge cleanup (no-ops in openai-direct mode).
    this.#novaReady = null;
    if (this.#novaWs) {
      this.#novaWs.onopen = null;
      this.#novaWs.onmessage = null;
      this.#novaWs.onerror = null;
      this.#novaWs.onclose = null;
      try {
        this.#novaWs.close();
      } catch {
        /* already closed */
      }
      this.#novaWs = null;
    }

    // Gemini cleanup (no-ops in other modes) — the shared capture/playback
    // paths are stopped just below with Nova's.
    this.#geminiReconnecting = false;
    this.#geminiReconnectPending = false;
    this.#geminiUsage = null;
    this.#geminiUserItem = '';
    this.#geminiAsstItem = '';
    this.#geminiThinking = false;
    this.#geminiResumeHandle = '';
    this.#geminiMinted = null;
    this.#geminiCallNames.clear();
    this.#geminiCancelled.clear();
    if (this.#geminiWs) {
      this.#geminiWs.onopen = null;
      this.#geminiWs.onmessage = null;
      this.#geminiWs.onerror = null;
      this.#geminiWs.onclose = null;
      try {
        this.#geminiWs.close();
      } catch {
        /* already closed */
      }
      this.#geminiWs = null;
    }

    this.#stopNovaCapture();
    this.#novaStopPlayback();
    if (this.#playbackCtx) {
      this.#playbackCtx.close().catch(() => {});
      this.#playbackCtx = null;
      this.#playbackGain = null;
    }

    if (this.#dc) {
      this.#dc.onclose = null;
      try {
        this.#dc.close();
      } catch {
        /* already closed */
      }
      this.#dc = null;
    }
    if (this.#pc) {
      try {
        this.#pc.close();
      } catch {
        /* already closed */
      }
      this.#pc = null;
    }
    if (this.#localStream) {
      for (const t of this.#localStream.getTracks()) t.stop();
      this.#localStream = null;
    }
    if (this.#audioEl) {
      this.#audioEl.srcObject = null;
      this.#audioEl = null;
    }
    if (this.#audioCtx) {
      this.#audioCtx.close().catch(() => {});
      this.#audioCtx = null;
      this.#gain = null;
    }
    this.#remoteStream = null;
  }

  // ---- remote audio path (hidden <audio> + GainNode for barge-in duck) ----

  #attachRemoteAudio() {
    if (!this.#remoteStream) return;
    // Chrome quirk: a remote WebRTC track only flows once it's attached to a
    // media element. We mute the element and do audible playback through
    // WebAudio so a GainNode can ramp it for barge-in (spec §2.2 step 1).
    if (!this.#audioEl) {
      this.#audioEl = document.createElement('audio');
      this.#audioEl.autoplay = true;
      this.#audioEl.setAttribute('playsinline', '');
      this.#audioEl.muted = true;
    }
    this.#audioEl.srcObject = this.#remoteStream;
    this.#audioEl.play().catch(() => {
      /* autoplay of a muted element rarely fails; audible path is WebAudio */
    });

    // The AudioContext is created inside the user-gesture-initiated connect
    // flow, so resume() is permitted.
    if (!this.#audioCtx) {
      this.#audioCtx = new (window.AudioContext || window.webkitAudioContext)();
      this.#gain = this.#audioCtx.createGain();
      this.#gain.connect(this.#audioCtx.destination);
    }
    const src = this.#audioCtx.createMediaStreamSource(this.#remoteStream);
    src.connect(this.#gain);
    this.#audioCtx.resume().catch(() => {});
  }

  #duckOutput(level = 0.0001) {
    if (!this.#gain || !this.#audioCtx) return;
    const now = this.#audioCtx.currentTime;
    const g = this.#gain.gain;
    g.cancelScheduledValues(now);
    g.setValueAtTime(Math.max(g.value, 0.0001), now);
    g.linearRampToValueAtTime(level, now + DUCK_RAMP_S);
  }

  #restoreOutput() {
    if (!this.#gain || !this.#audioCtx) return;
    const now = this.#audioCtx.currentTime;
    const g = this.#gain.gain;
    g.cancelScheduledValues(now);
    g.setValueAtTime(Math.max(g.value, 0.0001), now);
    g.linearRampToValueAtTime(1, now + 0.05);
  }

  // ---- outbound events / turn controls ----

  sendEvent(obj) {
    if (this.#mode === 'nova-bridge') {
      if (!this.#novaWs || this.#novaWs.readyState !== WebSocket.OPEN) {
        throw new RealtimeError('not_connected', 'The voice session is not connected.');
      }
      this.#novaSend(obj);
      return;
    }
    if (this.#mode === 'gemini-direct') {
      if (!this.#geminiWs || this.#geminiWs.readyState !== WebSocket.OPEN) {
        throw new RealtimeError('not_connected', 'The voice session is not connected.');
      }
      this.#geminiSend(obj);
      return;
    }
    if (!this.#dc || !this.#dcOpen) {
      throw new RealtimeError('not_connected', 'The voice session is not connected.');
    }
    this.#dc.send(JSON.stringify(obj));
  }

  /** PTT manual end-of-turn (spec §2.2 mode table): commit the input buffer
   * and ask for a response without waiting for VAD. */
  commitTurn() {
    this.sendEvent({ type: 'input_audio_buffer.commit' });
    this.sendEvent({ type: 'response.create' });
  }

  /** Cancel an in-flight response (used from `live-thinking`). */
  cancelResponse() {
    if (this.#mode === 'nova-bridge') {
      this.#novaBargeIn();
      return;
    }
    if (this.#mode === 'gemini-direct') {
      this.#geminiBargeIn();
      return;
    }
    this.sendEvent({ type: 'response.cancel' });
    if (this.#speaking) {
      this.#duckOutput();
      this.sendEvent({ type: 'output_audio_buffer.clear' });
      this.#speaking = false;
    }
  }

  /** Patient-mode gate: soft-duck and wait BARGE_CONFIRM_MS; if VAD speech
   * is still running when the window closes (no speech_stopped disarmed it),
   * the interjection is real → full barge-in. */
  #armBargeConfirm() {
    if (this.#pendingBarge) return;
    this.#duckOutput(PENDING_DUCK_LEVEL);
    this.#pendingBarge = setTimeout(() => {
      this.#pendingBarge = null;
      if (this.#speaking) this.bargeIn();
    }, BARGE_CONFIRM_MS);
  }

  /** Cancel a pending patient-mode barge (noise blip / assistant finished).
   * `restore` ramps the ducked output back up when the assistant is still
   * talking. */
  #disarmBargeConfirm(restore = false) {
    if (!this.#pendingBarge) return;
    clearTimeout(this.#pendingBarge);
    this.#pendingBarge = null;
    if (restore && this.#speaking) this.#restoreOutput();
  }

  /** Barge-in (spec §2.2): duck audio ~30ms, cancel the response, clear the
   * buffered assistant audio, return to listening immediately. Also called
   * by mic.mjs for a manual mid-speech tap. */
  bargeIn() {
    this.#disarmBargeConfirm();
    if (this.#mode === 'nova-bridge') {
      if (!this.#connected) return;
      this.#novaBargeIn();
      return;
    }
    if (this.#mode === 'gemini-direct') {
      if (!this.#connected) return;
      this.#geminiBargeIn();
      return;
    }
    if (!this.#dcOpen) return;
    this.#duckOutput();
    try {
      this.sendEvent({ type: 'response.cancel' });
      this.sendEvent({ type: 'output_audio_buffer.clear' });
    } catch {
      /* connection raced closed — drop handler owns it */
    }
    this.#speaking = false;
    this.#emit('bargein');
  }

  /** Keep-warm grace control (spec §2.2): disable/enable the mic track
   * without tearing the session down. */
  setMicEnabled(enabled) {
    if (!this.#localStream) return;
    for (const t of this.#localStream.getAudioTracks()) t.enabled = !!enabled;
  }

  /**
   * Mid-session settings application (owner request 2026-07-18): push a
   * changed "Mic pickup" (settings micEagerness) / turn-detection config to
   * the LIVE session via the GA `session.update` event on the oai-events
   * datachannel, instead of it only taking effect at the next mint.
   *
   * The audio.input object sent here MIRRORS internal/realtime/mint.go
   * buildAudioInput/buildTurnDetection — KEEP THE TWO IN SYNC:
   *   - type is always semantic_vad (mint.go does not forward the schema's
   *     turnDetection value either, so a turnDetection-only change yields
   *     this same object);
   *   - eagerness is forwarded only for the explicit low|medium|high
   *     choices; auto/empty/unknown keeps the API default;
   *   - low ("Patient") also sets interrupt_response:false — the server
   *     stops truncating on VAD blips and THIS client owns the barge-in
   *     decision (see #serverInterrupts / #armBargeConfirm);
   *   - noise_reduction + transcription are re-sent exactly as minted:
   *     session.update replaces the whole audio.input object, so omitting
   *     them would silently reset server-side noise reduction and kill the
   *     user-transcription events.
   *
   * Nova bridge: no-op (returns false) — the Bedrock session is held
   * server-side and has no client session.update surface. Gemini: also a
   * no-op — the session config is locked into the ephemeral token at mint,
   * so mid-session changes land at the next mint like Nova.
   * @returns {boolean} true if the update was sent to a live session.
   */
  updateAudioInput({ eagerness = 'auto' } = {}) {
    if (this.#mode === 'nova-bridge' || this.#mode === 'gemini-direct') return false;
    if (!this.#dc || !this.#dcOpen) return false;
    const turnDetection = { type: 'semantic_vad', interrupt_response: true };
    if (eagerness === 'low') {
      turnDetection.eagerness = 'low';
      turnDetection.interrupt_response = false;
    } else if (eagerness === 'medium' || eagerness === 'high') {
      turnDetection.eagerness = eagerness;
    }
    try {
      this.sendEvent({
        type: 'session.update',
        session: {
          type: 'realtime',
          audio: {
            input: {
              turn_detection: turnDetection,
              noise_reduction: { type: 'near_field' },
              transcription: { model: 'gpt-4o-mini-transcribe' },
            },
          },
        },
      });
    } catch {
      return false; // datachannel raced closed — drop handler owns recovery
    }
    // Flip the client-side barge-in policy with the server config (same
    // derivation as connect(): Patient mode = client-gated interruption).
    const serverInterrupts = turnDetection.interrupt_response !== false;
    if (serverInterrupts !== this.#serverInterrupts) {
      this.#serverInterrupts = serverInterrupts;
      // Leaving Patient mode with a confirm window armed: the server owns
      // interruption again, so resolve the pending gate and restore audio
      // (a real interjection will be truncated server-side).
      if (serverInterrupts) this.#disarmBargeConfirm(true);
    }
    return true;
  }

  /** Typed user message over the live session (composer while connected). */
  sendUserText(text) {
    const trimmed = (text || '').trim();
    if (!trimmed) return;
    this.sendEvent({
      type: 'conversation.item.create',
      item: {
        type: 'message',
        role: 'user',
        content: [{ type: 'input_text', text: trimmed }],
      },
    });
    this.sendEvent({ type: 'response.create' });
  }

  // ---- inbound event routing (spec §2.4 step 5) ----

  #onDcMessage(e) {
    let evt;
    try {
      evt = JSON.parse(e.data);
    } catch {
      return; // not JSON — ignore
    }
    if (!evt || typeof evt.type !== 'string') return;

    if (this.#tools.handleEvent(evt)) return;

    switch (evt.type) {
      case 'input_audio_buffer.speech_started':
        // Barge-in trigger: user (or, in a noisy room, *something*) spoke
        // while the assistant was talking. When the session was minted with
        // interrupt_response:true the server has already truncated the
        // response, so mirror it instantly (≤150ms, PRD FR-V03). In Patient
        // mode (interrupt_response:false, Mic pickup = low) hold: soft-duck
        // and confirm before cancelling, so an ambient blip doesn't kill
        // the response.
        if (this.#speaking) {
          if (this.#serverInterrupts) this.bargeIn();
          else this.#armBargeConfirm();
        }
        this.#emit('speechstarted');
        break;

      case 'input_audio_buffer.speech_stopped':
        // Speech ended inside the confirm window → it was a blip, not an
        // interjection: bring the assistant's audio back and keep going.
        this.#disarmBargeConfirm(true);
        this.#emit('speechstopped');
        break;

      case 'input_audio_buffer.committed':
        // The server judged that real user speech completed a turn. In
        // Patient mode a very short real utterance ("stop") can end before
        // the confirm window and get restored above — the commit is the
        // server's word that it was speech, so honor it and barge now.
        if (!this.#serverInterrupts && this.#speaking) this.bargeIn();
        break;

      case 'response.created':
        this.#emit('thinking');
        break;

      case 'output_audio_buffer.started':
        this.#speaking = true;
        this.#restoreOutput();
        this.#emit('speaking');
        break;

      case 'output_audio_buffer.stopped':
        this.#disarmBargeConfirm();
        if (this.#speaking) {
          this.#speaking = false;
          this.#emit('speakingended');
        }
        break;

      case 'output_audio_buffer.cleared':
        this.#disarmBargeConfirm();
        this.#speaking = false;
        break;

      case 'response.output_audio_transcript.delta':
      case 'response.output_text.delta': {
        const itemId = evt.item_id || evt.response_id || 'current';
        const text = (this.#assistantText.get(itemId) || '') + (evt.delta || '');
        this.#assistantText.set(itemId, text);
        // Belt-and-braces: some stacks deliver the first transcript delta
        // before output_audio_buffer.started.
        if (!this.#speaking && evt.type === 'response.output_audio_transcript.delta') {
          this.#speaking = true;
          this.#restoreOutput();
          this.#emit('speaking');
        }
        this.#emit('assistantdelta', { itemId, delta: evt.delta || '', text });
        break;
      }

      case 'response.output_audio_transcript.done':
      case 'response.output_text.done': {
        const itemId = evt.item_id || evt.response_id || 'current';
        if (this.#finalizedItems.has(itemId)) break; // audio+text double-final
        this.#finalizedItems.add(itemId);
        const text = evt.transcript ?? evt.text ?? this.#assistantText.get(itemId) ?? '';
        this.#assistantText.delete(itemId);
        this.#emit('assistantfinal', { itemId, text });
        break;
      }

      case 'conversation.item.input_audio_transcription.delta': {
        const itemId = evt.item_id || 'current-user';
        const text = (this.#userText.get(itemId) || '') + (evt.delta || '');
        this.#userText.set(itemId, text);
        this.#emit('userdelta', { itemId, delta: evt.delta || '', text });
        break;
      }

      case 'conversation.item.input_audio_transcription.completed': {
        const itemId = evt.item_id || 'current-user';
        const text = evt.transcript ?? this.#userText.get(itemId) ?? '';
        this.#userText.delete(itemId);
        this.#emit('userfinal', { itemId, text });
        break;
      }

      case 'conversation.item.input_audio_transcription.failed':
        this.#userText.delete(evt.item_id || 'current-user');
        this.#emit('usertranscriptfailed', { itemId: evt.item_id || '' });
        break;

      case 'response.done':
        if (evt.response && evt.response.usage) {
          this.#emit('usage', { usage: evt.response.usage, responseId: evt.response.id || '' });
        }
        this.#emit('responsedone');
        break;

      case 'error':
        this.#emit('servererror', { error: evt.error || evt });
        break;

      default:
        break; // unhandled event types are expected and fine
    }
  }
}
