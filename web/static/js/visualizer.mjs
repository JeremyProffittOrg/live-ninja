// visualizer.mjs — real speech-volume line graph for the conversation
// screen.
//
// Owner: WS-D (web client) — transcript + visualizer workstream.
//
// A scrolling line graph of the CURRENT audio level: while listening it
// plots the local mic's RMS volume, while the assistant speaks it plots the
// remote stream's RMS volume. Nothing is synthesized — if the line is flat,
// no audio is reaching the active source (which is exactly the diagnostic
// the old decorative "always-on wave" hid). The newest sample enters at the
// right edge and history scrolls left.
//
// The module owns only the <canvas> it is constructed with; the caller
// (conversation.mjs) attaches streams via setLocalStream()/setRemoteStream()
// and selects which one drives the graph with setActiveSource() as the mic
// state machine changes. aria-hidden — the state pill is the accessible
// rendering of the same information.
//
// No external dependencies; plain ES module, no bundler assumptions.

/** @typedef {'none'|'local'|'remote'} VisualizerSource */

/**
 * @typedef {Object} VisualizerOptions
 * @property {number} [sampleIntervalMs=50] How often a new volume sample is
 *   appended to the graph (each sample is one horizontal step).
 * @property {number} [pxPerSample=2] Horizontal pixels per sample — with the
 *   50ms cadence, 2px/sample scrolls one canvas width (~240px) in ~6s.
 * @property {number} [gain=3.5] Linear gain applied to the raw RMS before
 *   display; conversational speech RMS on a normalized mic sits around
 *   0.02–0.25, so this lifts it into the visible range without clipping.
 * @property {number} [fftSize=1024] AnalyserNode.fftSize for the
 *   time-domain window (~21ms at 48kHz — comfortably inside the 50ms
 *   sample cadence).
 * @property {number} [reducedMotionIntervalMs=400] Sample/redraw cadence
 *   used instead of requestAnimationFrame under prefers-reduced-motion —
 *   the graph becomes a slow discrete level meter rather than a smooth
 *   scroll.
 * @property {string} [colorFromVar='--ln-cyan'] CSS custom property for the
 *   line color.
 * @property {string} [colorToVar='--ln-teal-600'] CSS custom property for
 *   the under-line fill color.
 */

const DEFAULTS = {
  sampleIntervalMs: 50,
  pxPerSample: 2,
  gain: 3.5,
  fftSize: 1024,
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
 * Scrolling volume line graph with a devicePixelRatio-aware backing store
 * and a reduced-motion fallback.
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

    // Always decorative — redundant with the accessible state pill.
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

    // Ring buffer of volume samples (0..1), oldest → newest.
    this._history = new Float32Array(0);
    this._historyLen = 0;
    this._timeData = null; // sized lazily to the analyser's fftSize
    this._lastSampleAt = 0;

    this._running = false;
    this._rafId = null;
    this._intervalId = null;
    this._quietSamples = 0; // consecutive zero samples after source loss

    this._colors = { from: "#38d0ff", to: "#12b6b0" }; // fallback if CSS vars are missing
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

    // One baseline frame so the canvas isn't blank before the first start().
    this._draw();
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
   * Selects which attached stream's level drives the graph. Pass 'none'
   * for idle/thinking — the line runs down to the baseline and the loop
   * parks so an idle canvas costs nothing.
   * @param {VisualizerSource} source
   */
  setActiveSource(source) {
    if (source !== "none" && source !== "local" && source !== "remote") {
      throw new Error(`Visualizer.setActiveSource: invalid source "${source}"`);
    }
    if (this._activeSource === source) return;
    this._activeSource = source;
    this._quietSamples = 0;
    if (this._running) this._scheduleLoop();
  }

  /** Most recent volume sample (0..1) — lets callers (the mic test) check
   * whether any signal is arriving without their own audio graph. */
  get level() {
    return this._historyLen > 0 ? this._history[this._historyLen - 1] : 0;
  }

  /** Resumes the AudioContext (if any) and begins the sample/draw loop. */
  start() {
    this._running = true;
    if (this._audioCtx && this._audioCtx.state === "suspended") {
      // Best-effort; the caller only calls start() after a mic-granting
      // tap, so resume() should always be permitted in practice.
      this._audioCtx.resume().catch(() => {});
    }
    this._scheduleLoop();
  }

  /** Halts the loop (leaves the audio graph intact for a quick resume). */
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
    if (!this._running) return;

    if (this._reducedMotion()) {
      // Reduced motion: slow discrete level meter, no smooth scroll.
      const tick = () => {
        this._pushSample(this._readLevel());
        this._draw();
        this._parkIfQuiet();
      };
      tick();
      this._intervalId = setInterval(tick, this._opts.reducedMotionIntervalMs);
      return;
    }

    this._lastSampleAt = 0;
    const loop = (ts) => {
      if (this._lastSampleAt === 0 || ts - this._lastSampleAt >= this._opts.sampleIntervalMs) {
        // Catch up if frames were skipped so the scroll speed stays tied to
        // wall-clock time, not the frame rate.
        const behind =
          this._lastSampleAt === 0
            ? 1
            : Math.min(5, Math.floor((ts - this._lastSampleAt) / this._opts.sampleIntervalMs));
        const level = this._readLevel();
        for (let i = 0; i < behind; i++) this._pushSample(level);
        this._lastSampleAt = ts;
        this._draw();
        if (this._parkIfQuiet()) return;
      }
      this._rafId = requestAnimationFrame(loop);
    };
    this._rafId = requestAnimationFrame(loop);
  }

  /** With no active source, park the loop once the line has fully scrolled
   * down to (and along) the baseline. Returns true when parked. */
  _parkIfQuiet() {
    if (this._activeSource !== "none") {
      this._quietSamples = 0;
      return false;
    }
    this._quietSamples++;
    const capacity = this._history.length || 1;
    if (this._quietSamples > capacity) {
      this._stopLoop();
      return true;
    }
    return false;
  }

  _reducedMotion() {
    return this._mql ? this._mql.matches : false;
  }

  _onReducedMotionChange() {
    if (this._running) this._scheduleLoop();
  }

  /** Current volume of the active source: time-domain RMS through a gain
   * and a square-root curve (perceptual lift for quiet speech), 0..1.
   * No source / no analyser → 0. */
  _readLevel() {
    const analyser = this._activeAnalyser();
    if (!analyser) return 0;
    const n = analyser.fftSize;
    if (!this._timeData || this._timeData.length !== n) {
      this._timeData = new Float32Array(n);
    }
    let sumSq = 0;
    if (typeof analyser.getFloatTimeDomainData === "function") {
      analyser.getFloatTimeDomainData(this._timeData);
      for (let i = 0; i < n; i++) {
        const v = this._timeData[i];
        sumSq += v * v;
      }
    } else {
      // Safari fallback: byte samples centered on 128.
      const bytes = new Uint8Array(n);
      analyser.getByteTimeDomainData(bytes);
      for (let i = 0; i < n; i++) {
        const v = (bytes[i] - 128) / 128;
        sumSq += v * v;
      }
    }
    const rms = Math.sqrt(sumSq / n);
    return Math.min(1, Math.sqrt(rms * this._opts.gain));
  }

  _pushSample(level) {
    const cap = this._history.length;
    if (cap === 0) return;
    if (this._historyLen < cap) {
      this._history[this._historyLen++] = level;
    } else {
      this._history.copyWithin(0, 1);
      this._history[cap - 1] = level;
    }
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

    // Re-fit the history ring to the new width, preserving the newest
    // samples so a resize doesn't blank the graph.
    const capacity = Math.max(2, Math.ceil(cssWidth / this._opts.pxPerSample) + 1);
    if (capacity !== this._history.length) {
      const next = new Float32Array(capacity);
      const keep = Math.min(this._historyLen, capacity);
      for (let i = 0; i < keep; i++) {
        next[i] = this._history[this._historyLen - keep + i];
      }
      this._history = next;
      this._historyLen = keep;
    }
    this._draw();
  }

  _refreshColors() {
    if (typeof getComputedStyle !== "function" || typeof document === "undefined") return;
    const style = getComputedStyle(document.documentElement);
    const from = style.getPropertyValue(this._opts.colorFromVar).trim();
    const to = style.getPropertyValue(this._opts.colorToVar).trim();
    if (from) this._colors.from = from;
    if (to) this._colors.to = to;
  }

  _draw() {
    const ctx = this._ctx;
    const width = this._canvas.clientWidth || this._canvas.width;
    const height = this._canvas.clientHeight || this._canvas.height;
    ctx.clearRect(0, 0, width, height);
    if (width <= 0 || height <= 0) return;

    const baseY = height - 1.5; // baseline just above the bottom edge
    const topPad = 2; // keep full-scale peaks inside the canvas
    const usable = baseY - topPad;
    const step = this._opts.pxPerSample;
    const len = this._historyLen;

    // Baseline (always drawn, faint) so an idle/quiet graph reads as a
    // deliberate flat line, not an empty element.
    ctx.strokeStyle = this._colors.to;
    ctx.globalAlpha = 0.35;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(0, baseY);
    ctx.lineTo(width, baseY);
    ctx.stroke();
    ctx.globalAlpha = 1;

    if (len < 2) return;

    // Newest sample pinned to the right edge; history extends left.
    const xAt = (i) => width - (len - 1 - i) * step;
    const yAt = (i) => baseY - this._history[i] * usable;

    // Under-line fill.
    ctx.beginPath();
    ctx.moveTo(xAt(0), baseY);
    for (let i = 0; i < len; i++) ctx.lineTo(xAt(i), yAt(i));
    ctx.lineTo(xAt(len - 1), baseY);
    ctx.closePath();
    ctx.fillStyle = this._colors.to;
    ctx.globalAlpha = 0.18;
    ctx.fill();
    ctx.globalAlpha = 1;

    // The line itself.
    ctx.beginPath();
    ctx.moveTo(xAt(0), yAt(0));
    for (let i = 1; i < len; i++) ctx.lineTo(xAt(i), yAt(i));
    ctx.strokeStyle = this._colors.from;
    ctx.lineWidth = 1.5;
    ctx.lineJoin = "round";
    ctx.lineCap = "round";
    ctx.stroke();
  }
}
