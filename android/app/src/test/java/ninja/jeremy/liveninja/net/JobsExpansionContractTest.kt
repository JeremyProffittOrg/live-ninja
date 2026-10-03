package ninja.jeremy.liveninja.net

import io.mockk.every
import io.mockk.mockk
import io.mockk.coEvery
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.auth.StoredSession
import ninja.jeremy.liveninja.auth.TokenStore
import ninja.jeremy.liveninja.ui.jobs.*
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import okio.Buffer
import org.junit.Assert.*
import org.junit.Test
import retrofit2.Retrofit
import retrofit2.converter.kotlinx.serialization.asConverterFactory

class JobsExpansionContractTest {
    @Test fun timelineCommandsAndVoiceMutationUseRealRoutesAndPinnedCredentials() = runTest {
        val seen = mutableListOf<Pair<okhttp3.Request, String>>()
        val client = OkHttpClient.Builder().addInterceptor { chain ->
            val request = chain.request(); val buffer = Buffer(); request.body?.writeTo(buffer)
            seen += request to buffer.readUtf8()
            val payload = when {
                request.url.encodedPath.endsWith("/history") -> """{"entries":[{"id":"e1","jobId":"job-a","sequence":1,"role":"user","kind":"note","text":"Full saved note"}],"olderCursor":"older","newerCursor":"newer","retentionBoundary":"Since upgrade"}"""
                request.url.encodedPath.endsWith("/commands") -> """{"command":{"id":"n1","jobId":"job-a","kind":"note","status":"recorded"},"job":{"id":"job-a","version":2}}"""
                else -> """{"run":{"id":"r1","jobId":"job-a","status":"succeeded"},"job":{"id":"job-a","version":2}}"""
            }
            Response.Builder().request(request).protocol(Protocol.HTTP_1_1).code(200).message("OK").body(payload.toResponseBody("application/json".toMediaType())).build()
        }.build()
        val api = Retrofit.Builder().baseUrl("https://jobs-contract.invalid/").client(client).addConverterFactory(Json { ignoreUnknownKeys = true; explicitNulls = false }.asConverterFactory("application/json".toMediaType())).build().create(LiveNinjaApi::class.java)
        val tokenStore = mockk<TokenStore> { every { session() } returns StoredSession("test-a", 9999999999, "unused", 9999999999, "sid-a") }
        val repository = JobsRepository(api, tokenStore)
        val page = repository.historyPage(JobHistoryScope("sid-a", "job-a"), "older", HistoryDirection.OLDER)
        repository.historyPage(JobHistoryScope("sid-a", "job-a"), "newer", HistoryDirection.LATEST)
        repository.note("job-a", "A full note", 1, "note-request", expectedSessionId = "sid-a")
        repository.approveProposal(JobsVoiceProposal("call-a", "sid-a", "job_start", 0, jobId = "job-a", expectedVersion = 1), "voice-request")
        assertEquals("Full saved note", page.entries.single().text)
        assertEquals("older", seen[0].first.url.queryParameter("cursor")); assertNull(seen[0].first.url.queryParameter("after"))
        assertEquals("newer", seen[1].first.url.queryParameter("after")); assertNull(seen[1].first.url.queryParameter("cursor"))
        assertEquals("/api/v1/jobs/job-a/commands", seen[2].first.url.encodedPath)
        assertTrue(seen[2].second.contains("\"kind\":\"note\""))
        assertEquals("/api/v1/jobs/job-a/run", seen[3].first.url.encodedPath)
        listOf(0, 1, 3).forEach { assertEquals("Bearer test-a", seen[it].first.header("Authorization")); assertNotNull(seen[it].first.tag(AuthBoundRequest::class.java)) }
        assertTrue(seen[3].second.contains("voice-request")); assertFalse(seen[3].second.contains("confirm"))
        client.dispatcher.executorService.shutdown(); client.connectionPool.evictAll()
    }

    @Test fun differentSessionCannotDispatchProposal() = runTest {
        val api = mockk<LiveNinjaApi>()
        val tokenStore = mockk<TokenStore> { every { session() } returns StoredSession("test-b", 0, "unused", 0, "sid-b") }
        val repository = JobsRepository(api, tokenStore)
        val failure = runCatching { repository.approveProposal(JobsVoiceProposal("call-a", "sid-a", "job_start", 0, jobId = "job-a", expectedVersion = 1), "request-a") }.exceptionOrNull()
        assertTrue(failure is JobsSessionException)
        io.mockk.coVerify(exactly = 0) { api.runJobReviewed(any(), any(), any(), any()) }
    }

    @Test fun shortEventSummaryNeverHidesTheFullRunSnapshot() = runTest {
        val api = mockk<LiveNinjaApi>()
        val tokens = mockk<TokenStore> { every { session() } returns StoredSession("test-a", 0, "unused", 0, "sid-a") }
        val fullResult = "Retained result\n".repeat(10000)
        coEvery { api.jobHistory(any(), any(), any(), any(), any(), any()) } returns JobHistoryResponse(entries = listOf(JobHistoryEntryDto(id = "e1", jobId = "job-a", text = "Short progress", run = JobRunDto(instructions = "Original detailed intent", progress = "Short progress", result = fullResult, error = "Full failure explanation"))))
        val page = JobsRepository(api, tokens).historyPage(JobHistoryScope("sid-a", "job-a"), null, HistoryDirection.LATEST)
        val text = page.entries.single().text
        assertTrue(text.contains("Short progress"))
        assertTrue(text.contains("Original detailed intent"))
        assertTrue(text.contains(fullResult))
        assertTrue(text.contains("Full failure explanation"))
    }
}
