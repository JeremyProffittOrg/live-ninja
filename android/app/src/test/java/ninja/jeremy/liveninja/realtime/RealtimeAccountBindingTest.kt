package ninja.jeremy.liveninja.realtime

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.every
import io.mockk.mockk
import kotlinx.coroutines.runBlocking
import ninja.jeremy.liveninja.auth.DeviceIdentityStore
import ninja.jeremy.liveninja.auth.StoredSession
import ninja.jeremy.liveninja.net.AuthBoundRequest
import ninja.jeremy.liveninja.net.BoundJobsSession
import ninja.jeremy.liveninja.net.JobsSessionException
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.TranscriptUploadRequest
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.*
import org.junit.Test

class RealtimeAccountBindingTest {
    private val bound = mockk<BoundJobsSession>()
    private val device = mockk<DeviceIdentityStore> { every { deviceId } returns "test-device" }
    private val credentials = StoredSession("test-a-bearer", 9999999999, "test-refresh", 9999999999, "auth-a")

    @Test fun bootstrapUsesOriginalBearerAndDisablesCrossAccountRetry() = runBlocking {
        coEvery { bound.credentials("auth-a") } returns credentials
        var requests = 0
        val client = OkHttpClient.Builder().addInterceptor { chain ->
            requests++
            val request = chain.request()
            assertEquals("Bearer test-a-bearer", request.header("Authorization"))
            assertEquals("test-device", request.header("X-LN-Device-ID"))
            assertNotNull(request.tag(AuthBoundRequest::class.java))
            Response.Builder().request(request).protocol(Protocol.HTTP_1_1).code(200).message("OK")
                .body("""{"clientSecret":{"value":"test-provider-secret"},"sessionId":"conversation-a"}""".toResponseBody()).build()
        }.build()
        assertEquals("conversation-a", RealtimeSessionApi(client, bound, device).fetchSession("auth-a").sessionId)
        assertEquals(1, requests)
    }

    @Test fun changedAccountPreventsBootstrapHttp() = runBlocking {
        coEvery { bound.credentials("auth-a") } throws JobsSessionException()
        var requests = 0
        val client = OkHttpClient.Builder().addInterceptor { requests++; error("must not dispatch") }.build()
        assertTrue(runCatching { RealtimeSessionApi(client, bound, device).fetchSession("auth-a") }
            .exceptionOrNull() is JobsSessionException)
        assertEquals(0, requests)
    }

    @Test fun transcriptUsesCapturedOriginalAccountAndNeverOrdinaryUpload() = runBlocking {
        val api = mockk<LiveNinjaApi>()
        val body = TranscriptUploadRequest(sessionId = "conversation-a", final = true, turns = emptyList())
        coEvery { bound.credentials("auth-a") } returns credentials
        coEvery { api.uploadTranscriptBound(body, "Bearer test-a-bearer", any()) } returns Unit
        ApiTranscriptSink(api, bound).upload(body, "auth-a")
        coVerify(exactly = 1) { api.uploadTranscriptBound(body, "Bearer test-a-bearer", any()) }
        coVerify(exactly = 0) { api.uploadTranscript(any()) }
    }

    @Test fun changedAccountDropsAlreadyDrainedTranscriptBeforeHttp() = runBlocking {
        val api = mockk<LiveNinjaApi>()
        coEvery { bound.credentials("auth-a") } throws JobsSessionException()
        val body = TranscriptUploadRequest(sessionId = "conversation-a", final = true, turns = emptyList())
        assertTrue(runCatching { ApiTranscriptSink(api, bound).upload(body, "auth-a") }
            .exceptionOrNull() is JobsSessionException)
        coVerify(exactly = 0) { api.uploadTranscriptBound(any(), any(), any()) }
        coVerify(exactly = 0) { api.uploadTranscript(any()) }
    }
}
