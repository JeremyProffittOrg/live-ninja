package ninja.jeremy.liveninja.realtime

import io.mockk.every
import io.mockk.mockk
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.auth.*
import ninja.jeremy.liveninja.net.AuthBoundRequest
import ninja.jeremy.liveninja.ui.jobs.JobsProposalInbox
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.*
import org.junit.Test

class JobsToolProposalBridgeTest {
    private fun envelope() = """{"tool":"job_start","callId":"call-a","ok":false,"error":{"code":"confirmation_required","details":{"operation":"job_start","executionAvailable":true,"proposed":{"jobId":"job-a","expectedVersion":1},"job":{"id":"job-a","version":1,"title":"A private title"}}}}"""

    @Test fun invocationPinsCredentialSnapshotEvenWhenAccountChangesBeforeTransport() = runTest {
        val authFlow = MutableStateFlow<AuthState>(AuthState.SignedIn("sid-a"))
        val auth = mockk<AuthRepository> { every { state } returns authFlow }
        val tokens = mockk<TokenStore> { every { session() } returns StoredSession("test-a", 0, "unused", 0, "sid-a") }
        val inbox = JobsProposalInbox(auth)
        val client = OkHttpClient.Builder().addInterceptor { chain ->
            authFlow.value = AuthState.SignedIn("sid-b")
            every { tokens.session() } returns StoredSession("test-b", 0, "unused", 0, "sid-b")
            assertEquals("Bearer test-a", chain.request().header("Authorization"))
            assertNotNull(chain.request().tag(AuthBoundRequest::class.java))
            Response.Builder().request(chain.request()).protocol(Protocol.HTTP_1_1).code(409).message("Review required").body(envelope().toResponseBody("application/json".toMediaType())).build()
        }.build()
        ToolCallRouter(client, inbox, tokens).invoke(RealtimeEvent.FunctionCall("call-a", "job_start", "{}"), "sid-a")
        assertTrue(inbox.pending.value.isEmpty())
        client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
    }

    @Test fun mismatchedAuthAndCredentialSessionPreventsDispatch() = runTest {
        val authFlow = MutableStateFlow<AuthState>(AuthState.SignedIn("sid-a"))
        val auth = mockk<AuthRepository> { every { state } returns authFlow }
        val tokens = mockk<TokenStore> { every { session() } returns StoredSession("test-b", 0, "unused", 0, "sid-b") }
        val client = OkHttpClient.Builder().addInterceptor { error("Must not dispatch") }.build()
        val result = ToolCallRouter(client, JobsProposalInbox(auth), tokens).invoke(RealtimeEvent.FunctionCall("call-a", "job_start", "{}"), "sid-a")
        assertTrue(result.contains("session_changed"))
        client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
    }

    @Test fun jobsReadToolsRejectMissingOrOldRealtimeSessionWithoutDispatch() = runTest {
        val authFlow = MutableStateFlow<AuthState>(AuthState.SignedIn("sid-b"))
        val auth = mockk<AuthRepository> { every { state } returns authFlow }
        val tokens = mockk<TokenStore>()
        val client = OkHttpClient.Builder().addInterceptor { error("Old realtime session must not read jobs") }.build()
        val router = ToolCallRouter(client, JobsProposalInbox(auth), tokens)
        assertTrue(router.invoke(RealtimeEvent.FunctionCall("read-a", "job_list", "{}")).contains("session_changed"))
        assertTrue(router.invoke(RealtimeEvent.FunctionCall("read-b", "job_status", "{}"), "sid-a").contains("session_changed"))
        assertTrue(router.invoke(RealtimeEvent.FunctionCall("write-c", "memory_write", "{}"), "sid-a").contains("session_changed"))
        client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
    }
}
