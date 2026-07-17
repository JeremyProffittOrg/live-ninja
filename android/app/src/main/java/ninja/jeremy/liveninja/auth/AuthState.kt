package ninja.jeremy.liveninja.auth

/** Why a sign-in attempt (or an existing session) ended without a session. */
enum class AuthError {
    /** LWA reported the user cancelled/denied the consent screen. */
    LWA_DENIED,

    /** OAuth `state` mismatch or no pending login — possible CSRF, start over. */
    STATE_MISMATCH,

    /** Backend refused the account (owner + allowlist access policy). */
    NOT_ALLOWED,

    /** Code exchange failed at the backend/LWA (expired or reused code). */
    EXCHANGE_FAILED,

    /** Couldn't reach the backend. */
    NETWORK,

    /** The stored session was revoked or its refresh window lapsed. */
    SESSION_EXPIRED,
}

/** App-wide authentication state, published by [AuthRepository.state]. */
sealed interface AuthState {
    /** No session. [error] carries the reason the last attempt/session ended, if any. */
    data class SignedOut(val error: AuthError? = null) : AuthState

    /** Redirect received; exchanging the code for tokens. */
    data object Authorizing : AuthState

    /** Valid session on device. */
    data class SignedIn(val sessionId: String) : AuthState
}
