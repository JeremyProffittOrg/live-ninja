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
    val wakePhraseLabel: String = "Hey Live Ninja",
)

@HiltViewModel
class ConversationViewModel @Inject constructor(
    sessionControllerOpt: Optional<RealtimeSessionController>,
    private val overlay: LiveOverlayController,
    settingsStore: SettingsStore,
) : ViewModel() {

    private val sessionController: RealtimeSessionController? = sessionControllerOpt.orElse(null)

    private val _state = MutableStateFlow(ConversationUiState())
    val state: StateFlow<ConversationUiState> = _state

    private var eventsJob: Job? = null
    private var tickerJob: Job? = null
    private var bargeInFlashJob: Job? = null
    private var appInBackground = false

    init {
        viewModelScope.launch {
            settingsStore.document.collect { doc ->
                _state.update { it.copy(wakePhraseLabel = wakeLabelFor(doc.wakeWord)) }
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
        if (_state.value.micState in
            setOf(MicUiState.CONNECTING, MicUiState.LISTENING, MicUiState.SPEAKING)
        ) {
            return
        }
        _state.update {
            it.copy(
                micState = MicUiState.CONNECTING,
                error = null,
                errorDetail = null,
                sessionSeconds = 0,
            )
        }
        eventsJob?.cancel()
        eventsJob = viewModelScope.launch {
            launch { controller.events.collect(::onSessionEvent) }
            try {
                controller.start()
                _state.update { it.copy(micState = MicUiState.LISTENING) }
                startTicker()
                syncOverlay()
            } catch (e: Exception) {
                _state.update {
                    it.copy(
                        micState = MicUiState.ERROR,
                        error = ConversationError.SESSION_FAILED,
                        errorDetail = e.message,
                    )
                }
                eventsJob?.cancel()
            }
        }
    }

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
            runCatching { controller.stop() }
            eventsJob?.cancel()
            stopTicker()
            _state.update { it.copy(micState = MicUiState.IDLE, micMuted = false) }
            overlay.hide()
        }
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
            is SessionUiEvent.TranscriptDelta -> _state.update { current ->
                val turns = current.transcript.toMutableList()
                val index = turns.indexOfFirst { it.id == event.itemId && !it.isToolCall }
                if (index >= 0) {
                    val existing = turns[index]
                    turns[index] = existing.copy(
                        text = existing.text + event.textDelta,
                        done = event.done,
                    )
                } else {
                    turns += TranscriptTurn(
                        id = event.itemId,
                        role = event.role,
                        text = event.textDelta,
                        done = event.done,
                    )
                }
                current.copy(transcript = turns)
            }

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

            is SessionUiEvent.ToolCall -> _state.update { current ->
                current.copy(
                    transcript = current.transcript + TranscriptTurn(
                        id = event.itemId,
                        role = TranscriptRole.ASSISTANT,
                        text = "",
                        done = true,
                        toolName = event.name,
                        toolSummary = event.summary,
                    ),
                )
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
