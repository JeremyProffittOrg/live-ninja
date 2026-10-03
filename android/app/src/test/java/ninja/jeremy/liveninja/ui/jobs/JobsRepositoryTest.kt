package ninja.jeremy.liveninja.ui.jobs

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.mockk
import java.io.IOException
import java.util.concurrent.CancellationException
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.net.JobActionRequest
import ninja.jeremy.liveninja.net.JobDto
import ninja.jeremy.liveninja.net.JobInputDto
import ninja.jeremy.liveninja.net.JobResponse
import ninja.jeremy.liveninja.net.JobRunDto
import ninja.jeremy.liveninja.net.JobRunResponse
import ninja.jeremy.liveninja.net.JobRunsResponse
import ninja.jeremy.liveninja.net.JobsListResponse
import ninja.jeremy.liveninja.net.LiveNinjaApi
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test
import retrofit2.HttpException
import retrofit2.Response

class JobsRepositoryTest {
    private val api = mockk<LiveNinjaApi>()
    private val repo = JobsRepository(api)
    private val job = JobDto(id = "job_a", version = 4, status = "active", kind = "review")

    @Test
    fun nullableCollectionsAreEmptyWithoutInventingCapabilities() = runTest {
        coEvery { api.listJobs(null, 30) } returns JobsListResponse(jobs = null, nextCursor = "")
        coEvery { api.listJobRuns("job_a", "older", 30) } returns JobRunsResponse(runs = null, nextCursor = "")
        val page = repo.list()
        val history = repo.runs("job_a", "older")
        assertTrue(page.jobs!!.isEmpty())
        assertNull(page.capabilities)
        assertNull(page.nextCursor)
        assertTrue(history.runs!!.isEmpty())
        assertNull(history.nextCursor)
    }

    @Test
    fun transportFailureDoesNotRepeatAndExplicitRetryKeepsRequestFence() = runTest {
        val bodies = mutableListOf<JobActionRequest>()
        coEvery { api.runJob("job_a", capture(bodies)) } throws IOException("lost response") andThen
            JobRunResponse(JobRunDto(id = "run_a", jobId = "job_a", status = "waiting_approval"), job)
        try { repo.runNow("job_a", 3, "same-request-id"); throw AssertionError("failure expected") } catch (_: IOException) { }
        coVerify(exactly = 1) { api.runJob(any(), any()) }
        val response = repo.runNow("job_a", 3, "same-request-id")
        assertEquals("waiting_approval", response.run!!.status)
        assertEquals(listOf(JobActionRequest(3, "same-request-id"), JobActionRequest(3, "same-request-id")), bodies)
        coVerify(exactly = 0) { api.getJob(any()) }
    }

    @Test
    fun conflictPropagatesWithoutRefreshingVersionOrRepeatingApproval() = runTest {
        val body = """{"error":{"code":"version_conflict","message":"Refresh this job."}}""".toResponseBody("application/json".toMediaType())
        val conflict = HttpException(Response.error<JobRunResponse>(409, body))
        coEvery { api.jobRunAction("job_a", "run_a", "approve", any()) } throws conflict
        try {
            repo.runAction("job_a", "run_a", "approve", 3, "approve-exact-version")
            throw AssertionError("conflict expected")
        } catch (error: HttpException) {
            assertSame(conflict, error)
            assertEquals(409, error.code())
        }
        coVerify(exactly = 1) { api.jobRunAction("job_a", "run_a", "approve", JobActionRequest(3, "approve-exact-version")) }
        coVerify(exactly = 0) { api.getJob(any()) }
    }

    @Test
    fun cancellationPropagatesWithoutRetry() = runTest {
        val cancelled = CancellationException("screen closed")
        coEvery { api.jobAction(any(), any(), any()) } throws cancelled
        try { repo.jobAction("job_a", "pause", 4, "pause-request"); throw AssertionError("cancel expected") } catch (error: CancellationException) { assertSame(cancelled, error) }
        coVerify(exactly = 1) { api.jobAction(any(), any(), any()) }
    }

    @Test
    fun missingOrCrossJobReceiptNeverReportsSuccess() = runTest {
        coEvery { api.runJob(any(), any()) } returns JobRunResponse(run = null, job = job)
        try { repo.runNow("job_a", 4, "missing-receipt"); throw AssertionError("receipt required") } catch (_: IOException) { }
        coEvery { api.runJob(any(), any()) } returns JobRunResponse(JobRunDto(id = "run_a", jobId = "someone_else", status = "succeeded"), job)
        try { repo.runNow("job_a", 4, "crossjob-receipt"); throw AssertionError("receipt identity required") } catch (_: IOException) { }
    }

    @Test
    fun invalidRoutesVersionsAndUnsupportedProvidersNeverReachNetwork() = runTest {
        val invalid: List<suspend () -> Unit> = listOf(
            { repo.get("../auth") },
            { repo.runNow("job_a", 0, "valid-request") },
            { repo.jobAction("job_a", "delete-account", 4, "valid-request") },
            { repo.runAction("job_a", "run_a", "send-email", 4, "valid-request") },
            { repo.create(JobInputDto("Unconnected", kind = "coding"), "valid-request") },
            { repo.create(JobInputDto("Title"), "short") },
            { repo.create(JobInputDto("界".repeat(54)), "valid-request") },
        )
        invalid.forEach { call -> try { call(); throw AssertionError("validation expected") } catch (_: IllegalArgumentException) { } }
        coVerify(exactly = 0) { api.runJob(any(), any()) }
        coVerify(exactly = 0) { api.createJob(any()) }
        coVerify(exactly = 0) { api.jobAction(any(), any(), any()) }
        coVerify(exactly = 0) { api.jobRunAction(any(), any(), any(), any()) }
    }

    @Test
    fun requestIdsAreCreatedSeparatelyFromMutationExecution() {
        val first = JobsRepository.newRequestId()
        val second = JobsRepository.newRequestId()
        assertTrue(first.length in 8..128)
        assertFalse(first == second)
    }

    @Test
    fun missingSavedJobCannotBeTreatedAsSuccessfulCreate() = runTest {
        coEvery { api.createJob(any()) } returns JobResponse()
        try { repo.create(JobInputDto("Title"), "create-request"); throw AssertionError("saved job required") } catch (_: IOException) { }
        coVerify(exactly = 1) { api.createJob(any()) }
    }
}
