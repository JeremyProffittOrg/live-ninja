package ninja.jeremy.liveninja.net

import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.auth.StoredSession
import ninja.jeremy.liveninja.auth.TokenStore
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.*
import org.junit.Test

class BoundTokenRefresherTest {
    private fun stored(sid: String) = StoredSession("old", 0, "refresh", 9999999999, sid)
    private fun grant(sid: String) = """{"accessToken":"fresh","expiresAt":9999999999,"refreshToken":"rotated","refreshExpiresAt":9999999999,"sessionId":"$sid"}"""

    @Test fun oldAccountRefreshSuccessOrDenialCannotOverwriteOrClearNewAccount() {
        listOf(true, false).forEach { bound ->
        listOf(200, 403).forEach { code ->
            var session = stored("sid-a")
            val tokens = mockk<TokenStore> { every { session() } answers { session } }
            val client = OkHttpClient.Builder().addInterceptor { chain ->
                session = stored("sid-b")
                Response.Builder().request(chain.request()).protocol(Protocol.HTTP_1_1).code(code).message("fixture").body(grant("sid-a").toResponseBody("application/json".toMediaType())).build()
            }.build()
            val refresher = TokenRefresher(tokens, client, Json)
            assertEquals(RefreshOutcome.Transient, if (bound) refresher.refreshBoundBlocking("sid-a", "old") else refresher.refreshBlocking("old"))
            verify(exactly = 0) { tokens.updateFromRefresh(any(), any(), any(), any(), any()) }
            verify(exactly = 0) { tokens.clearSession() }
            assertEquals("sid-b", session.sessionId)
            client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
        }
        }
    }

    @Test fun mismatchedGrantSessionIsRejectedEvenIfStoredAccountHasNotChanged() {
        val tokens = mockk<TokenStore> { every { session() } returns stored("sid-a") }
        val client = OkHttpClient.Builder().addInterceptor { chain -> Response.Builder().request(chain.request()).protocol(Protocol.HTTP_1_1).code(200).message("fixture").body(grant("sid-b").toResponseBody("application/json".toMediaType())).build() }.build()
        assertEquals(RefreshOutcome.Transient, TokenRefresher(tokens, client, Json).refreshBoundBlocking("sid-a", "old"))
        verify(exactly = 0) { tokens.updateFromRefresh(any(), any(), any(), any(), any()) }
        client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
    }

    @Test fun initialDifferentAccountDoesNotMakeARefreshRequest() {
        val tokens = mockk<TokenStore> { every { session() } returns stored("sid-b") }
        val client = OkHttpClient.Builder().addInterceptor { error("Must not refresh another account") }.build()
        assertEquals(RefreshOutcome.Transient, TokenRefresher(tokens, client, Json).refreshBoundBlocking("sid-a", "old"))
        client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
    }
}
