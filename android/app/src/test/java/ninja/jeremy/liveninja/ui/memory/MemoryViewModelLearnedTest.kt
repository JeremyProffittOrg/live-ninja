package ninja.jeremy.liveninja.ui.memory

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.mockk
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import ninja.jeremy.liveninja.net.EntityListResponse
import ninja.jeremy.liveninja.net.GuideListResponse
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.MemoryAck
import ninja.jeremy.liveninja.net.RememberedDto
import ninja.jeremy.liveninja.net.RememberedListResponse
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.io.IOException

/**
 * The "Learned" tab (agentcore-memory records): the list maps the wire
 * shape, an account outside the rollout mode is shown as off rather than
 * empty, and forget removes the row only when the server accepted it.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class MemoryViewModelLearnedTest {

    private val api = mockk<LiveNinjaApi>()

    @Before
    fun setUp() {
        Dispatchers.setMain(UnconfinedTestDispatcher())
        coEvery { api.listEntities(any(), any(), any()) } returns EntityListResponse()
        coEvery { api.listGuides() } returns GuideListResponse()
    }

    @After
    fun tearDown() {
        Dispatchers.resetMain()
    }

    private fun viewModel() = MemoryViewModel(MemoryRepository(api))

    @Test
    fun loadIfNeeded_mapsLearnedRecordsAndKinds() {
        coEvery { api.listRemembered() } returns RememberedListResponse(
            enabled = true,
            items = listOf(
                RememberedDto(id = "r1", text = "The user's sister Sarah lives in Austin.", namespace = "/users/u1/facts/", createdAt = "2026-09-14T12:00:00Z"),
                RememberedDto(id = "r2", text = "Prefers Celsius", namespace = "/users/u1/preferences/"),
                RememberedDto(id = null, text = "no id"),
                RememberedDto(id = "r4", text = "   "),
            ),
        )

        val vm = viewModel()
        vm.loadIfNeeded()

        val s = vm.state.value
        assertTrue(s.learnedLoaded)
        assertTrue(s.learnedEnabled)
        assertEquals(listOf("r1", "r2"), s.learned.map { it.id })
        assertFalse(s.learned[0].isPreference)
        assertTrue(s.learned[1].isPreference)
        assertNull(s.learned[1].learnedLabel)
        assertTrue(s.learned[0].learnedLabel != null)
    }

    @Test
    fun loadIfNeeded_offAccountIsOffNotEmpty() {
        coEvery { api.listRemembered() } returns RememberedListResponse(enabled = false)

        val vm = viewModel()
        vm.loadIfNeeded()

        val s = vm.state.value
        assertTrue(s.learnedLoaded)
        assertFalse(s.learnedEnabled)
        assertTrue(s.learned.isEmpty())
        assertFalse(s.learnedError)
    }

    @Test
    fun refreshLearned_networkFailureIsAnError() {
        coEvery { api.listRemembered() } throws IOException("offline")

        val vm = viewModel()
        vm.refreshLearned()

        assertTrue(vm.state.value.learnedError)
        assertFalse(vm.state.value.learnedLoading)
    }

    @Test
    fun confirmForgetLearned_removesTheRowOnSuccessOnly() {
        coEvery { api.listRemembered() } returns RememberedListResponse(
            items = listOf(
                RememberedDto(id = "r1", text = "one", namespace = "/users/u1/facts/"),
                RememberedDto(id = "r2", text = "two", namespace = "/users/u1/facts/"),
            ),
        )
        coEvery { api.forgetRemembered("r1") } returns MemoryAck()
        coEvery { api.forgetRemembered("r2") } throws IOException("offline")

        val vm = viewModel()
        vm.refreshLearned()

        vm.requestForgetLearned(vm.state.value.learned[0])
        vm.confirmForgetLearned()
        assertEquals(listOf("r2"), vm.state.value.learned.map { it.id })
        assertNull(vm.state.value.confirmForgetLearned)

        vm.requestForgetLearned(vm.state.value.learned[0])
        vm.confirmForgetLearned()
        assertEquals(listOf("r2"), vm.state.value.learned.map { it.id })
        assertNull(vm.state.value.confirmForgetLearned)
        assertFalse(vm.state.value.forgetLearnedInProgress)

        coVerify(exactly = 1) { api.forgetRemembered("r1") }
        coVerify(exactly = 1) { api.forgetRemembered("r2") }
    }
}
