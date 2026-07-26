package ninja.jeremy.liveninja.net

import io.mockk.every
import io.mockk.mockk
import io.mockk.slot
import io.mockk.verify
import ninja.jeremy.liveninja.auth.StoredSession
import ninja.jeremy.liveninja.auth.DeviceIdentityStore
import ninja.jeremy.liveninja.auth.TokenStore
import okhttp3.Interceptor
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Protocol
import okhttp3.Request
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Regression suite for the bug that made the Android app unusable after ~15
 * minutes of continuous foreground use ("Couldn't start the conversation —
 * Forbidden").
 *
 * The access JWT lives 15 minutes, and the API Gateway authorizer answers an
 * expired one with its own `403 {"message":"Forbidden"}` rather than a 401 —
 * which `okhttp3.Authenticator` (and therefore [TokenAuthenticator]) can never
 * see. [AuthInterceptor] has to close that gap proactively and reactively
 * without rotating tokens on genuine authorization failures.
 */
class AuthInterceptorTest {

    private val tokenStore = mockk<TokenStore>(relaxed = true)
    private val refresher = mockk<TokenRefresher>()
    private val deviceIdentity = mockk<DeviceIdentityStore> {
        every { deviceId } returns "18c93d91-579a-4dc9-9f13-62e847d981dc"
    }
    private val interceptor = AuthInterceptor(tokenStore, refresher, deviceIdentity)

    private fun nowSeconds() = System.currentTimeMillis() / 1000

    private fun session(secondsToExpiry: Long, token: String = "stale") = StoredSession(
        accessToken = token,
        accessExpiresAt = nowSeconds() + secondsToExpiry,
        refreshToken = "refresh",
        refreshExpiresAt = nowSeconds() + 30L * 24 * 60 * 60,
        sessionId = "sid-test",
    )

    private fun response(request: Request, code: Int, body: String): Response =
        Response.Builder()
            .request(request)
            .protocol(Protocol.HTTP_1_1)
            .code(code)
            .message(if (code == 403) "Forbidden" else "OK")
            .body(body.toResponseBody("application/json".toMediaType()))
            .build()

    /** A chain that returns the given responses in order and records each request. */
    private fun chain(
        request: Request,
        vararg responses: Pair<Int, String>,
    ): Pair<Interceptor.Chain, MutableList<Request>> {
        val chain = mockk<Interceptor.Chain>()
        val seen = mutableListOf<Request>()
        val captured = slot<Request>()
        every { chain.request() } returns request
        var call = 0
        every { chain.proceed(capture(captured)) } answers {
            seen += captured.captured
            val (code, body) = responses[minOf(call++, responses.size - 1)]
            response(captured.captured, code, body)
        }
        return chain to seen
    }

    private fun apiRequest(path: String = "/api/v1/realtime/session"): Request =
        Request.Builder().url("https://live.jeremy.ninja$path").build()

    private val edgeDenial = 403 to """{"message":"Forbidden"}"""
    private val appDenial = 403 to """{"error":"forbidden","message":"Not allowed."}"""
    private val ok = 200 to """{"ok":true}"""

    @Test
    fun freshToken_attachesBearerAndNeverRefreshes() {
        every { tokenStore.accessToken() } returns "fresh"
        every { tokenStore.session() } returns session(secondsToExpiry = 600, token = "fresh")

        val (chain, seen) = chain(apiRequest(), ok)
        interceptor.intercept(chain)

        assertEquals(1, seen.size)
        assertEquals("Bearer fresh", seen[0].header("Authorization"))
        assertEquals(ClientId.HEADER_VALUE, seen[0].header("X-LN-Client"))
        assertEquals(deviceIdentity.deviceId, seen[0].header("X-LN-Device-ID"))
        verify(exactly = 0) { refresher.refreshBlocking(any()) }
    }

    @Test
    fun tokenAboutToLapse_refreshedBeforeTheRequestIsEverSent() {
        every { tokenStore.accessToken() } returns "stale"
        every { tokenStore.session() } returns session(secondsToExpiry = 5)
        every { refresher.refreshBlocking("stale") } returns RefreshOutcome.Refreshed("fresh")

        val (chain, seen) = chain(apiRequest(), ok)
        interceptor.intercept(chain)

        // One round trip, and it already carried the fresh bearer — the 403 the
        // old code produced never happens.
        assertEquals(1, seen.size)
        assertEquals("Bearer fresh", seen[0].header("Authorization"))
    }

    @Test
    fun edgeForbidden_refreshesAndReplaysExactlyOnce() {
        every { tokenStore.accessToken() } returns "stale"
        every { tokenStore.session() } returns session(secondsToExpiry = 600)
        every { refresher.refreshBlocking("stale") } returns RefreshOutcome.Refreshed("fresh")

        val (chain, seen) = chain(apiRequest(), edgeDenial, ok)
        val result = interceptor.intercept(chain)

        assertEquals(2, seen.size)
        assertEquals("Bearer stale", seen[0].header("Authorization"))
        assertEquals("Bearer fresh", seen[1].header("Authorization"))
        assertEquals(200, result.code)
        verify(exactly = 1) { refresher.refreshBlocking("stale") }
    }

    @Test
    fun applicationLevel403_isNotAnAuthChallenge_soNoTokenIsBurned() {
        every { tokenStore.accessToken() } returns "fresh"
        every { tokenStore.session() } returns session(secondsToExpiry = 600, token = "fresh")

        val (chain, seen) = chain(apiRequest(), appDenial)
        val result = interceptor.intercept(chain)

        assertEquals(1, seen.size)
        assertEquals(403, result.code)
        verify(exactly = 0) { refresher.refreshBlocking(any()) }
    }

    @Test
    fun edgeForbidden_butRefreshYieldsSameToken_doesNotReplay() {
        every { tokenStore.accessToken() } returns "stale"
        every { tokenStore.session() } returns session(secondsToExpiry = 600)
        every { refresher.refreshBlocking("stale") } returns RefreshOutcome.Refreshed("stale")

        val (chain, seen) = chain(apiRequest(), edgeDenial)
        val result = interceptor.intercept(chain)

        assertEquals(1, seen.size)
        assertEquals(403, result.code)
    }

    @Test
    fun edgeForbidden_sessionExpired_surfacesThe403WithoutLooping() {
        every { tokenStore.accessToken() } returns "stale"
        every { tokenStore.session() } returns session(secondsToExpiry = 600)
        every { refresher.refreshBlocking("stale") } returns RefreshOutcome.SessionExpired

        val (chain, seen) = chain(apiRequest(), edgeDenial)
        val result = interceptor.intercept(chain)

        assertEquals(1, seen.size)
        assertEquals(403, result.code)
    }

    @Test
    fun bootstrapRoutes_getNoBearerAndNeverRefresh() {
        every { tokenStore.accessToken() } returns "stale"
        every { tokenStore.session() } returns session(secondsToExpiry = 5)

        for (path in listOf("/auth/refresh", "/auth/lwa/exchange")) {
            val (chain, seen) = chain(apiRequest(path), edgeDenial)
            interceptor.intercept(chain)
            assertNull(seen[0].header("Authorization"))
            assertNull(seen[0].header("X-LN-Device-ID"))
        }
        verify(exactly = 0) { refresher.refreshBlocking(any()) }
    }

    @Test
    fun callerSuppliedBearer_isLeftAloneAndItsForbiddenIsNotOurs() {
        every { tokenStore.accessToken() } returns "stale"
        every { tokenStore.session() } returns session(secondsToExpiry = 5)

        val request = Request.Builder()
            .url("https://live.jeremy.ninja/api/v1/realtime/session")
            .header("Authorization", "Bearer ephemeral")
            .build()
        val (chain, seen) = chain(request, edgeDenial)
        interceptor.intercept(chain)

        assertEquals(1, seen.size)
        assertEquals("Bearer ephemeral", seen[0].header("Authorization"))
        assertNull(seen[0].header("X-LN-Device-ID"))
        verify(exactly = 0) { refresher.refreshBlocking(any()) }
    }
}
