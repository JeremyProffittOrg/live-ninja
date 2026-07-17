package ninja.jeremy.liveninja.net

import io.mockk.every
import io.mockk.justRun
import io.mockk.mockk
import io.mockk.verify
import java.io.IOException
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.async
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.auth.StoredSession
import ninja.jeremy.liveninja.auth.TokenStore
import okhttp3.Interceptor
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * TokenRefresher against a hermetic OkHttp client (an interceptor short-circuits
 * the network with canned responses, so the hardcoded backend URL never resolves).
 */
class TokenRefresherTest {

    private val json = Json { ignoreUnknownKeys = true }

    private fun session(
        access: String = "access-old",
        accessExp: Long = 0L, // expired long ago -> refresh proceeds
    ) = StoredSession(
        accessToken = access,
        accessExpiresAt = accessExp,
        refreshToken = "refresh-old",
        refreshExpiresAt = 4102444800,
        sessionId = "sess-1",
    )

    private fun cannedClient(code: Int, body: String, calls: AtomicInteger = AtomicInteger()): OkHttpClient =
        OkHttpClient.Builder()
            .addInterceptor(
                Interceptor { chain ->
                    calls.incrementAndGet()
                    Response.Builder()
                        .request(chain.request())
                        .protocol(Protocol.HTTP_1_1)
                        .code(code)
                        .message("canned")
                        .body(body.toResponseBody("application/json".toMediaType()))
                        .build()
                },
            )
            .build()

    private fun throwingClient(): OkHttpClient =
        OkHttpClient.Builder()
            .addInterceptor(Interceptor { throw IOException("network down") })
            .build()

    @Test
    fun successfulRefresh_persistsGrantAndReturnsFreshToken() {
        val store = mockk<TokenStore>()
        every { store.session() } returns session()
        justRun {
            store.updateFromRefresh(
                accessToken = "access-new",
                accessExpiresAt = 1893456000,
                refreshToken = "refresh-new",
                refreshExpiresAt = 1896048000,
                sessionId = "sess-1",
            )
        }
        val grant = """
            {"accessToken":"access-new","expiresAt":1893456000,
             "refreshToken":"refresh-new","refreshExpiresAt":1896048000,"sessionId":"sess-1"}
        """.trimIndent()
        val refresher = TokenRefresher(store, cannedClient(200, grant), json)

        val outcome = refresher.refreshBlocking("access-old")

        assertEquals(RefreshOutcome.Refreshed("access-new"), outcome)
        verify(exactly = 1) {
            store.updateFromRefresh("access-new", 1893456000, "refresh-new", 1896048000, "sess-1")
        }
    }

    @Test
    fun rejectedRefresh_clearsSessionAndEmitsSessionExpired() = runBlocking {
        val store = mockk<TokenStore>()
        every { store.session() } returns session()
        justRun { store.clearSession() }
        val refresher = TokenRefresher(
            store,
            cannedClient(401, """{"error":"invalid_refresh_token"}"""),
            json,
        )
        // Subscribe BEFORE triggering: replay=0, so only live collectors see it.
        val expired = async(start = CoroutineStart.UNDISPATCHED) {
            refresher.sessionExpired.first()
        }

        val outcome = refresher.refreshBlocking("access-old")

        assertEquals(RefreshOutcome.SessionExpired, outcome)
        verify(exactly = 1) { store.clearSession() }
        withTimeout(2_000) { expired.await() } // signed-out broadcast fired
    }

    @Test
    fun serverError_isTransient_sessionUntouched() {
        val store = mockk<TokenStore>()
        every { store.session() } returns session()
        val refresher = TokenRefresher(store, cannedClient(503, "oops"), json)

        assertEquals(RefreshOutcome.Transient, refresher.refreshBlocking("access-old"))
        verify(exactly = 0) { store.clearSession() }
    }

    @Test
    fun networkFailure_isTransient() {
        val store = mockk<TokenStore>()
        every { store.session() } returns session()
        val refresher = TokenRefresher(store, throwingClient(), json)

        assertEquals(RefreshOutcome.Transient, refresher.refreshBlocking("access-old"))
    }

    @Test
    fun signedOut_returnsSessionExpiredWithoutNetwork() {
        val store = mockk<TokenStore>()
        every { store.session() } returns null
        val calls = AtomicInteger()
        val refresher = TokenRefresher(store, cannedClient(200, "{}", calls), json)

        assertEquals(RefreshOutcome.SessionExpired, refresher.refreshBlocking(null))
        assertEquals(0, calls.get())
    }

    @Test
    fun alreadyRotatedByAnotherThread_returnsStoredTokenWithoutNetwork() {
        val store = mockk<TokenStore>()
        val farFuture = System.currentTimeMillis() / 1000 + 600
        every { store.session() } returns session(access = "access-new", accessExp = farFuture)
        val calls = AtomicInteger()
        val refresher = TokenRefresher(store, cannedClient(200, "{}", calls), json)

        // Caller still holds the OLD token; the store already has a fresh one.
        val outcome = refresher.refreshBlocking("access-old")

        assertEquals(RefreshOutcome.Refreshed("access-new"), outcome)
        assertEquals("no network call expected", 0, calls.get())
    }

    @Test
    fun malformedGrantBody_isTransient() {
        val store = mockk<TokenStore>()
        every { store.session() } returns session()
        val refresher = TokenRefresher(store, cannedClient(200, "not-json"), json)

        assertEquals(RefreshOutcome.Transient, refresher.refreshBlocking("access-old"))
    }
}
