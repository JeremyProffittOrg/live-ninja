package ninja.jeremy.liveninja.realtime

import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
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
import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import ninja.jeremy.liveninja.wake.WakeWordService
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import org.json.JSONObject

/**
 * The realtime workstream's implementation of the UI seam
 * [RealtimeSessionController] (ui/state/UiSeams.kt): one live GPT-Realtime
 * session — bootstrap via `GET /api/v1/realtime/session`, WebRTC media via
 * [RealtimeTransport], DataChannel events mapped to [SessionUiEvent]s, and
 * `function_call` round-trips through [ToolCallRouter], except server-declared
 * device-local tools handled in this process
 * (`POST /api/v1/tools/invoke` or local action → `function_call_output` →
 * `response.create`).
 *
 * Transport-level barge-in (response.cancel + 40 ms fade + jitter flush on
 * `input_audio_buffer.speech_started`) lives in [WebRtcTransport]; this class
 * only translates events for the UI.
 */
@Singleton
class RealtimeSessionCoordinator @Inject constructor(
    @ApplicationContext private val appContext: Context,
    @OpenAiRealtimeTransport private val webRtcTransport: RealtimeTransport,
    @NovaSonicTransport private val novaBridgeTransport: RealtimeTransport,
    @GeminiTransport private val geminiLiveTransport: RealtimeTransport,
    private val sessionApi: RealtimeSessionApi,
    private val toolRouter: ToolCallRouter,
    private val deviceVolumeTool: DeviceVolumeToolExecutor,
    private val deviceCameraTool: DeviceCameraToolExecutor,
    private val transcriptStore: TranscriptStore,
    private val transcriptUploader: TranscriptUploader,
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

    /** A device-local action held through the function-call response and its spoken follow-up. */
    private data class PendingDeviceAction(
        val action: DeviceSessionTool,
        val acknowledgementStarted: Boolean = false,
    )

    @Volatile
    private var pendingDeviceAction: PendingDeviceAction? = null
    private val lifecycleMutex = Mutex()

    private val _connected = MutableStateFlow(false)
    override val connected: StateFlow<Boolean> = _connected.asStateFlow()

    private val _events = MutableSharedFlow<SessionUiEvent>(extraBufferCapacity = 256)
    override val events: Flow<SessionUiEvent> = _events.asSharedFlow()

    private var eventsJob: Job? = null
    private var stateWatchJob: Job? = null

    /**
     * Live cost estimate for the session, plus the rates it is priced with.
     * Both are per-session: [sessionRates] is null until a bootstrap that
     * carried rates, which is what keeps the badge hidden on nova-bridge.
     */
    private val costTracker = SessionCostTracker()
    private var sessionRates: RealtimeRates? = null

    /**
     * Characters already emitted per transcript item, so the final
     * `...completed`/`.done` full-text event can be emitted as a remainder
     * delta without duplicating streamed text.
     */
    private val emittedChars = HashMap<String, Int>()

    override suspend fun start() {
        lifecycleMutex.withLock {
            if (_connected.value) return

            // Fresh conversation: clear the process-wide transcript so a UI
            // attaching mid-session (screen-on) renders only this session.
            pendingDeviceAction = null
            transcriptStore.clear()

            // Latency parallelization (02-voice §D.2): speculatively bootstrap
            // the WebRTC transport — factory + peer connection + offer + ICE
            // gathering, none of which needs the credential — concurrently with
            // the session fetch. The dominant openai-direct path joins the
            // prepared offer at the SDP POST inside connect(); the nova/gemini
            // paths discard it via abortPrepare(). A fetch failure aborts it too
            // so the error surface is identical to the serial path (the
            // speculative bootstrap's own failure is swallowed here and only
            // surfaces through connect() when the session actually resolves to
            // WebRTC).
            webRtcTransport.prepare()

            val session = try {
                sessionApi.fetchSession()
            } catch (t: Throwable) {
                webRtcTransport.abortPrepare()
                throw t
            }

            // Reset before the first turn can arrive: a stale total from the
            // previous session would otherwise be attributed to this one.
            costTracker.reset()
            sessionRates = session.rates

            // Start shipping turns to POST /api/v1/transcript for this session
            // (WS-5 M21.1). Must come after the fetch: the broker-issued
            // sessionId is what the server keys LOG#/CONV rows against.
            transcriptUploader.begin(
                session.sessionId,
                TranscriptUploader.engineForMode(session.mode),
            )

            // Route by the resolved engine pin. connect()'s two string params
            // are reused engine-agnostically: (credential, endpointUrl).
            val (credential, endpointUrl) = when (session.mode) {
                RealtimeSession.MODE_NOVA_BRIDGE -> {
                    webRtcTransport.abortPrepare()
                    transport = novaBridgeTransport
                    session.bridgeToken.orEmpty() to session.wsUrl.orEmpty()
                }

                RealtimeSession.MODE_GEMINI_DIRECT -> {
                    webRtcTransport.abortPrepare()
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
            session.quotaWarning
                ?.trim()
                ?.takeIf { it.isNotEmpty() }
                ?.let { emit(SessionUiEvent.SessionWarning(it)) }

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
            pendingDeviceAction = null
            transport.disconnect()
            emittedChars.clear()
            // Session-end seam: the final:true flush is what makes the backend
            // run topics-extract and persist the CONV record for History.
            // Carry the session's accrued estimate on the same flush — it is what
            // puts a cost on the CONV row, so it has to ride the final post
            // rather than a follow-up call that a dying process may never make.
            transcriptUploader.finish(costTracker.cost)
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

            is RealtimeEvent.AssistantAudioStopped ->
                emit(SessionUiEvent.AssistantSpeaking(speaking = false))

            is RealtimeEvent.ResponseDone -> {
                emit(SessionUiEvent.AssistantSpeaking(speaking = false))
                pendingDeviceAction
                    ?.takeIf { it.acknowledgementStarted }
                    ?.let { pending ->
                    pendingDeviceAction = null
                    scope.launch { runDeviceAction(pending.action) }
                }
                // response.done is the only carrier of per-turn token counts.
                costTracker.add(event.usage, sessionRates)?.let { cost ->
                    emit(
                        SessionUiEvent.CostUpdated(
                            usd = cost.usd,
                            textTokens = cost.textTokens,
                            audioTokens = cost.audioTokens,
                        ),
                    )
                }
            }

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
                LNLog.w(LogCategory.REALTIME, TAG, "realtime server error ${event.code}: ${event.message}")

            is RealtimeEvent.ResponseStarted -> {
                // response.created after a device function result identifies
                // the acknowledgement response. The earlier response.done is
                // the function-calling turn and must not trigger the action.
                pendingDeviceAction = pendingDeviceAction?.copy(
                    acknowledgementStarted = true,
                )
            }

            is RealtimeEvent.SessionCreated,
            is RealtimeEvent.SessionUpdated,
            is RealtimeEvent.SpeechStopped,
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
            // Device-local tools never reach the backend router. Session actions
            // are deferred until the assistant has spoken its confirmation;
            // volume/camera actions happen immediately and return their actual
            // device/storage result in the same Result shape as a backend tool.
            val deviceTool = DeviceSessionTool.forName(call.name)
            val output = when {
                deviceTool != null -> {
                    pendingDeviceAction = PendingDeviceAction(deviceTool)
                    deviceToolOutput(deviceTool, call.callId)
                }

                call.name == DEVICE_VOLUME_TOOL_NAME ->
                    deviceVolumeTool.execute(call.callId, call.argumentsJson)

                call.name == TAKE_PHOTO_TOOL_NAME || call.name == RECORD_VIDEO_TOOL_NAME ->
                    deviceCameraTool.execute(call.name, call.callId, call.argumentsJson)

                else -> toolRouter.invoke(call)
            }
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
            transcriptStore.addToolChip(itemId = call.callId, name = call.name, summary = summary)
            emit(SessionUiEvent.ToolCall(itemId = call.callId, name = call.name, summary = summary))
        }
    }

    /**
     * Perform a deferred device-local tool action, after the assistant's spoken
     * confirmation has completed.
     */
    private suspend fun runDeviceAction(action: DeviceSessionTool) {
        when (action) {
            DeviceSessionTool.STOP_LISTENING -> {
                // stop() ends the live session too, and clears the persisted
                // serviceEnabled intent so a sticky restart cannot resurrect it.
                runCatching { stop() }
                WakeWordService.stop(appContext)
            }

            DeviceSessionTool.START_NEW_CONVERSATION -> {
                // A full stop/start, not a transcript clear: the session id is what
                // the backend keys LOG#/CONV rows against, so only a genuinely new
                // session gives the new conversation its own History row.
                runCatching { stop() }
                runCatching { start() }
            }
        }
    }

    private fun emitDelta(itemId: String, role: TranscriptRole, delta: String, done: Boolean) {
        if (itemId.isEmpty() || (delta.isEmpty() && !done)) return
        emittedChars[keyFor(itemId, role)] = (emittedChars[keyFor(itemId, role)] ?: 0) + delta.length
        transcriptStore.appendDelta(itemId, role, delta, done)
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
        transcriptStore.appendDelta(itemId, role, remainder, done = true)
        emit(SessionUiEvent.TranscriptDelta(itemId, role, remainder, done = true))
        // Upload the whole finished turn (not the remainder) so the stored row
        // matches what the user actually said/heard.
        transcriptUploader.record(role, fullText)
    }

    private fun keyFor(itemId: String, role: TranscriptRole) = "$itemId/$role"

    private fun emit(event: SessionUiEvent) {
        if (!_events.tryEmit(event)) {
            LNLog.w(LogCategory.REALTIME, TAG, "session event buffer full; dropped ${event::class.simpleName}")
        }
    }

    private companion object {
        const val TAG = "RealtimeSessionCoord"
    }
}
