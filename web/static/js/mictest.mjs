// mictest.mjs — self-serve microphone check for the conversation page.
//
// Opens a native <dialog> (ids from templates/pages/conversation.html:
// micTestDialog/micTestStatus/micTestMeter/micTestDevice/micTestTips/
// micTestClose), acquires the mic with the SAME constraints + device pin the
// realtime session uses, and drives the shared Visualizer line graph so what
// the user sees here is exactly what a live session would capture. Verdicts:
//   - level above the speech threshold  → "we can hear you" + peak readout
//   - 6s of silence                     → "not hearing anything" + tips open
//   - getUserMedia failure              → specific copy per error + tips open
//
// The tips content (Chrome site-permission steps, Windows privacy, device
// selection, app conflicts) lives in the template — this module only opens
// the <details> when the outcome warrants it.

import { Visualizer } from './visualizer.mjs';
import { acquireMicStream } from './realtime.mjs';

const HEARD_THRESHOLD = 0.08; // displayed level (post sqrt-curve) that counts as speech
const SILENCE_MS = 6000;
const POLL_MS = 200;

export function createMicTest({ getMicDeviceId = () => null, document: doc = document } = {}) {
  const dlg = doc.getElementById('micTestDialog');
  const statusEl = doc.getElementById('micTestStatus');
  const meterEl = doc.getElementById('micTestMeter');
  const deviceEl = doc.getElementById('micTestDevice');
  const tipsEl = doc.getElementById('micTestTips');
  const closeBtn = doc.getElementById('micTestClose');
  if (!dlg || !statusEl || !meterEl || !closeBtn) {
    return { open: () => {} }; // partial page — mic test simply unavailable
  }

  let stream = null;
  let viz = null;
  let pollTimer = 0;
  let openedAt = 0;
  let peak = 0;
  let heard = false;

  function setStatus(text, { good = false, bad = false } = {}) {
    statusEl.textContent = text;
    statusEl.classList.toggle('is-good', good);
    statusEl.classList.toggle('is-bad', bad);
  }

  function cleanup() {
    clearInterval(pollTimer);
    pollTimer = 0;
    if (viz) {
      viz.destroy();
      viz = null;
    }
    if (stream) {
      for (const t of stream.getTracks()) t.stop();
      stream = null;
    }
  }

  function fail(err) {
    const name = err && err.name;
    if (name === 'NotAllowedError' || name === 'SecurityError') {
      setStatus(
        'Chrome is blocking microphone access for this site — see the Chrome tip below.',
        { bad: true },
      );
    } else if (name === 'NotFoundError' || name === 'OverconstrainedError') {
      setStatus('No microphone was found. Plug one in (or pick a different one in Settings) and re-open this test.', { bad: true });
    } else if (name === 'NotReadableError') {
      setStatus('The microphone is busy — another app (Zoom, Teams, OBS…) may be holding it.', { bad: true });
    } else {
      setStatus('Could not open the microphone. See the tips below.', { bad: true });
    }
    if (tipsEl) tipsEl.open = true;
  }

  function poll() {
    if (!viz) return;
    const level = viz.level;
    if (level > peak) peak = level;
    if (level >= HEARD_THRESHOLD) heard = true;

    if (heard) {
      setStatus(
        `We can hear you — peak level ${Math.round(peak * 100)}%. This mic works with Live Ninja.`,
        { good: true },
      );
    } else if (Date.now() - openedAt > SILENCE_MS) {
      setStatus(
        "We're not hearing anything. Speak at normal volume — if the line stays flat, work through the tips below.",
        { bad: true },
      );
      if (tipsEl) tipsEl.open = true;
    }
  }

  async function open() {
    peak = 0;
    heard = false;
    openedAt = Date.now();
    if (tipsEl) tipsEl.open = false;
    if (deviceEl) deviceEl.textContent = '';
    setStatus('Requesting microphone…');
    if (typeof dlg.showModal === 'function' && !dlg.open) dlg.showModal();

    try {
      stream = await acquireMicStream({ deviceId: getMicDeviceId() });
    } catch (err) {
      fail(err);
      return;
    }

    const track = stream.getAudioTracks()[0];
    if (deviceEl && track) {
      deviceEl.textContent = `Using: ${track.label || 'default microphone'}`;
    }

    viz = new Visualizer(meterEl);
    viz.setLocalStream(stream);
    viz.setActiveSource('local');
    viz.start();

    setStatus('Listening — say something…');
    openedAt = Date.now();
    clearInterval(pollTimer);
    pollTimer = setInterval(poll, POLL_MS);
  }

  function close() {
    cleanup();
    if (dlg.open) dlg.close();
  }

  closeBtn.addEventListener('click', close);
  // Esc / any native close path — release the mic no matter how it closed.
  dlg.addEventListener('close', cleanup);
  dlg.addEventListener('cancel', cleanup);

  return { open, close };
}
