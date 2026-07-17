/**
 * AudioWorklet processor for the Live Ninja wake-word engine.
 *
 * Runs on the audio rendering thread: takes the (usually 48 kHz) mic input,
 * linearly resamples it to 16 kHz mono, packs it into 1280-sample frames
 * (80 ms — openWakeWord's native chunk size) and posts each frame to the
 * main thread as a transferable Float32Array.
 *
 * Loaded by /static/js/wakeword.mjs via audioWorklet.addModule(); registered
 * under the name "ln-downsampler".
 *
 * No imports — AudioWorklet global scope only (sampleRate, AudioWorkletProcessor,
 * registerProcessor are provided by the spec).
 */

'use strict';

const TARGET_RATE = 16000;
const FRAME_SAMPLES = 1280; // 80 ms at 16 kHz

class LnDownsamplerProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this._ratio = sampleRate / TARGET_RATE; // e.g. 3.0 for 48 kHz
    this._pos = 0; // fractional read position into the current block; may be in [-1, 0)
    this._last = 0; // final sample of the previous block, for cross-block interpolation
    this._frame = new Float32Array(FRAME_SAMPLES);
    this._n = 0;
    this._stopped = false;
    this.port.onmessage = (e) => {
      if (e.data && e.data.type === 'stop') this._stopped = true;
    };
  }

  _push(sample) {
    this._frame[this._n++] = sample;
    if (this._n === FRAME_SAMPLES) {
      // Transfer a copy so the internal buffer can be reused immediately.
      const out = this._frame.slice(0);
      this.port.postMessage({ type: 'frame', samples: out }, [out.buffer]);
      this._n = 0;
    }
  }

  process(inputs) {
    if (this._stopped) return false; // let the node be garbage-collected
    const input = inputs[0];
    if (!input || input.length === 0) return true;
    const ch = input[0]; // mono: first channel only
    if (!ch || ch.length === 0) return true;

    const N = ch.length;
    let pos = this._pos;

    // pos < 0 means the interpolation window straddles the block boundary:
    // interpolate between the carried last sample and ch[0].
    while (pos < 0) {
      const frac = pos + 1; // in [0, 1)
      this._push(this._last * (1 - frac) + ch[0] * frac);
      pos += this._ratio;
    }
    while (pos + 1 < N) {
      const j = pos | 0;
      const frac = pos - j;
      this._push(ch[j] * (1 - frac) + ch[j + 1] * frac);
      pos += this._ratio;
    }
    this._pos = pos - N;
    this._last = ch[N - 1];
    return true;
  }
}

registerProcessor('ln-downsampler', LnDownsamplerProcessor);
