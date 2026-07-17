package ninja.jeremy.liveninja.net

import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.BuildConfig
import ninja.jeremy.liveninja.auth.TokenStore
import okhttp3.Interceptor
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
 * backend request.
 *
 * The bearer is skipped on the pre-session bootstrap routes
 * (`/auth/lwa/exchange`, `/auth/refresh`) where it is meaningless, and never
 * overrides an explicitly-set Authorization header (the realtime path talks
 * to OpenAI with its own ephemeral Bearer — but that goes to a different
 * host through a different client anyway).
 */
@Singleton
class AuthInterceptor @Inject constructor(
    private val tokenStore: TokenStore,
) : Interceptor {

    override fun intercept(chain: Interceptor.Chain): Response {
        val original = chain.request()
        val builder = original.newBuilder()
            .header("X-LN-Client", ClientId.HEADER_VALUE)

        val path = original.url.encodedPath
        val isBootstrapRoute = path.endsWith("/auth/lwa/exchange") || path.endsWith("/auth/refresh")
        if (!isBootstrapRoute && original.header("Authorization") == null) {
            tokenStore.accessToken()?.let { builder.header("Authorization", "Bearer $it") }
        }
        return chain.proceed(builder.build())
    }
}
