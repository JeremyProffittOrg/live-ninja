package ninja.jeremy.liveninja.realtime

import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.isActive
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
import kotlinx.coroutines.withContext
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
 * One short synchronous gate for every session-busy transition and every
 * idle-only car-audio mode commit.
 *
 * A start marks busy and reads the selected mode under the same monitor a
 * mode change uses to check idle and commit, so the two can never interleave:
 * either the commit runs first and the start sees it, or the start marks busy
 * first and the commit is rejected without writing. Callers must only run
 * short, non-suspending work under it and must never acquire the coordinator's
 * lifecycle mutex while holding it.
 */
internal class SessionBusyGate {
    /** Dedicated monitor type so diagnostics can identify this lock. */
    private class Monitor

    private val lock = Monitor()
    private val _sessionBusy = MutableStateFlow(false)
    val busy: StateFlow<Boolean> = _sessionBusy.asStateFlow()

    /** Marks busy, then reads [readStable] under the same lock. */
    fun <T> markBusyAndRead(readStable: () -> T): T =
        synchronized(lock) {
            _sessionBusy.value = true
            readStable()
        }

    fun set(busy: Boolean) {
        synchronized(lock) { _sessionBusy.value = busy }
    }

    /** Runs [commit] only while idle, atomically with the idle check. */
    fun runIfIdle(commit: () -> Unit): Boolean =
        synchronized(lock) {
            if (_sessionBusy.value) {
                false
            } else {
                commit()
                true
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
 * `play_media` is the one device-local tool that does NOT ask for a spoken
 * continuation after success: once a media app/search has been handed off, the
 * conversation mutes, sends its honest function output, and ends so the media
 * can be heard.
 *
 * Car audio (opt-in, [CarAudioSessionManager]): when selected, a start from
 * the visible app first runs the short-lived [CarAudioForegroundService]
 * (microphone|mediaPlayback, ongoing notification with End) and waits,
 * bounded, until it is verified foreground; only then are focus and an
 * observed hands-free route acquired, for ALL providers, before any mic or
 * network work. Any focus/route loss, notification End or task removal mutes
 * and ends the session and never restarts it; the route and the service are
 * released only after the transport has disconnected.
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
     * Device-local play_media executor. Field-injected (standard Hilt member
     * injection) so existing direct constructor users keep compiling; when it
     * is never set, play_media returns a structured not_supported error.
     */
    @Inject lateinit var deviceMediaTool: DeviceMediaToolExecutor

    /**
     * Shared car-audio owner. Field-injected for the same reason; when never
     * set (direct construction in tests) sessions use legacy routing.
     */
    @Inject lateinit var carAudio: CarAudioSessionManager

    /**
     * The shared singleton car-audio preference (the source of truth the
     * manager's selection reflects). Field-injected for the same reason; when
     * never set, [tryChangeCarAudioMode] refuses every change.
     */
    @Inject lateinit var carAudioPreferences: CarAudioPreferences

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

    /** One start() call; End cancels exactly this attempt, never a newer one. */
    private class PendingStart {
        @Volatile var job: Job? = null
        @Volatile var endedByUser = false
    }

    private class SessionBinding(
        val authSessionId: String,
        val generation: Long,
        val pendingStart: PendingStart,
    ) {
        @Volatile var valid = true
        @Volatile var transport: RealtimeTransport? = null
        val tools = SupervisorJob()

        /** Car-audio session (service + lease); null in legacy mode or once released. */
        @Volatile var carSession: CarAudioSessionHandle? = null

        /**
         * Set synchronously by the car-audio callback (any thread). Checked by
         * [requireCurrent] before `connected` is published, so an interruption
         * that races a slow start under the lifecycle lock fails that start.
         */
        @Volatile var carInterruption: CarAudioFailure? = null

        /** Notification End / task removal / service death for this session. */
        @Volatile var carEndRequested = false

        /** webRtcTransport.prepare() was called and not yet consumed or aborted. */
        @Volatile var webRtcPrepared = false
    }

    /** Identity of the current session; mutes of the shared transports are gated on it. */
    private val bindingGuard = SessionIdentityGuard<SessionBinding>()
    private var activeBinding: SessionBinding?
        get() = bindingGuard.current
        set(value) {
            bindingGuard.set(value)
        }

    private fun currentAuthSessionId(): String? =
        (auth.state.value as? AuthState.SignedIn)?.sessionId?.takeIf { it.isNotBlank() }

    /** Bound to the current account, regardless of car interruption (upload decisions). */
    private fun isAuthCurrent(binding: SessionBinding): Boolean =
        binding.valid && activeBinding === binding &&
            binding.authSessionId == currentAuthSessionId()

    private fun isCurrent(binding: SessionBinding): Boolean =
        isAuthCurrent(binding) && binding.carInterruption == null && !binding.carEndRequested

    private fun requireCurrent(binding: SessionBinding) {
        if (binding.carEndRequested) throw CarAudioException(CarAudioFailure.ENDED_BY_USER)
        binding.carInterruption?.let { throw CarAudioException(it) }
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

    private fun carAudioSelected(): Boolean = ::carAudio.isInitialized && carAudio.isSelected

    /**
     * Idempotent. Callers must have disconnected (or aborted) the transport
     * first: releases route/focus, then stops the session service.
     */
    private fun releaseCarAudio(binding: SessionBinding) {
        val handle = binding.carSession ?: return
        binding.carSession = null
        try {
            handle.release()
        } catch (e: Exception) {
            LNLog.w(LogCategory.AUDIO, TAG, "car audio release failed", e)
        }
    }

    private fun silenceTransport(t: RealtimeTransport, reason: String) {
        bestEffort("mute $reason") { t.setMicMuted(true) }
        bestEffort("stop playback $reason") { t.stopPlayback() }
    }

    /** Mutes the shared transport only while [binding] is still the active session. */
    private fun silenceIfActive(binding: SessionBinding, reason: String): Boolean =
        bindingGuard.withCurrent(binding) {
            binding.transport?.let { silenceTransport(it, reason) }
            true
        } ?: false

    private fun scheduleClose(binding: SessionBinding) {
        scope.launch {
            lifecycleMutex.withLock {
                // Never stop a replacement session.
                if (activeBinding === binding) closeBinding(binding, upload = isAuthCurrent(binding))
            }
        }
    }

    /**
     * Car focus/route loss. Runs synchronously on the platform callback thread.
     * Identity first: a callback captured before its session was released must
     * never mute the shared transport of a newer session. Then record the
     * interruption (fails an in-flight start before it can publish
     * `connected`), silence mic and playback at once, and end the session
     * under the lifecycle lock. Never restarts.
     */
    private fun onCarAudioInterrupted(binding: SessionBinding, failure: CarAudioFailure) {
        if (activeBinding !== binding || binding.carInterruption != null) return
        val wasLive = _connected.value
        binding.carInterruption = failure
        LNLog.i(LogCategory.AUDIO, TAG, "car audio interrupted: ${failure.code}")
        if (!silenceIfActive(binding, "after car audio ${failure.code}")) return
        if (wasLive) emit(SessionUiEvent.SessionError(failure.userMessage))
        scheduleClose(binding)
    }

    /**
     * Notification End, task removal or service death for [binding]'s car
     * session. Never waits for the lifecycle lock: an in-progress start is
     * cancelled directly, a live session is muted and closed.
     */
    private fun onCarForegroundEnd(binding: SessionBinding) {
        if (binding.carEndRequested) return
        binding.carEndRequested = true
        LNLog.i(LogCategory.AUDIO, TAG, "car audio session end requested")
        silenceIfActive(binding, "after car audio end")
        binding.pendingStart.endedByUser = true
        binding.pendingStart.job?.cancel()
        scheduleClose(binding)
    }

    private val _connected = MutableStateFlow(false)
    override val connected: StateFlow<Boolean> = _connected.asStateFlow()

    /** Owns every session-busy transition and every idle-only mode commit. */
    private val busyGate = SessionBusyGate()

    /**
     * True from the moment a start() holds the lifecycle lock (set before its
     * first suspension, under [busyGate]) until that session's teardown has
     * fully completed — for every provider, car audio or not. Reset on every
     * failed or cancelled start and every teardown. Local UI uses it for
     * display; actual mode changes go through [tryChangeCarAudioMode].
     */
    val sessionBusy: StateFlow<Boolean> = busyGate.busy

    /** Called only under lifecycleMutex, after cleanup has finished. */
    private fun syncSessionBusy() {
        busyGate.set(activeBinding != null)
    }

    /**
     * Atomically changes the local car-audio mode only while no conversation
     * is starting or live. The idle check and `setEnabled` run under the same
     * short gate a start uses to mark itself busy and read the selected mode,
     * so a start either sees this change or the change is rejected unwritten.
     * Never suspends, never takes the lifecycle lock, never starts anything.
     */
    fun tryChangeCarAudioMode(enabled: Boolean): Boolean {
        if (!::carAudioPreferences.isInitialized) return false
        val preferences = carAudioPreferences
        return runIfSessionIdle {
            if (preferences.isEnabled != enabled) preferences.setEnabled(enabled)
        }
    }

    /**
     * Idle-only helper: runs [commit] (short, synchronous, non-suspending)
     * under the busy gate only when no session is starting or live.
     */
    internal fun runIfSessionIdle(commit: () -> Unit): Boolean = busyGate.runIfIdle(commit)

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
                if (isAuthCurrent(binding)) return@collect
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

    /**
     * The start runs in its own child scope so a car-audio End can cancel
     * exactly this attempt promptly — including while it still waits for the
     * lifecycle lock — without cancelling the caller.
     */
    private suspend fun startForSession(expectedSessionId: String? = null) {
        val pending = PendingStart()
        try {
            coroutineScope {
                pending.job = this.coroutineContext[Job]
                lifecycleMutex.withLock { startLocked(expectedSessionId, pending) }
            }
        } catch (e: CancellationException) {
            if (pending.endedByUser && currentCoroutineContext().isActive) {
                throw CarAudioException(CarAudioFailure.ENDED_BY_USER)
            }
            throw e
        }
    }

    private suspend fun startLocked(expectedSessionId: String?, pending: PendingStart) {
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
        val binding = SessionBinding(authSessionId, deviceActionState.currentGeneration(), pending)
        activeBinding = binding

        var published = false
        try {
            // Busy before this attempt's first suspension; reset by its cleanup.
            // The selected mode is read under the SAME gate that marks busy, so
            // an idle-only mode change either committed first (and is seen
            // here) or is rejected; it cannot change until cleanup completes.
            val useCarAudio = busyGate.markBusyAndRead { carAudioSelected() }
            localToolResults.reset()
            transcriptStore.clear()

            // Car audio first: verified session foreground service, then focus +
            // observed hands-free route, before any mic or network work. Its
            // failure is the clearest error. No speaker fallback while opted in.
            if (useCarAudio) {
                binding.carSession = carAudio.startSession(
                    onEnd = { onCarForegroundEnd(binding) },
                    onInterrupted = { failure -> onCarAudioInterrupted(binding, failure) },
                )
                requireCurrent(binding)
            }

            // Latency parallelization (02-voice §D.2): speculatively bootstrap
            // the WebRTC transport — factory + peer connection + offer + ICE
            // gathering, none of which needs the credential — concurrently with
            // the session fetch. The dominant openai-direct path joins the
            // prepared offer at the SDP POST inside connect(); the nova/gemini
            // paths discard it via abortPrepare(). Any failure before a
            // transport is selected (including prepare() itself throwing)
            // aborts it in [abortStart], before the car route is released.
            binding.webRtcPrepared = true
            webRtcTransport.prepare()

            val session = sessionApi.fetchSession(binding.authSessionId).also { requireCurrent(binding) }

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
                    abortSpeculativePrepare(binding)
                    transport = novaBridgeTransport
                    session.bridgeToken.orEmpty() to session.wsUrl.orEmpty()
                }

                RealtimeSession.MODE_GEMINI_DIRECT -> {
                    abortSpeculativePrepare(binding)
                    transport = geminiLiveTransport
                    session.accessToken?.value.orEmpty() to session.geminiEndpoint.orEmpty()
                }

                RealtimeSession.MODE_VOICE_LIVE_DIRECT -> {
                    abortSpeculativePrepare(binding)
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
            requireCurrent(binding)
            sessionTransport.connect(credential, endpointUrl)
            requireCurrent(binding)
            // A car interruption or End after this point is still handled:
            // its callback queued a close behind this lock.
            _connected.value = true
            published = true
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
                        withContext(NonCancellable) {
                            teardownAfterTransportEnd(binding, sessionTransport, state)
                        }
                    }
                    if (state == TransportState.FAILED) {
                        _events.tryEmit(
                            SessionUiEvent.SessionError("The voice connection dropped."),
                        )
                    }
                }
            }
        } finally {
            if (!published) withContext(NonCancellable) { abortStart(binding) }
        }
    }

    /** RealtimeTransport.abortPrepare() suspends, so this must too. */
    private suspend fun abortSpeculativePrepare(binding: SessionBinding) {
        if (!binding.webRtcPrepared) return
        binding.webRtcPrepared = false
        webRtcTransport.abortPrepare()
    }

    /**
     * Cleanup for a start that never published `connected`, on any failure or
     * cancellation (runs NonCancellable). The car route and session service
     * are released only after the transport (or the speculative prepare) is
     * shut down.
     */
    private suspend fun abortStart(binding: SessionBinding) {
        if (activeBinding === binding && binding.transport != null) {
            closeBinding(binding, upload = false)
            return
        }
        try {
            if (binding.webRtcPrepared) {
                // Suspending call: a plain try/catch, not the non-suspending
                // bestEffort lambda. Any failure (including a cancellation
                // thrown by the transport) is logged so the rest of the
                // cleanup below always runs.
                try {
                    abortSpeculativePrepare(binding)
                } catch (e: Exception) {
                    LNLog.w(LogCategory.REALTIME, TAG, "abort speculative WebRTC prepare failed", e)
                }
            }
            invalidate(binding)
            if (activeBinding === binding) activeBinding = null
        } finally {
            releaseCarAudio(binding)
            syncSessionBusy()
        }
    }

    /** Transport FAILED/CLOSED teardown; called under lifecycleMutex, NonCancellable. */
    private suspend fun teardownAfterTransportEnd(
        binding: SessionBinding,
        sessionTransport: RealtimeTransport,
        state: TransportState,
    ) {
        try {
            try {
                eventsJob?.cancelAndJoin()
            } finally {
                eventsJob = null
            }
            try {
                sessionTransport.disconnect()
            } catch (e: Exception) {
                LNLog.w(LogCategory.REALTIME, TAG, "disconnect after transport $state failed", e)
                silenceTransport(sessionTransport, "after failed disconnect")
            }
        } finally {
            releaseCarAudio(binding)
            try {
                if (isAuthCurrent(binding)) transcriptUploader.finish(costTracker.cost)
                else transcriptUploader.discard()
            } catch (e: Exception) {
                LNLog.w(LogCategory.REALTIME, TAG, "transcript finalise after transport $state failed", e)
            }
            invalidate(binding, clearTranscript = false)
            activeBinding = null
            _connected.value = false
            syncSessionBusy()
        }
    }

    override suspend fun stop() {
        lifecycleMutex.withLock {
            activeBinding?.let { closeBinding(it, upload = isAuthCurrent(it)) }
        }
    }

    /**
     * Called only under lifecycleMutex; an old teardown cannot close a new
     * session. NonCancellable with a complete outer try/finally: a cancelled
     * caller can never skip transport shutdown, lease release or the service
     * stop.
     */
    private suspend fun closeBinding(binding: SessionBinding, upload: Boolean) {
        withContext(NonCancellable) {
            if (activeBinding !== binding) {
                // Already closed: only make sure its own car session is gone.
                releaseCarAudio(binding)
                syncSessionBusy()
                return@withContext
            }
            try {
                // Stop watching first so a deliberate teardown never reads as an error.
                stateWatchJob?.cancel()
                stateWatchJob = null
                try {
                    eventsJob?.cancelAndJoin()
                } finally {
                    eventsJob = null
                }
                // A normal final flush retains this account's binding. Auth changes
                // discard private buffered turns instead of uploading under new auth.
                try {
                    if (upload) transcriptUploader.finish(costTracker.cost) else transcriptUploader.discard()
                } catch (e: Exception) {
                    LNLog.w(LogCategory.REALTIME, TAG, "transcript finalise during cleanup failed", e)
                }
                invalidate(binding, clearTranscript = !upload)
                val sessionTransport = binding.transport ?: transport
                try {
                    sessionTransport.disconnect()
                } catch (e: Exception) {
                    LNLog.w(LogCategory.REALTIME, TAG, "disconnect during session cleanup failed", e)
                    silenceTransport(sessionTransport, "after failed disconnect")
                }
            } finally {
                activeBinding = null
                _connected.value = false
                emittedChars.clear()
                localToolResults.reset()
                // After the transport has released mic/playback: never expose a
                // still-live stream on the restored handset route.
                releaseCarAudio(binding)
                syncSessionBusy()
            }
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
            var mediaHandoff = false
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

                call.name == PLAY_MEDIA_TOOL_NAME -> {
                    // Single-flight per call id: a redelivered call reuses the
                    // first result and never launches a second time.
                    var launched = false
                    val delivery = localToolResults.getOrExecute(call.callId) {
                        val result = executePlayMedia(call, binding, generation)
                        launched = result.launched
                        result.output
                    }
                    shouldRespond = delivery.shouldRespond
                    mediaHandoff = delivery.shouldRespond && launched
                    delivery.output
                }

                else -> toolRouter.invoke(call, binding.authSessionId)
            }
            if (mediaHandoff) {
                finishMediaHandoff(call, output, binding, generation, sessionTransport)
                return@launch
            }
            if (!shouldRespond || !isCurrent(binding) || !deviceActionState.isCurrent(generation)) return@launch
            sessionTransport.sendEvent(functionOutputEvent(call.callId, output))
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

    private suspend fun executePlayMedia(
        call: RealtimeEvent.FunctionCall,
        binding: SessionBinding,
        generation: Long,
    ): DeviceMediaToolResult {
        if (!::deviceMediaTool.isInitialized) {
            return DeviceMediaToolResult(playMediaUnavailableOutput(call.callId), launched = false)
        }
        // The predicate is re-evaluated on the main thread right before every
        // activity start, so a binding that went stale during the dispatcher
        // hop (sign-out, stop, replacement session) cannot launch anything.
        return deviceMediaTool.execute(call.callId, call.argumentsJson) {
            isCurrent(binding) && deviceActionState.isCurrent(generation)
        }
    }

    /**
     * A media app/search was actually started. Silence this conversation at
     * once, hand the model the honest result WITHOUT response.create (no voice
     * over the launched media), then end only this bound session on the root
     * scope — binding.tools is cancelled by that very teardown.
     *
     * The teardown is scheduled from a finally block so cancellation or an
     * error while muting, stopping audio, sending the output or adding the
     * chip can never skip it; such a CancellationException still propagates
     * to the caller after the cleanup has been scheduled.
     */
    private fun finishMediaHandoff(
        call: RealtimeEvent.FunctionCall,
        output: String,
        binding: SessionBinding,
        generation: Long,
        sessionTransport: RealtimeTransport,
    ) {
        try {
            if (isCurrent(binding) && deviceActionState.isCurrent(generation)) {
                bestEffort("mute after media handoff") { sessionTransport.setMicMuted(true) }
                bestEffort("stop playback after media handoff") { sessionTransport.stopPlayback() }
                bestEffort("media function output") {
                    sessionTransport.sendEvent(functionOutputEvent(call.callId, output))
                }
                if (isCurrent(binding)) {
                    val summary = playMediaChipSummary(output)
                    transcriptStore.addToolChip(itemId = call.callId, name = call.name, summary = summary)
                    emit(SessionUiEvent.ToolCall(itemId = call.callId, name = call.name, summary = summary))
                }
            }
        } finally {
            // Root scope, not binding.tools: this must run even when the tool
            // job is already cancelled.
            scheduleClose(binding)
        }
    }

    private fun bestEffort(label: String, block: () -> Unit) {
        try {
            block()
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            LNLog.w(LogCategory.REALTIME, TAG, "$label failed", e)
        }
    }

    private fun functionOutputEvent(callId: String, output: String): JSONObject =
        JSONObject()
            .put("type", "conversation.item.create")
            .put(
                "item",
                JSONObject()
                    .put("type", "function_call_output")
                    .put("call_id", callId)
                    .put("output", output),
            )

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
