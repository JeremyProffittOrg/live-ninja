package ninja.jeremy.liveninja.realtime

import io.mockk.coEvery
import io.mockk.mockk
import java.io.IOException
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
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

    private fun coordinator(): RealtimeSessionCoordinator {
        coEvery { sessionApi.fetchSession() } returns RealtimeSession(
            clientSecret = "ephemeral-token",
            expiresAt = null,
            model = "gpt-realtime",
            voice = "cedar",
            sessionId = "rs-1",
            quotaWarning = null,
        )
        return RealtimeSessionCoordinator(transport, novaTransport, geminiTransport, sessionApi, toolRouter, TranscriptStore())
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
    fun start_geminiDirect_routesToGeminiTransportAndPrimes() = runBlocking {
        val endpoint = "wss://generativelanguage.googleapis.com/ws/" +
            "google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContentConstrained"
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
        val coord = RealtimeSessionCoordinator(transport, novaTransport, geminiTransport, sessionApi, toolRouter, TranscriptStore())

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
        val coord = RealtimeSessionCoordinator(transport, novaTransport, geminiTransport, sessionApi, toolRouter, TranscriptStore())

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
        val coord = RealtimeSessionCoordinator(transport, novaTransport, geminiTransport, sessionApi, toolRouter, TranscriptStore())
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
}
