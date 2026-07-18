/**
 * wakeword.mjs — local, in-browser wake-word detection for Live Ninja.
 *
 * Engine: openWakeWord's three-stage ONNX pipeline (melspectrogram →
 * speech-embedding → per-phrase detector) running on onnxruntime-web (WASM
 * execution provider, vendored under /static/vendor/ort/ — no CDN at
 * runtime, per the CSP: script/connect 'self' + api.openai.com only).
 *
 * Audio path: getUserMedia mic (or a caller-supplied MediaStream) →
 * AudioWorklet ("ln-downsampler", /static/js/wakeword-worklet.js) which
 * resamples to 16 kHz mono and emits 1280-sample (80 ms) frames → main
 * thread ONNX inference.
 *
 * Model resolution: tries the backend wake-word manifest first
 * (GET /api/v1/wakeword/{id}/model?platform=web — the M6 contract,
 * contracts/api.md). Until M6 ships that route, or on any manifest/network
 * failure, it falls back to the bundled openWakeWord "hey jarvis" models in
 * /static/models/ (pinned by SHA-256 below, downloaded at author time from
 * the openWakeWord v0.5.1 GitHub release). Every model buffer is SHA-256
 * verified before a session is created.
 *
 * Support policy (plan.md M3 DoD: wake word OFF by default, click-to-talk
 * guaranteed): `isWakeWordSupported()` is false — and `start()` rejects with
 * err.code === "unsupported" — when AudioWorklet, WebAssembly, subtle-crypto
 * or getUserMedia is missing, or when the CSP blocks WASM compilation (needs
 * `'wasm-unsafe-eval'` in script-src). Callers (mic.mjs) must then hide the
 * hands-free toggle and keep push-to-talk. NOTE — deliberate deviation from
 * the task brief's "no SharedArrayBuffer → unsupported": SAB requires
 * cross-origin isolation (COOP/COEP headers) that live.jeremy.ninja does not
 * send, so gating on SAB would disable wake word everywhere. onnxruntime-web
 * runs fine single-threaded without SAB; we use threads only when
 * crossOriginIsolated is true and fall back to numThreads=1 otherwise.
 *
 * Usage:
 *   import { createWakeWordEngine, isWakeWordSupported } from './wakeword.mjs';
 *   const engine = createWakeWordEngine({
 *     wakeWordId: settings.wakeWord,          // e.g. "hey-live-ninja"
 *     sensitivity: settings.sensitivity,      // 0..1 (settings.schema.json)
 *     onDetect: ({ score, phrase }) => startTurn(),
 *     onStateChange: (state) => updateUi(state),
 *     onError: (err) => toast(err),
 *   });
 *   await engine.start();                     // or engine.start({ stream })
 *   ...
 *   await engine.stop();
 *
 * Everything (onnxruntime + models ≈ 15 MB total, cached by the service
 * worker after first load) is lazy-loaded inside start(); importing this
 * module costs nothing.
 */

'use strict';

// ---------------------------------------------------------------------------
// Pinned artifacts (verify-before-use).
// onnxruntime-web 1.20.1 (dist/, cdn.jsdelivr.net, fetched at author time):
//   ort.wasm.min.mjs             sha256 f53ed4792e758e7232f779d479eb8931b97ff3ab2e3b9a33ee00d1251ffdaad6
//   ort-wasm-simd-threaded.mjs   sha256 745eb7c0ce6f18a6aa521971b2877babc7ffb27eecb58ab3bc6e5ef4692672e8
//   ort-wasm-simd-threaded.wasm  sha256 207d02be4591c156b0a98f024f3d58005b5b04c92274d759fb390338c63559ea
// (also recorded in web/static/vendor/ort/PINNED.txt)
// ---------------------------------------------------------------------------

const ORT_MODULE_URL = '/static/vendor/ort/ort.wasm.min.mjs';
const ORT_WASM_DIR = '/static/vendor/ort/';
const WORKLET_URL = '/static/js/wakeword-worklet.js';

/** openWakeWord v0.5.1 release assets, bundled until the M6 manifest route ships. */
const BUNDLED_MODELS = {
  id: 'hey-jarvis',
  phrase: 'Hey Jarvis',
  melspectrogram: {
    url: '/static/models/melspectrogram.onnx',
    sha256: 'ba2b0e0f8b7b875369a2c89cb13360ff53bac436f2895cced9f479fa65eb176f',
  },
  embedding: {
    url: '/static/models/embedding_model.onnx',
    sha256: '70d164290c1d095d1d4ee149bc5e00543250a7316b59f31d056cff7bd3075c1f',
  },
  detector: {
    url: '/static/models/hey_jarvis_v0.1.onnx',
    sha256: '94a13cfe60075b132f6a472e7e462e8123ee70861bc3fb58434a73712ee0d2cb',
    window: 16,
  },
};

// openWakeWord pipeline geometry.
const FRAME_SAMPLES = 1280; // 80 ms @ 16 kHz, matches the worklet
const N_MELS = 32;
const MEL_WINDOW = 76; // mel frames per embedding
const MEL_STEP = 8; // embedding hop, in mel frames
const EMB_DIM = 96;
const DEFAULT_DET_WINDOW = 16; // embeddings per detector inference
const REFRACTORY_MS = 2000; // ignore re-triggers for 2 s after a detection
const MAX_QUEUE = 15; // ~1.2 s of backlog before frames are dropped

function supportFailure() {
  if (typeof WebAssembly !== 'object') return 'WebAssembly is unavailable';
  if (typeof window === 'undefined' || typeof window.AudioWorkletNode === 'undefined') {
    return 'AudioWorklet is unavailable';
  }
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    return 'getUserMedia is unavailable';
  }
  if (!crypto || !crypto.subtle) return 'WebCrypto subtle is unavailable (insecure context?)';
  return null;
}

/** True when this browser can run the WASM wake-word engine at all. */
export function isWakeWordSupported() {
  return supportFailure() === null;
}

function unsupportedError(msg) {
  const err = new Error(`Wake word unsupported: ${msg}`);
  err.code = 'unsupported';
  return err;
}

async function sha256Hex(buf) {
  const digest = await crypto.subtle.digest('SHA-256', buf);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}

async function fetchVerified(url, expectedSha256) {
  const resp = await fetch(url, { credentials: 'same-origin' });
  if (!resp.ok) throw new Error(`fetch ${url}: HTTP ${resp.status}`);
  const buf = await resp.arrayBuffer();
  if (expectedSha256) {
    const got = await sha256Hex(buf);
    if (got !== expectedSha256.toLowerCase()) {
      throw new Error(`SHA-256 mismatch for ${url}: got ${got}, want ${expectedSha256}`);
    }
  }
  return buf;
}

/**
 * Resolve the model set for a wake-word id: backend manifest when available
 * (M6+), bundled "hey jarvis" otherwise. Never throws — the bundled set is
 * the guaranteed fallback.
 */
async function resolveModels(wakeWordId) {
  if (wakeWordId) {
    try {
      const ctl = new AbortController();
      const timer = setTimeout(() => ctl.abort(), 4000);
      const resp = await fetch(
        `/api/v1/wakeword/${encodeURIComponent(wakeWordId)}/model?platform=web`,
        { credentials: 'same-origin', signal: ctl.signal },
      );
      clearTimeout(timer);
      if (resp.ok) {
        const manifest = await resp.json();
        const m = manifest && manifest.models;
        if (
          m &&
          m.melspectrogram && m.melspectrogram.url &&
          m.embedding && m.embedding.url &&
          m.detector && m.detector.url
        ) {
          return {
            id: wakeWordId,
            phrase: manifest.phrase || wakeWordId,
            melspectrogram: m.melspectrogram,
            embedding: m.embedding,
            detector: { window: DEFAULT_DET_WINDOW, ...m.detector },
          };
        }
        // M6 single-model manifest (internal/wakeword ModelManifest): the
        // served artifact is the per-phrase DETECTOR only — openWakeWord's
        // melspectrogram + embedding stages are phrase-independent and ship
        // bundled, so pair the trained detector with them.
        if (manifest && manifest.url && manifest.sha256) {
          return {
            id: wakeWordId,
            phrase: manifest.phrase || wakeWordId,
            melspectrogram: BUNDLED_MODELS.melspectrogram,
            embedding: BUNDLED_MODELS.embedding,
            detector: {
              window: DEFAULT_DET_WINDOW,
              url: manifest.url,
              sha256: manifest.sha256,
            },
          };
        }
      }
      // 404 = unknown id / model not trained yet — fall through to bundled.
    } catch {
      // Network/timeout/shape failure — fall through to bundled.
    }
  }
  return BUNDLED_MODELS;
}

/**
 * Wake-word engine. One instance per page is expected; start()/stop() may be
 * called repeatedly (hands-free toggle).
 *
 * States: 'idle' → 'loading' → 'listening' → ('idle' after stop);
 * 'unsupported' / 'error' are terminal until the next start() attempt.
 */
export class WakeWordEngine {
  constructor(opts = {}) {
    this.onDetect = opts.onDetect || null;
    this.onStateChange = opts.onStateChange || null;
    this.onError = opts.onError || null;
    this.wakeWordId = opts.wakeWordId || null;
    this._threshold = 0.5;
    this.setSensitivity(typeof opts.sensitivity === 'number' ? opts.sensitivity : 0.5);

    this._state = isWakeWordSupported() ? 'idle' : 'unsupported';
    this._ort = null;
    this._sessions = null; // { mel, emb, det }
    this._names = null; // cached input/output names
    this._detWindow = DEFAULT_DET_WINDOW;
    this._phrase = null;

    this._ctx = null;
    this._node = null;
    this._source = null;
    this._sink = null;
    this._stream = null;
    this._ownStream = false;

    this._queue = [];
    this._busy = false;
    this._melFrames = [];
    this._melPos = 0;
    this._embBuf = [];
    this._lastDetect = 0;
    this._generation = 0; // invalidates in-flight work across stop()/start()
  }

  get state() {
    return this._state;
  }

  get supported() {
    return isWakeWordSupported();
  }

  get phrase() {
    return this._phrase;
  }

  /** sensitivity 0..1 (settings.schema.json) → detector score threshold. */
  setSensitivity(sensitivity) {
    const s = Math.min(1, Math.max(0, Number(sensitivity) || 0));
    // s=0 → 0.9 (strict), s=0.5 → 0.5, s=1 → 0.1 (eager).
    this._threshold = Math.min(0.95, Math.max(0.05, 0.9 - 0.8 * s));
  }

  _setState(state) {
    if (this._state === state) return;
    this._state = state;
    if (this.onStateChange) {
      try {
        this.onStateChange(state);
      } catch (e) {
        console.error('[wakeword] onStateChange handler threw', e);
      }
    }
  }

  _fail(err) {
    this._setState(err && err.code === 'unsupported' ? 'unsupported' : 'error');
    if (this.onError) {
      try {
        this.onError(err);
      } catch (e) {
        console.error('[wakeword] onError handler threw', e);
      }
    }
  }

  /**
   * Start listening. Options:
   *   stream — an existing mic MediaStream to tap (the engine will NOT stop
   *            its tracks on stop()); omitted → the engine opens its own mic
   *            with AEC/NS/AGC on and releases it on stop().
   * Rejects with err.code === 'unsupported' when the browser can't run the
   * engine (callers fall back to click-to-talk).
   */
  async start(opts = {}) {
    if (this._state === 'listening' || this._state === 'loading') return;
    const failure = supportFailure();
    if (failure) {
      const err = unsupportedError(failure);
      this._fail(err);
      throw err;
    }

    const gen = ++this._generation;
    this._setState('loading');
    try {
      await this._loadRuntimeAndModels();
      if (gen !== this._generation) return; // stopped while loading

      if (opts.stream) {
        this._stream = opts.stream;
        this._ownStream = false;
      } else {
        this._stream = await navigator.mediaDevices.getUserMedia({
          audio: {
            echoCancellation: true,
            noiseSuppression: true,
            autoGainControl: true,
            channelCount: 1,
          },
        });
        this._ownStream = true;
      }
      if (gen !== this._generation) {
        this._releaseAudio();
        return;
      }

      this._ctx = new AudioContext();
      if (this._ctx.state === 'suspended') await this._ctx.resume();
      await this._ctx.audioWorklet.addModule(WORKLET_URL);
      if (gen !== this._generation) {
        this._releaseAudio();
        return;
      }

      this._node = new AudioWorkletNode(this._ctx, 'ln-downsampler', {
        numberOfInputs: 1,
        numberOfOutputs: 1,
        channelCount: 1,
      });
      this._node.port.onmessage = (e) => {
        if (e.data && e.data.type === 'frame') this._enqueue(e.data.samples, gen);
      };
      this._source = this._ctx.createMediaStreamSource(this._stream);
      // A muted sink keeps the worklet pulled by the rendering graph without
      // ever echoing the mic to the speakers.
      this._sink = this._ctx.createGain();
      this._sink.gain.value = 0;
      this._source.connect(this._node);
      this._node.connect(this._sink);
      this._sink.connect(this._ctx.destination);

      this._resetPipeline();
      this._setState('listening');
    } catch (err) {
      this._releaseAudio();
      const wrapped =
        err && err.code === 'unsupported'
          ? err
          : this._classifyStartError(err);
      this._fail(wrapped);
      throw wrapped;
    }
  }

  /** Stop listening and release the mic (if the engine opened it). Idempotent. */
  async stop() {
    this._generation++;
    this._releaseAudio();
    this._resetPipeline();
    if (this._state !== 'unsupported') this._setState('idle');
  }

  // -- internals ------------------------------------------------------------

  _classifyStartError(err) {
    // CSP without 'wasm-unsafe-eval' surfaces as a CompileError/TypeError
    // from WebAssembly instantiation inside onnxruntime — treat as
    // unsupported (environmental, not transient).
    const msg = String((err && err.message) || err);
    if (
      err instanceof WebAssembly.CompileError ||
      /wasm|WebAssembly/i.test(msg) && /CSP|blocked|disallowed|unsafe-eval/i.test(msg)
    ) {
      return unsupportedError(`WASM blocked (${msg})`);
    }
    return err instanceof Error ? err : new Error(msg);
  }

  async _loadRuntimeAndModels() {
    if (!this._ort) {
      const ort = await import(ORT_MODULE_URL);
      ort.env.wasm.wasmPaths = ORT_WASM_DIR;
      // Threads need SharedArrayBuffer (cross-origin isolation); single
      // thread is the safe default everywhere else. See header comment.
      ort.env.wasm.numThreads = self.crossOriginIsolated
        ? Math.min(4, navigator.hardwareConcurrency || 1)
        : 1;
      this._ort = ort;
    }
    if (!this._sessions) {
      const models = await resolveModels(this.wakeWordId);
      this._phrase = models.phrase;
      this._detWindow = (models.detector && models.detector.window) || DEFAULT_DET_WINDOW;

      const [melBuf, embBuf, detBuf] = await Promise.all([
        fetchVerified(models.melspectrogram.url, models.melspectrogram.sha256),
        fetchVerified(models.embedding.url, models.embedding.sha256),
        fetchVerified(models.detector.url, models.detector.sha256),
      ]);
      const sessOpts = { executionProviders: ['wasm'] };
      const [mel, emb, det] = await Promise.all([
        this._ort.InferenceSession.create(melBuf, sessOpts),
        this._ort.InferenceSession.create(embBuf, sessOpts),
        this._ort.InferenceSession.create(detBuf, sessOpts),
      ]);
      this._sessions = { mel, emb, det };
      this._names = {
        melIn: mel.inputNames[0],
        melOut: mel.outputNames[0],
        embIn: emb.inputNames[0],
        embOut: emb.outputNames[0],
        detIn: det.inputNames[0],
        detOut: det.outputNames[0],
      };
      await this._warmUp();
    }
  }

  async _warmUp() {
    const { mel, emb, det } = this._sessions;
    const ort = this._ort;
    await mel.run({
      [this._names.melIn]: new ort.Tensor('float32', new Float32Array(FRAME_SAMPLES), [1, FRAME_SAMPLES]),
    });
    await emb.run({
      [this._names.embIn]: new ort.Tensor(
        'float32',
        new Float32Array(MEL_WINDOW * N_MELS),
        [1, MEL_WINDOW, N_MELS, 1],
      ),
    });
    await det.run({
      [this._names.detIn]: new ort.Tensor(
        'float32',
        new Float32Array(this._detWindow * EMB_DIM),
        [1, this._detWindow, EMB_DIM],
      ),
    });
  }

  _resetPipeline() {
    this._queue = [];
    this._melFrames = [];
    this._melPos = 0;
    this._embBuf = [];
  }

  _enqueue(samples, gen) {
    if (gen !== this._generation || this._state !== 'listening') return;
    this._queue.push(samples);
    // Realtime discipline: if inference falls behind, drop the oldest audio
    // rather than growing an unbounded backlog.
    while (this._queue.length > MAX_QUEUE) this._queue.shift();
    if (!this._busy) this._drain(gen);
  }

  async _drain(gen) {
    this._busy = true;
    try {
      while (this._queue.length > 0 && gen === this._generation) {
        const frame = this._queue.shift();
        await this._processFrame(frame, gen);
      }
    } catch (err) {
      if (gen === this._generation) {
        this._releaseAudio();
        this._fail(err instanceof Error ? err : new Error(String(err)));
      }
    } finally {
      this._busy = false;
    }
  }

  async _processFrame(frame, gen) {
    const ort = this._ort;
    const { mel, emb, det } = this._sessions;

    // 1) melspectrogram: [1, 1280] -> [1, 1, n, 32]
    const melOut = await mel.run({
      [this._names.melIn]: new ort.Tensor('float32', frame, [1, frame.length]),
    });
    if (gen !== this._generation) return;
    const melTensor = melOut[this._names.melOut];
    const nFrames = melTensor.dims[melTensor.dims.length - 2];
    const md = melTensor.data;
    for (let i = 0; i < nFrames; i++) {
      const fr = new Float32Array(N_MELS);
      for (let j = 0; j < N_MELS; j++) {
        // openWakeWord's canonical scaling of the mel output.
        fr[j] = md[i * N_MELS + j] / 10 + 2;
      }
      this._melFrames.push(fr);
    }

    // 2) embedding over each complete 76-frame window, hop 8.
    while (this._melFrames.length - this._melPos >= MEL_WINDOW) {
      const win = new Float32Array(MEL_WINDOW * N_MELS);
      for (let i = 0; i < MEL_WINDOW; i++) {
        win.set(this._melFrames[this._melPos + i], i * N_MELS);
      }
      const embOut = await emb.run({
        [this._names.embIn]: new ort.Tensor('float32', win, [1, MEL_WINDOW, N_MELS, 1]),
      });
      if (gen !== this._generation) return;
      this._embBuf.push(Float32Array.from(embOut[this._names.embOut].data));
      if (this._embBuf.length > this._detWindow) this._embBuf.shift();
      this._melPos += MEL_STEP;
      if (this._melPos > 512) {
        this._melFrames.splice(0, this._melPos);
        this._melPos = 0;
      }

      // 3) detector once the embedding window is full.
      if (this._embBuf.length === this._detWindow) {
        const flat = new Float32Array(this._detWindow * EMB_DIM);
        for (let i = 0; i < this._detWindow; i++) flat.set(this._embBuf[i], i * EMB_DIM);
        const detOut = await det.run({
          [this._names.detIn]: new ort.Tensor('float32', flat, [1, this._detWindow, EMB_DIM]),
        });
        if (gen !== this._generation) return;
        const score = Number(detOut[this._names.detOut].data[0]);
        const now = Date.now();
        if (score >= this._threshold && now - this._lastDetect >= REFRACTORY_MS) {
          this._lastDetect = now;
          if (this.onDetect) {
            try {
              this.onDetect({ score, wakeWordId: this.wakeWordId, phrase: this._phrase });
            } catch (e) {
              console.error('[wakeword] onDetect handler threw', e);
            }
          }
        }
      }
    }
  }

  _releaseAudio() {
    if (this._node) {
      try {
        this._node.port.postMessage({ type: 'stop' });
        this._node.disconnect();
      } catch { /* already torn down */ }
      this._node = null;
    }
    if (this._source) {
      try {
        this._source.disconnect();
      } catch { /* already torn down */ }
      this._source = null;
    }
    if (this._sink) {
      try {
        this._sink.disconnect();
      } catch { /* already torn down */ }
      this._sink = null;
    }
    if (this._ctx) {
      const ctx = this._ctx;
      this._ctx = null;
      ctx.close().catch(() => {});
    }
    if (this._stream && this._ownStream) {
      for (const track of this._stream.getTracks()) track.stop();
    }
    this._stream = null;
    this._ownStream = false;
  }
}

/** Factory matching the task brief's {start, stop, onDetect} interface. */
export function createWakeWordEngine(opts) {
  return new WakeWordEngine(opts);
}
