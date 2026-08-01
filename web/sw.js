/* Live Ninja service worker.
 *
 * Served at /sw.js (root path => scope "/" without a Service-Worker-Allowed
 * header). Registered by /static/js/sw-register.mjs.
 *
 * Strategy (per docs/web-ui-spec.md §0 and the house cache-hygiene rules):
 *   - HTML documents / navigations : network-first, cached copy only as an
 *     offline fallback (stale-HTML deploys must never require a hard refresh).
 *   - /static/* assets             : stale-while-revalidate (they are
 *     fingerprinted/immutable at build, so serving cached is always safe and
 *     the background revalidate keeps unfingerprinted entries fresh).
 *
 *     That "always safe" holds because URLs here are content-addressed, and
 *     for JS modules it holds ONLY because of the import map that
 *     internal/webapp/assets.go stamps into every page. Templates always
 *     loaded the ENTRY module by its fingerprinted URL, but until 2026-08-01
 *     the modules imported their siblings by LOGICAL path — so this cache
 *     could pair a just-deployed module with a pre-deploy sibling, module
 *     linking failed, and the whole page went silently inert (seen in
 *     production; plan.md §4.3). Removing that import map re-arms this
 *     branch as a page-killer. Do not weaken it here instead: SWR over
 *     genuinely content-addressed URLs is correct.
 *   - /api/*, /auth/*, /healthz, /.well-known/*, any cross-origin request
 *     (api.openai.com WebRTC/SDP calls), and every non-GET: NEVER intercepted —
 *     we return before respondWith so live data, auth cookies, SSE/WebRTC
 *     signaling and CSRF semantics are untouched by the cache layer.
 *
 * Versioning: bump SW_VERSION on any change to the caching strategy; activate
 * deletes every other ln-* cache so stale shells are dropped fleet-wide.
 */

'use strict';

const SW_VERSION = 'v4';
const CACHE_HTML = `ln-html-${SW_VERSION}`;
const CACHE_STATIC = `ln-static-${SW_VERSION}`;
const CURRENT_CACHES = [CACHE_HTML, CACHE_STATIC];

self.addEventListener('install', (event) => {
  // Activate the new worker immediately; there is no precache manifest —
  // HTML is network-first anyway and static assets fill in on first use.
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      try {
        const names = await caches.keys();
        await Promise.all(
          names
            .filter((n) => n.startsWith('ln-') && !CURRENT_CACHES.includes(n))
            .map((n) => caches.delete(n)),
        );
      } catch {
        // Cache Storage can be unavailable (privacy mode/quota corruption);
        // activation and network access must still proceed.
      }
      await self.clients.claim();
    })(),
  );
});

/**
 * True when the request must bypass the service worker entirely (no
 * respondWith call — the browser handles it natively).
 */
function bypassed(request, url) {
  if (request.method !== 'GET') return true;
  // Cross-origin: covers https://api.openai.com/v1/realtime/calls and any
  // other third-party fetch. Never cached, never intercepted.
  if (url.origin !== self.location.origin) return true;
  const p = url.pathname;
  if (p.startsWith('/api/') || p.startsWith('/v1/')) return true;
  if (p.startsWith('/auth/')) return true;
  if (p.startsWith('/.well-known/')) return true;
  if (p === '/healthz') return true;
  if (p === '/sw.js') return true; // the browser's SW update pipeline owns this
  return false;
}

function isNavigation(request) {
  return (
    request.mode === 'navigate' ||
    request.destination === 'document' ||
    (request.headers.get('accept') || '').includes('text/html')
  );
}

/** Minimal self-contained offline page (design tokens inlined). */
function offlineResponse() {
  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>Offline — Live Ninja</title>
<style>
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
    font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
    background:#060d18;color:#f4f8fb;text-align:center}
  main{padding:2rem;max-width:26rem}
  h1{font-size:1.4rem;margin:0 0 .5rem}
  p{color:#9db2cc;margin:0 0 1.25rem;line-height:1.5}
  button{min-height:44px;padding:.6rem 1.4rem;border-radius:999px;border:1px solid rgba(122,160,205,.3);
    background:#22e0d0;color:#052220;font-size:1rem;font-weight:600;cursor:pointer}
  button:focus-visible{outline:none;box-shadow:0 0 0 3px rgba(56,208,255,.55)}
</style></head><body><main>
<h1>You&rsquo;re offline</h1>
<p>Live Ninja needs a connection for voice conversations. Reconnect and try again.</p>
<button onclick="location.reload()">Retry</button>
</main></body></html>`;
  return new Response(html, {
    status: 503,
    headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' },
  });
}

/** Network-first for HTML/navigations; cache is only an offline fallback. */
async function networkFirstHTML(request) {
  try {
    const fresh = await fetch(request);
    // Only cache clean, non-redirected 200s: caching a redirected response
    // and replaying it for a navigation throws in several browsers, and the
    // landing page 302s to /conversation for authed users.
    if (fresh && fresh.status === 200 && !fresh.redirected) {
      try {
        const cache = await caches.open(CACHE_HTML);
        await cache.put(request, fresh.clone());
      } catch {
        // Cache quota/storage failures must not turn a good network response
        // into the offline page; this request simply lacks an offline copy.
      }
    }
    return fresh;
  } catch {
    try {
      const cache = await caches.open(CACHE_HTML);
      const cached =
        (await cache.match(request, { ignoreSearch: true })) || (await cache.match('/'));
      if (cached) return cached;
    } catch {
      // No usable Cache Storage fallback.
    }
    return offlineResponse();
  }
}

/** True for the content-addressed paths emitted by assets.go. */
function isFingerprintedStatic(url) {
  return /\.[0-9a-f]{12}\.[^/]+$/i.test(url.pathname);
}

function needsNetworkModuleReload(url) {
  return url.pathname.startsWith('/static/js/') && !isFingerprintedStatic(url);
}

/** Fetch and durably refresh one same-origin static asset. */
async function revalidateStatic(request) {
  try {
    const url = new URL(request.url);
    // Dependency modules use logical paths because import statements cannot
    // call the server-side asset() helper. Force those revalidations past the
    // browser HTTP/module cache; otherwise a successful SWR can put the same
    // pre-deploy module back into Cache Storage indefinitely. Fingerprinted
    // paths are content-addressed and can safely use the browser cache.
    const networkRequest = needsNetworkModuleReload(url)
      ? new Request(request, { cache: 'reload' })
      : request;
    const resp = await fetch(networkRequest);
    if (resp && resp.status === 200 && resp.type === 'basic') {
      try {
        const cache = await caches.open(CACHE_STATIC);
        await cache.put(request, resp.clone());
      } catch {
        // Serve the fresh asset even when Cache Storage is unavailable.
      }
    }
    return resp;
  } catch {
    return undefined;
  }
}

/** Stale-while-revalidate for same-origin static assets. */
async function staleWhileRevalidate(request, revalidate) {
  try {
    const cache = await caches.open(CACHE_STATIC);
    const cached = await cache.match(request);
    if (cached) {
      return cached;
    }
  } catch {
    // Continue network-only when Cache Storage cannot be opened/read.
  }
  const fresh = await revalidate;
  return fresh || Response.error();
}

self.addEventListener('fetch', (event) => {
  const request = event.request;
  let url;
  try {
    url = new URL(request.url);
  } catch {
    return;
  }
  if (bypassed(request, url)) return; // browser handles it natively

  if (isNavigation(request)) {
    event.respondWith(networkFirstHTML(request));
    return;
  }
  if (url.pathname.startsWith('/static/')) {
    // waitUntil must be registered while the fetch event is still active.
    // revalidateStatic resolves only after cache.put, so the worker cannot be
    // terminated between the network response and the durable cache write.
    const revalidate = revalidateStatic(request);
    event.waitUntil(revalidate);
    event.respondWith(staleWhileRevalidate(request, revalidate));
    return;
  }
  // Anything else same-origin (none expected today): leave to the network.
});
