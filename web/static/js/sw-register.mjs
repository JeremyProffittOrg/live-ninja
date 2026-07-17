/**
 * Service-worker registration for the Live Ninja PWA.
 *
 * Import once from the base layout:
 *   <script type="module" src="/static/js/sw-register.mjs"></script>
 * (importing the module registers automatically; `registerServiceWorker` is
 * also exported for callers that want the registration handle).
 *
 * Update model: /sw.js calls skipWaiting()+clients.claim(), so a new worker
 * takes control as soon as it installs. We deliberately do NOT force a page
 * reload on controllerchange — a live WebRTC conversation must never be
 * interrupted by a deploy. HTML is network-first in the worker, so the next
 * ordinary navigation always gets fresh markup anyway.
 */

const SW_URL = '/sw.js';
const UPDATE_CHECK_MS = 60 * 60 * 1000; // hourly while the tab stays open

let registrationPromise = null;

export function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return Promise.resolve(null);
  if (registrationPromise) return registrationPromise;

  registrationPromise = navigator.serviceWorker
    .register(SW_URL, { scope: '/' })
    .then((reg) => {
      // Periodic + on-foreground update checks so long-lived tabs pick up
      // deploys without waiting for a navigation.
      setInterval(() => reg.update().catch(() => {}), UPDATE_CHECK_MS);
      document.addEventListener('visibilitychange', () => {
        if (document.visibilityState === 'visible') {
          reg.update().catch(() => {});
        }
      });
      return reg;
    })
    .catch((err) => {
      // Non-fatal: the app is fully functional without offline support.
      console.warn('[sw-register] service worker registration failed:', err);
      return null;
    });

  return registrationPromise;
}

// Auto-register on import (after load so SW installation never competes with
// first-paint resource fetches).
if ('serviceWorker' in navigator) {
  if (document.readyState === 'complete') {
    registerServiceWorker();
  } else {
    window.addEventListener('load', () => registerServiceWorker(), { once: true });
  }
}
