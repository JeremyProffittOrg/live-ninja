package ninja.jeremy.liveninja.net

import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.BuildConfig
import ninja.jeremy.liveninja.auth.DeviceIdentityStore
import ninja.jeremy.liveninja.auth.TokenStore
import okhttp3.Interceptor
import okhttp3.Request
import okhttp3.Response

/**
 * `X-LN-Client` capability-negotiation header value (contracts/headers.md):
 * `android/<semver>+<build>`.
 */
object ClientId {
    val HEADER_VALUE: String = "android/${BuildConfig.VERSION_NAME}+r${BuildConfig.VERSION_CODE}"
}

/**
 * Attaches `X-LN-Client` and `Authorization: Bearer <access JWT>` to every
 * backend request, and owns the two halves of keeping that bearer valid.
 *
 * The bearer is skipped on the pre-session bootstrap routes
 * (`/auth/lwa/exchange`, `/auth/refresh`) where it is meaningless, and never
 * overrides an explicitly-set Authorization header (the realtime path talks
 * to OpenAI with its own ephemeral Bearer — but that goes to a different
 * host through a different client anyway).
 *
 * ## Why refresh lives here and not only in [TokenAuthenticator]
 *
 * The access JWT lives 15 minutes (internal/auth/session.go `AccessTokenTTL`),
 * but this app's edge **never answers an expired one with 401**: the API
 * Gateway Lambda authorizer (cmd/authorizer/main.go) returns
 * `IsAuthorized: false`, and HTTP API synthesizes its own
 * `403 {"message":"Forbidden"}` — the request never reaches Fiber, so nothing
 * can choose a 401. `okhttp3.Authenticator` is only ever invoked on 401/407,
 * so [TokenAuthenticator] could not see that denial and never refreshed:
 * once the token lapsed, every authed call 403'd until the app happened to be
 * foregrounded again (`AuthRepository.onStart`). Symptom on device: "Couldn't
 * start the conversation — Forbidden" after ~15 minutes of continuous use,
 * cured by backgrounding the app, which made it look intermittent.
 *
 * So this interceptor does both halves:
 *  1. **Proactive** — refresh before attaching when the stored token is
 *     within [TOKEN_REFRESH_MARGIN_SECONDS] of lapsing. Removes the 403 at
 *     the source, which is the common case.
 *  2. **Reactive** — if the edge still denies (clock skew, a session revoked
 *     server-side, a token that lapsed mid-flight), refresh once and replay
 *     the request exactly once.
 *
 * [TokenAuthenticator] stays wired for genuine 401s from Fiber itself.
 * [TokenRefresher] is single-flight and built on the bare (unauthorized)
 * OkHttp client, so calling it from inside this interceptor cannot recurse.
 */
@Singleton
class AuthInterceptor @Inject constructor(
    private val tokenStore: TokenStore,
    private val refresher: TokenRefresher,
    private val deviceIdentity: DeviceIdentityStore,
) : Interceptor {

    override fun intercept(chain: Interceptor.Chain): Response {
        val original = chain.request()
        val path = original.url.encodedPath
        val isBootstrapRoute = path.endsWith("/auth/lwa/exchange") || path.endsWith("/auth/refresh")

        // An explicitly-set Authorization header belongs to the caller (e.g. a
        // third-party ephemeral bearer): attach nothing, refresh nothing, and
        // do not interpret its 403s as ours.
        val ownsAuth = !isBootstrapRoute && original.header("Authorization") == null

        var token = if (ownsAuth) tokenStore.accessToken() else null
        if (ownsAuth && isAccessStale()) {
            // Proactive rotation. A Transient/SessionExpired outcome is not
            // fatal here — fall through with whatever token we have and let
            // the server be the judge, exactly as before this change.
            (refresher.refreshBlocking(token) as? RefreshOutcome.Refreshed)?.let {
                token = it.accessToken
            }
        }

        val response = chain.proceed(authorized(original, token))
        if (!ownsAuth || !isEdgeAuthDenial(response)) return response

        // Reactive rotation, once. Only an actually-different token justifies a
        // replay; otherwise we would burn a rotation on a genuine authorization
        // failure and still get the same 403.
        val refreshed = refresher.refreshBlocking(token) as? RefreshOutcome.Refreshed
            ?: return response
        if (refreshed.accessToken == token) return response

        response.close()
        return chain.proceed(authorized(original, refreshed.accessToken))
    }

    private fun authorized(request: Request, token: String?): Request {
        val builder = request.newBuilder().header("X-LN-Client", ClientId.HEADER_VALUE)
        if (token != null && request.header("Authorization") == null) {
            builder.header("Authorization", "Bearer $token")
            // Random app-instance id, never a hardware identifier. The backend
            // validates ownership before using it as a settings/device target.
            builder.header("X-LN-Device-ID", deviceIdentity.deviceId)
        }
        return builder.build()
    }

    private fun isAccessStale(): Boolean {
        val session = tokenStore.session() ?: return false
        val now = System.currentTimeMillis() / 1000
        return session.accessExpiresAt - now < TOKEN_REFRESH_MARGIN_SECONDS
    }

    companion object {
        /**
         * Refresh this far ahead of expiry. Smaller than
         * `AuthRepository.ACCESS_REFRESH_MARGIN_SECONDS` (5 min) on purpose:
         * that one decides whether a foregrounding event is worth a rotation,
         * this one is a per-request last line of defence and should not rotate
         * more eagerly than it must.
         */
        const val TOKEN_REFRESH_MARGIN_SECONDS = 60L

        /** Cap on the body we peek to classify a 403 (the real one is 23 bytes). */
        private const val DENIAL_PEEK_BYTES = 256L

        /**
         * True for API Gateway's *own* authorizer denial, as distinct from an
         * application-level 403 from Fiber.
         *
         * The edge emits exactly `{"message":"Forbidden"}` with no `error`
         * field; every Fiber 403 carries this system's error taxonomy
         * (`{"error":"...","message":"..."}` — contracts/api.md). Discriminating
         * on that keeps a real "you are not allowed to do this" 403 from
         * triggering a pointless token rotation on every retry.
         *
         * [Response.peekBody] leaves the body readable by the caller.
         */
        internal fun isEdgeAuthDenial(response: Response): Boolean {
            if (response.code != 403) return false
            val body = try {
                response.peekBody(DENIAL_PEEK_BYTES).string()
            } catch (_: Exception) {
                return false
            }
            return !body.contains("\"error\"") && body.contains("\"Forbidden\"")
        }
    }
}
