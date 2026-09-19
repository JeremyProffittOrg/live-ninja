// Shared Android APK card for the public landing page and /downloads.
// Fetches GET /v1/app/android/latest (public, no auth) and fills a download
// link. On a phone the browser downloads the APK; the user taps it to install.

const LATEST_PATH = '/v1/app/android/latest';

export function mountAndroidAppCard(rootId) {
  const root = document.getElementById(rootId);
  if (!root) return;
  void load(root);
}

async function load(root) {
  const status = root.querySelector('[data-android-status]');
  const actions = root.querySelector('[data-android-actions]');
  const link = root.querySelector('[data-android-download]');
  try {
    const resp = await fetch(LATEST_PATH, { cache: 'no-store' });
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const meta = await resp.json();
    if (!meta || typeof meta.url !== 'string' || !meta.url.endsWith('.apk')) {
      throw new Error('bad metadata');
    }
    if (status) {
      status.textContent = `Version ${meta.versionName} (${meta.versionCode}) — ${formatBytes(meta.sizeBytes)}.`;
    }
    if (link) {
      link.href = meta.url;
      link.removeAttribute('hidden');
    }
    if (actions) actions.hidden = false;
  } catch {
    if (status) {
      status.textContent = 'The Android app is not published right now. Try again later.';
    }
    if (link) link.setAttribute('hidden', '');
  }
}

function formatBytes(n) {
  const bytes = Number(n);
  if (!Number.isFinite(bytes) || bytes <= 0) return '';
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
