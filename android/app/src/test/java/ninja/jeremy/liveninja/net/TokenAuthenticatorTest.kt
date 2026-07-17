package ninja.jeremy.liveninja.net

import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import okhttp3.Protocol
import okhttp3.Request
import okhttp3.Response
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * TokenAuthenticator's 401 retry-once state machine: refresh + replay exactly
 * once, never on the bootstrap routes, give up on repeat 401s or dead sessions.
 */
class TokenAuthenticatorTest {

    private val refresher = mockk<TokenRefresher>()
    private val authenticator = TokenAuthenticator(refresher)

    private fun request(path: String, bearer: String? = null): Request =
        Request.Builder()
            .url("https://live.jeremy.ninja$path")
            .apply { if (bearer != null) header("Authorization", "Bearer $bearer") }
            .build()

    private fun response401(request: Request, prior: Response? = null): Response =
        Response.Builder()
            .request(request)
            .protocol(Protocol.HTTP_1_1)
            .code(401)
            .message("Unauthorized")
            .priorResponse(prior)
            .build()

    @Test
    fun refreshSucceeds_requestReplayedWithFreshBearer() {
        every { refresher.refreshBlocking("stale") } returns RefreshOutcome.Refreshed("fresh")

        val retry = authenticator.authenticate(
            null,
            response401(request("/api/v1/realtime/session", bearer = "stale")),
        )

        assertEquals("Bearer fresh", retry?.header("Authorization"))
        verify(exactly = 1) { refresher.refreshBlocking("stale") }
    }

    @Test
    fun bootstrapRoutes_neverAuthenticated() {
        assertNull(authenticator.authenticate(null, response401(request("/auth/lwa/exchange"))))
        assertNull(authenticator.authenticate(null, response401(request("/auth/refresh"))))
        verify(exactly = 0) { refresher.refreshBlocking(any()) }
    }

    @Test
    fun secondConsecutive401_givesUpWithoutRefreshing() {
        val req = request("/api/v1/tools/invoke", bearer = "fresh")
        // priorResponse chain of depth 2 = this request already went through
        // one authenticate() pass.
        val second = response401(req, prior = response401(req))

        assertNull(authenticator.authenticate(null, second))
        verify(exactly = 0) { refresher.refreshBlocking(any()) }
    }

    @Test
    fun sessionExpired_abortsRetry() {
        every { refresher.refreshBlocking(any()) } returns RefreshOutcome.SessionExpired
        assertNull(
            authenticator.authenticate(
                null,
                response401(request("/api/v1/realtime/session", bearer = "stale")),
            ),
        )
    }

    @Test
    fun transientRefreshFailure_abortsRetry() {
        every { refresher.refreshBlocking(any()) } returns RefreshOutcome.Transient
        assertNull(
            authenticator.authenticate(
                null,
                response401(request("/api/v1/realtime/session", bearer = "stale")),
            ),
        )
    }
}
