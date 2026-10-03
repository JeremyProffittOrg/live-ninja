package ninja.jeremy.liveninja.net

import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import io.mockk.coVerify
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.auth.StoredSession
import ninja.jeremy.liveninja.auth.TokenStore
import ninja.jeremy.liveninja.ui.jobs.JobsRepository
import org.junit.Assert.*
import org.junit.Test

class BoundJobsSessionTest {
    private fun session(id: String, token: String = "test-old", expiry: Long = 0) = StoredSession(token, expiry, "test-refresh", 9999999999, id)

    @Test fun expiredTokenRefreshesWithinTheSameReviewedSessionBeforeDispatch() = runTest {
        var stored = session("sid-a")
        val tokens = mockk<TokenStore> { every { session() } answers { stored } }
        val refresher = mockk<TokenRefresher> { every { refreshBoundBlocking("sid-a", "test-old") } answers {
            stored = session("sid-a", "test-fresh", 9999999999); RefreshOutcome.Refreshed("test-fresh")
        } }
        assertEquals("test-fresh", BoundJobsSession(tokens, refresher).credentials("sid-a").accessToken)
        verify(exactly = 1) { refresher.refreshBoundBlocking("sid-a", "test-old") }
    }

    @Test fun changedSessionBeforeOrDuringRefreshNeverReturnsItsCredentials() = runTest {
        var stored = session("sid-b")
        val tokens = mockk<TokenStore> { every { session() } answers { stored } }
        val refresher = mockk<TokenRefresher>()
        val binder = BoundJobsSession(tokens, refresher)
        assertTrue(runCatching { binder.credentials("sid-a") }.exceptionOrNull() is JobsSessionException)
        verify(exactly = 0) { refresher.refreshBoundBlocking(any(), any()) }
        stored = session("sid-a")
        every { refresher.refreshBoundBlocking(any(), any()) } answers { stored = session("sid-b", expiry = 9999999999); RefreshOutcome.Transient }
        assertTrue(runCatching { binder.credentials("sid-a") }.exceptionOrNull() is JobsSessionException)
    }

    @Test fun everyMutationRequiresTheSessionCapturedByTheUi() = runTest {
        val api = mockk<LiveNinjaApi>()
        val tokens = mockk<TokenStore> { every { session() } returns session("sid-b", expiry = 9999999999) }
        val repo = JobsRepository(api, tokens, BoundJobsSession(tokens, mockk()))
        val calls: List<suspend () -> Unit> = listOf(
            { repo.create(JobInputDto("Title", "Notes"), "request-a", "sid-a") },
            { repo.update("job-a", JobInputDto("Title", "Notes"), 1, "request-a", "sid-a") },
            { repo.jobAction("job-a", "pause", 1, "request-a", "sid-a") },
            { repo.runNow("job-a", 1, "request-a", "sid-a") },
            { repo.runAction("job-a", "run-a", "approve", 1, "request-a", "sid-a") },
            { repo.note("job-a", "Note", 1, "request-a", expectedSessionId = "sid-a") },
            { repo.create(JobInputDto("Title", "Notes"), "request-a") },
        )
        calls.forEach { assertTrue(runCatching { it() }.exceptionOrNull() is JobsSessionException) }
        coVerify(exactly = 0) { api.createJobReviewed(any(), any(), any()) }
        coVerify(exactly = 0) { api.updateJobReviewed(any(), any(), any(), any()) }
        coVerify(exactly = 0) { api.jobActionReviewed(any(), any(), any(), any(), any()) }
        coVerify(exactly = 0) { api.runJobReviewed(any(), any(), any(), any()) }
        coVerify(exactly = 0) { api.jobRunActionReviewed(any(), any(), any(), any(), any(), any()) }
        coVerify(exactly = 0) { api.jobCommand(any(), any(), any(), any()) }
    }
}
