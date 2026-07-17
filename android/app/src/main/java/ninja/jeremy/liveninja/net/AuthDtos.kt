package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Wire DTOs for the backend auth surface (internal/webapp/auth_routes.go).
 * Field names match the backend JSON exactly; epochs are unix seconds.
 */

/** POST /auth/lwa/exchange request body. */
@Serializable
data class LwaExchangeRequest(
    val code: String,
    val codeVerifier: String,
    val redirectURI: String,
)

/** POST /auth/refresh request body (Android sends the wire refresh token). */
@Serializable
data class RefreshRequest(
    val refreshToken: String,
)

/**
 * Token grant returned by both exchange and refresh:
 * a 15-minute access JWT plus the rotated 30-day refresh token.
 */
@Serializable
data class TokenGrant(
    val accessToken: String,
    val expiresAt: Long,
    val refreshToken: String? = null,
    val refreshExpiresAt: Long? = null,
    val sessionId: String? = null,
)

/** Error envelope the backend uses for auth failures: {"error": "..."} . */
@Serializable
data class ApiError(
    val error: String? = null,
    val message: String? = null,
)

/** POST /auth/logout and /api/v1/auth/logout-all acknowledge with {"ok": true}. */
@Serializable
data class LogoutAck(
    val ok: Boolean = true,
)
