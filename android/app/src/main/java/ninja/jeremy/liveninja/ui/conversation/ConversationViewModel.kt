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
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.realtime.LiveEventsClient
import ninja.jeremy.liveninja.realtime.NudgeMerge
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
    /**
     * The account's OTHER signed-in devices, as they last described themselves
     * (§6 WS-5 M5.1). This device is deliberately absent: the state pill a few
     * pixels away already says what this one is doing, and presence is
     * throttled, so a self row would disagree with the pill for up to a second
     * and read as a bug.
     *
     * Empty renders nothing at all rather than "no other devices" — a user with
     * one device should not be told about a fleet they do not have.
     */
    val peers: List<PeerPresence> = emptyList(),
)

/**
 * One other device on the roster, already reduced to what the screen draws.
 *
 * [state] is the five-value cross-client vocabulary from the wire contract
 * (idle/connecting/listening/thinking/speaking), NOT [MicUiState]: the web
 * client has states Android does not and vice versa, and the roster has to be
 * able to render a peer running the other implementation.
 */
data class PeerPresence(
    val deviceId: String,
    val persona: String,
    val state: String,
)

/**
 * Reduces the raw roster to the rows this screen draws.
 *
 * Pure, and separate from the collector, because both rules it applies are one
 * line each and both fail silently when dropped: showing our own row makes the
 * app look like it is talking to itself, and a blank device id is a peer whose
 * presence payload disagreed with its own topic — it has no stable identity, so
 * it cannot be de-duplicated against anything and would flicker in and out.
 */
internal fun rosterFor(peers: List<LiveEventsClient.Peer>, self: String?): List<PeerPresence> =
    peers
        .filter { it.deviceId.isNotBlank() && it.deviceId != self }
        .map { PeerPresence(it.deviceId, it.persona, it.state) }

/** What can be done with a change another device made, right now. */
internal enum class NudgeDelivery {
    /** A session is live and nobody is mid-sentence: say it. */
    SPEAK,

    /** Live but mid-turn — keep it queued and try again when the turn ends. */
    HOLD,

    /** No session to speak it through: surface it on screen instead of dropping it. */
    QUIET,
}

/**
 * Whether a held change can be spoken right now.
 *
 * Pure, and deliberately consulted TWICE per delivery — once before claiming
 * the turn-taking lock and again after it resolves. [LiveEventsClient.claimSpeakingTurn]
 * suspends for 400ms of real time, and both of this function's inputs can
 * change inside that window:
 *
 *  - The assistant can START a turn during the settle. Deciding only up front
 *    lands the injected "[Automatic update]" mid-sentence, which is the exact
 *    interruption the guard exists to prevent. The pre-diff code could not
 *    produce this because it checked and spoke in one synchronous block; the
 *    moment a suspend appeared between the two, the check stopped meaning
 *    anything without this second reading.
 *  - The session can DROP during the settle. `RealtimeSessionCoordinator.sendUserText`
 *    opens with `if (text.isBlank() || !connected.value) return`, so speaking
 *    into a dead transport discards the text silently — and the caller has by
 *    then emptied the queue and cancelled the 60s quiet fallback, so the change
 *    is never spoken, never shown, and never retried. Hence [sessionConnected]
 *    rather than the mic state alone: the mic state is this ViewModel's view of
 *    the session and it lags the transport.
 */
internal fun nudgeDelivery(micState: MicUiState, sessionConnected: Boolean): NudgeDelivery = when {
    !sessionConnected -> NudgeDelivery.QUIET
    micState == MicUiState.SPEAKING -> NudgeDelivery.HOLD
    micState == MicUiState.LISTENING -> NudgeDelivery.SPEAK
    else -> NudgeDelivery.QUIET
}

/**
 * Whether the MQTT event stream is still earning the battery it costs.
 *
 * Two callers, one rule, because the two halves are useless apart: the socket
 * is held into the background only for a live session, so it must also be
 * dropped when THAT session ends while still backgrounded. Without the second
 * caller nothing in this ViewModel ever runs again for an app that is already
 * backgrounded, and the WebSocket plus its 30-second ping loop live on
 * indefinitely — precisely the always-on background socket §6 WS-4 M4.2 chose
 * against for One UI battery reasons.
 *
 * The session half is not negotiable in the other direction either: a
 * screen-off wake-word session must stay on the stream, because a device that
 * cannot see the turn-taking lock cannot claim it and speaks over every other
 * surface (§6 WS-5 M5.2).
 */
internal fun shouldHoldEventStream(appInBackground: Boolean, sessionActive: Boolean): Boolean =
    !appInBackground || sessionActive

@HiltViewModel
class ConversationViewModel @Inject constructor(
    sessionControllerOpt: Optional<RealtimeSessionController>,
    private val overlay: LiveOverlayController,
    private val settingsStore: SettingsStore,
    private val transcriptStore: TranscriptStore,
    private val modelManager: ModelManager,
    private val liveEvents: LiveEventsClient,
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

        // §6 WS-4 M4.3 — another device changed shared state. Same three
        // guards as web: never mid-turn, never our own edit (LiveEventsClient
        // filters that), never without a session to speak through.
        viewModelScope.launch {
            liveEvents.changes.collect { change -> change?.let(::onRemoteChange) }
        }

        // §6 WS-5 M5.1 — tell the other devices what this one is doing, on
        // every transition. Driven off _state in one place rather than sprinkled
        // through the transitions, so a transition added later cannot forget to
        // do it; LiveEventsClient throttles what actually reaches the wire.
        viewModelScope.launch {
            _state.map { it.micState }.distinctUntilChanged().collect { publishPresenceState() }
        }

        // ...and show what they said back. The roster is the visible half of
        // M5.1: without it the presence traffic is real but invisible, and the
        // user has no way to tell "the other device answered" from "nothing
        // happened". LiveEventsClient.peers includes THIS device, because the
        // `#` subscription echoes our own retained presence, so the self row is
        // filtered here against the client id the server issued us.
        viewModelScope.launch {
            liveEvents.peers.collect { peers ->
                val roster = rosterFor(peers, liveEvents.clientId)
                _state.update { it.copy(peers = roster) }
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
        // A live session must be on the event stream even when the app is
        // backgrounded and the screen is off: a device that cannot see the
        // turn-taking lock cannot claim it, and speaks over every other
        // surface (§6 WS-5 M5.2). start() is idempotent.
        liveEvents.start()
        _state.update {
            if (it.micState in setOf(MicUiState.LISTENING, MicUiState.SPEAKING)) {
                it
            } else {
                it.copy(micState = MicUiState.LISTENING, error = null, errorDetail = null)
            }
        }
        startTicker()
        syncOverlay()
        // After the state update, not before: held changes are only deliverable
        // once this ViewModel considers the session live, and delivering them
        // first sent them out as a silent on-screen notice instead.
        deliverHeldChanges()
    }

    /** The session dropped/ended (transport closed, remote stop, or our stop()). */
    private fun onSessionEnded() {
        // Both fleet-facing steps run AHEAD of the ERROR guard below. That
        // guard is about what the screen keeps showing; a session that ended in
        // an error has stopped being able to speak just as completely as one
        // that ended cleanly, and no peer can tell the two apart.
        //
        // Whatever this device was holding, it can no longer say — free the lock
        // now rather than leaving the rest of the fleet quiet for the full
        // expiry over a session that already ended.
        liveEvents.releaseSpeakingTurn()
        // A live session is the ONLY thing that keeps the event stream open
        // across a background transition (see onAppBackgrounded), and that
        // justification has just disappeared. onAppBackgrounded does not fire
        // again for an app that is already backgrounded, so if the socket is
        // not dropped here nothing ever drops it. `sessionActive = false` is a
        // statement of fact rather than a shortcut: the session ended, and
        // _state still says LISTENING for another two lines.
        if (!shouldHoldEventStream(appInBackground, sessionActive = false)) liveEvents.stop()
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
        // Same reasoning as the server-VAD barge-in above: the response this
        // device holds the lock for is being cancelled, so the lock goes back.
        liveEvents.releaseSpeakingTurn()
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
    /**
     * Changes held back, oldest first — waiting for the assistant to stop
     * talking, or for another device to give the turn-taking lock up.
     *
     * A queue rather than the single slot this started as (§6 WS-5): with three
     * surfaces live a second change overwrote the first and the user was told
     * about neither, which is invisible in a two-device test and exactly what
     * three produce. Bounded and drop-oldest, because an unbounded backlog of
     * edits nobody has heard about is its own bug.
     */
    private val pendingChanges = ArrayDeque<LiveEventsClient.Change>()

    /** The in-flight claim/settle/arbitrate round, so two cannot overlap. */
    private var lockJob: Job? = null

    /** The quiet-fallback deadline for whatever is currently held. */
    private var quietDeadlineJob: Job? = null

    /**
     * Deliver a cross-device change. Speaking over the assistant mid-sentence
     * is worse than being a moment late, so a change that lands while it is
     * talking is held and flushed when the session returns to listening.
     *
     * Everything unprompted also goes through the fleet-wide speaking lock:
     * every signed-in device learns about an edit in the same millisecond, and
     * without the lock every one of them answers (§6 WS-5 M5.2).
     */
    private fun onRemoteChange(change: LiveEventsClient.Change) {
        pendingChanges.addLast(change)
        while (pendingChanges.size > NudgeMerge.CAP) pendingChanges.removeFirst()
        armQuietDeadline()
        deliverHeldChanges()
    }

    /**
     * Try to say what is held. Called on arrival, at the end of this device's
     * own turn, and when a session becomes live — the three moments at which
     * either the mid-turn guard or the lock might have stopped being in the way.
     */
    private fun deliverHeldChanges() {
        if (pendingChanges.isEmpty()) return
        when (deliveryNow()) {
            // Mid-turn. Stay queued; the end-of-turn handler calls back here.
            NudgeDelivery.HOLD -> return
            // Nothing to speak through: surface it quietly instead of dropping
            // it, so the user still learns another device moved.
            NudgeDelivery.QUIET -> {
                drainQuietly()
                return
            }
            NudgeDelivery.SPEAK -> Unit
        }
        if (lockJob?.isActive == true) return
        lockJob = viewModelScope.launch {
            // Suspends for the settle window. Losing is not an error: another
            // device is answering, and what is held here folds into this
            // device's next turn instead of becoming a second interruption.
            if (!liveEvents.claimSpeakingTurn()) return@launch
            // 400ms of real time passed inside that call. Ask again before
            // committing to anything: the assistant may have started talking,
            // or the transport may have gone. Nothing is drained on this path —
            // the queue and its quiet deadline are left exactly as they were,
            // so the change is re-offered at the end of the next turn or
            // surfaced on screen by the fallback, rather than consumed by a
            // sendUserText that silently discards it.
            if (deliveryNow() != NudgeDelivery.SPEAK) {
                liveEvents.releaseSpeakingTurn()
                return@launch
            }
            val held = drainHeld()
            if (held.isEmpty()) {
                liveEvents.releaseSpeakingTurn()
                return@launch
            }
            speak(held)
        }
    }

    /** [nudgeDelivery] against this ViewModel's live inputs. */
    private fun deliveryNow(): NudgeDelivery =
        nudgeDelivery(_state.value.micState, sessionController?.connected?.value == true)

    /**
     * The commit point: past here this device is speaking unprompted, so
     * nothing may reach it without having won [LiveEventsClient.claimSpeakingTurn].
     */
    private fun speak(changes: List<LiveEventsClient.Change>) {
        sessionController?.sendUserText(NudgeMerge.prompt(changes))
    }

    private fun drainQuietly() {
        val held = drainHeld()
        if (held.isEmpty()) return
        _state.update { it.copy(sessionWarning = NudgeMerge.notice(held)) }
    }

    private fun drainHeld(): List<LiveEventsClient.Change> {
        quietDeadlineJob?.cancel()
        quietDeadlineJob = null
        val held = pendingChanges.toList()
        pendingChanges.clear()
        return held
    }

    /**
     * A held change must never simply evaporate because the user stopped
     * talking to this device. If nothing has flushed the queue within
     * [QUIET_FALLBACK_MS] it lands on the same on-screen surface an offline
     * change uses — silently, with no voice.
     */
    private fun armQuietDeadline() {
        if (quietDeadlineJob?.isActive == true) return
        quietDeadlineJob = viewModelScope.launch {
            delay(QUIET_FALLBACK_MS)
            drainQuietly()
        }
    }

    /**
     * What this device tells its peers it is doing (§6 WS-5 M5.1). Led by the
     * singleton session's own `connected` rather than by [MicUiState] alone:
     * this ViewModel dies with the Activity, and a session started by the wake
     * word with the screen off must not be advertised to the fleet as idle.
     */
    private fun publishPresenceState() {
        val connected = sessionController?.connected?.value == true
        val micState = _state.value.micState
        liveEvents.setState(
            when {
                connected && micState == MicUiState.SPEAKING -> "speaking"
                connected -> "listening"
                micState == MicUiState.REQUESTING_MIC || micState == MicUiState.CONNECTING ->
                    "connecting"
                else -> "idle"
            },
        )
    }

    fun onAppBackgrounded() {
        appInBackground = true
        // Hold the socket ONLY while a session is live — see
        // [shouldHoldEventStream] for both halves of that rule. onSessionEnded
        // applies the same rule when the session, and with it the reason to
        // hold the socket, ends after this point.
        if (!shouldHoldEventStream(appInBackground = true, sessionActive = sessionActive())) {
            liveEvents.stop()
        }
        if (sessionActive()) {
            overlay.show()
            syncOverlay()
        }
    }

    fun onAppForegrounded() {
        appInBackground = false
        liveEvents.start()
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
                val turnEnded = !event.speaking && _state.value.micState == MicUiState.SPEAKING
                _state.update { current ->
                    when {
                        current.micState == MicUiState.LISTENING && event.speaking ->
                            current.copy(micState = MicUiState.SPEAKING)
                        current.micState == MicUiState.SPEAKING && !event.speaking ->
                            current.copy(micState = MicUiState.LISTENING)
                        else -> current
                    }
                }
                if (turnEnded) {
                    // This device's turn is over: hand the lock back before
                    // anything else wants it, then say what arrived while it was
                    // talking. Until this landed, the held change only flushed
                    // when a session STARTED, so "flushed when the session
                    // returns to listening" meant "at the next session".
                    liveEvents.releaseSpeakingTurn()
                    deliverHeldChanges()
                }
                syncOverlay()
            }

            SessionUiEvent.UserSpeechStarted -> {
                // Barge-in cancels the response this device claimed the turn
                // for, so the lock goes back now. Nothing is delivered here —
                // the user is mid-sentence, and injecting a turn on top of them
                // is the interruption the lock exists to avoid.
                liveEvents.releaseSpeakingTurn()
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

    private companion object {
        /**
         * How long a change waits for a turn to fold it into before it gives up
         * and becomes an on-screen notice instead. Long enough that a normal
         * pause in a conversation does not trip it; short enough that a user who
         * walked away still finds out what happened.
         */
        const val QUIET_FALLBACK_MS = 60_000L
    }
}
