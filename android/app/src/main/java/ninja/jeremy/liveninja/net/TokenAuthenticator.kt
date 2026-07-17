package ninja.jeremy.liveninja.net

import javax.inject.Inject
import javax.inject.Singleton
import okhttp3.Authenticator
import okhttp3.Request
import okhttp3.Response
import okhttp3.Route

/**
 * 401 retry-once: when a backend call bounces with 401, rotate the refresh
 * token (single-flight via [TokenRefresher]) and replay the request exactly
 * once with the fresh access JWT. A second 401 for the same call gives up —
 * OkHttp then surfaces the 401 to the caller.
 */
@Singleton
class TokenAuthenticator @Inject constructor(
    private val refresher: TokenRefresher,
) : Authenticator {

    override fun authenticate(route: Route?, response: Response): Request? {
        val request = response.request

        // Never authenticate the bootstrap routes themselves.
        val path = request.url.encodedPath
        if (path.endsWith("/auth/lwa/exchange") || path.endsWith("/auth/refresh")) return null

        // Only retry once: if this request already carries a token minted by
        // a previous authenticate() pass, stop.
        if (responseCount(response) >= 2) return null

        val staleToken = request.header("Authorization")?.removePrefix("Bearer ")
        return when (val outcome = refresher.refreshBlocking(staleToken)) {
            is RefreshOutcome.Refreshed ->
                request.newBuilder()
                    .header("Authorization", "Bearer ${outcome.accessToken}")
                    .build()
            RefreshOutcome.SessionExpired, RefreshOutcome.Transient -> null
        }
    }

    private fun responseCount(response: Response): Int {
        var count = 1
        var prior = response.priorResponse
        while (prior != null) {
            count++
            prior = prior.priorResponse
        }
        return count
    }
}
