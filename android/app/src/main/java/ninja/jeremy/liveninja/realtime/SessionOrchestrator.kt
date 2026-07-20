package ninja.jeremy.liveninja.realtime

import android.app.KeyguardManager
import android.content.Context
import android.media.AudioAttributes
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.media.ToneGenerator
import android.os.PowerManager
import android.os.SystemClock
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.assistant.AssistSource
import ninja.jeremy.liveninja.assistant.AssistTrigger
import ninja.jeremy.liveninja.assistant.AssistantEvents
import ninja.jeremy.liveninja.audio.WakeWordDetection
import ninja.jeremy.liveninja.log.LNLog
import ninja.jeremy.liveninja.log.LogCategory
import ninja.jeremy.liveninja.ui.state.RealtimeSessionController
import ninja.jeremy.liveninja.ui.state.SettingsStore
import ninja.jeremy.liveninja.wake.WakeEvents

/** Android side-effects the orchestrator fires around a session; faked in tests. */
interface SessionEffects {
    /** Acquire the session-scoped PARTIAL_WAKE_LOCK (30-min hard cap). */
    fun acquireWakeLock()
    fun releaseWakeLock()
    fun requestAudioFocus()
    fun abandonAudioFocus()
    /** ~150 ms acknowledge earcon at detection (0 ms perceived latency). */
    fun playEarcon()
}

/** Screen/keyguard state the locked-session gate reads; faked in tests. */
interface DeviceLockState {
    /** True when the screen is on/interactive. */
    val isInteractive: Boolean

    /** True when the keyguard is showing. */
    val isKeyguardLocked: Boolean
}

/**
 * Android-free state machine for wake/assist → realtime-session handoff
 * (02-voice §B2). Split from [SessionOrchestrator] purely for unit testability
 * (mirrors the LogSink/LogSinkCore split): tests drive it with fake flows, a
 * fake [RealtimeSessionController], fake [SessionEffects]/[DeviceLockState], and
 * an injectable clock.
 *
 * Duplicate guard (the primary fix for the zero-collector defect): once a
 * session is starting or active, ALL further triggers — wake OR assist — are
 * ignored until it ends. AssistantEvents' `replay = 1` re-delivery is additionally
 * de-duplicated by [AssistTrigger.timestampMillis].
 *
 * Locked/asleep gate: when `lockedSessions = false`, wake triggers are dropped
 * while `!isInteractive || isKeyguardLocked` (screen-asleep counts even with no
 * secure lock configured).
 */
class SessionOrchestratorCore(
    private val controller: RealtimeSessionController?,
    private val effects: SessionEffects,
    private val lockState: DeviceLockState,
    private val emitAssistTrigger: (AssistTrigger) -> Unit,
    private val lockedSessionsAllowed: () -> Boolean,
    private val clock: () -> Long = { SystemClock.elapsedRealtime() },
    private val scope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.Default),
) {
    private enum class Phase { IDLE, STARTING, ACTIVE }

    private val phaseLock = Any()
    private var phase = Phase.IDLE

    /** Timestamp of the last assist trigger handled — swallows replay=1 re-delivery. */
    @Volatile
    private var handledAssistTs = Long.MIN_VALUE

    private val _sessionActive = MutableStateFlow(false)

    /** True while a session is live — WakeWordService pauses the engine on this. */
    val sessionActive: StateFlow<Boolean> = _sessionActive.asStateFlow()

    private val _launchedWhileLocked = MutableStateFlow(false)

    /** Keyguard state at the launch of the current session (KeyguardGate input). */
    val launchedWhileLocked: StateFlow<Boolean> = _launchedWhileLocked.asStateFlow()

    /** Wire up the wake/assist/connected collectors. Call once. */
    fun bind(detections: Flow<WakeWordDetection>, triggers: Flow<AssistTrigger>) {
        scope.launch { detections.collect { onWake(it) } }
        scope.launch { triggers.collect { onAssist(it) } }
        controller?.let { c -> scope.launch { c.connected.collect { onConnectedChange(it) } } }
    }

    private fun onWake(detection: WakeWordDetection) {
        if (!lockedSessionsAllowed() && (!lockState.isInteractive || lockState.isKeyguardLocked)) {
            LNLog.i(LogCategory.WAKE, TAG, "wake ignored: locked/asleep and lockedSessions disabled")
            return
        }
        val locked = lockState.isKeyguardLocked
        if (!beginSession()) {
            LNLog.d(LogCategory.WAKE, TAG, "wake ignored: session already starting/active")
            return
        }
        // Surface an assist trigger for the UI (navigate to Conversation + carry
        // launchedWhileLocked for KeyguardGate). Our own triggers collector sees
        // this echo but the phase guard + timestamp dedupe swallow it.
        val ts = clock()
        handledAssistTs = ts
        emitAssistTrigger(AssistTrigger(AssistSource.WAKE_WORD, locked, ts))
        LNLog.i(LogCategory.WAKE, TAG, "wake detected (${detection.phrase}) → starting session (locked=$locked)")
        launchStart(locked)
    }

    private fun onAssist(trigger: AssistTrigger) {
        if (trigger.timestampMillis == handledAssistTs) return // replay / self-echo
        handledAssistTs = trigger.timestampMillis
        if (!beginSession()) {
            LNLog.d(LogCategory.REALTIME, TAG, "assist ignored: session already starting/active")
            return
        }
        LNLog.i(LogCategory.REALTIME, TAG, "assist trigger (${trigger.source}) → starting session")
        launchStart(trigger.launchedWhileLocked)
    }

    private fun beginSession(): Boolean = synchronized(phaseLock) {
        if (phase != Phase.IDLE) return@synchronized false
        phase = Phase.STARTING
        true
    }

    private fun launchStart(locked: Boolean) {
        val c = controller
        if (c == null) {
            LNLog.w(LogCategory.REALTIME, TAG, "no realtime controller bound; aborting session start")
            synchronized(phaseLock) { phase = Phase.IDLE }
            return
        }
        _launchedWhileLocked.value = locked
        effects.acquireWakeLock()
        _sessionActive.value = true // pause the wake engine (WakeWordService observes)
        effects.playEarcon()
        effects.requestAudioFocus()
        scope.launch {
            try {
                c.start()
                synchronized(phaseLock) { phase = Phase.ACTIVE }
                LNLog.i(LogCategory.REALTIME, TAG, "session connected")
            } catch (t: Throwable) {
                LNLog.e(LogCategory.REALTIME, TAG, "session start failed", t)
                teardown()
            }
        }
    }

    private fun onConnectedChange(connected: Boolean) {
        val active = synchronized(phaseLock) { phase == Phase.ACTIVE }
        if (!connected && active) {
            LNLog.i(LogCategory.REALTIME, TAG, "session ended (transport closed) → resuming wake engine")
            teardown()
        }
    }

    /** Explicit end (notification "End" action / manual stop). */
    suspend fun stop() {
        controller?.let { runCatching { it.stop() } }
        teardown()
    }

    private fun teardown() {
        effects.abandonAudioFocus()
        effects.releaseWakeLock()
        _sessionActive.value = false // resume wake engine immediately (bypasses 60 s retry)
        synchronized(phaseLock) { phase = Phase.IDLE }
    }

    private companion object {
        const val TAG = "SessionOrchestrator"
    }
}

/**
 * `@Singleton` wake→session orchestrator (02-voice §B2 / locked design decision).
 *
 * Collects [WakeEvents.detections] and [AssistantEvents.triggers] and drives one
 * realtime session at a time via [RealtimeSessionController]. All session *logic*
 * lives here (the "god service" mitigation from 01-platform): the wake FGS only
 * reflects [sessionActive] into its run mode and foreground type.
 *
 * Hilt singletons are lazy, so this is injected into BOTH `WakeWordService` and
 * `MainActivity` — the manual/assist entry path must work even with the wake
 * service disabled. Session side-effects (session-scoped PARTIAL_WAKE_LOCK,
 * transient voice-communication audio focus, ToneGenerator ACK earcon) are the
 * concrete [SessionEffects] below; the pure state machine is [SessionOrchestratorCore].
 */
@Singleton
class SessionOrchestrator @Inject constructor(
    @ApplicationContext private val context: Context,
    private val wakeEvents: WakeEvents,
    private val assistantEvents: AssistantEvents,
    private val controller: RealtimeSessionController,
    private val settingsStore: SettingsStore,
) {
    private val powerManager = context.getSystemService(PowerManager::class.java)
    private val audioManager = context.getSystemService(AudioManager::class.java)
    private val keyguardManager = context.getSystemService(KeyguardManager::class.java)

    private var wakeLock: PowerManager.WakeLock? = null
    private var focusRequest: AudioFocusRequest? = null

    @Volatile
    private var toneGenerator: ToneGenerator? = null

    private val focusListener = AudioManager.OnAudioFocusChangeListener { change ->
        LNLog.d(LogCategory.AUDIO, TAG, "audio focus change: $change")
    }

    private val effects = object : SessionEffects {
        override fun acquireWakeLock() {
            if (wakeLock?.isHeld == true) return
            val lock = powerManager?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, WAKELOCK_TAG)
            wakeLock = lock
            runCatching { lock?.acquire(WAKELOCK_TIMEOUT_MS) }
                .onFailure { LNLog.w(LogCategory.WAKE, TAG, "wakelock acquire failed", it) }
        }

        override fun releaseWakeLock() {
            runCatching { if (wakeLock?.isHeld == true) wakeLock?.release() }
                .onFailure { LNLog.w(LogCategory.WAKE, TAG, "wakelock release failed", it) }
            wakeLock = null
        }

        override fun requestAudioFocus() {
            val am = audioManager ?: return
            val request = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN_TRANSIENT)
                .setAudioAttributes(
                    AudioAttributes.Builder()
                        .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
                        .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build(),
                )
                .setOnAudioFocusChangeListener(focusListener)
                .build()
            focusRequest = request
            runCatching { am.requestAudioFocus(request) }
                .onFailure { LNLog.w(LogCategory.AUDIO, TAG, "audio focus request failed", it) }
        }

        override fun abandonAudioFocus() {
            val am = audioManager ?: return
            focusRequest?.let { runCatching { am.abandonAudioFocusRequest(it) } }
            focusRequest = null
        }

        override fun playEarcon() {
            // Pre-baked decision: ToneGenerator ACK tone, no raw asset today.
            runCatching {
                val tone = toneGenerator ?: ToneGenerator(AudioManager.STREAM_NOTIFICATION, EARCON_VOLUME)
                    .also { toneGenerator = it }
                tone.startTone(ToneGenerator.TONE_PROP_ACK, EARCON_DURATION_MS)
            }.onFailure { LNLog.w(LogCategory.AUDIO, TAG, "earcon failed", it) }
        }
    }

    private val lockState = object : DeviceLockState {
        override val isInteractive: Boolean get() = powerManager?.isInteractive != false
        override val isKeyguardLocked: Boolean get() = keyguardManager?.isKeyguardLocked == true
    }

    private val core = SessionOrchestratorCore(
        controller = controller,
        effects = effects,
        lockState = lockState,
        emitAssistTrigger = assistantEvents::emit,
        lockedSessionsAllowed = { settingsStore.document.value.lockedSessions },
    )

    /** True while a session is live — WakeWordService drives its SESSION mode from this. */
    val sessionActive: StateFlow<Boolean> get() = core.sessionActive

    /** Keyguard state at the current session's launch (KeyguardGate input). */
    val launchedWhileLocked: StateFlow<Boolean> get() = core.launchedWhileLocked

    init {
        core.bind(wakeEvents.detections, assistantEvents.triggers)
    }

    /** End the active session (notification "End" action / manual stop). */
    suspend fun stop() = core.stop()

    private companion object {
        const val TAG = "SessionOrchestrator"
        const val WAKELOCK_TAG = "liveninja:realtime-session"

        /** 30-min hard cap on the session wakelock (00-requirements baked default). */
        const val WAKELOCK_TIMEOUT_MS = 30L * 60 * 1000

        /** ToneGenerator ACK earcon: ~80 % volume, ~150 ms (02-voice §B2). */
        const val EARCON_VOLUME = 80
        const val EARCON_DURATION_MS = 150
    }
}
