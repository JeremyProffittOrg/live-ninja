package ninja.jeremy.liveninja.realtime

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import java.io.IOException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import ninja.jeremy.liveninja.ui.state.SessionUiEvent
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/**
 * Transport/session state machine: connect wiring, FAILED -> error surface,
 * deliberate stop() staying silent, transcript delta/remainder accounting, and
 * the function-call round trip — all against a fake [RealtimeTransport].
 */
class RealtimeSessionCoordinatorTest {

    private class FakeTransport : RealtimeTransport {
        private val _state = MutableStateFlow(TransportState.IDLE)
        override val state: StateFlow<TransportState> = _state
        private val _events = MutableSharedFlow<RealtimeEvent>(extraBufferCapacity = 64)
        override val events: SharedFlow<RealtimeEvent> = _events
        override var halfDuplex: Boolean = false

        val connectCalls = mutableListOf<Pair<String, String>>()
        val primedSessions = mutableListOf<RealtimeSession>()
        val sentEvents = mutableListOf<JSONObject>()
        var disconnects = 0
        var failConnect = false

        // M8.3 latency-parallelization instrumentation.
        var prepareCalls = 0
        var abortPrepareCalls = 0
        var preWarmCalls = 0

        override fun prime(session: RealtimeSession) {
            primedSessions += session
        }

        override fun preWarm() {
            preWarmCalls++
        }

        override fun prepare() {
            prepareCalls++
        }

        override suspend fun abortPrepare() {
            abortPrepareCalls++
        }

        override suspend fun connect(ephemeralToken: String, callsUrl: String) {
            connectCalls += ephemeralToken to callsUrl
            if (failConnect) throw IOException("sdp negotiation failed")
            _state.value = TransportState.CONNECTED
        }

        override fun sendEvent(event: JSONObject) {
            sentEvents += event
        }

        override fun setMicMuted(muted: Boolean) = Unit
        override fun stopPlayback() = Unit

        override suspend fun disconnect() {
            disconnects++
            _state.value = TransportState.CLOSED
        }

        suspend fun serverEvent(event: RealtimeEvent) {
            // The coordinator collects on Dispatchers.Default; wait for a subscriber
            // so hot emissions are never dropped before collection starts.
            withTimeout(2_000) {
                while (_events.subscriptionCount.value == 0) delay(5)
            }
            _events.emit(event)
        }

        fun driveState(state: TransportState) {
            _state.value = state
        }
    }

    private val transport = FakeTransport()
    // Nova/Gemini transports for the qualified constructor slots; the default
    // session below is openai-direct, so the coordinator selects [transport].
    private val novaTransport = FakeTransport()
    private val geminiTransport = FakeTransport()
    private val sessionApi = mockk<RealtimeSessionApi>()
    private val toolRouter = mockk<ToolCallRouter>()
    private val deviceVolumeTool = mockk<DeviceVolumeToolExecutor>()
    private val deviceCameraTool = mockk<DeviceCameraToolExecutor>()

    private fun coordinator(): RealtimeSessionCoordinator {
        coEvery { sessionApi.fetchSession() } returns RealtimeSession(
            clientSecret = "ephemeral-token",
            expiresAt = null,
            model = "gpt-realtime",
            voice = "cedar",
            sessionId = "rs-1",
            quotaWarning = null,
        )
        return RealtimeSessionCoordinator(
            // Only used to stop the wake service for the device-local stop_listening
            // tool, which this suite does not exercise.
            mockk<android.content.Context>(relaxed = true),
            transport, novaTransport, geminiTransport, sessionApi, toolRouter, deviceVolumeTool,
            deviceCameraTool,
            TranscriptStore(), TranscriptUploader(NoopTranscriptSink, CoroutineScope(SupervisorJob())),
        )
    }

    /** Collect coordinator UI events into [sink] and wait until [predicate] matches one. */
    private suspend fun CoroutineScope.collectInto(
        coord: RealtimeSessionCoordinator,
        sink: MutableList<SessionUiEvent>,
    ): Job = launch(start = CoroutineStart.UNDISPATCHED) {
        coord.events.collect { sink.add(it) }
    }

    private suspend fun awaitUntil(message: String, predicate: () -> Boolean) {
        try {
            withTimeout(3_000) {
                while (!predicate()) delay(10)
            }
        } catch (e: kotlinx.coroutines.TimeoutCancellationException) {
            fail("timed out waiting: $message")
        }
    }

    @Test
    fun start_fetchesSessionAndConnectsTransport() = runBlocking {
        val coord = coordinator()
        coord.start()

        assertTrue(coord.connected.value)
        assertEquals(
            listOf("ephemeral-token" to ninja.jeremy.liveninja.config.BackendConfig.OPENAI_REALTIME_CALLS_URL),
            transport.connectCalls,
        )
        coord.stop()
    }

    @Test
    fun start_emitsQuotaWarningWithoutTurningItIntoSessionError() = runBlocking {
        val coord = coordinator()
        coEvery { sessionApi.fetchSession() } returns RealtimeSession(
            clientSecret = "ephemeral-token",
            expiresAt = null,
            model = "gpt-realtime",
            voice = "cedar",
            sessionId = "rs-warning",
            quotaWarning = "  OpenAI budget warning: estimated \$19.25 remaining this month.  ",
        )
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)

        coord.start()

        awaitUntil("nonfatal quota warning") {
            seen.any { it is SessionUiEvent.SessionWarning }
        }
        assertTrue(coord.connected.value)
        assertEquals(
            "OpenAI budget warning: estimated \$19.25 remaining this month.",
            seen.filterIsInstance<SessionUiEvent.SessionWarning>().single().message,
        )
        assertTrue(seen.none { it is SessionUiEvent.SessionError })

        coord.stop()
        job.cancel()
    }

    @Test
    fun deviceSessionActionWaitsForAcknowledgementResponseToFinish() = runBlocking {
        val coord = coordinator()
        coord.start()

        transport.serverEvent(
            RealtimeEvent.FunctionCall(
                callId = "new-conversation-call",
                name = DeviceSessionTool.NAME_START_NEW_CONVERSATION,
                argumentsJson = "{}",
            ),
        )
        awaitUntil("device tool result and acknowledgement request") {
            transport.sentEvents.any { it.optString("type") == "response.create" }
        }

        // This ends the response that issued the function call. Acting here
        // would tear down the session before the assistant can acknowledge it.
        transport.serverEvent(RealtimeEvent.ResponseDone("calling-response"))
        delay(50)
        assertEquals(0, transport.disconnects)
        assertEquals(1, transport.connectCalls.size)

        transport.serverEvent(RealtimeEvent.ResponseStarted("ack-response"))
        transport.serverEvent(RealtimeEvent.ResponseDone("ack-response"))
        awaitUntil("fresh session after spoken acknowledgement") {
            transport.disconnects == 1 && transport.connectCalls.size == 2
        }

        coord.stop()
    }

    @Test
    fun deviceActionState_rejectsStaleWritesAndClearsAcknowledgedActionsOnGenerationAdvance() {
        val state = DeviceActionSessionState()
        state.advanceGeneration()
        val staleGeneration = state.currentGeneration()

        state.advanceGeneration()
        val currentGeneration = state.currentGeneration()
        assertTrue(
            state.setPending(
                DeviceSessionTool.STOP_LISTENING,
                currentGeneration,
            ),
        )
        assertFalse(
            state.setPending(
                DeviceSessionTool.START_NEW_CONVERSATION,
                staleGeneration,
            ),
        )

        state.markAcknowledgementStarted()
        val pending = state.takeAcknowledged()
        assertEquals(DeviceSessionTool.STOP_LISTENING, pending?.action)
        assertEquals(currentGeneration, pending?.generation)
        assertNull(state.takeAcknowledged())

        assertTrue(
            state.setPending(
                DeviceSessionTool.START_NEW_CONVERSATION,
                currentGeneration,
            ),
        )
        state.markAcknowledgementStarted()
        state.advanceGeneration()
        assertNull(state.takeAcknowledged())
    }

    @Test
    fun start_geminiDirect_routesToGeminiTransportAndPrimes() = runBlocking {
        val endpoint = "wss://generativelanguage.googleapis.com/ws/" +
            "google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained"
        coEvery { sessionApi.fetchSession() } returns RealtimeSession(
            mode = RealtimeSession.MODE_GEMINI_DIRECT,
            clientSecret = "",
            expiresAt = null,
            model = "gemini-3.1-flash-live-preview",
            voice = "Kore",
            sessionId = "rs-2",
            quotaWarning = null,
            geminiEndpoint = endpoint,
            accessToken = GeminiAccessToken(
                value = "auth_tokens/abc123",
                expiresAt = "2026-07-19T12:30:00Z",
                newSessionExpiresAt = "2026-07-19T12:02:00Z",
            ),
            sessionConfig = JSONObject().put("model", "models/gemini-3.1-flash-live-preview"),
        )
        val coord = RealtimeSessionCoordinator(
            mockk<android.content.Context>(relaxed = true),
            transport, novaTransport, geminiTransport, sessionApi, toolRouter, deviceVolumeTool,
            deviceCameraTool,
            TranscriptStore(), TranscriptUploader(NoopTranscriptSink, CoroutineScope(SupervisorJob())),
        )

        coord.start()

        assertTrue(coord.connected.value)
        // The Gemini transport gets (accessToken.value, geminiEndpoint) and
        // the full bootstrap via prime(); the other transports stay idle.
        assertEquals(listOf("auth_tokens/abc123" to endpoint), geminiTransport.connectCalls)
        assertEquals(1, geminiTransport.primedSessions.size)
        assertEquals(
            RealtimeSession.MODE_GEMINI_DIRECT,
            geminiTransport.primedSessions.single().mode,
        )
        assertTrue(transport.connectCalls.isEmpty())
        assertTrue(novaTransport.connectCalls.isEmpty())
        coord.stop()
    }

    @Test
    fun geminiUsage_updatesBadgeAndFinalTranscriptCost() = runBlocking {
        val rates = RealtimeRates(
            textInPer1M = 0.75,
            textOutPer1M = 4.50,
            audioInPer1M = 3.00,
            audioOutPer1M = 12.00,
            cachedTextInPer1M = 0.75,
            cachedAudioInPer1M = 3.00,
        )
        coEvery { sessionApi.fetchSession() } returns RealtimeSession(
            mode = RealtimeSession.MODE_GEMINI_DIRECT,
            clientSecret = "",
            expiresAt = null,
            model = "gemini-3.1-flash-live-preview",
            voice = "Kore",
            sessionId = "rs-gemini-cost",
            quotaWarning = null,
            geminiEndpoint = "wss://example/gemini",
            accessToken = GeminiAccessToken("auth_tokens/abc", null, null),
            sessionConfig = JSONObject().put("model", "models/gemini"),
            rates = rates,
        )
        val transcriptSink = CapturingTranscriptSink()
        val coord = RealtimeSessionCoordinator(
            mockk<android.content.Context>(relaxed = true),
            transport, novaTransport, geminiTransport, sessionApi, toolRouter, deviceVolumeTool,
            deviceCameraTool,
            TranscriptStore(), TranscriptUploader(transcriptSink, this),
        )
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)
        coord.start()

        val usage = GeminiUsageNormalizer.normalize(
            JSONObject(
                """
                {
                  "totalTokenCount": 4000000,
                  "promptTokensDetails": [
                    {"modality":"TEXT","tokenCount":1000000},
                    {"modality":"AUDIO","tokenCount":1000000}
                  ],
                  "responseTokensDetails": [
                    {"modality":"TEXT","tokenCount":1000000},
                    {"modality":"AUDIO","tokenCount":1000000}
                  ]
                }
                """.trimIndent(),
            ),
        )
        geminiTransport.serverEvent(RealtimeEvent.Usage(usage))

        awaitUntil("Gemini cost badge update") {
            seen.any { it is SessionUiEvent.CostUpdated }
        }
        val badge = seen.filterIsInstance<SessionUiEvent.CostUpdated>().single()
        assertEquals(20.25, badge.usd, 1e-9)
        assertEquals(2_000_000, badge.textTokens)
        assertEquals(2_000_000, badge.audioTokens)

        coord.stop()
        awaitUntil("final transcript cost payload") {
            transcriptSink.requests.any { it.final }
        }
        val persisted = transcriptSink.requests.single { it.final }.cost!!
        assertEquals(20.25, persisted.usd, 1e-9)
        assertEquals(2_000_000, persisted.textTokens)
        assertEquals(2_000_000, persisted.audioTokens)
        job.cancel()
    }

    @Test
    fun start_openaiDirect_prewarmsWebRtcAndKeepsPreparedBootstrap() = runBlocking {
        // Latency parallelization (02-voice §D.2): the openai-direct path speculatively
        // prepares the WebRTC transport and does NOT abort it — connect() joins the offer.
        val coord = coordinator()
        coord.start()

        assertEquals(1, transport.prepareCalls)
        assertEquals(0, transport.abortPrepareCalls)
        assertEquals(1, transport.connectCalls.size)
        coord.stop()
    }

    @Test
    fun start_geminiDirect_abortsSpeculativeWebRtcBootstrap() = runBlocking {
        coEvery { sessionApi.fetchSession() } returns RealtimeSession(
            mode = RealtimeSession.MODE_GEMINI_DIRECT,
            clientSecret = "",
            expiresAt = null,
            model = "gemini-3.1-flash-live-preview",
            voice = "Kore",
            sessionId = "rs-2",
            quotaWarning = null,
            geminiEndpoint = "wss://example/gemini",
            accessToken = GeminiAccessToken("auth_tokens/abc", null, null),
            sessionConfig = JSONObject().put("model", "models/gemini"),
        )
        val coord = RealtimeSessionCoordinator(
            mockk<android.content.Context>(relaxed = true),
            transport, novaTransport, geminiTransport, sessionApi, toolRouter, deviceVolumeTool,
            deviceCameraTool,
            TranscriptStore(), TranscriptUploader(NoopTranscriptSink, CoroutineScope(SupervisorJob())),
        )

        coord.start()

        // WebRTC was speculatively prepared, then discarded when the session resolved to Gemini.
        assertEquals(1, transport.prepareCalls)
        assertEquals(1, transport.abortPrepareCalls)
        assertTrue(transport.connectCalls.isEmpty())
        coord.stop()
    }

    @Test
    fun fetchFailure_abortsSpeculativeBootstrapAndPropagates() = runBlocking {
        // A session-fetch failure must abort the speculative WebRTC bootstrap and
        // surface the identical error (02-voice §D.2, failure-path parity).
        coEvery { sessionApi.fetchSession() } throws IOException("session mint failed")
        val coord = RealtimeSessionCoordinator(
            mockk<android.content.Context>(relaxed = true),
            transport, novaTransport, geminiTransport, sessionApi, toolRouter, deviceVolumeTool,
            deviceCameraTool,
            TranscriptStore(), TranscriptUploader(NoopTranscriptSink, CoroutineScope(SupervisorJob())),
        )
        try {
            coord.start()
            fail("expected fetch failure to propagate")
        } catch (e: IOException) {
            // expected
        }
        assertEquals(1, transport.prepareCalls)
        assertEquals(1, transport.abortPrepareCalls)
        assertTrue(transport.connectCalls.isEmpty())
        assertFalse(coord.connected.value)
    }

    @Test
    fun start_isIdempotentWhileConnected() = runBlocking {
        val coord = coordinator()
        coord.start()
        coord.start() // second call must not renegotiate

        assertEquals(1, transport.connectCalls.size)
        coord.stop()
    }

    @Test
    fun connectFailure_propagatesAndStaysDisconnected() = runBlocking {
        transport.failConnect = true
        val coord = coordinator()
        try {
            coord.start()
            fail("expected connect failure to propagate")
        } catch (e: IOException) {
            // expected
        }
        assertFalse(coord.connected.value)
    }

    @Test
    fun transportFailed_flipsDisconnectedAndEmitsSessionError() = runBlocking {
        val coord = coordinator()
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)

        coord.start()
        transport.driveState(TransportState.FAILED)

        awaitUntil("SessionError after FAILED") {
            seen.any { it is SessionUiEvent.SessionError }
        }
        awaitUntil("connected=false after FAILED") { !coord.connected.value }
        job.cancel()
    }

    @Test
    fun deliberateStop_disconnectsWithoutError() = runBlocking {
        val coord = coordinator()
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)

        coord.start()
        coord.stop()
        delay(100) // give any (wrong) error emission a chance to surface

        assertEquals(1, transport.disconnects)
        assertFalse(coord.connected.value)
        assertTrue(seen.none { it is SessionUiEvent.SessionError })
        job.cancel()
    }

    @Test
    fun transcript_deltasStream_thenFinalEmitsOnlyRemainder() = runBlocking {
        val coord = coordinator()
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)
        coord.start()

        transport.serverEvent(RealtimeEvent.UserTranscriptDelta("item1", "hello "))
        transport.serverEvent(RealtimeEvent.UserTranscriptDelta("item1", "wor"))
        transport.serverEvent(RealtimeEvent.UserTranscriptCompleted("item1", "hello world"))

        awaitUntil("final done delta") {
            seen.filterIsInstance<SessionUiEvent.TranscriptDelta>().any { it.done }
        }
        val deltas = seen.filterIsInstance<SessionUiEvent.TranscriptDelta>()
        assertEquals(listOf("hello ", "wor", "ld"), deltas.map { it.textDelta })
        assertEquals(TranscriptRole.USER, deltas.last().role)
        assertTrue(deltas.last().done)
        // Reassembled text has no duplication.
        assertEquals("hello world", deltas.joinToString("") { it.textDelta })
        coord.stop()
        job.cancel()
    }

    @Test
    fun functionCall_roundTripsOutputThenResponseCreate() = runBlocking {
        coEvery { toolRouter.invoke(any()) } returns """{"ok":true,"output":{"sum":42}}"""
        val coord = coordinator()
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)
        coord.start()

        transport.serverEvent(
            RealtimeEvent.FunctionCall(callId = "call-1", name = "calc", argumentsJson = """{"a":40,"b":2}"""),
        )

        awaitUntil("tool chip event") { seen.any { it is SessionUiEvent.ToolCall } }
        awaitUntil("both client events sent") { transport.sentEvents.size >= 2 }

        val first = transport.sentEvents[0]
        assertEquals("conversation.item.create", first.getString("type"))
        val item = first.getJSONObject("item")
        assertEquals("function_call_output", item.getString("type"))
        assertEquals("call-1", item.getString("call_id"))
        assertEquals("response.create", transport.sentEvents[1].getString("type"))

        val chip = seen.filterIsInstance<SessionUiEvent.ToolCall>().first()
        assertEquals("calc", chip.name)
        assertEquals("completed", chip.summary)
        coord.stop()
        job.cancel()
    }

    @Test
    fun setVolume_isExecutedLocallyAndNeverReachesBackendRouter() = runBlocking {
        val localOutput =
            """{"tool":"set_volume","callId":"volume-1","ok":true,"output":{"stream":"media","level":60}}"""
        val coord = coordinator()
        every {
            deviceVolumeTool.execute(
                "volume-1",
                """{"action":"set","level":60}""",
            )
        } returns localOutput
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)
        coord.start()

        transport.serverEvent(
            RealtimeEvent.FunctionCall(
                callId = "volume-1",
                name = DEVICE_VOLUME_TOOL_NAME,
                argumentsJson = """{"action":"set","level":60}""",
            ),
        )

        awaitUntil("device-local volume output") { transport.sentEvents.size >= 2 }
        verify(exactly = 1) {
            deviceVolumeTool.execute(
                "volume-1",
                """{"action":"set","level":60}""",
            )
        }
        coVerify(exactly = 0) { toolRouter.invoke(any()) }
        val output = transport.sentEvents.first()
            .getJSONObject("item")
            .getString("output")
        assertEquals(localOutput, output)
        assertEquals("response.create", transport.sentEvents[1].getString("type"))

        coord.stop()
        job.cancel()
    }

    @Test
    fun takePhoto_isExecutedLocallyAndNeverReachesBackendRouter() = runBlocking {
        val localOutput =
            """{"tool":"take_photo","callId":"photo-1","ok":true,"output":{"deliverableId":"d-1","name":"photo.jpg"}}"""
        val coord = coordinator()
        coEvery {
            deviceCameraTool.execute(
                TAKE_PHOTO_TOOL_NAME,
                "photo-1",
                """{"camera":"front"}""",
            )
        } returns localOutput
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)
        coord.start()

        transport.serverEvent(
            RealtimeEvent.FunctionCall(
                callId = "photo-1",
                name = TAKE_PHOTO_TOOL_NAME,
                argumentsJson = """{"camera":"front"}""",
            ),
        )

        awaitUntil("device-local camera output") { transport.sentEvents.size >= 2 }
        coVerify(exactly = 1) {
            deviceCameraTool.execute(
                TAKE_PHOTO_TOOL_NAME,
                "photo-1",
                """{"camera":"front"}""",
            )
        }
        coVerify(exactly = 0) { toolRouter.invoke(any()) }
        val output = transport.sentEvents.first()
            .getJSONObject("item")
            .getString("output")
        assertEquals(localOutput, output)
        assertEquals("response.create", transport.sentEvents[1].getString("type"))

        coord.stop()
        job.cancel()
    }

    @Test
    fun duplicateInFlightCameraCall_reusesOneCaptureResult() = runBlocking {
        val localOutput =
            """{"tool":"take_photo","callId":"photo-retry","ok":true,"output":{"deliverableId":"d-retry","name":"photo.jpg"}}"""
        val captureStarted = CompletableDeferred<Unit>()
        val captureResult = CompletableDeferred<String>()
        coEvery {
            deviceCameraTool.execute(
                TAKE_PHOTO_TOOL_NAME,
                "photo-retry",
                """{"camera":"back"}""",
            )
        } coAnswers {
            captureStarted.complete(Unit)
            captureResult.await()
        }
        val coord = coordinator()
        coord.start()
        val duplicate = RealtimeEvent.FunctionCall(
            callId = "photo-retry",
            name = TAKE_PHOTO_TOOL_NAME,
            argumentsJson = """{"camera":"back"}""",
        )

        transport.serverEvent(duplicate)
        withTimeout(2_000) { captureStarted.await() }
        transport.serverEvent(duplicate)
        delay(100)
        coVerify(exactly = 1) {
            deviceCameraTool.execute(
                TAKE_PHOTO_TOOL_NAME,
                "photo-retry",
                """{"camera":"back"}""",
            )
        }

        captureResult.complete(localOutput)
        awaitUntil("one camera result is returned") {
            transport.sentEvents.count {
                it.optString("type") == "conversation.item.create"
            } == 1
        }
        delay(100)
        val outputs = transport.sentEvents
            .filter { it.optString("type") == "conversation.item.create" }
            .map { it.getJSONObject("item").getString("output") }
        assertEquals(listOf(localOutput), outputs)
        assertEquals(1, transport.sentEvents.count { it.optString("type") == "response.create" })
        coVerify(exactly = 0) { toolRouter.invoke(any()) }

        coord.stop()
    }

    @Test
    fun duplicateCompletedRelativeVolumeCall_reusesResultOnlyWithinSession() = runBlocking {
        val localOutput =
            """{"tool":"set_volume","callId":"volume-retry","ok":true,"output":{"action":"increase","level":60}}"""
        every {
            deviceVolumeTool.execute(
                "volume-retry",
                """{"action":"increase"}""",
            )
        } returns localOutput
        val coord = coordinator()
        coord.start()
        val duplicate = RealtimeEvent.FunctionCall(
            callId = "volume-retry",
            name = DEVICE_VOLUME_TOOL_NAME,
            argumentsJson = """{"action":"increase"}""",
        )

        transport.serverEvent(duplicate)
        awaitUntil("first relative-volume result") {
            transport.sentEvents.count {
                it.optString("type") == "conversation.item.create"
            } == 1
        }
        transport.serverEvent(duplicate)
        delay(100)

        verify(exactly = 1) {
            deviceVolumeTool.execute(
                "volume-retry",
                """{"action":"increase"}""",
            )
        }
        val outputs = transport.sentEvents
            .filter { it.optString("type") == "conversation.item.create" }
            .map { it.getJSONObject("item").getString("output") }
        assertEquals(listOf(localOutput), outputs)
        assertEquals(1, transport.sentEvents.count { it.optString("type") == "response.create" })
        coVerify(exactly = 0) { toolRouter.invoke(any()) }

        coord.stop()
        transport.sentEvents.clear()
        coord.start()
        transport.serverEvent(duplicate)
        awaitUntil("same call id executes again in the next session") {
            transport.sentEvents.any {
                it.optString("type") == "conversation.item.create"
            }
        }
        verify(exactly = 2) {
            deviceVolumeTool.execute(
                "volume-retry",
                """{"action":"increase"}""",
            )
        }
        coord.stop()
    }

    @Test
    fun assistantSpeaking_followsAudioStartStop() = runBlocking {
        val coord = coordinator()
        val seen = mutableListOf<SessionUiEvent>()
        val job = collectInto(coord, seen)
        coord.start()

        transport.serverEvent(RealtimeEvent.AssistantAudioStarted)
        transport.serverEvent(RealtimeEvent.AssistantAudioStopped)

        awaitUntil("speaking start+stop") {
            seen.filterIsInstance<SessionUiEvent.AssistantSpeaking>().map { it.speaking } == listOf(true, false)
        }
        coord.stop()
        job.cancel()
    }

    /**
     * The coordinator tests are about event mapping, not transcript upload; this keeps the
     * uploader inert so they don't need a network fake.
     */
    private object NoopTranscriptSink : TranscriptSink {
        override suspend fun upload(body: ninja.jeremy.liveninja.net.TranscriptUploadRequest) = Unit
    }

    private class CapturingTranscriptSink : TranscriptSink {
        val requests = mutableListOf<ninja.jeremy.liveninja.net.TranscriptUploadRequest>()

        override suspend fun upload(body: ninja.jeremy.liveninja.net.TranscriptUploadRequest) {
            requests += body
        }
    }
}
