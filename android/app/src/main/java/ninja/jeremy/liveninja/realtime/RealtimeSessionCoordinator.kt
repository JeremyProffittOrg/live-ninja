package ninja.jeremy.liveninja.realtime

import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.cancelAndJoin
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
import ninja.jeremy.liveninja.ui.state.TranscriptRole
import org.json.JSONObject
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState

/**
 * Generation-bound state for a deferred device-session action.
 *
 * Tool calls run on worker coroutines while session start/stop and transport
 * events can arrive on other threads. Updating generation and pending action
 * under one monitor prevents a stale tool job from overwriting state installed
 * by a newer session.
 */
internal class DeviceActionSessionState {
    data class Pending(
        val action: DeviceSessionTool,
        val generation: Long,
        val acknowledgementStarted: Boolean = false,
    )

    private val lock = Any()
    private var generation = 0L
    private var pending: Pending? = null

    fun advanceGeneration() {
        synchronized(lock) {
            generation++
            pending = null
        }
    }

    fun currentGeneration(): Long = synchronized(lock) { generation }

    fun isCurrent(candidate: Long): Boolean =
        synchronized(lock) { candidate == generation }

    fun setPending(action: DeviceSessionTool, candidate: Long): Boolean =
        synchronized(lock) {
            if (candidate != generation) {
                false
            } else {
                pending = Pending(action, candidate)
                true
            }
        }

    fun markAcknowledgementStarted() {
        synchronized(lock) {
            val current = pending
            pending = if (current?.generation == generation) {
                current.copy(acknowledgementStarted = true)
            } else {
                null
            }
        }
    }

    fun takeAcknowledged(): Pending? =
        synchronized(lock) {
            val current = pending
            if (current?.generation == generation && current.acknowledgementStarted) {
                pending = null
                current
            } else {
                if (current?.generation != generation) pending = null
                null
            }
        }
}

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
    @ApplicationContext @Suppress("UnusedPrivateProperty") private val appContext: Context,
    @OpenAiRealtimeTransport private val webRtcTransport: RealtimeTransport,
    @NovaSonicTransport private val novaBridgeTransport: RealtimeTransport,
    @GeminiTransport private val geminiLiveTransport: RealtimeTransport,
    @VoiceLiveRealtimeTransport private val voiceLiveTransport: RealtimeTransport,
    private val sessionApi: RealtimeSessionApi,
    private val toolRouter: ToolCallRouter,
    private val deviceVolumeTool: DeviceVolumeToolExecutor,
    private val deviceCameraTool: DeviceCameraToolExecutor,
    private val transcriptStore: TranscriptStore,
    private val transcriptUploader: TranscriptUploader,
    private val auth: AuthRepository,
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

    // Tool executors and the transport event collector run on this scope. An
    // uncaught throwable here used to reach the default handler and kill the
    // process mid-conversation; log it and tell the UI instead.
    private val uncaught = CoroutineExceptionHandler { _, t ->
        LNLog.e(LogCategory.REALTIME, TAG, "uncaught error in session scope", t)
        emit(SessionUiEvent.SessionError("Something went wrong in the voice session."))
    }
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default + uncaught)
    private val deviceActionState = DeviceActionSessionState()
    private val lifecycleMutex = Mutex()
    private class SessionBinding(val authSessionId: String, val generation: Long) {
        @Volatile var valid = true
        var transport: RealtimeTransport? = null
        val tools = SupervisorJob()
    }
    @Volatile private var activeBinding: SessionBinding? = null

    private fun currentAuthSessionId(): String? =
        (auth.state.value as? AuthState.SignedIn)?.sessionId?.takeIf { it.isNotBlank() }

    private fun isCurrent(binding: SessionBinding): Boolean =
        binding.valid && activeBinding === binding &&
            binding.authSessionId == currentAuthSessionId()

    private fun requireCurrent(binding: SessionBinding) {
        if (!isCurrent(binding)) throw RealtimeSessionException(
            "session_changed", "Your sign-in session changed. Start a new conversation.", 401,
        )
    }

    private fun invalidate(binding: SessionBinding, clearTranscript: Boolean = true, cleanup: Boolean = true) {
        binding.valid = false
        binding.tools.cancel()
        if (cleanup && activeBinding === binding) {
            deviceActionState.advanceGeneration()
            transcriptUploader.discard()
            if (clearTranscript) transcriptStore.clear()
        }
    }

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
    private val localToolResults = LocalToolCallResults()

    /**
     * Characters already emitted per transcript item, so the final
     * `...completed`/`.done` full-text event can be emitted as a remainder
     * delta without duplicating streamed text.
     */
    private val emittedChars = HashMap<String, Int>()

    init {
        scope.launch(start = CoroutineStart.UNDISPATCHED) {
            var previousSessionId = currentAuthSessionId()
            auth.state.collect {
                val currentSessionId = currentAuthSessionId()
                val changed = currentSessionId != previousSessionId
                previousSessionId = currentSessionId
                val binding = activeBinding
                if (binding == null) {
                    if (changed) lifecycleMutex.withLock {
                        // start() may have established the new account while
                        // this observer waited. Never discard that new buffer.
                        if (activeBinding == null) {
                            transcriptUploader.discard()
                            transcriptStore.clear()
                        }
                    }
                    return@collect
                }
                if (isCurrent(binding)) return@collect
                // Invalidate before waiting for lifecycle cleanup. Every event
                // and tool continuation also checks auth.state synchronously.
                invalidate(binding, cleanup = false)
                lifecycleMutex.withLock {
                    if (activeBinding === binding) closeBinding(binding, upload = false)
                }
            }
        }
    }

    override suspend fun start() = startForSession()

    private suspend fun startForSession(expectedSessionId: String? = null) {
        lifecycleMutex.withLock {
            if (expectedSessionId != null && currentAuthSessionId() != expectedSessionId) return
            activeBinding?.let {
                if (_connected.value && isCurrent(it)) return
                closeBinding(it, upload = false)
            }
            val authSessionId = currentAuthSessionId() ?: throw RealtimeSessionException(
                "not_authenticated", "Sign in before starting a conversation.", 401,
            )

            // Fresh conversation: clear the process-wide transcript so a UI
            // attaching mid-session (screen-on) renders only this session.
            deviceActionState.advanceGeneration()
            val binding = SessionBinding(authSessionId, deviceActionState.currentGeneration())
            activeBinding = binding
            localToolResults.reset()
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
                sessionApi.fetchSession(binding.authSessionId).also { requireCurrent(binding) }
            } catch (t: Throwable) {
                webRtcTransport.abortPrepare()
                invalidate(binding)
                if (activeBinding === binding) activeBinding = null
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
                binding.authSessionId,
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

                RealtimeSession.MODE_VOICE_LIVE_DIRECT -> {
                    webRtcTransport.abortPrepare()
                    transport = voiceLiveTransport
                    session.accessToken?.value.orEmpty() to session.voiceLiveEndpoint.orEmpty()
                }

                else -> {
                    transport = webRtcTransport
                    session.clientSecret to session.callsUrl
                }
            }
            binding.transport = transport
            // Engines needing more than (credential, endpoint) — e.g. the
            // Gemini setup frame — take it from the full bootstrap (no-op
            // for the others).
            transport.prime(session)
            requireCurrent(binding)

            emittedChars.clear()
            // Join, not just cancel: a cancelled collector stays subscribed to
            // transport.events until it actually unwinds, and a hot emission in
            // that window lands in the dying subscriber's buffer and is never
            // replayed to the collector launched below — the new session's first
            // event would simply vanish. onTransportEvent is synchronous and the
            // only lifecycle-mutex users run in their own launched coroutines,
            // so joining under the lock cannot deadlock.
            eventsJob?.cancelAndJoin()
            val sessionTransport = transport
            eventsJob = scope.launch {
                sessionTransport.events.collect { event ->
                    if (isCurrent(binding)) onTransportEvent(event, binding)
                }
            }
            try {
                requireCurrent(binding)
                sessionTransport.connect(credential, endpointUrl)
                requireCurrent(binding)
            } catch (t: Throwable) {
                closeBinding(binding, upload = false)
                throw t
            }
            _connected.value = true
            session.quotaWarning
                ?.trim()
                ?.takeIf { it.isNotEmpty() }
                ?.let { emit(SessionUiEvent.SessionWarning(it)) }

            stateWatchJob?.cancel()
            stateWatchJob = scope.launch {
                sessionTransport.state.collect { state ->
                    if (state != TransportState.FAILED && state != TransportState.CLOSED) return@collect
                    if (!_connected.value) return@collect
                    // A transport that failed or was closed by the far end still
                    // holds the mic, the playback path and the communication
                    // audio mode until disconnect() runs, and its transcript is
                    // never finalised (no final:true flush, so no History row).
                    // Nothing else releases it: the orchestrator only reacts to
                    // `connected`, and the next start() replaces the transport's
                    // state without releasing the old session. Release under the
                    // lifecycle lock first, and flip `connected` last — that flip
                    // is what resumes the wake engine and lets a new session start.
                    lifecycleMutex.withLock {
                        if (!_connected.value || activeBinding !== binding) return@withLock
                        eventsJob?.cancelAndJoin()
                        eventsJob = null
                        try {
                            sessionTransport.disconnect()
                        } catch (e: CancellationException) {
                            throw e
                        } catch (e: Exception) {
                            LNLog.w(LogCategory.REALTIME, TAG, "disconnect after transport $state failed", e)
                        }
                        if (isCurrent(binding)) transcriptUploader.finish(costTracker.cost)
                        else transcriptUploader.discard()
                        invalidate(binding, clearTranscript = false)
                        activeBinding = null
                        _connected.value = false
                    }
                    if (state == TransportState.FAILED) {
                        _events.tryEmit(
                            SessionUiEvent.SessionError("The voice connection dropped."),
                        )
                    }
                }
            }
        }
    }

    override suspend fun stop() {
        lifecycleMutex.withLock {
            activeBinding?.let { closeBinding(it, upload = isCurrent(it)) }
        }
    }

    /** Called only under lifecycleMutex; an old teardown cannot close a new session. */
    private suspend fun closeBinding(binding: SessionBinding, upload: Boolean) {
        if (activeBinding !== binding) return
        // Stop watching first so a deliberate teardown never reads as an error.
        stateWatchJob?.cancel()
        stateWatchJob = null
        // The state watcher takes lifecycleMutex, so cancel it without joining.
        eventsJob?.cancelAndJoin()
        eventsJob = null
        // A normal final flush retains this account's binding. Auth changes
        // discard private buffered turns instead of uploading under new auth.
        if (upload) transcriptUploader.finish(costTracker.cost) else transcriptUploader.discard()
        invalidate(binding, clearTranscript = !upload)
        try {
            (binding.transport ?: transport).disconnect()
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            LNLog.w(LogCategory.REALTIME, TAG, "disconnect during session cleanup failed", e)
        } finally {
            activeBinding = null
            _connected.value = false
            emittedChars.clear()
            localToolResults.reset()
        }
    }

    override fun setMicMuted(muted: Boolean) {
        transport.setMicMuted(muted)
    }

    override fun interruptAssistant() {
        transport.stopPlayback()
    }

    // ---- event mapping ----

    private fun onTransportEvent(event: RealtimeEvent, binding: SessionBinding) {
        if (!isCurrent(binding)) return
        when (event) {
            is RealtimeEvent.SpeechStarted ->
                emit(SessionUiEvent.UserSpeechStarted)

            is RealtimeEvent.AssistantAudioStarted ->
                emit(SessionUiEvent.AssistantSpeaking(speaking = true))

            is RealtimeEvent.AssistantAudioStopped ->
                emit(SessionUiEvent.AssistantSpeaking(speaking = false))

            is RealtimeEvent.ResponseDone -> {
                emit(SessionUiEvent.AssistantSpeaking(speaking = false))
                deviceActionState.takeAcknowledged()?.let { pending ->
                    scope.launch {
                        if (isCurrent(binding) && deviceActionState.isCurrent(pending.generation)) {
                            runDeviceAction(pending.action, binding)
                        }
                    }
                }
                applyUsage(event.usage)
            }

            is RealtimeEvent.Usage -> applyUsage(event.usage)

            is RealtimeEvent.UserTranscriptDelta ->
                emitDelta(event.itemId, TranscriptRole.USER, event.delta, done = false)

            is RealtimeEvent.UserTranscriptCompleted ->
                emitFinal(event.itemId, TranscriptRole.USER, event.text)

            is RealtimeEvent.AssistantTranscriptDelta ->
                emitDelta(event.itemId, TranscriptRole.ASSISTANT, event.delta, done = false)

            is RealtimeEvent.AssistantTranscriptDone ->
                emitFinal(event.itemId, TranscriptRole.ASSISTANT, event.text)

            is RealtimeEvent.FunctionCall -> handleFunctionCall(event, binding)

            is RealtimeEvent.ServerError ->
                // In-band server errors (e.g. a cancel racing a finished
                // response) are usually benign; a fatal one also drops the
                // peer connection, which the state watcher reports.
                LNLog.w(LogCategory.REALTIME, TAG, "realtime server error ${event.code}: ${event.message}")

            is RealtimeEvent.ResponseStarted -> {
                // response.created after a device function result identifies
                // the acknowledgement response. The earlier response.done is
                // the function-calling turn and must not trigger the action.
                deviceActionState.markAcknowledgementStarted()
            }

            is RealtimeEvent.SessionCreated,
            is RealtimeEvent.SessionUpdated,
            is RealtimeEvent.SpeechStopped,
            is RealtimeEvent.Other,
            -> Unit
        }
    }

    /**
     * Fold either OpenAI's response.done usage or Gemini's normalized usage
     * event through the one tracker that backs both the live badge and the
     * final transcript persistence payload.
     */
    private fun applyUsage(usage: JSONObject?) {
        costTracker.add(usage, sessionRates)?.let { cost ->
            emit(
                SessionUiEvent.CostUpdated(
                    usd = cost.usd,
                    textTokens = cost.textTokens,
                    audioTokens = cost.audioTokens,
                ),
            )
        }
    }

    /**
     * Tool round-trip (FR-V04): execute server-side, then hand the result
     * back to the model and ask it to continue the spoken response.
     */
    private fun handleFunctionCall(call: RealtimeEvent.FunctionCall, binding: SessionBinding) {
        val generation = binding.generation
        val sessionTransport = binding.transport ?: return
        scope.launch(binding.tools) {
            if (!isCurrent(binding) || !deviceActionState.isCurrent(generation)) return@launch

            // Device-local tools never reach the backend router. Session actions
            // are deferred until the assistant has spoken its confirmation;
            // volume/camera actions happen immediately and return their actual
            // device/storage result in the same Result shape as a backend tool.
            val deviceTool = DeviceSessionTool.forName(call.name)
            var shouldRespond = true
            val output = when {
                deviceTool != null -> {
                    if (!deviceActionState.setPending(deviceTool, generation)) return@launch
                    deviceToolOutput(deviceTool, call.callId)
                }

                call.name == DEVICE_VOLUME_TOOL_NAME -> {
                    val delivery = localToolResults.getOrExecute(call.callId) {
                        deviceVolumeTool.execute(call.callId, call.argumentsJson)
                    }
                    shouldRespond = delivery.shouldRespond
                    delivery.output
                }

                call.name == TAKE_PHOTO_TOOL_NAME || call.name == RECORD_VIDEO_TOOL_NAME -> {
                    val delivery = localToolResults.getOrExecute(call.callId) {
                        deviceCameraTool.execute(call.name, call.callId, call.argumentsJson)
                    }
                    shouldRespond = delivery.shouldRespond
                    delivery.output
                }

                else -> toolRouter.invoke(call, binding.authSessionId)
            }
            if (!shouldRespond || !isCurrent(binding) || !deviceActionState.isCurrent(generation)) return@launch
            sessionTransport.sendEvent(
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
            if (!isCurrent(binding)) return@launch
            sessionTransport.sendEvent(JSONObject().put("type", "response.create"))

            val summary = runCatching {
                val json = JSONObject(output)
                if (json.optBoolean("ok")) "completed" else
                    json.optJSONObject("error")?.optString("message").orEmpty().ifEmpty { "failed" }
            }.getOrDefault("completed")
            if (!isCurrent(binding)) return@launch
            transcriptStore.addToolChip(itemId = call.callId, name = call.name, summary = summary)
            emit(SessionUiEvent.ToolCall(itemId = call.callId, name = call.name, summary = summary))
        }
    }

    /**
     * Inject a text turn and ask for a reply. Mirrors what realtime.mjs's
     * sendUserText does on web, and uses the same two events the tool-output
     * path above already sends — so there is one way this client puts words
     * into a session, not two.
     */
    override fun sendUserText(text: String) {
        if (text.isBlank() || !connected.value) return
        val binding = activeBinding?.takeIf { isCurrent(it) } ?: return
        val sessionTransport = binding.transport ?: return
        runCatching {
            sessionTransport.sendEvent(
                JSONObject()
                    .put("type", "conversation.item.create")
                    .put(
                        "item",
                        JSONObject()
                            .put("type", "message")
                            .put("role", "user")
                            .put(
                                "content",
                                org.json.JSONArray().put(
                                    JSONObject().put("type", "input_text").put("text", text),
                                ),
                            ),
                    ),
            )
            if (isCurrent(binding)) sessionTransport.sendEvent(JSONObject().put("type", "response.create"))
        }.onFailure {
            // The transport raced closed. A missed notification is not worth
            // surfacing to the user.
            LNLog.w(LogCategory.REALTIME, TAG, "sendUserText failed", it)
        }
    }

    /**
     * Perform a deferred device-local tool action, after the assistant's spoken
     * confirmation has completed.
     */
    private suspend fun runDeviceAction(action: DeviceSessionTool, binding: SessionBinding) {
        val closed = lifecycleMutex.withLock {
            if (!isCurrent(binding)) return@withLock false
            closeBinding(binding, upload = true)
            true
        }
        if (closed && action == DeviceSessionTool.START_NEW_CONVERSATION) {
            runCatching { startForSession(binding.authSessionId) }
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
