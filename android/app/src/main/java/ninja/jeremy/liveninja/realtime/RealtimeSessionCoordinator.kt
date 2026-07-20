package ninja.jeremy.liveninja.realtime

import android.util.Log
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import ninja.jeremy.liveninja.ui.state.RealtimeSessionController
import ninja.jeremy.liveninja.ui.state.SessionUiEvent
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import org.json.JSONObject

/**
 * The realtime workstream's implementation of the UI seam
 * [RealtimeSessionController] (ui/state/UiSeams.kt): one live GPT-Realtime
 * session — bootstrap via `GET /api/v1/realtime/session`, WebRTC media via
 * [RealtimeTransport], DataChannel events mapped to [SessionUiEvent]s, and
 * `function_call` round-trips through [ToolCallRouter]
 * (`POST /api/v1/tools/invoke` → `function_call_output` → `response.create`).
 *
 * Transport-level barge-in (response.cancel + 40 ms fade + jitter flush on
 * `input_audio_buffer.speech_started`) lives in [WebRtcTransport]; this class
 * only translates events for the UI.
 */
@Singleton
class RealtimeSessionCoordinator @Inject constructor(
    @OpenAiRealtimeTransport private val webRtcTransport: RealtimeTransport,
    @NovaSonicTransport private val novaBridgeTransport: RealtimeTransport,
    @GeminiTransport private val geminiLiveTransport: RealtimeTransport,
    private val sessionApi: RealtimeSessionApi,
    private val toolRouter: ToolCallRouter,
) : RealtimeSessionController {

    /**
     * The transport for the *current* session, selected per the resolved
     * `voiceEngine` pin (FR-VE-03): WebRTC-to-OpenAI for `openai-direct`, the
     * Nova Sonic bridge for `nova-bridge`, client-direct Gemini Live for
     * `gemini-direct` (M13). All satisfy [RealtimeTransport], so every method
     * below is engine-agnostic.
     */
    @Volatile
    private var transport: RealtimeTransport = webRtcTransport

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val lifecycleMutex = Mutex()

    private val _connected = MutableStateFlow(false)
    override val connected: StateFlow<Boolean> = _connected.asStateFlow()

    private val _events = MutableSharedFlow<SessionUiEvent>(extraBufferCapacity = 256)
    override val events: Flow<SessionUiEvent> = _events.asSharedFlow()

    private var eventsJob: Job? = null
    private var stateWatchJob: Job? = null

    /**
     * Characters already emitted per transcript item, so the final
     * `...completed`/`.done` full-text event can be emitted as a remainder
     * delta without duplicating streamed text.
     */
    private val emittedChars = HashMap<String, Int>()

    override suspend fun start() {
        lifecycleMutex.withLock {
            if (_connected.value) return

            val session = sessionApi.fetchSession()

            // Route by the resolved engine pin. connect()'s two string params
            // are reused engine-agnostically: (credential, endpointUrl).
            val (credential, endpointUrl) = when (session.mode) {
                RealtimeSession.MODE_NOVA_BRIDGE -> {
                    transport = novaBridgeTransport
                    session.bridgeToken.orEmpty() to session.wsUrl.orEmpty()
                }

                RealtimeSession.MODE_GEMINI_DIRECT -> {
                    transport = geminiLiveTransport
                    session.accessToken?.value.orEmpty() to session.geminiEndpoint.orEmpty()
                }

                else -> {
                    transport = webRtcTransport
                    session.clientSecret to session.callsUrl
                }
            }
            // Engines needing more than (credential, endpoint) — e.g. the
            // Gemini setup frame — take it from the full bootstrap (no-op
            // for the others).
            transport.prime(session)

            emittedChars.clear()
            eventsJob?.cancel()
            eventsJob = scope.launch { transport.events.collect(::onTransportEvent) }
            try {
                transport.connect(credential, endpointUrl)
            } catch (t: Throwable) {
                eventsJob?.cancel()
                eventsJob = null
                throw t
            }
            _connected.value = true

            stateWatchJob?.cancel()
            stateWatchJob = scope.launch {
                transport.state.collect { state ->
                    if (state == TransportState.FAILED && _connected.value) {
                        _connected.value = false
                        _events.tryEmit(
                            SessionUiEvent.SessionError("The voice connection dropped."),
                        )
                        eventsJob?.cancel()
                    } else if (state == TransportState.CLOSED) {
                        _connected.value = false
                    }
                }
            }
        }
    }

    override suspend fun stop() {
        lifecycleMutex.withLock {
            // Stop watching first so a deliberate teardown never reads as an error.
            stateWatchJob?.cancel()
            stateWatchJob = null
            eventsJob?.cancel()
            eventsJob = null
            _connected.value = false
            transport.disconnect()
            emittedChars.clear()
        }
    }

    override fun setMicMuted(muted: Boolean) {
        transport.setMicMuted(muted)
    }

    override fun interruptAssistant() {
        transport.stopPlayback()
    }

    // ---- event mapping ----

    private fun onTransportEvent(event: RealtimeEvent) {
        when (event) {
            is RealtimeEvent.SpeechStarted ->
                emit(SessionUiEvent.UserSpeechStarted)

            is RealtimeEvent.AssistantAudioStarted ->
                emit(SessionUiEvent.AssistantSpeaking(speaking = true))

            is RealtimeEvent.AssistantAudioStopped,
            is RealtimeEvent.ResponseDone,
            ->
                emit(SessionUiEvent.AssistantSpeaking(speaking = false))

            is RealtimeEvent.UserTranscriptDelta ->
                emitDelta(event.itemId, TranscriptRole.USER, event.delta, done = false)

            is RealtimeEvent.UserTranscriptCompleted ->
                emitFinal(event.itemId, TranscriptRole.USER, event.text)

            is RealtimeEvent.AssistantTranscriptDelta ->
                emitDelta(event.itemId, TranscriptRole.ASSISTANT, event.delta, done = false)

            is RealtimeEvent.AssistantTranscriptDone ->
                emitFinal(event.itemId, TranscriptRole.ASSISTANT, event.text)

            is RealtimeEvent.FunctionCall -> handleFunctionCall(event)

            is RealtimeEvent.ServerError ->
                // In-band server errors (e.g. a cancel racing a finished
                // response) are usually benign; a fatal one also drops the
                // peer connection, which the state watcher reports.
                Log.w(TAG, "realtime server error ${event.code}: ${event.message}")

            is RealtimeEvent.SessionCreated,
            is RealtimeEvent.SessionUpdated,
            is RealtimeEvent.SpeechStopped,
            is RealtimeEvent.ResponseStarted,
            is RealtimeEvent.Other,
            -> Unit
        }
    }

    /**
     * Tool round-trip (FR-V04): execute server-side, then hand the result
     * back to the model and ask it to continue the spoken response.
     */
    private fun handleFunctionCall(call: RealtimeEvent.FunctionCall) {
        scope.launch {
            val output = toolRouter.invoke(call)
            transport.sendEvent(
                JSONObject()
                    .put("type", "conversation.item.create")
                    .put(
                        "item",
                        JSONObject()
                            .put("type", "function_call_output")
                            .put("call_id", call.callId)
                            .put("output", output),
                    ),
            )
            transport.sendEvent(JSONObject().put("type", "response.create"))

            val summary = runCatching {
                val json = JSONObject(output)
                if (json.optBoolean("ok")) "completed" else
                    json.optJSONObject("error")?.optString("message").orEmpty().ifEmpty { "failed" }
            }.getOrDefault("completed")
            emit(SessionUiEvent.ToolCall(itemId = call.callId, name = call.name, summary = summary))
        }
    }

    private fun emitDelta(itemId: String, role: TranscriptRole, delta: String, done: Boolean) {
        if (itemId.isEmpty() || (delta.isEmpty() && !done)) return
        emittedChars[keyFor(itemId, role)] = (emittedChars[keyFor(itemId, role)] ?: 0) + delta.length
        emit(SessionUiEvent.TranscriptDelta(itemId, role, delta, done))
    }

    /**
     * Final full-text event: emit only the tail not already streamed as
     * deltas (covers both delta-then-done and completed-only transcription).
     */
    private fun emitFinal(itemId: String, role: TranscriptRole, fullText: String) {
        if (itemId.isEmpty()) return
        val sent = emittedChars.remove(keyFor(itemId, role)) ?: 0
        val remainder = if (fullText.length > sent) fullText.substring(sent) else ""
        emit(SessionUiEvent.TranscriptDelta(itemId, role, remainder, done = true))
    }

    private fun keyFor(itemId: String, role: TranscriptRole) = "$itemId/$role"

    private fun emit(event: SessionUiEvent) {
        if (!_events.tryEmit(event)) {
            Log.w(TAG, "session event buffer full; dropped ${event::class.simpleName}")
        }
    }

    private companion object {
        const val TAG = "RealtimeSessionCoord"
    }
}
