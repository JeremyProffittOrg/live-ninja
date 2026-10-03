package ninja.jeremy.liveninja.net

import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import ninja.jeremy.liveninja.ui.jobs.JobsRepository
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Request
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import okio.Buffer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import retrofit2.Retrofit
import retrofit2.converter.kotlinx.serialization.asConverterFactory

/** Real Retrofit + serialization, with an in-process transport: no network/credentials. */
class JobsApiContractTest {
    private val json = Json { ignoreUnknownKeys = true; explicitNulls = false }
    private data class Observed(val request: Request, val body: String)

    @Test
    fun everyNativeRouteUsesTheExactBackendContractAndAuthenticatedClient() = runTest {
        val observed = mutableListOf<Observed>()
        val client = OkHttpClient.Builder()
            .addInterceptor { chain -> chain.proceed(chain.request().newBuilder().header("X-LN-Test-Auth-Stack", "shared-client").build()) }
            .addInterceptor { chain ->
                val request = chain.request()
                val buffer = Buffer()
                request.body?.writeTo(buffer)
                synchronized(observed) { observed.add(Observed(request, buffer.readUtf8())) }
                val body = when {
                    request.method == "GET" && request.url.encodedPath.endsWith("/runs") -> """{"runs":null,"nextCursor":""}"""
                    request.method == "GET" && request.url.encodedPath.endsWith("/jobs/") -> """{"jobs":[],"capabilities":{"scheduling":false}}"""
                    request.url.encodedPath.endsWith("/run") || request.url.encodedPath.contains("/runs/") -> """{"job":{"id":"job_a","version":4},"run":{"id":"run_a","jobId":"job_a","status":"waiting_approval"}}"""
                    else -> """{"job":{"id":"job_a","version":4}}"""
                }
                Response.Builder().request(request).protocol(Protocol.HTTP_1_1).code(200).message("OK").body(body.toResponseBody("application/json".toMediaType())).build()
            }.build()
        val api = Retrofit.Builder().baseUrl("https://jobs-contract.invalid/").client(client)
            .addConverterFactory(json.asConverterFactory("application/json".toMediaType())).build().create(LiveNinjaApi::class.java)
        val repository = JobsRepository(api)
        repository.list("page one")
        repository.get("job_a")
        repository.create(JobInputDto("Title", "Exact body", "review"), "create-request")
        repository.update("job_a", JobInputDto("Changed", kind = "review"), 3, "edit-request")
        repository.jobAction("job_a", "pause", 3, "pause-request")
        repository.runs("job_a", "run_old")
        repository.runNow("job_a", 3, "run-request")
        repository.runAction("job_a", "run_a", "approve", 3, "approve-request")
        repository.runAction("job_a", "run_a", "cancel", 3, "cancel-request")
        repository.runAction("job_a", "run_a", "retry", 3, "retry-request")

        assertEquals(listOf("GET", "GET", "POST", "PATCH", "POST", "GET", "POST", "POST", "POST", "POST"), observed.map { it.request.method })
        assertEquals(listOf("/api/v1/jobs/", "/api/v1/jobs/job_a", "/api/v1/jobs/", "/api/v1/jobs/job_a", "/api/v1/jobs/job_a/pause", "/api/v1/jobs/job_a/runs", "/api/v1/jobs/job_a/run", "/api/v1/jobs/job_a/runs/run_a/approve", "/api/v1/jobs/job_a/runs/run_a/cancel", "/api/v1/jobs/job_a/runs/run_a/retry"), observed.map { it.request.url.encodedPath })
        assertTrue(observed.all { it.request.header("X-LN-Test-Auth-Stack") == "shared-client" })
        assertEquals("page one", observed[0].request.url.queryParameter("cursor"))
        assertEquals("30", observed[0].request.url.queryParameter("limit"))
        assertEquals("run_old", observed[5].request.url.queryParameter("cursor"))
        val create = json.parseToJsonElement(observed[2].body).jsonObject
        assertEquals("Exact body", create.getValue("instructions").jsonPrimitive.content)
        assertEquals("create-request", create.getValue("requestId").jsonPrimitive.content)
        assertFalse(create.containsKey("expectedVersion"))
        val update = json.parseToJsonElement(observed[3].body).jsonObject
        assertEquals("3", update.getValue("expectedVersion").jsonPrimitive.content)
        val approval = json.parseToJsonElement(observed[7].body).jsonObject
        assertEquals(setOf("expectedVersion", "requestId"), approval.keys)
        assertFalse(approval.containsKey("confirm"))
        client.dispatcher.executorService.shutdown()
        client.connectionPool.evictAll()
    }
}
