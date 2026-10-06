package ninja.jeremy.liveninja.realtime

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.every
import io.mockk.mockk
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import kotlin.coroutines.CoroutineContext
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.Runnable
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.ui.state.SessionUiEvent
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/**
 * play_media integration with [RealtimeSessionCoordinator]: launch-once
 * handoff that mutes and ends only its bound session without response.create,
 * failures that keep talking, and stale-session predicates that deny launch.
 */
class DeviceMediaCoordinatorTest {

    private class FakeTransport : RealtimeTransport {
        private val _state = MutableStateFlow(TransportState.IDLE)
        override val state: StateFlow<TransportState> = _state
        private val _events = MutableSharedFlow<RealtimeEvent>(extraBufferCapacity = 64)
        override val events: SharedFlow<RealtimeEvent> = _events
        override var halfDuplex: Boolean = false

        val connectCalls = CopyOnWriteArrayList<Pair<String, String>>()
        val sentEvents = CopyOnWriteArrayList<JSONObject>()
        /** Every sendEvent call, including ones that [sendFailure] made throw. */
        val sendAttempts = CopyOnWriteArrayList<JSONObject>()
        val micMuted = CopyOnWriteArrayList<Boolean>()
        val stopPlaybackCalls = AtomicInteger()
        @Volatile var disconnects = 0

        /** Send-failure seam: a non-null Throwable is thrown instead of sending. */
        @Volatile var sendFailure: ((JSONObject) -> Throwable?)? = null

        override fun prime(session: RealtimeSession) = Unit
        override fun preWarm() = Unit
        override fun prepare() = Unit
        override suspend fun abortPrepare() = Unit

        override suspend fun connect(ephemeralToken: String, callsUrl: String) {
            connectCalls += ephemeralToken to callsUrl
            _state.value = TransportState.CONNECTED
        }

        override fun sendEvent(event: JSONObject) {
            sendAttempts += event
            sendFailure?.invoke(event)?.let { throw it }
            sentEvents += event
        }

        override fun setMicMuted(muted: Boolean) {
            micMuted += muted
        }

        override fun stopPlayback() {
            stopPlaybackCalls.incrementAndGet()
        }

        override suspend fun disconnect() {
            disconnects++
            _state.value = TransportState.CLOSED
        }

        suspend fun serverEvent(event: RealtimeEvent) {
            withTimeout(2_000) {
                while (_events.subscriptionCount.value == 0) delay(5)
            }
            _events.emit(event)
        }
    }

    /** Simulates the dispatcher hop to the main thread, running a hook first. */
    private class HopDispatcher(private val beforeHop: () -> Unit) : CoroutineDispatcher() {
        override fun dispatch(context: CoroutineContext, block: Runnable) {
            beforeHop()
            Dispatchers.Default.dispatch(context, block)
        }
    }

    private val transport = FakeTransport()
    private val novaTransport = FakeTransport()
    private val geminiTransport = FakeTransport()
    private val voiceLiveTransport = FakeTransport()
    private val authState = MutableStateFlow<AuthState>(AuthState.SignedIn("dummy-auth-a"))
    private val auth = mockk<AuthRepository> { every { state } returns authState }
    private val sessionApi = mockk<RealtimeSessionApi>()
    private val toolRouter = mockk<ToolCallRouter>()
    private val deviceVolumeTool = mockk<DeviceVolumeToolExecutor>()
    private val deviceCameraTool = mockk<DeviceCameraToolExecutor>()
    private val transcriptStore = TranscriptStore()

    private fun coordinator(
        gateway: FakeMediaGateway?,
        main: CoroutineContext = Dispatchers.Unconfined,
    ): RealtimeSessionCoordinator {
        coEvery { sessionApi.fetchSession(any()) } returns RealtimeSession(
            clientSecret = "dummy-ephemeral-token-not-a-secret",
            expiresAt = null,
            model = "gpt-realtime",
            voice = "cedar",
            sessionId = "rs-media",
            quotaWarning = null,
        )
        val coord = RealtimeSessionCoordinator(
            mockk<android.content.Context>(relaxed = true),
            transport, novaTransport, geminiTransport, voiceLiveTransport, sessionApi, toolRouter, deviceVolumeTool,
            deviceCameraTool,
            transcriptStore, TranscriptUploader(NoopTranscriptSink, CoroutineScope(SupervisorJob())), auth,
        )
        if (gateway != null) {
            coord.deviceMediaTool = DeviceMediaToolExecutor(DeviceMediaToolHandler(gateway, main))
        }
        return coord
    }

    private fun mediaCall(callId: String = "media-1") = RealtimeEvent.FunctionCall(
        callId = callId,
        name = PLAY_MEDIA_TOOL_NAME,
        argumentsJson = """{"kind":"music","query":"Clair de Lune"}""",
    )

    private fun functionOutputs(): List<JSONObject> = transport.sentEvents
        .filter { it.optString("type") == "conversation.item.create" }
        .map { JSONObject(it.getJSONObject("item").getString("output")) }

    private fun responseCreates(): Int =
        transport.sentEvents.count { it.optString("type") == "response.create" }

    private fun CoroutineScope.collectInto(
        coord: RealtimeSessionCoordinator,
        sink: MutableList<SessionUiEvent>,
    ): Job = launch(start = CoroutineStart.UNDISPATCHED) {
        coord.events.collect { sink.add(it) }
    }

    private suspend fun awaitUntil(message: String, predicate: () -> Boolean) {
        try {
            withTimeout(15_000) {
                while (!predicate()) delay(10)
            }
        } catch (e: kotlinx.coroutines.TimeoutCancellationException) {
            fail("timed out waiting: $message")
        }
    }

    @Test
    fun successfulLaunch_happensOnceMutesStopsAudioSendsHonestOutputAndEndsSession() = runBlocking {
        val gateway = FakeMediaGateway()
        val coord = coordinator(gateway)
        val seen = CopyOnWriteArrayList<SessionUiEvent>()
        val job = collectInto(coord, seen)
        coord.start()

        transport.serverEvent(mediaCall())

        awaitUntil("session ended after handoff") { transport.disconnects == 1 && !coord.connected.value }
        assertEquals(1, gateway.started.size)
        assertTrue(gateway.started.single() is MediaLaunchCandidate.PlayFromSearch)
        val outputs = functionOutputs()
        assertEquals(1, outputs.size)
        assertTrue(outputs.single().getBoolean("ok"))
        val out = outputs.single().getJSONObject("output")
        assertEquals("playback_requested", out.getString("status"))
        assertFalse(out.getBoolean("playbackConfirmed"))
        assertEquals(0, responseCreates())
        assertTrue(transport.micMuted.contains(true))
        assertTrue(transport.stopPlaybackCalls.get() >= 1)
        awaitUntil("media tool chip") {
            seen.any { it is SessionUiEvent.ToolCall && it.name == PLAY_MEDIA_TOOL_NAME }
        }
        val chip = seen.filterIsInstance<SessionUiEvent.ToolCall>().single()
        assertTrue(chip.summary.contains("not confirmed"))
        coVerify(exactly = 0) { toolRouter.invoke(any(), any()) }
        job.cancel()
    }

    @Test
    fun cancellationOnFunctionOutputAfterLaunch_stillEndsExactlyOriginalSession() = runBlocking {
        val gateway = FakeMediaGateway()
        val coord = coordinator(gateway)
        coord.start()
        transport.sendFailure = { event ->
            if (event.optString("type") == "conversation.item.create") {
                CancellationException("function output send cancelled")
            } else {
                null
            }
        }

        transport.serverEvent(mediaCall("media-cancel"))

        awaitUntil("original session ended despite cancellation") {
            transport.disconnects == 1 && !coord.connected.value
        }
        delay(100)
        assertEquals("launched exactly once", 1, gateway.started.size)
        assertEquals(1, gateway.startAttempts.get())
        assertEquals(
            1,
            transport.sendAttempts.count { it.optString("type") == "conversation.item.create" },
        )
        assertTrue(transport.sendAttempts.none { it.optString("type") == "response.create" })
        assertTrue(functionOutputs().isEmpty())
        assertEquals(0, responseCreates())
        assertTrue(transport.micMuted.contains(true))
        assertEquals(0, novaTransport.disconnects)
        assertEquals(0, geminiTransport.disconnects)
        assertEquals(0, voiceLiveTransport.disconnects)
        coVerify(exactly = 0) { toolRouter.invoke(any(), any()) }

        // A replacement session started afterwards is never stopped by the
        // original handoff's cleanup.
        transport.sendFailure = null
        coord.start()
        assertTrue(coord.connected.value)
        delay(300)
        assertTrue("replacement session must stay connected", coord.connected.value)
        assertEquals(1, transport.disconnects)
        assertEquals(1, gateway.started.size)
        coord.stop()
    }

    @Test
    fun duplicateInFlightCall_launchesOnceAndSendsOneOutput() = runBlocking {
        val entered = CountDownLatch(1)
        val release = CountDownLatch(1)
        val gateway = FakeMediaGateway().apply {
            starter = {
                entered.countDown()
                release.await(10, TimeUnit.SECONDS)
            }
        }
        val coord = coordinator(gateway)
        coord.start()

        transport.serverEvent(mediaCall("media-dup"))
        awaitUntil("first launch entered") { entered.count == 0L }
        transport.serverEvent(mediaCall("media-dup"))
        delay(150)
        release.countDown()

        awaitUntil("session ended after handoff") { transport.disconnects == 1 && !coord.connected.value }
        delay(100)
        assertEquals(1, gateway.started.size)
        assertEquals(1, functionOutputs().size)
        assertEquals(0, responseCreates())
    }

    @Test
    fun launchFailure_keepsConnectionAndContinuesResponse() = runBlocking {
        val gateway = FakeMediaGateway().apply { resolver = { false } }
        val coord = coordinator(gateway)
        coord.start()

        transport.serverEvent(mediaCall())

        awaitUntil("error result and response.create") { responseCreates() == 1 }
        val output = functionOutputs().single()
        assertFalse(output.getBoolean("ok"))
        assertEquals("no_handler", output.getJSONObject("error").getString("code"))
        delay(100)
        assertTrue(coord.connected.value)
        assertEquals(0, transport.disconnects)
        assertTrue(transport.micMuted.isEmpty())
        assertTrue(gateway.started.isEmpty())
        coord.stop()
    }

    @Test
    fun lockedDevice_returnsErrorAndConversationContinues() = runBlocking {
        val gateway = FakeMediaGateway().apply { state = MediaForegroundState.LOCKED }
        val coord = coordinator(gateway)
        coord.start()

        transport.serverEvent(mediaCall())

        awaitUntil("locked error and response.create") { responseCreates() == 1 }
        val error = functionOutputs().single().getJSONObject("error")
        assertEquals("device_locked", error.getString("code"))
        assertTrue(error.getString("message").contains("open Live Ninja"))
        assertTrue(coord.connected.value)
        assertTrue(gateway.started.isEmpty())
        coord.stop()
    }

    @Test
    fun authChangeDuringMainThreadHop_deniesLaunch() = runBlocking {
        val gateway = FakeMediaGateway()
        val hop = HopDispatcher { authState.value = AuthState.SignedIn("dummy-auth-b") }
        val coord = coordinator(gateway, hop)
        coord.start()

        transport.serverEvent(mediaCall())

        awaitUntil("old session closed by auth change") { transport.disconnects == 1 && !coord.connected.value }
        delay(100)
        assertTrue(gateway.started.isEmpty())
        assertTrue(functionOutputs().isEmpty())
        assertEquals(0, responseCreates())
    }

    @Test
    fun authChangeBetweenResolveAndStart_deniesLaunch() = runBlocking {
        val gateway = FakeMediaGateway().apply {
            resolver = {
                authState.value = AuthState.SignedIn("dummy-auth-b")
                true
            }
        }
        val coord = coordinator(gateway)
        coord.start()

        transport.serverEvent(mediaCall())

        awaitUntil("old session closed by auth change") { transport.disconnects == 1 && !coord.connected.value }
        delay(100)
        assertTrue(gateway.started.isEmpty())
        assertTrue(functionOutputs().isEmpty())
    }

    @Test
    fun lateLaunchResult_neverStopsOrWritesIntoReplacementSession() = runBlocking {
        val entered = CountDownLatch(1)
        val release = CountDownLatch(1)
        val gateway = FakeMediaGateway().apply {
            starter = {
                entered.countDown()
                release.await(10, TimeUnit.SECONDS)
            }
        }
        val coord = coordinator(gateway)
        coord.start()

        transport.serverEvent(mediaCall("media-old"))
        awaitUntil("launch entered") { entered.count == 0L }
        coord.stop()
        assertEquals(1, transport.disconnects)
        coord.start()
        assertTrue(coord.connected.value)
        assertEquals(2, transport.connectCalls.size)

        release.countDown()
        delay(300)

        assertTrue("replacement session must stay connected", coord.connected.value)
        assertEquals(1, transport.disconnects)
        assertTrue(functionOutputs().isEmpty())
        assertTrue(transport.micMuted.isEmpty())
        coord.stop()
    }

    @Test
    fun withoutInjectedExecutor_returnsStructuredErrorAndNeverRoutesToBackend() = runBlocking {
        val coord = coordinator(gateway = null)
        coord.start()

        transport.serverEvent(mediaCall())

        awaitUntil("error and response.create") { responseCreates() == 1 }
        val output = functionOutputs().single()
        assertFalse(output.getBoolean("ok"))
        assertEquals("not_supported", output.getJSONObject("error").getString("code"))
        assertTrue(coord.connected.value)
        coVerify(exactly = 0) { toolRouter.invoke(any(), any()) }
        coord.stop()
    }

    private object NoopTranscriptSink : TranscriptSink {
        override suspend fun upload(body: ninja.jeremy.liveninja.net.TranscriptUploadRequest, expectedSessionId: String) = Unit
    }
}
