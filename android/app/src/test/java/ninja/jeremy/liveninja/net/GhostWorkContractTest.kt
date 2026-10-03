package ninja.jeremy.liveninja.net

import io.mockk.*
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.async
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.auth.*
import ninja.jeremy.liveninja.ui.jobs.*
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.*
import org.junit.Test
import retrofit2.Retrofit
import retrofit2.converter.kotlinx.serialization.asConverterFactory

@OptIn(ExperimentalCoroutinesApi::class)
class GhostWorkContractTest {
    private val authState = MutableStateFlow<AuthState>(AuthState.SignedIn("sid-a"))
    private val auth = mockk<AuthRepository> { every { state } returns authState }
    private val bound = mockk<BoundJobsSession> { coEvery { credentials("sid-a") } returns StoredSession("test-a", 9999999999, "unused", 9999999999, "sid-a") }

    @Test fun canonicalRoutesKeepSnakeCaseStringSequenceAndPinnedOriginalSession() = runTest {
        val seen = mutableListOf<okhttp3.Request>()
        val client = OkHttpClient.Builder().addInterceptor { chain ->
            val request = chain.request(); seen += request
            val payload = when (request.url.encodedPath.substringAfterLast('/')) {
                "nodes" -> """{"nodes":[{"node_id":"OFFICEPC","state":"live","connected":true}],"source":"ghost","binding":"explicit_provider_selection"}"""
                "jobs" -> """{"events":[{"event_id":"scheduled-a","node":"OFFICEPC","run_at":"2030-01-01T00:00:00Z","last_run_status":"succeeded","runs":[{"run_id":"run-a","summary":"Full summary"}]}],"source":"ghost","runHistoryLimit":10,"providerSessionBindingAvailable":false}"""
                "sessions" -> """{"version":1,"node_id":"OFFICEPC","sessions":[{"session_id":"actual-provider","date":"2030-01-01"}],"coverage":"retained_only","next_cursor":"next-session"}"""
                else -> """{"version":1,"node_id":"OFFICEPC","session_id":"actual-provider","events":[{"id":"event-a","sequence":"00000000000000000000001:part:0","kind":"tool_result","text":"Full tool text","part":0,"more":true}],"resume_cursor":"tail","coverage":"retained_only","gaps":[]}"""
            }
            Response.Builder().request(request).protocol(Protocol.HTTP_1_1).code(200).message("OK").body(payload.toResponseBody("application/json".toMediaType())).build()
        }.build()
        val api = Retrofit.Builder().baseUrl("https://ghost-contract.invalid/").client(client).addConverterFactory(Json { ignoreUnknownKeys = true }.asConverterFactory("application/json".toMediaType())).build().create(LiveNinjaApi::class.java)
        val repo = GhostWorkRepository(api, bound, auth)
        assertEquals("live", repo.nodes("sid-a").nodes!!.single().state)
        val jobs = repo.jobs("sid-a"); assertEquals(false, jobs.providerSessionBindingAvailable); assertEquals(10, jobs.runHistoryLimit)
        repo.sessions("sid-a", "OFFICEPC", "session cursor")
        val events = repo.events(GhostHistoryScope("sid-a", "OFFICEPC", "actual-provider"), "event cursor")
        assertEquals("00000000000000000000001:part:0", events.events!!.single().sequence); assertNull(events.events.single().timestamp); assertTrue(events.events.single().more)
        assertEquals("session cursor", seen[2].url.queryParameter("cursor")); assertEquals("actual-provider", seen[3].url.queryParameter("session_id")); assertEquals("OFFICEPC", seen[3].url.queryParameter("node_id"))
        seen.forEach { assertEquals("GET", it.method); assertEquals("Bearer test-a", it.header("Authorization")); assertNotNull(it.tag(AuthBoundRequest::class.java)) }
        client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
    }

    @Test fun accountChangedBeforeDispatchOrAfterResponseNeverReturnsProviderContent() = runTest {
        val api = mockk<LiveNinjaApi>(); val repo = GhostWorkRepository(api, bound, auth)
        authState.value = AuthState.SignedIn("sid-b")
        assertTrue(runCatching { repo.nodes("sid-a") }.exceptionOrNull() is JobsSessionException)
        coVerify(exactly = 0) { api.ghostNodes(any(), any()) }
        authState.value = AuthState.SignedIn("sid-a")
        val gate = CompletableDeferred<GhostNodesResponse>()
        coEvery { api.ghostNodes(any(), any()) } coAnswers { gate.await() }
        val result = async { runCatching { repo.nodes("sid-a") } }; runCurrent()
        authState.value = AuthState.SignedIn("sid-b"); gate.complete(GhostNodesResponse(listOf(GhostNodeDto("private-a")))); runCurrent()
        assertTrue(result.await().exceptionOrNull() is JobsSessionException)
    }

    @Test fun mismatchedProviderScopeIsRejected() = runTest {
        val api = mockk<LiveNinjaApi>()
        coEvery { api.ghostEvents(any(), any(), any(), any(), any()) } returns GhostEventsResponse(1, "OFFICEPC", "other-provider", emptyList(), coverage = "retained_only")
        val result = runCatching { GhostWorkRepository(api, bound, auth).events(GhostHistoryScope("sid-a", "OFFICEPC", "actual-provider"), null) }
        assertTrue(result.isFailure)
    }
}
