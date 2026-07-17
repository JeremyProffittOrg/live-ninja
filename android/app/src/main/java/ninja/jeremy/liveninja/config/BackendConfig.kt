package ninja.jeremy.liveninja.config

/**
 * Backend endpoints and public identifiers, per contracts/api.md.
 * These are all public values — no secrets belong in this file.
 */
object BackendConfig {
    /** Fiber backend origin. */
    const val BASE_URL: String = "https://live.jeremy.ninja"

    /** Login-with-Amazon public client id (Custom Tabs + PKCE flow). */
    const val LWA_CLIENT_ID: String =
        "amzn1.application-oa2-client.ba90ca5c0e9d4b559e091ccc79152f16"

    /** App-Link return URL registered with LWA (assetlinks served by backend, M8). */
    const val LWA_APP_LINK_REDIRECT: String = "$BASE_URL/auth/lwa/app-callback"

    /** Custom-scheme fallback redirect for dev before App Links verification. */
    const val LWA_CUSTOM_SCHEME_REDIRECT: String = "ninja.jeremy.liveninja://lwa"

    /** LWA authorization endpoint (Authorization Code + PKCE, Custom Tabs). */
    const val LWA_AUTHORIZE_URL: String = "https://www.amazon.com/ap/oa"

    /** The only LWA scope Live Ninja requests. */
    const val LWA_SCOPE: String = "profile"

    /**
     * Android code exchange: POST {code, codeVerifier, redirectURI} ->
     * {accessToken, expiresAt, refreshToken, refreshExpiresAt, sessionId}
     * (canonical path per contracts/api.md; backend also aliases /api/v1/auth/lwa/exchange).
     */
    const val AUTH_EXCHANGE_URL: String = "$BASE_URL/auth/lwa/exchange"

    /** Rotate refresh token -> new 15-min access JWT. POST {refreshToken}. */
    const val AUTH_REFRESH_URL: String = "$BASE_URL/auth/refresh"

    /** Revoke the current session (Bearer JWT identifies it). Idempotent. */
    const val AUTH_LOGOUT_URL: String = "$BASE_URL/auth/logout"

    /** Log out everywhere: bump tokensValidAfter + revoke all sessions. Bearer required. */
    const val AUTH_LOGOUT_ALL_URL: String = "$BASE_URL/api/v1/auth/logout-all"

    /** Realtime bootstrap: returns a short-lived OpenAI ephemeral token. */
    const val REALTIME_SESSION_URL: String = "$BASE_URL/api/v1/realtime/session"

    /** Server-side tool router: POST {tool, args, callId, idempotencyKey}. */
    const val TOOLS_INVOKE_URL: String = "$BASE_URL/api/v1/tools/invoke"

    /** OpenAI Realtime WebRTC call endpoint (SDP offer POST, Bearer ephemeral token). */
    const val OPENAI_REALTIME_CALLS_URL: String = "https://api.openai.com/v1/realtime/calls"
}
