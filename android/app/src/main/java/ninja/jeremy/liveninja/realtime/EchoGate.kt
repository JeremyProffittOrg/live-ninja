package ninja.jeremy.liveninja.realtime

import android.os.SystemClock
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.ui.state.SettingsStore
import org.json.JSONObject

/**
 * Half-duplex mic guard — the fallback half of the M21.2 fix (Android §4.3).
 *
 * Everything reduces to a single absolute deadline: the mic is gated until
 * [suppressUntilMs]. Nothing here is sticky, which is deliberate — a sticky
 * "assistant is speaking" flag wedges the mic shut forever the one time a stop
 * event is dropped, and a mic that never reopens is a worse defect than the echo
 * it prevents. Every input either extends the deadline (evidence the speaker is
 * live) or shortens it (evidence it went quiet), and the longest any single
 * signal can hold the gate closed is [speakingWatchdogMs].
 *
 * The class is Android-free and clock-injected so the whole state machine is
 * unit-testable ([EchoGateTest]); [EchoGate] adds the timer that pushes the mic
 * back on when the deadline passes.
 */
class EchoGateCore(
    /**
     * Gate tail after playout evidence stops. Covers what the stop *event* does
     * not: the remote jitter buffer / local `AudioTrack` buffer still draining
     * plus the room's acoustic tail.
     */
    private val tailMs: Long = DEFAULT_TAIL_MS,
    /**
     * Tail after a local flush (barge-in / manual stop). Short on purpose: the
     * speaker has already been faded and flushed and the user is mid-sentence,
     * so anything longer clips the very speech the barge-in was for.
     */
    private val flushTailMs: Long = DEFAULT_FLUSH_TAIL_MS,
    /**
     * Cap applied when the *response* finishes. `response.done` fires when
     * generation is complete, which is before playout drains — so it must not
     * open the gate, only bound it, in case the playout-stopped event never
     * arrives.
     */
    private val responseDoneTailMs: Long = DEFAULT_RESPONSE_DONE_TAIL_MS,
    /**
     * Ceiling for a playout-start with no further evidence. The WebRTC path
     * hears "playout started" once and then nothing until it stops, so the start
     * event has to gate optimistically; this bounds the damage of a lost stop.
     */
    private val speakingWatchdogMs: Long = DEFAULT_SPEAKING_WATCHDOG_MS,
) {
    private val lock = Any()

    /** Absolute clock value the mic stays gated until. */
    private var suppressUntilMs = Long.MIN_VALUE

    /**
     * The owner switch. False makes every query below return "mic open", so the
     * guard can be compared against the software AEC alone without touching this
     * state machine's wiring.
     */
    @Volatile
    var enabled: Boolean = false

    /**
     * The server says assistant playout began and will not tell us again until it
     * ends (OpenAI `output_audio_buffer.started`). Gates until the watchdog.
     */
    fun onAssistantAudioStarted(nowMs: Long) = extendTo(nowMs + speakingWatchdogMs)

    /**
     * Direct evidence that assistant audio is being played *right now* — one call
     * per PCM chunk handed to the `AudioTrack` on the Nova/Gemini paths. Keeps the
     * gate closed for [tailMs] past the last chunk, so a long queue drains under
     * the gate without needing the watchdog.
     */
    fun onAssistantAudioPlaying(nowMs: Long) = extendTo(nowMs + tailMs)

    /** Playout finished (`output_audio_buffer.stopped`/`.cleared`, `speaking.stop`). */
    fun onAssistantAudioStopped(nowMs: Long) = shortenTo(nowMs + tailMs)

    /** Generation finished; playout may still be draining (see [responseDoneTailMs]). */
    fun onAssistantResponseDone(nowMs: Long) = shortenTo(nowMs + responseDoneTailMs)

    /** Local playback was faded and flushed (barge-in / manual stop). */
    fun onPlaybackFlushed(nowMs: Long) = shortenTo(nowMs + flushTailMs)

    /** Session teardown: forget the deadline so the next session starts open. */
    fun reset() = synchronized(lock) { suppressUntilMs = Long.MIN_VALUE }

    /** True when locally-captured audio must not be sent upstream. */
    fun micSuppressed(nowMs: Long): Boolean =
        enabled && synchronized(lock) { nowMs < suppressUntilMs }

    /**
     * The clock value at which [micSuppressed] would flip back to false, or null
     * when the mic is already open. Drives [EchoGate]'s reopen timer.
     */
    fun nextChangeAtMs(nowMs: Long): Long? =
        if (micSuppressed(nowMs)) synchronized(lock) { suppressUntilMs } else null

    private fun extendTo(deadlineMs: Long) = synchronized(lock) {
        if (deadlineMs > suppressUntilMs) suppressUntilMs = deadlineMs
    }

    /**
     * Only ever pulls the deadline in. A stop/flush for audio that was never
     * gated must leave the mic open rather than gate it retroactively.
     */
    private fun shortenTo(deadlineMs: Long) = synchronized(lock) {
        if (deadlineMs < suppressUntilMs) suppressUntilMs = deadlineMs
    }

    companion object {
        /** Jitter/AudioTrack drain + acoustic tail after playout evidence stops. */
        const val DEFAULT_TAIL_MS = 400L

        /** After a ~40 ms fade + local flush the speaker is already silent. */
        const val DEFAULT_FLUSH_TAIL_MS = 120L

        /** Generous bound on playout still queued when `response.done` arrives. */
        const val DEFAULT_RESPONSE_DONE_TAIL_MS = 3_000L

        /** Longest a single playout-start may hold the gate with no other signal. */
        const val DEFAULT_SPEAKING_WATCHDOG_MS = 60_000L
    }
}

/**
 * [EchoGateCore] plus the timer that reopens the mic on its own.
 *
 * The WebRTC transport enables/disables a track — state it has to be *pushed*,
 * not polled — so the deadline needs something to fire when it passes. That
 * something is one coroutine at a time: while a timer is in flight, later
 * playout evidence just moves the deadline, and the timer re-checks and re-arms
 * when it wakes. Which is why keepalive-per-PCM-chunk costs nothing.
 */
class EchoGate(
    private val scope: CoroutineScope,
    private val core: EchoGateCore = EchoGateCore(),
    private val clock: () -> Long = { SystemClock.elapsedRealtime() },
    private val onSuppressionChanged: (Boolean) -> Unit,
) {
    private val timerLock = Any()
    private var timerJob: Job? = null

    /** Deadline [timerJob] is currently sleeping until; MAX when no timer is armed. */
    private var scheduledDeadlineMs = Long.MAX_VALUE

    /** Last value handed to [onSuppressionChanged]; suppresses duplicate edges. */
    @Volatile
    private var reported = false

    var enabled: Boolean
        get() = core.enabled
        set(value) {
            core.enabled = value
            evaluate()
        }

    fun assistantAudioStarted() = mutate { core.onAssistantAudioStarted(it) }

    fun assistantAudioPlaying() = mutate { core.onAssistantAudioPlaying(it) }

    fun assistantAudioStopped() = mutate { core.onAssistantAudioStopped(it) }

    fun assistantResponseDone() = mutate { core.onAssistantResponseDone(it) }

    fun playbackFlushed() = mutate { core.onPlaybackFlushed(it) }

    /** True when locally-captured audio must not be sent upstream, as of now. */
    fun micSuppressed(): Boolean = core.micSuppressed(clock())

    fun reset() {
        synchronized(timerLock) {
            timerJob?.cancel()
            timerJob = null
            scheduledDeadlineMs = Long.MAX_VALUE
        }
        core.reset()
        evaluate()
    }

    private fun mutate(apply: (Long) -> Unit) {
        apply(clock())
        evaluate()
    }

    private fun evaluate() {
        val now = clock()
        val suppressed = core.micSuppressed(now)
        if (suppressed != reported) {
            reported = suppressed
            onSuppressionChanged(suppressed)
        }
        arm(now)
    }

    /**
     * Arm (or re-arm) the reopen timer for the current deadline.
     *
     * A timer already sleeping for an *earlier-or-equal* deadline is left alone —
     * it re-evaluates and re-arms when it wakes, which is what makes
     * per-PCM-chunk keepalives free. A deadline that moved *earlier* (a stop or a
     * flush shortening the gate) does need the timer replaced, or the mic would
     * stay muted until the old, longer deadline.
     */
    private fun arm(nowMs: Long) {
        val deadline = core.nextChangeAtMs(nowMs) ?: return
        synchronized(timerLock) {
            val armed = timerJob?.isActive == true
            if (armed && scheduledDeadlineMs <= deadline) return
            if (armed) timerJob?.cancel()
            scheduledDeadlineMs = deadline
            timerJob = scope.launch {
                delay((deadline - nowMs).coerceAtLeast(1L))
                // Clear before re-evaluating so the re-arm above is not blocked by
                // this very job still counting as armed.
                synchronized(timerLock) {
                    timerJob = null
                    scheduledDeadlineMs = Long.MAX_VALUE
                }
                evaluate()
            }
        }
    }
}

/**
 * Owner switch for the half-duplex guard (WS-5 M21.2).
 *
 * Read once per session by each transport, so flipping it takes effect on the
 * next conversation rather than mid-turn. Default ON: the self-echo loop is
 * reproduced on the owner's tablet, while the AEC3 change the guard backs up is
 * still unverified on hardware (M23.2) — and if it turns out this build has no
 * software APM to fall back on, the guard is the only thing standing between the
 * assistant and answering itself. Turn it off to measure the echo canceller
 * alone; the cost of ON is that voice barge-in stops working (the mic is muted
 * while the assistant is audible, so server VAD cannot hear an interruption —
 * the UI's stop-playback control still can).
 */
@Singleton
class EchoGuardPolicy @Inject constructor(
    private val settingsStore: SettingsStore,
) {
    val halfDuplexMicGuard: Boolean
        get() = halfDuplexMicGuard(settingsStore.document.value.raw)

    companion object {
        /**
         * Additive key in the canonical settings document. Unknown keys survive
         * every local write (SettingsStore.update deep-copies `raw`), so a value
         * set by settings sync or a future Settings row persists untouched.
         */
        const val KEY_HALF_DUPLEX_MIC_GUARD = "halfDuplexMicGuard"

        const val DEFAULT_HALF_DUPLEX_MIC_GUARD = true

        /** Pure projection of the key, so the default is testable without a Context. */
        fun halfDuplexMicGuard(raw: JSONObject): Boolean =
            raw.optBoolean(KEY_HALF_DUPLEX_MIC_GUARD, DEFAULT_HALF_DUPLEX_MIC_GUARD)
    }
}
