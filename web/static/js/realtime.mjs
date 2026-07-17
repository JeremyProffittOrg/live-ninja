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
// CSP note: the only cross-origin request in this file targets
// https://api.openai.com, matching the spec §0 `connect-src` allowlist.

import { authFetch, createToolDispatcher, ApiError } from './toolclient.mjs';

const SESSION_PATH = '/api/v1/realtime/session';
const OPENAI_CALLS_URL = 'https://api.openai.com/v1/realtime/calls';
const DC_OPEN_TIMEOUT_MS = 10_000;
const ICE_GATHER_TIMEOUT_MS = 2_000;
const RATE_LIMIT_MAX_WAIT_S = 15;
const DUCK_RAMP_S = 0.03; // spec §2.2: ~30ms ramp, not an abrupt cut

/** Typed error for session bootstrap / connection failures. `code` is the
 * server envelope's `error` (quota_exceeded, rate_limited,
 * broker_unavailable, ...) or a client-side code (sdp_failed). */
export class RealtimeError extends Error {
  constructor(code, message, extras = {}) {
    super(message);
    this.name = 'RealtimeError';
    this.code = code;
    // quota_exceeded: kind ("daily_minutes"|"monthly_tokens"), used, limit,
    // resetAt. rate_limited: retryAfterSeconds.
    Object.assign(this, extras);
  }

  static fromApiError(err) {
    if (!(err instanceof ApiError)) {
      return new RealtimeError('mint_failed', 'Could not start a voice session.');
    }
    const b = err.body || {};
    switch (err.code) {
      case 'quota_exceeded':
        return new RealtimeError('quota_exceeded', err.message, {
          kind: b.kind || '',
          used: b.used,
          limit: b.limit,
          resetAt: b.resetAt || '',
        });
      case 'rate_limited':
        return new RealtimeError('rate_limited', err.message, {
          retryAfterSeconds: Number(b.retryAfterSeconds) || 0,
        });
      case 'broker_unavailable':
        return new RealtimeError('broker_unavailable', err.message);
      default:
        return new RealtimeError(err.code || 'mint_failed', err.message);
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
        const apiErr = new ApiError(resp.status, body);
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

      if (!body || !body.clientSecret || !body.clientSecret.value) {
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
    if (this.#pc) throw new RealtimeError('already_connected', 'Session is already connected.');
    this.#closing = false;

    const minted = await this.#mint();
    this.#sessionId = minted.sessionId || 'web-' + Date.now().toString(36);
    this.#model = minted.model || '';
    this.#voice = minted.voice || '';

    this.#localStream = stream || (await acquireMicStream({ deviceId: micDeviceId }));

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
