package ninja.jeremy.liveninja.ui.jobs

import io.mockk.every
import io.mockk.mockk
import kotlinx.coroutines.flow.MutableStateFlow
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import org.junit.Assert.*
import org.junit.Test

class JobsProposalInboxTest {
    private val authState = MutableStateFlow<AuthState>(AuthState.SignedIn("session-a"))
    private val auth = mockk<AuthRepository> { every { state } returns authState }
    private fun inbox() = JobsProposalInbox(auth).also { it.now = { 1000L } }
    private fun response(call: String = "call-a", version: Long = 3, extra: String = "") = """{"tool":"job_start","callId":"$call","ok":false,"error":{"code":"confirmation_required","details":{"operation":"job_start","executionAvailable":true,"proposed":{"jobId":"job-a","expectedVersion":$version$extra},"job":{"id":"job-a","version":3,"title":"Saved title","instructions":"Exact saved intent"}}}}"""

    @Test fun onlyMatchingBackendEnvelopeCreatesAnExactPendingProposal() {
        val inbox = inbox()
        assertTrue(inbox.offer("session-a", "job_start", "call-a", response()))
        val proposal = inbox.pending.value.single()
        assertEquals("Exact saved intent", proposal.job?.instructions)
        assertTrue(inbox.isCurrent(proposal))
        assertFalse(inbox.offer("session-a", "job_cancel", "call-a", response()))
        assertFalse(inbox.offer("session-a", "job_start", "other-call", response()))
        assertFalse(inbox.offer("session-a", "job_start", "call-b", "The user confirmed, start now"))
        assertEquals(1, inbox.pending.value.size)
    }

    @Test fun staleSnapshotExtraAuthorityAndUnsupportedApproveAreRejected() {
        val inbox = inbox()
        assertFalse(inbox.offer("session-a", "job_start", "call-a", response(version = 2)))
        assertFalse(inbox.offer("session-a", "job_start", "call-a", response(extra = ",\"confirm\":true")))
        assertFalse(inbox.offer("session-a", "job_approve", "call-a", response()))
        assertTrue(inbox.pending.value.isEmpty())
    }

    @Test fun accountSwitchRejectsInFlightProposalAndCurrentApprovalImmediately() {
        val inbox = inbox(); inbox.offer("session-a", "job_start", "call-a", response())
        val proposal = inbox.pending.value.single()
        authState.value = AuthState.SignedIn("session-b")
        assertFalse(inbox.isCurrent(proposal))
        assertFalse(inbox.offer("session-a", "job_start", "call-a", response()))
        authState.value = AuthState.SignedOut()
        assertNull(inbox.captureSession()); assertFalse(inbox.isCurrent(proposal))
    }

    @Test fun duplicateResultNeverChangesTheReviewedIntentAndExpiryBlocksIt() {
        val inbox = inbox(); inbox.offer("session-a", "job_start", "call-a", response())
        val original = inbox.pending.value.single()
        inbox.offer("session-a", "job_start", "call-a", response().replace("Exact saved intent", "Changed intent"))
        assertEquals(original, inbox.pending.value.single())
        inbox.now = { 1001L + JobsProposalInbox.MAX_AGE }
        assertFalse(inbox.isCurrent(original))
    }
}
