package ninja.jeremy.liveninja.ui.jobs

import io.mockk.*
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.*
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.net.*
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.After
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test
import retrofit2.HttpException
import retrofit2.Response

@OptIn(ExperimentalCoroutinesApi::class)
class GhostExplorerViewModelTest {
    private val dispatcher = StandardTestDispatcher()
    private val authState = MutableStateFlow<AuthState>(AuthState.SignedIn("sid-a"))
    private val auth = mockk<AuthRepository> { every { state } returns authState }
    private val repository = mockk<GhostWorkRepository>()
    @Before fun setup() {
        Dispatchers.setMain(dispatcher)
        coEvery { repository.nodes("sid-a") } returns GhostNodesResponse(listOf(GhostNodeDto("OFFICEPC")))
        coEvery { repository.jobs("sid-a") } returns GhostJobsResponse(listOf(GhostScheduledEventDto("scheduled-a")), runHistoryLimit = 10)
        coEvery { repository.sessions("sid-a", "OFFICEPC", null) } returns GhostSessionsResponse(1, "OFFICEPC", listOf(GhostSessionDto("provider-a")), coverage = "retained_only")
        coEvery { repository.events(any(), any()) } returns GhostEventsResponse(1, "OFFICEPC", "provider-a", listOf(GhostHistoryEventDto("private-a", "001", text = "Private A text")), resume_cursor = "tail", coverage = "retained_only")
    }
    @After fun cleanup() { Dispatchers.resetMain() }
    private fun ready(): GhostExplorerViewModel {
        val vm = GhostExplorerViewModel(repository, auth); vm.loadIfNeeded(); dispatcher.scheduler.runCurrent()
        vm.handle(GhostExplorerEvent.Node("OFFICEPC")); dispatcher.scheduler.runCurrent()
        return vm
    }

    @Test fun explicitReturnedNodeAndSessionAreRequiredAndDiscoveryFollowsEmptyPages() {
        coEvery { repository.sessions("sid-a", "OFFICEPC", null) } returns GhostSessionsResponse(1, "OFFICEPC", emptyList(), "next", "retained_only")
        coEvery { repository.sessions("sid-a", "OFFICEPC", "next") } returns GhostSessionsResponse(1, "OFFICEPC", listOf(GhostSessionDto("provider-a"), GhostSessionDto("provider-a")), coverage = "retained_only")
        val vm = ready()
        assertEquals(1, vm.state.value.sessions.size)
        vm.handle(GhostExplorerEvent.Node("invented")); vm.handle(GhostExplorerEvent.Session("latest")); dispatcher.scheduler.runCurrent()
        assertNull(vm.state.value.archive.scope)
        vm.handle(GhostExplorerEvent.Session("provider-a")); dispatcher.scheduler.runCurrent()
        assertEquals(GhostHistoryScope("sid-a", "OFFICEPC", "provider-a"), vm.state.value.archive.scope)
        coVerify(exactly = 1) { repository.events(any(), any()) }
    }

    @Test fun accountChangeBeforeObserverRunsCannotOpenOldSelectionUnderNewAccount() {
        val vm = ready()
        authState.value = AuthState.SignedIn("sid-b") // Observer deliberately has not run.
        vm.handle(GhostExplorerEvent.Session("provider-a")); vm.handle(GhostExplorerEvent.Refresh)
        dispatcher.scheduler.runCurrent()
        coVerify(exactly = 0) { repository.events(any(), any()) }
        coVerify(exactly = 0) { repository.sessions("sid-b", any(), any()) }
        assertTrue(vm.state.value.nodes.isEmpty()); assertTrue(vm.state.value.jobs.isEmpty()); assertTrue(vm.state.value.sessions.isEmpty()); assertNull(vm.state.value.archive.scope)
    }

    @Test fun archiveDenialClearsInventoryAndStopsAllFurtherReads() {
        val vm = ready(); vm.handle(GhostExplorerEvent.Session("provider-a")); dispatcher.scheduler.runCurrent()
        coEvery { repository.events(any(), "tail") } throws HttpException(Response.error<GhostEventsResponse>(403, "{}".toResponseBody()))
        vm.poll(); dispatcher.scheduler.runCurrent()
        assertTrue(vm.state.value.denied); assertTrue(vm.state.value.nodes.isEmpty()); assertTrue(vm.state.value.jobs.isEmpty()); assertTrue(vm.state.value.archive.events.isEmpty())
        vm.poll(); vm.handle(GhostExplorerEvent.Refresh); vm.handle(GhostExplorerEvent.Rescan); vm.loadIfNeeded(); dispatcher.scheduler.runCurrent()
        coVerify(exactly = 2) { repository.events(any(), any()) }; coVerify(exactly = 1) { repository.nodes(any()) }
    }

    @Test fun lateInventoryCannotLandAfterAccountTransition() {
        val gate = CompletableDeferred<GhostJobsResponse>()
        coEvery { repository.jobs("sid-a") } coAnswers { gate.await() }
        val vm = GhostExplorerViewModel(repository, auth); vm.loadIfNeeded(); dispatcher.scheduler.runCurrent()
        authState.value = AuthState.SignedIn("sid-b"); dispatcher.scheduler.runCurrent()
        gate.complete(GhostJobsResponse(listOf(GhostScheduledEventDto("private-a")))); dispatcher.scheduler.runCurrent()
        assertTrue(vm.state.value.nodes.isEmpty()); assertTrue(vm.state.value.jobs.isEmpty()); assertFalse(vm.state.value.loading)
    }
}
