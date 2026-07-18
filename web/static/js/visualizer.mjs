// visualizer.mjs — real audio-reactive canvas visualizer for the
// conversation screen's mic/assistant levels.
//
// Owner: WS-D (M3 web client) — transcript + visualizer workstream.
// Spec: docs/web-ui-spec.md §2.3 ("Visualizer bars, decorative... Web Audio
// AnalyserNode on the local mic track (listening) and remote track
// (speaking)... aria-hidden=true"), plan.md M3 "Web §6"
// ("visualizer.mjs (AnalyserNode→canvas, aria-hidden, prefers-reduced-motion)"),
// PRD.md FR-W04 ("canvas visualizer aria-hidden").
//
// Scope note: the mockups' `.viz` element is CSS-animated span bars used as
// a visual/motion reference only (per the spec's "mockups vs. this spec"
// note) — this module replaces that with a *real* Web Audio-driven canvas,
// per the task brief. It owns only the <canvas> element it is constructed
// with; it never touches the transcript, settings, or the realtime
// datachannel — the caller (realtime.mjs / the mic state machine, both out
// of this file's ownership) is responsible for calling setLocalStream(),
// setRemoteStream(), and setActiveSource() as the call state changes.
//
// No external dependencies; plain ES module, no bundler assumptions.

/** @typedef {'none'|'local'|'remote'} VisualizerSource */

/**
 * @typedef {Object} VisualizerOptions
 * @property {number} [barCount=32] Number of bars drawn across the canvas.
 * @property {number} [fftSize=64] AnalyserNode.fftSize (power of 2, >=32).
 *   Frequency-bin count is fftSize/2; barCount should not exceed that.
 * @property {number} [gapRatio=0.35] Fraction of each bar's slot left as
 *   gap between bars (0–0.9).
 * @property {number} [minBarRatio=0.06] Minimum bar height as a fraction of
 *   canvas height, so bars never fully disappear (matches the mockup's
 *   idle "8px of 48px" resting height).
 * @property {number} [attack=0.6] Lerp factor toward a rising level
 *   (0–1, higher = snappier). Ignored under reduced motion (snaps instantly).
 * @property {number} [release=0.15] Lerp factor toward a falling level.
 *   Ignored under reduced motion.
 * @property {number} [reducedMotionIntervalMs=400] Redraw cadence used
 *   instead of requestAnimationFrame when the user prefers reduced motion —
 *   this is what makes the reduced-motion mode a "static level meter"
 *   rather than a continuously-animated one: discrete, infrequent updates
 *   with no inter-frame interpolation.
 * @property {string} [colorFromVar='--ln-cyan'] CSS custom property read
 *   from :root for the bar gradient's top color.
 * @property {string} [colorToVar='--ln-teal-600'] CSS custom property read
 *   from :root for the bar gradient's bottom color.
 */

const DEFAULTS = {
  barCount: 32,
  fftSize: 64,
  gapRatio: 0.35,
  minBarRatio: 0.06,
  // A gentle always-present traveling wave shown while a source is active
  // (listening/speaking) so the user has an unmistakable "it's live / capturing"
  // cue even during quiet moments; real audio spikes the bars above it.
  // Suppressed under prefers-reduced-motion (that mode is a static level meter).
  idleAmplitude: 0.22,
  idlePhaseStep: 0.14,
  attack: 0.6,
  release: 0.15,
  reducedMotionIntervalMs: 400,
  colorFromVar: "--ln-cyan",
  colorToVar: "--ln-teal-600",
};

function reducedMotionMediaQuery() {
  return typeof window !== "undefined" && typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-reduced-motion: reduce)")
    : null;
}

/** Cross-browser add/remove for MediaQueryList change events. */
function watchMediaQuery(mql, handler) {
  if (!mql) return () => {};
  if (typeof mql.addEventListener === "function") {
    mql.addEventListener("change", handler);
    return () => mql.removeEventListener("change", handler);
  }
  // Safari <14 fallback.
  mql.addListener(handler);
  return () => mql.removeListener(handler);
}

/**
 * Audio-reactive canvas bar visualizer with a devicePixelRatio-aware
 * backing store and a reduced-motion fallback.
 */
export class Visualizer {
  /**
   * @param {HTMLCanvasElement} canvasEl
   * @param {VisualizerOptions} [options]
   */
  constructor(canvasEl, options = {}) {
    if (!canvasEl || typeof canvasEl.getContext !== "function") {
      throw new Error("Visualizer requires a <canvas> element");
    }
    this._canvas = canvasEl;
    this._ctx = canvasEl.getContext("2d");
    this._opts = { ...DEFAULTS, ...options };

    // Always decorative — redundant with the accessible state pill (§2.3/§2.8).
    this._canvas.setAttribute("aria-hidden", "true");

    /** @type {AudioContext|null} */
    this._audioCtx = null;
    this._localStream = null;
    this._remoteStream = null;
    this._localSourceNode = null;
    this._remoteSourceNode = null;
    this._localAnalyser = null;
    this._remoteAnalyser = null;
    /** @type {VisualizerSource} */
    this._activeSource = "none";

    this._barLevels = new Float32Array(this._opts.barCount);
    this._freqData = null; // sized lazily once an AnalyserNode exists
    this._phase = 0; // advances the idle traveling wave (normal-motion only)

    this._running = false;
    this._rafId = null;
    this._intervalId = null;

    this._colors = { from: "#38d0ff", to: "#12b6b0" }; // safe fallback if CSS vars are missing
    this._refreshColors();

    this._mql = reducedMotionMediaQuery();
    this._unwatchMql = watchMediaQuery(this._mql, () => this._onReducedMotionChange());

    this._themeObserver = null;
    if (typeof MutationObserver !== "undefined" && typeof document !== "undefined") {
      this._themeObserver = new MutationObserver(() => this._refreshColors());
      this._themeObserver.observe(document.documentElement, {
        attributes: true,
        attributeFilter: ["data-theme"],
      });
    }
    this._darkSchemeMql =
      typeof window !== "undefined" && typeof window.matchMedia === "function"
        ? window.matchMedia("(prefers-color-scheme: dark)")
        : null;
    this._unwatchDarkScheme = watchMediaQuery(this._darkSchemeMql, () => this._refreshColors());

    this._resizeObserver = null;
    if (typeof ResizeObserver !== "undefined") {
      this._resizeObserver = new ResizeObserver(() => this._resizeBackingStore());
      this._resizeObserver.observe(this._canvas);
    }
    this._resizeBackingStore();

    // Draw one idle frame immediately so the canvas isn't blank before the
    // first stream/start() call.
    this._drawFlat();
  }

  /**
   * Attaches (or clears, if `stream` is null) the local mic MediaStream.
   * Safe to call before/without an existing AudioContext — one is created
   * lazily on first stream attach.
   * @param {MediaStream|null} stream
   */
  setLocalStream(stream) {
    this._localStream = stream;
    if (this._localSourceNode) {
      this._localSourceNode.disconnect();
      this._localSourceNode = null;
      this._localAnalyser = null;
    }
    if (stream) {
      this._ensureAudioContext();
      this._localAnalyser = this._audioCtx.createAnalyser();
      this._localAnalyser.fftSize = this._opts.fftSize;
      this._localSourceNode = this._audioCtx.createMediaStreamSource(stream);
      this._localSourceNode.connect(this._localAnalyser);
      // Not connected to destination: this graph only analyzes, it never
      // plays audio (the mic must never be looped back to speakers).
    }
  }

  /**
   * Attaches (or clears) the remote assistant-audio MediaStream (the same
   * stream driving the conversation page's hidden `<audio>` element —
   * attaching an analyser here does not affect that element's playback,
   * a MediaStream supports multiple independent consumers).
   * @param {MediaStream|null} stream
   */
  setRemoteStream(stream) {
    this._remoteStream = stream;
    if (this._remoteSourceNode) {
      this._remoteSourceNode.disconnect();
      this._remoteSourceNode = null;
      this._remoteAnalyser = null;
    }
    if (stream) {
      this._ensureAudioContext();
      this._remoteAnalyser = this._audioCtx.createAnalyser();
      this._remoteAnalyser.fftSize = this._opts.fftSize;
      this._remoteSourceNode = this._audioCtx.createMediaStreamSource(stream);
      this._remoteSourceNode.connect(this._remoteAnalyser);
    }
  }

  /**
   * Selects which attached stream's analyser drives the bars. Pass 'none'
   * for idle/thinking (draws — and then stops on — a single flat frame,
   * so the canvas costs nothing while there is nothing to visualize).
   * @param {VisualizerSource} source
   */
  setActiveSource(source) {
    if (source !== "none" && source !== "local" && source !== "remote") {
      throw new Error(`Visualizer.setActiveSource: invalid source "${source}"`);
    }
    this._activeSource = source;
    if (source === "none") {
      this._stopLoop();
      this._decayToFlatThenDraw();
    } else if (this._running) {
      // Re-(re)schedule explicitly rather than assuming a loop is already
      // in flight for the *sampling* case — a prior setActiveSource('none')
      // may currently be running its decay-to-flat rAF chain, which would
      // otherwise keep drawing toward flat and never resume real sampling.
      this._scheduleLoop();
    }
  }

  /** Resumes the AudioContext (if any) and begins the draw loop. */
  start() {
    this._running = true;
    if (this._audioCtx && this._audioCtx.state === "suspended") {
      // Best-effort; ignored if it rejects (e.g. no user gesture yet —
      // the caller only calls start() after a mic-granting tap, so this
      // should always succeed in practice).
      this._audioCtx.resume().catch(() => {});
    }
    if (this._activeSource === "none") {
      this._drawFlat();
      return;
    }
    this._scheduleLoop();
  }

  /** Halts the draw loop (leaves the audio graph intact for a quick resume). */
  stop() {
    this._running = false;
    this._stopLoop();
  }

  /** Fully tears down: stops the loop, disconnects nodes, closes the AudioContext, detaches observers. */
  destroy() {
    this.stop();
    if (this._localSourceNode) this._localSourceNode.disconnect();
    if (this._remoteSourceNode) this._remoteSourceNode.disconnect();
    if (this._audioCtx) {
      this._audioCtx.close().catch(() => {});
    }
    if (this._resizeObserver) this._resizeObserver.disconnect();
    if (this._themeObserver) this._themeObserver.disconnect();
    this._unwatchDarkScheme();
    this._unwatchMql();
  }

  // ---------------------------------------------------------------------
  // Internals
  // ---------------------------------------------------------------------

  _ensureAudioContext() {
    if (this._audioCtx) return;
    const Ctx = window.AudioContext || window.webkitAudioContext;
    this._audioCtx = new Ctx();
  }

  _activeAnalyser() {
    if (this._activeSource === "local") return this._localAnalyser;
    if (this._activeSource === "remote") return this._remoteAnalyser;
    return null;
  }

  _stopLoop() {
    if (this._rafId != null) {
      cancelAnimationFrame(this._rafId);
      this._rafId = null;
    }
    if (this._intervalId != null) {
      clearInterval(this._intervalId);
      this._intervalId = null;
    }
  }

  _scheduleLoop() {
    this._stopLoop();
    if (!this._running || this._activeSource === "none") return;

    if (this._reducedMotion()) {
      // Reduced motion: throttled, non-interpolated updates — a "static
      // level meter" rather than a smooth animation.
      const tick = () => {
        this._sampleLevels(/* snap */ true);
        this._draw();
      };
      tick();
      this._intervalId = setInterval(tick, this._opts.reducedMotionIntervalMs);
      return;
    }

    const loop = () => {
      this._sampleLevels(/* snap */ false);
      this._draw();
      this._rafId = requestAnimationFrame(loop);
    };
    this._rafId = requestAnimationFrame(loop);
  }

  _reducedMotion() {
    return this._mql ? this._mql.matches : false;
  }

  _onReducedMotionChange() {
    // Switch cadence immediately if a call is in progress.
    if (this._running && this._activeSource !== "none") {
      this._scheduleLoop();
    }
  }

  /**
   * Reads the active analyser's frequency data, buckets it into barCount
   * groups, and updates this._barLevels (0..1 per bar) — either snapped
   * (reduced motion) or lerped toward the target (normal motion, with
   * separate attack/release rates for a punchier rise and a smoother fall).
   */
  _sampleLevels(snap) {
    const analyser = this._activeAnalyser();
    const { barCount } = this._opts;

    // Idle floor: a gentle traveling wave so the strip is visibly alive while a
    // source is active. Zero under reduced motion (snap) — that mode is a plain
    // level meter. Advance the phase once per frame here (not per bar).
    if (!snap) this._phase += this._opts.idlePhaseStep;
    const idleAmp = snap ? 0 : this._opts.idleAmplitude;
    const floorAt = (i) => {
      if (idleAmp === 0) return this._opts.minBarRatio;
      const wave = 0.5 + 0.5 * Math.sin(this._phase - i * 0.55);
      return this._opts.minBarRatio + wave * idleAmp;
    };

    if (!analyser) {
      for (let i = 0; i < barCount; i++) {
        const target = floorAt(i);
        this._barLevels[i] = snap ? target : this._lerp(this._barLevels[i], target, this._opts.release);
      }
      return;
    }

    const bins = analyser.frequencyBinCount;
    if (!this._freqData || this._freqData.length !== bins) {
      this._freqData = new Uint8Array(bins);
    }
    analyser.getByteFrequencyData(this._freqData);

    const binsPerBar = Math.max(1, Math.floor(bins / barCount));
    for (let i = 0; i < barCount; i++) {
      let sum = 0;
      const start = i * binsPerBar;
      const end = Math.min(bins, start + binsPerBar);
      for (let b = start; b < end; b++) sum += this._freqData[b];
      const avg = end > start ? sum / (end - start) : 0;
      // Real audio spikes above the animated idle floor.
      const target = Math.max(floorAt(i), avg / 255);

      if (snap) {
        this._barLevels[i] = target;
      } else {
        const rate = target > this._barLevels[i] ? this._opts.attack : this._opts.release;
        this._barLevels[i] = this._lerp(this._barLevels[i], target, rate);
      }
    }
  }

  _lerp(from, to, t) {
    return from + (to - from) * t;
  }

  /** Draws a single flat/idle frame, then (once, not looping) leaves the canvas alone. */
  _drawFlat() {
    this._barLevels.fill(this._opts.minBarRatio);
    this._draw({ idle: true });
  }

  /** On transition to "none", ease the existing bars down to flat over a
   * few frames rather than snapping, then park (matches the mockup's
   * idle opacity/height resting state without a perpetual rAF loop). */
  _decayToFlatThenDraw() {
    if (this._reducedMotion()) {
      this._drawFlat();
      return;
    }
    let frames = 0;
    const step = () => {
      frames += 1;
      let allFlat = true;
      for (let i = 0; i < this._barLevels.length; i++) {
        this._barLevels[i] = this._lerp(this._barLevels[i], this._opts.minBarRatio, 0.25);
        if (Math.abs(this._barLevels[i] - this._opts.minBarRatio) > 0.01) allFlat = false;
      }
      this._draw({ idle: allFlat });
      if (!allFlat && frames < 60) {
        this._rafId = requestAnimationFrame(step);
      } else {
        this._rafId = null;
      }
    };
    this._stopLoop();
    step();
  }

  _resizeBackingStore() {
    const dpr = window.devicePixelRatio || 1;
    const cssWidth = this._canvas.clientWidth || this._canvas.width || 1;
    const cssHeight = this._canvas.clientHeight || this._canvas.height || 1;
    const targetWidth = Math.max(1, Math.round(cssWidth * dpr));
    const targetHeight = Math.max(1, Math.round(cssHeight * dpr));
    if (this._canvas.width !== targetWidth || this._canvas.height !== targetHeight) {
      this._canvas.width = targetWidth;
      this._canvas.height = targetHeight;
    }
    // All subsequent drawing happens in CSS-pixel coordinates.
    this._ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    this._draw({ idle: this._activeSource === "none" });
  }

  _refreshColors() {
    if (typeof getComputedStyle !== "function" || typeof document === "undefined") return;
    const style = getComputedStyle(document.documentElement);
    const from = style.getPropertyValue(this._opts.colorFromVar).trim();
    const to = style.getPropertyValue(this._opts.colorToVar).trim();
    if (from) this._colors.from = from;
    if (to) this._colors.to = to;
  }

  _draw(opts = {}) {
    const ctx = this._ctx;
    const width = this._canvas.clientWidth || this._canvas.width;
    const height = this._canvas.clientHeight || this._canvas.height;
    ctx.clearRect(0, 0, width, height);
    if (width <= 0 || height <= 0) return;

    const { barCount, gapRatio } = this._opts;
    const slot = width / barCount;
    const barWidth = Math.max(1, slot * (1 - gapRatio));
    const gradient = ctx.createLinearGradient(0, 0, 0, height);
    gradient.addColorStop(0, this._colors.from);
    gradient.addColorStop(1, this._colors.to);
    ctx.fillStyle = gradient;
    ctx.globalAlpha = opts.idle ? 0.35 : 1;

    for (let i = 0; i < barCount; i++) {
      const level = this._barLevels[i] ?? this._opts.minBarRatio;
      const barHeight = Math.max(2, level * height);
      const x = i * slot + (slot - barWidth) / 2;
      const y = (height - barHeight) / 2;
      const radius = Math.min(barWidth / 2, 3);
      this._roundRect(ctx, x, y, barWidth, barHeight, radius);
      ctx.fill();
    }
    ctx.globalAlpha = 1;
  }

  _roundRect(ctx, x, y, w, h, r) {
    if (typeof ctx.roundRect === "function") {
      ctx.beginPath();
      ctx.roundRect(x, y, w, h, r);
      return;
    }
    // Fallback for browsers without CanvasRenderingContext2D.roundRect.
    const radius = Math.min(r, w / 2, h / 2);
    ctx.beginPath();
    ctx.moveTo(x + radius, y);
    ctx.arcTo(x + w, y, x + w, y + h, radius);
    ctx.arcTo(x + w, y + h, x, y + h, radius);
    ctx.arcTo(x, y + h, x, y, radius);
    ctx.arcTo(x, y, x + w, y, radius);
    ctx.closePath();
  }
}
