package ninja.jeremy.liveninja.net

import io.mockk.*
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.realtime.RealtimeSessionCoordinator
import ninja.jeremy.liveninja.ui.files.PhoneRecordingsViewModel
import ninja.jeremy.liveninja.wake.WakePreferences
import org.junit.After
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class PhoneRecordingsTest {
    private val api = mockk<LiveNinjaApi>()
    private val busy = MutableStateFlow(false)
    private val listening = MutableStateFlow(false)
    private val coordinator = mockk<RealtimeSessionCoordinator>()
    private val wake = mockk<WakePreferences>()
    private val row = PhoneRecordingDto(callSid = "CA" + "a".repeat(32), recordingStatus = "ready")
    private val url = "https://live-ninja-deliverables-759775734231.s3.amazonaws.com/phone-trial/run/audio.mp3?X-Amz-Signature=test"
    @Before fun setup() {
        Dispatchers.setMain(UnconfinedTestDispatcher())
        every { coordinator.sessionBusy } returns busy
        every { wake.serviceEnabledFlow } returns listening
        coEvery { api.phoneRecordings("messages", null) } returns PhoneRecordingsResponse(listOf(row), "messages", listOf("messages", "trial"), "next")
        coEvery { api.phoneRecordingMedia(any(), any(), any()) } returns PhoneRecordingMedia(url, 300)
    }
    @After fun cleanup() { Dispatchers.resetMain() }
    private fun vm() = PhoneRecordingsViewModel(api, mockk(relaxed = true), coordinator, wake)

    @Test fun missingRecordingAndFutureFieldsDecodeWithoutInventingAudio() {
        val response = Json { ignoreUnknownKeys = true }.decodeFromString<PhoneRecordingsResponse>("""{"source":"messages","items":[{"requestId":"pending","recordingStatus":"absent","future":true}]}""")
        assertFalse(response.items.single().ready)
        assertEquals("pending", response.items.single().id)
    }
    @Test fun mediaUrlRejectsUntrustedTransportHostAndCredentials() {
        assertEquals(url, verifiedRecordingUrl(url))
        listOf(url.replace("https:", "http:"), url.replace(".s3.amazonaws.com", ".s3.amazonaws.com.evil.test"),
            url.replace("https://", "https://user:password@"), url.substringBefore('?'), url + "#fragment").forEach {
            assertTrue(runCatching { verifiedRecordingUrl(it) }.isFailure)
        }
    }
    @Test fun paginationDeduplicatesAndRetainsRowsOnFailure() {
        val vm = vm(); vm.load()
        coEvery { api.phoneRecordings("messages", "next") } throws java.io.IOException()
        vm.load(more = true)
        assertEquals(listOf(row), vm.state.value.items)
        assertNotNull(vm.state.value.message)
        coEvery { api.phoneRecordings("messages", "next") } returns PhoneRecordingsResponse(listOf(row), "messages")
        vm.load(more = true)
        assertEquals(1, vm.state.value.items.size)
        assertNull(vm.state.value.cursor)
    }
    @Test fun unavailableAudioAndCaptureNeverRequestPlayback() {
        val vm = vm()
        vm.media(row.copy(recordingStatus = "expired"), false)
        busy.value = true; vm.media(row, false)
        busy.value = false; listening.value = true; vm.media(row, false)
        coVerify(exactly = 0) { api.phoneRecordingMedia(any(), any(), any()) }
    }
    @Test fun newCaptureStopsPlaybackAndCancelsPendingLink() {
        val vm = vm(); vm.media(row, false)
        assertEquals(url, vm.state.value.playbackUrl)
        busy.value = true
        assertNull(vm.state.value.playbackUrl)
        busy.value = false
        val gate = CompletableDeferred<PhoneRecordingMedia>()
        coEvery { api.phoneRecordingMedia(any(), any(), any()) } coAnswers { gate.await() }
        vm.media(row, false); listening.value = true
        gate.complete(PhoneRecordingMedia(url, 300))
        assertNull(vm.state.value.playbackUrl)
        assertFalse(vm.state.value.mediaBusy)
    }
    @Test fun backgroundStopAndSourceChangeDiscardPendingPlayback() {
        val vm = vm()
        val gate = CompletableDeferred<PhoneRecordingMedia>()
        coEvery { api.phoneRecordingMedia(any(), any(), any()) } coAnswers { gate.await() }
        vm.media(row, false); vm.stopPlayback()
        gate.complete(PhoneRecordingMedia(url, 300))
        assertNull(vm.state.value.playbackUrl)
        coEvery { api.phoneRecordings("trial", null) } returns PhoneRecordingsResponse(emptyList(), "trial")
        vm.load("trial")
        assertEquals("trial", vm.state.value.source)
        assertTrue(vm.state.value.items.isEmpty())
    }
}
