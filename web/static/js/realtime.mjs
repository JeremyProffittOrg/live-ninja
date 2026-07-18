// realtime.mjs — OpenAI Realtime WebRTC transport (docs/web-ui-spec.md §2.4).
//
// Responsibilities (plan.md M3 / WS-D "realtime JS"):
//   - Session mint: `GET /api/v1/realtime/session` (via toolclient's
//     authFetch) with typed 402 quota_exceeded / 429 rate_limited / 502
//     broker_unavailable handling (spec §2.5 table). 429 auto-retries once
//     after `retryAfterSeconds` before surfacing an error.
//   - WebRTC: RTCPeerConnection + mic track (AEC/NS/AGC true), `oai-events`
//     datachannel, SDP offer → POST https://api.openai.com/v1/realtime/calls
//     (Authorization: Bearer <ephemeral>, Content-Type: application/sdp) →
//     answer SDP; remote audio via ontrack → hidden <audio> + GainNode.
//   - Event routing: transcript deltas, user transcription, tool calls
//     (delegated to toolclient's dispatcher), speaking/turn lifecycle.
//   - Barge-in: on `input_audio_buffer.speech_started` while assistant audio
//     is playing → 30ms gain ramp to silence + `response.cancel` +
//     `output_audio_buffer.clear`, then immediate return to listening (never
//     waits for the cancel ack, spec §2.2).
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
// `GET /api/v1/realtime/session` returns one of two shapes, resolved from the
// device's `voiceEngine` pin (contracts/settings.schema.json):
//   { mode:"openai-direct", clientSecret, model, voice, ... }  ← default path,
//     the WebRTC-to-OpenAI transport below (unchanged).
//   { mode:"nova-bridge",  wsUrl, token, model?, voice?, sessionId }  ← a
//     Nova-pinned device; audio flows device⇄backend-bridge⇄Bedrock Nova
//     Sonic. Bedrock's bidirectional stream is server-held (SigV4 + HTTP/2),
//     so it can't be client-direct — the browser instead opens a WebSocket to
//     the bridge and streams raw PCM16 frames both ways.
//
// This class exposes ONE public surface (the same events, same methods) for
// both modes, so mic.mjs / the conversation page don't branch. A response
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
// 'self'` and needs no extra allowlist entry.

import { authFetch, createToolDispatcher, ApiError } from './toolclient.mjs';

const SESSION_PATH = '/api/v1/realtime/session';
const OPENAI_CALLS_URL = 'https://api.openai.com/v1/realtime/calls';
const DC_OPEN_TIMEOUT_MS = 10_000;
const ICE_GATHER_TIMEOUT_MS = 2_000;
const RATE_LIMIT_MAX_WAIT_S = 15;
const DUCK_RAMP_S = 0.03; // spec §2.2: ~30ms ramp, not an abrupt cut

// Nova bridge: how long to wait for the bridge's `session.start` after the
// WebSocket opens, and the PCM sample rates Nova Sonic uses when the bridge
// doesn't override them in `session.start` (16 kHz in, 24 kHz out).
const NOVA_START_TIMEOUT_MS = 12_000;
const NOVA_DEFAULT_IN_RATE = 16_000;
const NOVA_DEFAULT_OUT_RATE = 24_000;
// ScriptProcessor block size for mic capture (frames per PCM chunk sent).
const NOVA_CAPTURE_FRAMES = 2048;

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

function waitForIceGathering(pc, timeoutMs) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(done, timeoutMs); // trickle-less best effort
    function done() {
      clearTimeout(timer);
      pc.removeEventListener('icegatheringstatechange', check);
      resolve();
    }
    function check() {
      if (pc.iceGatheringState === 'complete') done();
    }
    pc.addEventListener('icegatheringstatechange', check);
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
 *   bargein        {}                        — barge-in executed
 *   assistantdelta {itemId, delta, text}     — streaming assistant transcript
 *   assistantfinal {itemId, text}
 *   userdelta      {itemId, delta, text}     — streaming user transcription
 *   userfinal      {itemId, text}
 *   usertranscriptfailed {itemId}
 *   toolcall       {tool, callId, args}
 *   toolresult     {tool, callId, result}
 *   toolerror      {tool, callId}
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
  #closing = false;
  #connected = false;
  #sessionId = '';
  #model = '';
  #voice = '';
  #tools = null;
  #assistantText = new Map(); // itemId -> accumulated transcript
  #userText = new Map();
  #finalizedItems = new Set();

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

  constructor({ sessionPath = SESSION_PATH, callsUrl = OPENAI_CALLS_URL } = {}) {
    super();
    this.sessionPath = sessionPath;
    this.callsUrl = callsUrl;
    this.#tools = createToolDispatcher({
      sendEvent: (evt) => this.sendEvent(evt),
      onToolCall: (d) => this.#emit('toolcall', d),
      onToolResult: (d) => this.#emit('toolresult', d),
      onToolError: (d) => this.#emit('toolerror', { tool: d.tool, callId: d.callId }),
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
    let attempt = 0;
    for (;;) {
      let resp;
      try {
        resp = await authFetch(this.sessionPath);
      } catch (err) {
        if (err instanceof ApiError || (err && err.name === 'AuthLostError')) throw err;
        throw new RealtimeError('broker_unavailable', 'Could not reach the voice service.');
      }

      const warning = resp.headers.get('X-LN-Quota-Warning');
      if (warning) this.#emit('quotawarning', { message: warning });

      let body = null;
      try {
        body = await resp.json();
      } catch {
        /* handled below via !resp.ok / missing clientSecret */
      }

      if (!resp.ok) {
        const apiErr = new ApiError(resp.status, body, undefined, resp.headers.get('X-LN-Txn') || '');
        const rtErr = RealtimeError.fromApiError(apiErr);
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

      // Two valid success shapes (FR-VE-03). A missing `mode` is the pre-M12
      // openai-direct shape.
      const mode = body && body.mode ? body.mode : 'openai-direct';
      if (mode === 'nova-bridge') {
        if (!body || !body.wsUrl) {
          throw new RealtimeError('mint_failed', 'The voice service returned an invalid Nova session.');
        }
      } else if (!body || !body.clientSecret || !body.clientSecret.value) {
        throw new RealtimeError('mint_failed', 'The voice service returned an invalid session.');
      }
      return body;
    }
  }

  // ---- connect / teardown ----

  /**
   * Full bootstrap: mint → peer connection → SDP exchange → datachannel
   * open. `stream` is a pre-acquired mic stream (mic.mjs acquires it first
   * so permission-denied is distinguishable from connection errors); if
   * omitted, acquires one here with `micDeviceId`.
   */
  async connect({ stream = null, micDeviceId = null } = {}) {
    if (this.#pc || this.#novaWs) {
      throw new RealtimeError('already_connected', 'Session is already connected.');
    }
    this.#closing = false;

    const minted = await this.#mint();
    this.#mode = minted.mode || 'openai-direct';
    this.#sessionId = minted.sessionId || 'web-' + Date.now().toString(36);
    this.#model = minted.model || '';
    this.#voice = minted.voice || '';

    // The mic stream is load-bearing for both transports: a WebRTC audio track
    // for openai-direct, the PCM capture source for nova-bridge.
    this.#localStream = stream || (await acquireMicStream({ deviceId: micDeviceId }));

    if (this.#mode === 'nova-bridge') {
      await this.#connectNovaBridge(minted);
    } else {
      await this.#connectOpenAI(minted);
    }
  }

  // ---- openai-direct: WebRTC to OpenAI Realtime (unchanged) ----

  async #connectOpenAI(minted) {
    const pc = new RTCPeerConnection();
    this.#pc = pc;

    for (const track of this.#localStream.getAudioTracks()) {
      pc.addTrack(track, this.#localStream);
    }

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

    try {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await waitForIceGathering(pc, ICE_GATHER_TIMEOUT_MS);

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

      await dcOpen;
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
        ws.onclose = () => {
          if (this.#novaReady) this.#novaReady.reject(new RealtimeError('bridge_failed', 'The voice bridge closed before starting.'));
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
      const ws = this.#novaWs;
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      // Respect the mic gate (setMicEnabled / keep-warm) and user mute.
      const tracks = this.#localStream ? this.#localStream.getAudioTracks() : [];
      if (tracks.length && !tracks[0].enabled) return;

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

  #duckOutput() {
    if (!this.#gain || !this.#audioCtx) return;
    const now = this.#audioCtx.currentTime;
    const g = this.#gain.gain;
    g.cancelScheduledValues(now);
    g.setValueAtTime(Math.max(g.value, 0.0001), now);
    g.linearRampToValueAtTime(0.0001, now + DUCK_RAMP_S);
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
    this.sendEvent({ type: 'response.cancel' });
    if (this.#speaking) {
      this.#duckOutput();
      this.sendEvent({ type: 'output_audio_buffer.clear' });
      this.#speaking = false;
    }
  }

  /** Barge-in (spec §2.2): duck audio ~30ms, cancel the response, clear the
   * buffered assistant audio, return to listening immediately. Also called
   * by mic.mjs for a manual mid-speech tap. */
  bargeIn() {
    if (this.#mode === 'nova-bridge') {
      if (!this.#connected) return;
      this.#novaBargeIn();
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
        // Barge-in trigger: user spoke while the assistant was talking.
        if (this.#speaking) this.bargeIn();
        this.#emit('speechstarted');
        break;

      case 'input_audio_buffer.speech_stopped':
        this.#emit('speechstopped');
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
        if (this.#speaking) {
          this.#speaking = false;
          this.#emit('speakingended');
        }
        break;

      case 'output_audio_buffer.cleared':
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
