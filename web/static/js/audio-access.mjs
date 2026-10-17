// Microphone capture and private recording playback must not overlap across tabs.
// Shared capture permits the existing wake-word and conversation microphones.
const LOCK = 'live-ninja-private-recording-audio';

export function acquireAudioAccess(mode, locks = globalThis.navigator?.locks) {
  if (!locks) {
    if (mode === 'shared') return Promise.resolve(() => {});
    return Promise.reject(new Error('Private playback needs a browser with audio coordination. Download the recording instead.'));
  }
  return new Promise((resolve, reject) => {
    locks.request(LOCK, { mode, ifAvailable: true }, async lock => {
      if (!lock) {
        const error = new Error('Close loaded recordings, or end conversations and turn off Always listening in other tabs, then retry.');
        error.code = 'recording_audio_busy';
        throw error;
      }
      await new Promise(release => resolve(release));
    }).catch(reject);
  });
}

export async function acquireGuardedMicrophone(constraints, nav = navigator) {
  const release = await acquireAudioAccess('shared', nav.locks);
  try {
    const stream = await nav.mediaDevices.getUserMedia(constraints);
    const tracks = stream.getTracks();
    const active = new Set(tracks);
    const ended = track => { active.delete(track); if (!active.size) release(); };
    for (const track of tracks) {
      const stop = track.stop.bind(track);
      track.stop = () => { try { stop(); } finally { ended(track); } };
      track.addEventListener('ended', () => ended(track), { once: true });
    }
    if (!tracks.length) release();
    return stream;
  } catch (error) {
    release();
    throw error;
  }
}
