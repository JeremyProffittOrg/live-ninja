package ninja.jeremy.liveninja.net

import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.auth.TokenStore
import ninja.jeremy.liveninja.config.BackendConfig
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody

/** Outcome of a blocking refresh attempt. */
sealed interface RefreshOutcome {
    /** New access token in hand (already persisted to the [TokenStore]). */
    data class Refreshed(val accessToken: String) : RefreshOutcome

    /** The backend rejected the refresh token — the session is dead and cleared. */
    data object SessionExpired : RefreshOutcome

    /** Transient failure (network, 5xx). Session untouched; try again later. */
    data object Transient : RefreshOutcome
}

/**
 * Single-flight refresh-token rotation against `POST /auth/refresh`.
 *
 * Deliberately built on a bare [OkHttpClient] (no [TokenAuthenticator], no
 * [AuthInterceptor]) so the authenticator can call it without recursion, and
 * runs synchronously because OkHttp authenticators are blocking by contract.
 * All entry points funnel through one lock, so concurrent 401s from parallel
 * requests produce exactly one rotation — the backend treats reuse of an
 * already-rotated refresh token as theft and revokes the whole family.
 */
@Singleton
class TokenRefresher @Inject constructor(
    private val tokenStore: TokenStore,
    private val baseClient: OkHttpClient,
    private val json: Json,
) {

    private val lock = Any()

    private val _sessionExpired = MutableSharedFlow<Unit>(extraBufferCapacity = 1)

    /** Emits when a refresh is rejected outright (listeners flip UI to signed-out). */
    val sessionExpired: SharedFlow<Unit> = _sessionExpired

    /**
     * Rotate the refresh token and mint a new access JWT.
     *
     * [staleAccessToken] is the token the caller found insufficient (expired
     * locally, or bounced with a 401). If another thread already refreshed
     * while we waited on the lock, the fresh stored token is returned without
     * a second network call.
     */
    fun refreshBlocking(staleAccessToken: String?): RefreshOutcome = synchronized(lock) {
        val session = tokenStore.session()
            ?: return RefreshOutcome.SessionExpired // signed out already
        val now = System.currentTimeMillis() / 1000

        // Someone else rotated while we waited for the lock.
        if (session.accessToken != staleAccessToken && session.accessExpiresAt > now + 30) {
            return RefreshOutcome.Refreshed(session.accessToken)
        }

        val body = json.encodeToString(
            RefreshRequest.serializer(),
            RefreshRequest(refreshToken = session.refreshToken),
        )
        val request = Request.Builder()
            .url(BackendConfig.AUTH_REFRESH_URL)
            .header("X-LN-Client", ClientId.HEADER_VALUE)
            .post(body.toRequestBody("application/json; charset=utf-8".toMediaType()))
            .build()

        val response = try {
            baseClient.newCall(request).execute()
        } catch (e: java.io.IOException) {
            return RefreshOutcome.Transient
        }

        response.use { resp ->
            val payload = resp.body?.string().orEmpty()
            return when {
                resp.isSuccessful -> {
                    val grant = try {
                        json.decodeFromString(TokenGrant.serializer(), payload)
                    } catch (e: kotlinx.serialization.SerializationException) {
                        return RefreshOutcome.Transient
                    }
                    tokenStore.updateFromRefresh(
                        accessToken = grant.accessToken,
                        accessExpiresAt = grant.expiresAt,
                        refreshToken = grant.refreshToken,
                        refreshExpiresAt = grant.refreshExpiresAt,
                        sessionId = grant.sessionId,
                    )
                    RefreshOutcome.Refreshed(grant.accessToken)
                }
                resp.code in 400..499 -> {
                    // invalid_refresh_token / refresh_reused / session_revoked /
                    // account_unavailable: the session is unrecoverable.
                    tokenStore.clearSession()
                    _sessionExpired.tryEmit(Unit)
                    RefreshOutcome.SessionExpired
                }
                else -> RefreshOutcome.Transient
            }
        }
    }
}
