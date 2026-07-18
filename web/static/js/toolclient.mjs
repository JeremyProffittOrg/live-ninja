// toolclient.mjs — authenticated API access + realtime tool dispatch.
//
// Ownership (plan.md M3 / WS-D "realtime JS"): this module owns
//   1. The web surface's in-memory access-JWT lifecycle: the JWT is never
//      persisted (localStorage/sessionStorage are XSS-readable); it lives in
//      a module-scope variable and is (re)minted from the HttpOnly
//      `__Host-ln_rt` refresh cookie via `POST /api/v1/auth/refresh`
//      (auth_routes.go returns `{accessToken, expiresAt}` — expiresAt is
//      unix *seconds*). Every `/api/v1/*` XHR attaches
//      `Authorization: Bearer <jwt>`; a 401 triggers exactly one forced
//      refresh + one retry (refresh-once-on-401).
//   2. CSRF: state-changing requests echo the non-HttpOnly `__Host-ln_csrf`
//      cookie in `X-LN-CSRF` (middleware.go CSRFProtect double-submit).
//   3. Realtime function-call dispatch: `response.function_call_arguments.done`
//      → `POST /api/v1/tools/invoke {tool,args,idempotencyKey,callId}` →
//      `conversation.item.create` (`function_call_output`) + `response.create`
//      back over the `oai-events` datachannel (docs/web-ui-spec.md §2.4).
//
// Other modules in this workstream (realtime.mjs, transcriptsink.mjs) import
// `authFetch`/`apiJSON` from here rather than from api.mjs so the realtime
// module graph is self-contained; if/when api.mjs (spec §0) wants to share
// this machinery it can re-export from here — the semantics match the spec's
// `apiFetch` contract (CSRF header, typed error on non-2xx).

const REFRESH_PATH = '/api/v1/auth/refresh';
const CSRF_COOKIE = '__Host-ln_csrf';
const CSRF_HEADER = 'X-LN-CSRF';
// Refresh proactively when the token is within 30s of expiry — avoids a
// guaranteed 401 round trip on a token that is about to lapse mid-flight.
const EXPIRY_SKEW_MS = 30_000;

let accessToken = null;
let accessExpiresAtMs = 0;
let refreshInFlight = null;
const authLostHandlers = new Set();

/** Typed error for any non-2xx /api/v1 response, carrying the parsed error
 * envelope from api_routes.go. Two envelope shapes are accepted: the canonical
 * observability shape `{error: {code, message, txId}}` and the legacy flat
 * shape `{error: "<code>", message}`. `txId` is the backend transaction ref —
 * taken from the envelope, or from the `X-LN-Txn` response header when the
 * caller passes it (headerTxId). */
export class ApiError extends Error {
  constructor(status, body, fallbackMessage, headerTxId) {
    // Nested envelope `{error: {code, message, txId}}` vs legacy `{error, message}`.
    const env = body && typeof body.error === 'object' && body.error ? body.error : null;
    super(
      (env && typeof env.message === 'string' && env.message) ||
        (body && typeof body.message === 'string' && body.message) ||
        fallbackMessage ||
        `Request failed (HTTP ${status})`,
    );
    this.name = 'ApiError';
    this.status = status;
    this.code =
      (env && typeof env.code === 'string' && env.code) ||
      (body && typeof body.error === 'string' && body.error) ||
      '';
    this.body = body || {};
    this.txId =
      (env && typeof env.txId === 'string' && env.txId) ||
      (body && typeof body.txId === 'string' && body.txId) ||
      (typeof headerTxId === 'string' && headerTxId) ||
      '';
  }
}

/** Thrown when the refresh cookie itself is invalid/expired — the user has
 * no session left and must sign in again. */
export class AuthLostError extends Error {
  constructor() {
    super('Your session has expired — sign in again.');
    this.name = 'AuthLostError';
  }
}

/** Read the CSRF double-submit cookie (NOT HttpOnly, by design). */
export function readCsrfToken() {
  const prefix = CSRF_COOKIE + '=';
  for (const part of document.cookie.split('; ')) {
    if (part.startsWith(prefix)) return decodeURIComponent(part.slice(prefix.length));
  }
  return '';
}

/**
 * Register a handler for terminal auth loss (refresh rejected). With no
 * handlers registered the default behavior is a redirect to `/` (the landing
 * page — an unauthenticated user never belongs on /conversation or
 * /settings). Returns an unsubscribe function.
 */
export function onAuthLost(fn) {
  authLostHandlers.add(fn);
  return () => authLostHandlers.delete(fn);
}

function emitAuthLost() {
  if (authLostHandlers.size === 0) {
    window.location.assign('/');
    return;
  }
  for (const fn of authLostHandlers) {
    try {
      fn();
    } catch {
      /* a broken handler must not mask auth loss for the others */
    }
  }
}

async function parseJsonSafe(resp) {
  try {
    return await resp.json();
  } catch {
    return null;
  }
}

/** Single-flight refresh: concurrent callers share one in-flight request so
 * a burst of 401s can't stampede the rotate-on-use refresh endpoint (a
 * double rotate would trip the reuse-detection family revoke). */
function refreshAccessToken() {
  if (refreshInFlight) return refreshInFlight;
  refreshInFlight = (async () => {
    const headers = {};
    const csrf = readCsrfToken();
    if (csrf) headers[CSRF_HEADER] = csrf;

    let resp;
    try {
      resp = await fetch(REFRESH_PATH, {
        method: 'POST',
        headers,
        credentials: 'same-origin',
      });
    } catch {
      throw new ApiError(0, null, 'Network error while refreshing your session.');
    }

    if (resp.status === 401 || resp.status === 403) {
      accessToken = null;
      accessExpiresAtMs = 0;
      const err = new AuthLostError();
      emitAuthLost();
      throw err;
    }
    if (!resp.ok) {
      throw new ApiError(resp.status, await parseJsonSafe(resp), 'Could not refresh your session.');
    }

    const body = await resp.json();
    if (!body || typeof body.accessToken !== 'string' || body.accessToken === '') {
      throw new ApiError(resp.status, body, 'Malformed refresh response.');
    }
    accessToken = body.accessToken;
    accessExpiresAtMs = (Number(body.expiresAt) || 0) * 1000;
    return accessToken;
  })().finally(() => {
    refreshInFlight = null;
  });
  return refreshInFlight;
}

/** Return a currently-valid access JWT, refreshing if absent/near expiry.
 * `force: true` bypasses the cache (the 401-retry path). */
export function ensureAccessToken({ force = false } = {}) {
  if (!force && accessToken && Date.now() < accessExpiresAtMs - EXPIRY_SKEW_MS) {
    return Promise.resolve(accessToken);
  }
  return refreshAccessToken();
}

/**
 * Authenticated fetch for `/api/v1/*`:
 *   - attaches `Authorization: Bearer <in-memory JWT>` (minting one first if
 *     needed) and `X-LN-CSRF` on non-GET/HEAD;
 *   - on a 401 response, forces one refresh and retries exactly once;
 *   - `json:` serializes a JSON body + sets Content-Type;
 *   - `keepalive: true` is honored (transcript flush on pagehide).
 * Returns the raw Response (callers that want parsed-envelope-or-throw use
 * apiJSON below).
 */
export async function authFetch(path, options = {}) {
  const {
    method = 'GET',
    json,
    body,
    headers = {},
    signal,
    keepalive = false,
    retryOn401 = true,
  } = options;

  const doFetch = async (token) => {
    const h = { ...headers, Authorization: 'Bearer ' + token };
    if (method !== 'GET' && method !== 'HEAD') {
      const csrf = readCsrfToken();
      if (csrf) h[CSRF_HEADER] = csrf;
    }
    let payload = body;
    if (json !== undefined) {
      h['Content-Type'] = 'application/json';
      payload = JSON.stringify(json);
    }
    return fetch(path, {
      method,
      headers: h,
      body: payload,
      signal,
      keepalive,
      credentials: 'same-origin',
    });
  };

  let token = await ensureAccessToken();
  let resp = await doFetch(token);
  if (resp.status === 401 && retryOn401) {
    token = await ensureAccessToken({ force: true });
    resp = await doFetch(token);
  }
  return resp;
}

/** authFetch + parse: resolves the parsed JSON body on 2xx, throws ApiError
 * (with the server's error envelope) on anything else. */
export async function apiJSON(path, options = {}) {
  const resp = await authFetch(path, options);
  const parsed = await parseJsonSafe(resp);
  if (!resp.ok) throw new ApiError(resp.status, parsed, undefined, resp.headers.get('X-LN-Txn') || '');
  return parsed;
}

function randomId() {
  if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
    return globalThis.crypto.randomUUID();
  }
  // Non-cryptographic fallback is fine here — idempotency keys only need
  // per-session uniqueness, not unguessability.
  return 'id-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}

/**
 * Realtime tool dispatcher (spec §2.4).
 *
 * `sendEvent(obj)` must transmit a JSON event on the `oai-events`
 * datachannel (RealtimeSession#sendEvent). Optional observability hooks:
 * `onToolCall({tool, callId, args})`, `onToolResult({tool, callId, result})`,
 * `onToolError({tool, callId, error})` — the conversation page uses these to
 * render live tool-result cards (spec §2.3, never pre-seeded).
 *
 * Failure posture: a failed backend invoke still sends a
 * `function_call_output` carrying `{error, message}` followed by
 * `response.create`, so the model can recover conversationally instead of
 * the session hanging on a missing tool output.
 */
export function createToolDispatcher({
  sendEvent,
  invokePath = '/api/v1/tools/invoke',
  onToolCall,
  onToolResult,
  onToolError,
} = {}) {
  if (typeof sendEvent !== 'function') {
    throw new TypeError('createToolDispatcher requires a sendEvent(obj) function');
  }

  const inFlight = new Set();

  async function dispatch({ name, callId, argsJson }) {
    if (!callId || inFlight.has(callId)) return; // duplicate .done events
    inFlight.add(callId);

    let output;
    let args = null;
    try {
      args = argsJson ? JSON.parse(argsJson) : {};
    } catch {
      output = {
        error: 'invalid_arguments',
        message: 'The function-call arguments were not valid JSON.',
      };
    }

    if (args !== null) {
      try {
        if (onToolCall) onToolCall({ tool: name, callId, args });
        const result = await apiJSON(invokePath, {
          method: 'POST',
          json: { tool: name, args, idempotencyKey: randomId(), callId },
        });
        output = result;
        if (onToolResult) onToolResult({ tool: name, callId, result });
      } catch (err) {
        output =
          err instanceof ApiError
            ? { error: err.code || 'tool_failed', message: err.message, txId: err.txId || undefined }
            : { error: 'tool_failed', message: 'The tool call failed.' };
        if (onToolError) onToolError({ tool: name, callId, error: err });
      }
    }

    inFlight.delete(callId);
    try {
      sendEvent({
        type: 'conversation.item.create',
        item: {
          type: 'function_call_output',
          call_id: callId,
          output: JSON.stringify(output),
        },
      });
      sendEvent({ type: 'response.create' });
    } catch {
      // Datachannel already closed (session ended mid-tool-call) — nothing
      // left to deliver the output to; the connectionlost path owns recovery.
    }
  }

  /** Feed every parsed datachannel event here; returns true when consumed. */
  function handleEvent(evt) {
    if (evt && evt.type === 'response.function_call_arguments.done') {
      dispatch({ name: evt.name, callId: evt.call_id, argsJson: evt.arguments });
      return true;
    }
    return false;
  }

  return { dispatch, handleEvent };
}
