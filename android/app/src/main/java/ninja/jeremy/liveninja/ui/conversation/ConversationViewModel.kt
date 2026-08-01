package ninja.jeremy.liveninja.ui.conversation

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import java.util.Optional
import javax.inject.Inject
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.realtime.SessionCost
import ninja.jeremy.liveninja.realtime.TranscriptStore
import ninja.jeremy.liveninja.wake.ModelManager
import ninja.jeremy.liveninja.ui.overlay.LiveOverlayController
import ninja.jeremy.liveninja.ui.overlay.OverlayMicState
import ninja.jeremy.liveninja.ui.state.RealtimeSessionController
import ninja.jeremy.liveninja.ui.state.SessionUiEvent
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.ui.state.TranscriptRole

/**
 * Conversation-screen mic state machine — mirrors the web client's
 * `idle → requesting-mic → connecting → live-listening ⇄ live-speaking → ending`
 * plus `error/denied` (plan.md M3 §2.5, applied to Android in M4).
 */
enum class MicUiState { IDLE, REQUESTING_MIC, CONNECTING, LISTENING, SPEAKING, ENDING, ERROR }

/** Why the screen is in [MicUiState.ERROR]; the screen maps these to strings. */
enum class ConversationError { ENGINE_NOT_WIRED, MIC_DENIED, SESSION_FAILED }

/** One rendered transcript row: a speech bubble or a tool-call chip. */
data class TranscriptTurn(
    val id: String,
    val role: TranscriptRole,
    val text: String,
    val done: Boolean,
    val toolName: String? = null,
    val toolSummary: String? = null,
) {
    val isToolCall: Boolean get() = toolName != null
}

data class ConversationUiState(
    val micState: MicUiState = MicUiState.IDLE,
    val transcript: List<TranscriptTurn> = emptyList(),
    val micMuted: Boolean = false,
    /** True briefly after a barge-in so the screen can flash the interrupt visual. */
    val bargeInFlash: Boolean = false,
    val sessionSeconds: Int = 0,
    val error: ConversationError? = null,
    val errorDetail: String? = null,
    /** Wake phrase label for the idle caption ("Listening for …"). */
    val wakePhraseLabel: String = "Hey Jarvis",
    /** Catalog id the user selected in Settings (may have no model on this device yet). */
    val selectedWakeWordId: String = "",
    /** Catalog id of the head model actually loaded — what the detector can match. */
    val activeWakeWordId: String = "",
    /**
     * Running list-price cost estimate for the live session, or null when no
     * estimate is available (before the first usage report, or on an engine that
     * surfaces none). Null renders no badge at all — showing "$0.000" for an
     * unpriced engine would be a lie, not a zero.
     */
    val sessionCost: SessionCost? = null,
    /** Nonfatal quota/budget notice supplied by session bootstrap. */
    val sessionWarning: String? = null,
    /**
     * Mic pickup (contracts/settings.schema.json `micEagerness`): how quickly
     * semantic VAD calls a pause the end of a turn. "auto" leaves the API
     * default alone and is what an untouched document reads as.
     */
    val micEagerness: String = "auto",
)

@HiltViewModel
class ConversationViewModel @Inject constructor(
    sessionControllerOpt: Optional<RealtimeSessionController>,
    private val overlay: LiveOverlayController,
    private val settingsStore: SettingsStore,
    private val transcriptStore: TranscriptStore,
    private val modelManager: ModelManager,
) : ViewModel() {

    private val sessionController: RealtimeSessionController? = sessionControllerOpt.orElse(null)

    private val _state = MutableStateFlow(ConversationUiState())
    val state: StateFlow<ConversationUiState> = _state

    private var startJob: Job? = null
    private var tickerJob: Job? = null
    private var bargeInFlashJob: Job? = null
    private var appInBackground = false

    /** Previous `connected` sample, so the init collector fires only on real transitions. */
    private var lastConnected: Boolean? = null

    init {
        // Transcript is process-wide (survives screen-off/backgrounded sessions):
        // render TranscriptStore, don't accumulate from events (02-voice §B4).
        viewModelScope.launch {
            transcriptStore.turns.collect { turns ->
                _state.update { it.copy(transcript = turns.map(::toUiTurn)) }
            }
        }
        viewModelScope.launch {
            settingsStore.document.collect { doc ->
                // Deliberately NOT doc.wakeWord. The selected catalog id is an
                // aspiration; the phrase the detector can actually match is whatever
                // head model is loaded. Advertising a phrase with no model behind it
                // is how the home screen ended up promising "Hey Live Ninja" on a
                // build that only bundles hey_jarvis (WS-5 M21.3).
                _state.update {
                    it.copy(selectedWakeWordId = doc.wakeWord, micEagerness = doc.micEagerness)
                }
            }
        }
        // The wake caption follows the loaded head model (WS-5 M21.3): ModelManager
        // emits on every verified swap, so the hint changes the moment a newly
        // downloaded phrase becomes active.
        viewModelScope.launch {
            modelManager.headModel.collect { ref ->
                _state.update {
                    it.copy(
                        activeWakeWordId = ref.wakeWordId,
                        wakePhraseLabel = wakeLabelFor(ref.wakeWordId),
                    )
                }
            }
        }

        // Mic state is derived from the singleton session's `connected` — so a
        // session started with the screen off (by SessionOrchestrator) is
        // reflected the moment the UI attaches, not only on an in-app tap.
        sessionController?.let { controller ->
            viewModelScope.launch { controller.events.collect(::onSessionEvent) }
            viewModelScope.launch {
                controller.connected.collect { connected ->
                    val previous = lastConnected
                    lastConnected = connected
                    when {
                        previous == null -> if (connected) onSessionBecameLive()
                        connected && !previous -> onSessionBecameLive()
                        !connected && previous -> onSessionEnded()
                    }
                }
            }
        }
    }

    /** The screen verified RECORD_AUDIO is granted (or just got granted) — connect. */
    fun startSession() {
        val controller = sessionController
        if (controller == null) {
            _state.update {
                it.copy(micState = MicUiState.ERROR, error = ConversationError.ENGINE_NOT_WIRED)
            }
            return
        }
        if (controller.connected.value || _state.value.micState in
            setOf(MicUiState.CONNECTING, MicUiState.LISTENING, MicUiState.SPEAKING)
        ) {
            // Already live (e.g. wake-started) or connecting — just reflect it.
            if (controller.connected.value) onSessionBecameLive()
            return
        }
        _state.update {
            it.copy(
                micState = MicUiState.CONNECTING,
                error = null,
                errorDetail = null,
                sessionSeconds = 0,
                // Clear the previous session's total now rather than waiting for
                // the first usage report, so the badge never shows the last
                // conversation's cost against this one.
                sessionCost = null,
                sessionWarning = null,
            )
        }
        startJob?.cancel()
        startJob = viewModelScope.launch {
            try {
                controller.start()
                // LISTENING + ticker are driven by the connected collector.
            } catch (e: Exception) {
                _state.update {
                    it.copy(
                        micState = MicUiState.ERROR,
                        error = ConversationError.SESSION_FAILED,
                        errorDetail = e.message,
                    )
                }
            }
        }
    }

    /** A session became live (in-app tap OR wake/assist-started while screen off). */
    private fun onSessionBecameLive() {
        _state.update {
            if (it.micState in setOf(MicUiState.LISTENING, MicUiState.SPEAKING)) {
                it
            } else {
                it.copy(micState = MicUiState.LISTENING, error = null, errorDetail = null)
            }
        }
        startTicker()
        syncOverlay()
    }

    /** The session dropped/ended (transport closed, remote stop, or our stop()). */
    private fun onSessionEnded() {
        if (_state.value.micState == MicUiState.ERROR) return
        stopTicker()
        _state.update { it.copy(micState = MicUiState.IDLE, micMuted = false) }
        overlay.hide()
    }

    private fun toUiTurn(entry: TranscriptStore.Entry): TranscriptTurn = TranscriptTurn(
        id = entry.id,
        role = entry.role,
        text = entry.text,
        done = entry.done,
        toolName = entry.toolName,
        toolSummary = entry.toolSummary,
    )

    /** The screen is about to launch the RECORD_AUDIO runtime prompt. */
    fun onRequestingMicPermission() {
        _state.update { it.copy(micState = MicUiState.REQUESTING_MIC, error = null) }
    }

    fun onMicPermissionResult(granted: Boolean) {
        if (granted) {
            startSession()
        } else {
            _state.update {
                it.copy(micState = MicUiState.ERROR, error = ConversationError.MIC_DENIED)
            }
        }
    }

    fun endSession() {
        val controller = sessionController ?: run {
            _state.update { it.copy(micState = MicUiState.IDLE, error = null) }
            return
        }
        _state.update { it.copy(micState = MicUiState.ENDING) }
        viewModelScope.launch {
            // stop() flips `connected` → false, which onSessionEnded() reflects
            // (idle state, ticker, overlay). Set ENDING here for the interim.
            runCatching { controller.stop() }
        }
    }

    /**
     * New conversation (owner 2026-08-01: the control existed on web but had no
     * equivalent in the app).
     *
     * When a session is live this is a full stop/start, NOT a transcript clear —
     * the session id is what the backend keys its LOG#/CONV rows against, so
     * only a genuinely new session gives the new conversation its own History
     * row. That is exactly what the `start_new_conversation` tool does when the
     * user asks for this out loud (RealtimeSessionCoordinator.runDeviceAction),
     * and the two paths must not diverge.
     *
     * When nothing is live there is no session to replace, so it just discards
     * the turns still on screen — the next session will clear the store anyway,
     * and leaving the last conversation visible under a button labelled "new"
     * is the confusing half of the two.
     */
    fun startNewConversation() {
        val controller = sessionController
        if (controller == null || !controller.connected.value) {
            transcriptStore.clear()
            _state.update { it.copy(sessionSeconds = 0, sessionCost = null, sessionWarning = null) }
            return
        }
        _state.update { it.copy(micState = MicUiState.ENDING) }
        viewModelScope.launch {
            runCatching { controller.stop() }
            // startSession() resets the seconds/cost/warning triple and drives
            // the state machine; going through it keeps one definition of what
            // "a session is starting" means.
            startSession()
        }
    }

    /**
     * Mic pickup (low|medium|high|auto). Persisted to the settings document and
     * consumed SERVER-side at the next mint (internal/webapp/api_routes.go reads
     * `micEagerness` out of the effective document; internal/realtime/mint.go
     * turns it into turn_detection.eagerness), so it deliberately does not
     * claim to change the live session — [ConversationUiState.micEagerness]
     * drives the chips and the screen says so while a session is up.
     */
    fun setMicEagerness(value: String) {
        settingsStore.setMicEagerness(value)
    }

    fun toggleMute() {
        val muted = !_state.value.micMuted
        sessionController?.setMicMuted(muted)
        _state.update { it.copy(micMuted = muted) }
    }

    /**
     * Push-to-talk / tap-to-interrupt: cancels assistant playback and returns
     * the session to listening (local barge-in, plan.md M4 §4.3).
     */
    fun interruptAndListen() {
        val controller = sessionController ?: return
        if (_state.value.micState !in setOf(MicUiState.SPEAKING, MicUiState.LISTENING)) return
        controller.interruptAssistant()
        if (_state.value.micMuted) {
            controller.setMicMuted(false)
        }
        _state.update { it.copy(micState = MicUiState.LISTENING, micMuted = false) }
        flashBargeIn()
        syncOverlay()
    }

    fun acknowledgeError() {
        _state.update { it.copy(micState = MicUiState.IDLE, error = null, errorDetail = null) }
    }

    fun dismissSessionWarning() {
        _state.update { it.copy(sessionWarning = null) }
    }

    /** MainActivity lifecycle hooks — drive the floating overlay bubble. */
    fun onAppBackgrounded() {
        appInBackground = true
        if (sessionActive()) {
            overlay.show()
            syncOverlay()
        }
    }

    fun onAppForegrounded() {
        appInBackground = false
        overlay.hide()
    }

    private fun onSessionEvent(event: SessionUiEvent) {
        when (event) {
            // Transcript rows (deltas + tool chips) are accumulated in the
            // process-wide TranscriptStore and rendered from there; the events
            // below only drive mic state / transient visuals.
            is SessionUiEvent.TranscriptDelta -> Unit
            is SessionUiEvent.ToolCall -> Unit

            is SessionUiEvent.CostUpdated ->
                _state.update {
                    it.copy(
                        sessionCost = SessionCost(
                            usd = event.usd,
                            textTokens = event.textTokens,
                            audioTokens = event.audioTokens,
                        ),
                    )
                }

            is SessionUiEvent.SessionWarning ->
                _state.update { it.copy(sessionWarning = event.message) }

            is SessionUiEvent.AssistantSpeaking -> {
                _state.update { current ->
                    when {
                        current.micState == MicUiState.LISTENING && event.speaking ->
                            current.copy(micState = MicUiState.SPEAKING)
                        current.micState == MicUiState.SPEAKING && !event.speaking ->
                            current.copy(micState = MicUiState.LISTENING)
                        else -> current
                    }
                }
                syncOverlay()
            }

            SessionUiEvent.UserSpeechStarted -> {
                _state.update { current ->
                    if (current.micState == MicUiState.SPEAKING) {
                        current.copy(micState = MicUiState.LISTENING)
                    } else {
                        current
                    }
                }
                flashBargeIn()
                syncOverlay()
            }

            is SessionUiEvent.SessionError -> {
                _state.update {
                    it.copy(
                        micState = MicUiState.ERROR,
                        error = ConversationError.SESSION_FAILED,
                        errorDetail = event.message,
                    )
                }
                stopTicker()
                overlay.hide()
            }
        }
    }

    private fun sessionActive(): Boolean =
        _state.value.micState in setOf(MicUiState.CONNECTING, MicUiState.LISTENING, MicUiState.SPEAKING)

    private fun syncOverlay() {
        if (!appInBackground || !sessionActive()) return
        overlay.update(
            if (_state.value.micState == MicUiState.SPEAKING) {
                OverlayMicState.SPEAKING
            } else {
                OverlayMicState.LISTENING
            },
        )
    }

    private fun flashBargeIn() {
        bargeInFlashJob?.cancel()
        bargeInFlashJob = viewModelScope.launch {
            _state.update { it.copy(bargeInFlash = true) }
            delay(1800)
            _state.update { it.copy(bargeInFlash = false) }
        }
    }

    private fun startTicker() {
        tickerJob?.cancel()
        tickerJob = viewModelScope.launch {
            while (true) {
                delay(1000)
                _state.update { it.copy(sessionSeconds = it.sessionSeconds + 1) }
            }
        }
    }

    private fun stopTicker() {
        tickerJob?.cancel()
        tickerJob = null
    }

    private fun wakeLabelFor(id: String): String =
        id.split('-').joinToString(" ") { part ->
            part.replaceFirstChar { c -> c.uppercaseChar() }
        }

    override fun onCleared() {
        overlay.hide()
        super.onCleared()
    }
}
