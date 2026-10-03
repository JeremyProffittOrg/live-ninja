package ninja.jeremy.liveninja.ui.jobs

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.mockk
import io.mockk.every
import kotlinx.coroutines.flow.MutableStateFlow
import ninja.jeremy.liveninja.auth.AuthState
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.net.*
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.After
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test
import retrofit2.HttpException
import retrofit2.Response
import java.io.IOException

@OptIn(ExperimentalCoroutinesApi::class)
class JobsViewModelTest {
    private val repository = mockk<JobsRepository>()
    private val job = JobDto(id = "job-1", title = "Saved job", instructions = "Saved notes", kind = "review", status = "active", version = 1, schedule = JobScheduleDto(timezone = "UTC"))
    private val capabilities = JobsCapabilitiesDto(reminder = true, review = true, scheduling = true)

    @Before fun setup() {
        Dispatchers.setMain(UnconfinedTestDispatcher())
        coEvery { repository.list(any()) } returns JobsListResponse(jobs = listOf(job), capabilities = capabilities)
        coEvery { repository.get(any()) } returns JobResponse(job)
        coEvery { repository.runs(any(), any()) } returns JobRunsResponse(runs = emptyList())
    }
    @After fun cleanup() { Dispatchers.resetMain() }
    private fun vm() = JobsViewModel(repository).also { it.loadIfNeeded() }
    private fun conflict() = HttpException(Response.error<JobResponse>(409, "{}".toResponseBody("application/json".toMediaType())))

    @Test fun duplicateSaveClicksSubmitOnceAndKeepTheEditorWhilePending() {
        val gate = CompletableDeferred<JobResponse>()
        coEvery { repository.create(any(), any(), any()) } coAnswers { gate.await() }
        val vm = vm(); vm.newJob(); vm.updateDraft { it.copy(title = "New title", instructions = "New notes") }
        vm.saveDraft(); vm.saveDraft()
        assertTrue(vm.state.value.busy); assertNotNull(vm.state.value.draft)
        coVerify(exactly = 1) { repository.create(any(), any()) }
        gate.complete(JobResponse(job))
        assertFalse(vm.state.value.busy); assertNull(vm.state.value.draft)
    }

    @Test fun uncertainRunRetryUsesTheSameVersionAndRequestIdentity() {
        val ids = mutableListOf<String>(); var calls = 0
        coEvery { repository.runNow(any(), any(), any()) } coAnswers {
            ids += thirdArg<String>(); calls++
            if (calls == 1) throw IOException("lost response")
            JobRunResponse(JobRunDto(id = "run-1", jobId = job.id, status = "waiting_approval"), job.copy(version = 2))
        }
        val vm = vm(); vm.openJob(job); vm.requestAction("run")
        assertTrue(vm.state.value.uncertain)
        vm.refresh(quiet = true)
        vm.requestAction("run")
        assertEquals(2, ids.size); assertEquals(ids[0], ids[1]); assertFalse(vm.state.value.uncertain)
        coVerify(exactly = 2) { repository.runNow("job-1", 1, any()) }
    }

    @Test fun staleEditPreservesDraftAndRequiresExplicitReload() {
        coEvery { repository.update(any(), any(), any(), any()) } throws conflict()
        val vm = vm(); vm.openJob(job); vm.editJob(); vm.updateDraft { it.copy(title = "Unsaved title") }; vm.saveDraft()
        assertEquals("Unsaved title", vm.state.value.draft?.title)
        assertTrue(vm.state.value.draftConflict); assertEquals(R.string.jobs_conflict, vm.state.value.draftError)
        vm.saveDraft(); coVerify(exactly = 1) { repository.update(any(), any(), any(), any()) }
        coEvery { repository.get(job.id) } returns JobResponse(job.copy(title = "New server version", version = 2))
        vm.reloadDraft()
        assertEquals("New server version", vm.state.value.draft?.title); assertEquals(2L, vm.state.value.draft?.version)
        assertFalse(vm.state.value.draftConflict)
    }

    @Test fun approvalKeepsExactRunAndJobVersionEvenWhenLatestJobChanges() {
        val savedRun = JobRunDto(id = "run-1", jobId = job.id, title = "Original checkpoint", instructions = "Exact saved instructions", status = "waiting_approval", attempt = 1)
        coEvery { repository.runAction(any(), any(), any(), any(), any()) } throws conflict()
        val vm = vm(); vm.openJob(job); vm.requestAction("approve", savedRun)
        coEvery { repository.get(job.id) } returns JobResponse(job.copy(title = "Changed title", instructions = "Changed instructions", version = 2))
        vm.refresh()
        assertEquals(2L, vm.state.value.selected?.version)
        assertEquals("Exact saved instructions", vm.state.value.confirmation?.run?.instructions)
        vm.confirmAction(); vm.confirmAction()
        coVerify(exactly = 1) { repository.runAction(job.id, "run-1", "approve", 1, any()) }
        assertTrue(vm.state.value.confirmationStale); assertNotNull(vm.state.value.confirmation)
    }

    @Test fun unknownCapabilitiesAndDisabledSchedulingFailClosed() {
        coEvery { repository.list(any()) } returns JobsListResponse(jobs = emptyList(), capabilities = null)
        val vm = vm(); vm.newJob(); assertNull(vm.state.value.draft)
        coEvery { repository.list(any()) } returns JobsListResponse(jobs = emptyList(), capabilities = capabilities.copy(scheduling = false))
        vm.refresh(); vm.newJob(); vm.updateDraft { it.copy(title = "Recurring", instructions = "Notes", scheduleKind = "daily") }; vm.saveDraft()
        assertEquals(R.string.jobs_scheduler_disabled, vm.state.value.draftError)
        coVerify(exactly = 0) { repository.create(any(), any()) }
    }

    @Test fun quietRefreshPreservesLoadedOlderRuns() {
        val first = (1..20).map { JobRunDto(id = "run-$it", jobId = job.id) }
        val older = (21..23).map { JobRunDto(id = "run-$it", jobId = job.id) }
        coEvery { repository.runs(job.id, null) } returns JobRunsResponse(first, "older")
        coEvery { repository.runs(job.id, "older") } returns JobRunsResponse(older)
        val vm = vm(); vm.openJob(job); vm.loadOlderRuns()
        assertEquals(23, vm.state.value.runs.size)
        vm.refresh(quiet = true)
        assertEquals(23, vm.state.value.runs.size)
        vm.refresh()
        assertEquals(20, vm.state.value.runs.size)
    }

    @Test fun lateDetailResponseCannotReplaceAnotherSelectedJob() {
        val gate = CompletableDeferred<JobResponse>()
        coEvery { repository.get("job-1") } coAnswers { gate.await() }
        val second = job.copy(id = "job-2", title = "Other job")
        coEvery { repository.get("job-2") } returns JobResponse(second)
        val vm = vm(); vm.openJob(job); vm.openJob(second); gate.complete(JobResponse(job))
        assertEquals("job-2", vm.state.value.selected?.id)
        assertFalse(vm.state.value.detailLoading)
    }

    @Test fun olderRunsWaitForAnOutstandingRefresh() {
        val first = JobRunDto(id = "run-first", jobId = job.id)
        val older = JobRunDto(id = "run-older", jobId = job.id)
        coEvery { repository.runs(job.id, null) } returns JobRunsResponse(listOf(first), "older")
        coEvery { repository.runs(job.id, "older") } returns JobRunsResponse(listOf(older))
        val vm = vm(); vm.openJob(job)
        val pendingGet = CompletableDeferred<JobResponse>()
        coEvery { repository.get(job.id) } coAnswers { pendingGet.await() }
        vm.refresh(quiet = true); vm.loadOlderRuns()
        coVerify(exactly = 0) { repository.runs(job.id, "older") }
        pendingGet.complete(JobResponse(job)); vm.loadOlderRuns()
        assertEquals(listOf("run-first", "run-older"), vm.state.value.runs.map { it.id })
    }

    @Test fun olderRunsBlockRefreshAndDuplicatePaginationUntilCompleted() {
        val first = JobRunDto(id = "run-first", jobId = job.id)
        val older = JobRunDto(id = "run-older", jobId = job.id)
        val pendingPage = CompletableDeferred<JobRunsResponse>()
        coEvery { repository.runs(job.id, null) } returns JobRunsResponse(listOf(first), "older")
        coEvery { repository.runs(job.id, "older") } coAnswers { pendingPage.await() }
        val vm = vm(); vm.openJob(job); vm.loadOlderRuns(); vm.loadOlderRuns(); vm.refresh(quiet = true)
        coVerify(exactly = 1) { repository.runs(job.id, "older") }
        coVerify(exactly = 1) { repository.list(null) }
        pendingPage.complete(JobRunsResponse(listOf(older)))
        assertEquals(listOf("run-first", "run-older"), vm.state.value.runs.map { it.id })
        assertFalse(vm.state.value.runsLoading)
    }

    @Test fun preMutationDetailResponseCannotRestoreOldApprovalState() {
        val staleGet = CompletableDeferred<JobResponse>()
        val savedRun = JobRunDto(id = "run-1", jobId = job.id, status = "waiting_approval", instructions = "Saved intent")
        val approved = savedRun.copy(status = "succeeded")
        val latest = job.copy(version = 2, lastRun = approved)
        val vm = vm(); vm.openJob(job)
        coEvery { repository.get(job.id) } coAnswers { staleGet.await() }
        vm.refresh()
        coEvery { repository.runAction(any(), any(), any(), any(), any()) } returns JobRunResponse(approved, latest)
        coEvery { repository.runs(job.id, null) } returns JobRunsResponse(listOf(approved))
        vm.requestAction("approve", savedRun); vm.confirmAction()
        staleGet.complete(JobResponse(job.copy(lastRun = savedRun)))
        assertEquals(2L, vm.state.value.selected?.version)
        assertEquals("succeeded", vm.state.value.runs.single().status)
        assertFalse(vm.state.value.loading)
    }

    @Test fun preMutationRunPageCannotOverwriteNewReceipt() {
        val staleRuns = CompletableDeferred<JobRunsResponse>()
        val savedRun = JobRunDto(id = "run-1", jobId = job.id, status = "waiting_approval")
        val approved = savedRun.copy(status = "succeeded")
        coEvery { repository.runs(job.id, null) } coAnswers { staleRuns.await() }
        val vm = vm(); vm.openJob(job)
        coEvery { repository.runAction(any(), any(), any(), any(), any()) } returns JobRunResponse(approved, job.copy(version = 2))
        coEvery { repository.runs(job.id, null) } returns JobRunsResponse(listOf(approved))
        vm.requestAction("approve", savedRun); vm.confirmAction()
        staleRuns.complete(JobRunsResponse(listOf(savedRun)))
        assertEquals(2L, vm.state.value.selected?.version)
        assertEquals("succeeded", vm.state.value.runs.single().status)
        assertFalse(vm.state.value.runsLoading)
    }

    @Test fun preMutationListPageCannotRestoreAnOldVersion() {
        val vm = vm(); vm.openJob(job)
        val staleList = CompletableDeferred<JobsListResponse>()
        coEvery { repository.list(any()) } coAnswers { staleList.await() }
        vm.refresh()
        coEvery { repository.jobAction(any(), any(), any(), any()) } returns JobResponse(job.copy(version = 2, status = "paused"))
        vm.requestAction("pause"); vm.confirmAction()
        staleList.complete(JobsListResponse(listOf(job), capabilities = capabilities))
        assertEquals(2L, vm.state.value.jobs.single().version)
        assertEquals("paused", vm.state.value.selected?.status)
        assertFalse(vm.state.value.loading)
    }

    @Test fun noteRequiresExplicitReviewAndKeepsItsExactTextDuringSubmission() {
        val gate = CompletableDeferred<JobCommandResponse>()
        coEvery { repository.note(any(), any(), any(), any(), any()) } coAnswers { gate.await() }
        val vm = vm(); vm.openJob(job)
        vm.handle(JobsEvent.Note("The exact note")); vm.handle(JobsEvent.ReviewNote)
        vm.handle(JobsEvent.Note("Changed draft behind dialog"))
        coVerify(exactly = 0) { repository.note(any(), any(), any(), any(), any()) }
        vm.confirmAction(); vm.confirmAction()
        coVerify(exactly = 1) { repository.note(job.id, "The exact note", 1, any(), null) }
        gate.complete(JobCommandResponse(JobCommandDto(id = "n1", jobId = job.id, status = "recorded"), job.copy(version = 2)))
        assertEquals("", vm.state.value.noteDraft)
        assertNull(vm.state.value.confirmation)
    }

    @Test fun oldAccountSaveCompletionCannotRepopulateTheNewAccountScreen() {
        val auth = MutableStateFlow<AuthState>(AuthState.SignedIn("sid-a"))
        val proposals = MutableStateFlow<List<JobsVoiceProposal>>(emptyList())
        val inbox = mockk<JobsProposalInbox> {
            every { authState } returns auth
            every { pending } returns proposals
            every { captureSession() } answers { (auth.value as? AuthState.SignedIn)?.sessionId }
        }
        val gate = CompletableDeferred<JobResponse>()
        coEvery { repository.create(any(), any(), any()) } coAnswers { gate.await() }
        val vm = JobsViewModel(repository, inbox); vm.loadIfNeeded(); vm.newJob()
        vm.updateDraft { it.copy(title = "Account A private title", instructions = "Private notes") }; vm.saveDraft()
        coEvery { repository.list(any()) } returns JobsListResponse(emptyList(), capabilities = capabilities)
        auth.value = AuthState.SignedIn("sid-b")
        gate.complete(JobResponse(job.copy(title = "Account A private title")))
        assertNull(vm.state.value.selected); assertTrue(vm.state.value.jobs.isEmpty()); assertNull(vm.state.value.draft)
        assertFalse(vm.state.value.busy)
    }

    @Test fun voiceProposalNeverExecutesBeforeReviewAndAnExplicitClick() {
        val auth = MutableStateFlow<AuthState>(AuthState.SignedIn("sid-a"))
        val proposal = JobsVoiceProposal("call-a", "sid-a", "job_start", 0, job = job, jobId = job.id, expectedVersion = 1)
        val proposals = MutableStateFlow(listOf(proposal))
        val inbox = mockk<JobsProposalInbox> {
            every { authState } returns auth; every { pending } returns proposals
            every { captureSession() } returns "sid-a"; every { isCurrent(proposal) } returns true
            every { dismiss(any()) } answers { proposals.value = emptyList() }
        }
        val gate = CompletableDeferred<JobResponse>()
        coEvery { repository.approveProposal(any(), any()) } coAnswers { gate.await() }
        coEvery { repository.historyPage(any(), any(), any()) } coAnswers { JobHistoryPage(firstArg()) }
        val vm = JobsViewModel(repository, inbox); vm.loadIfNeeded()
        coVerify(exactly = 0) { repository.approveProposal(any(), any()) }
        vm.handle(JobsEvent.ReviewVoice(proposal))
        assertEquals("Saved notes", vm.state.value.voiceReview?.job?.instructions)
        coVerify(exactly = 0) { repository.approveProposal(any(), any()) }
        vm.handle(JobsEvent.ConfirmVoice); vm.handle(JobsEvent.ConfirmVoice)
        coVerify(exactly = 1) { repository.approveProposal(proposal, any()) }
        gate.complete(JobResponse(job.copy(version = 2)))
        assertNull(vm.state.value.voiceReview); assertFalse(vm.state.value.busy)
    }
}
