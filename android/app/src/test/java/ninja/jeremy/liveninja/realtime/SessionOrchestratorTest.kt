package ninja.jeremy.liveninja.realtime

import java.io.IOException
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import ninja.jeremy.liveninja.assistant.AssistSource
import ninja.jeremy.liveninja.assistant.AssistTrigger
import ninja.jeremy.liveninja.audio.WakeWordDetection
import ninja.jeremy.liveninja.ui.state.SessionUiEvent
import ninja.jeremy.liveninja.ui.state.RealtimeSessionController
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import kotlinx.coroutines.flow.Flow

/**
 * [SessionOrchestratorCore] state machine — wake/assist → session handoff, the
 * duplicate guard (the zero-collector fix), AssistantEvents replay dedupe, the
 * locked/asleep gate, and teardown on stop / transport close. Fake flows,
 * controller, effects, lock-state, and clock; no Android.
 */
class SessionOrchestratorTest {

    private class FakeController(private val failStart: Boolean = false) : RealtimeSessionController {
        private val _connected = MutableStateFlow(false)
        override val connected: StateFlow<Boolean> = _connected
        override val events: Flow<SessionUiEvent> = MutableSharedFlow()
        var startCount = 0
        var stopCount = 0

        override suspend fun start() {
            startCount++
            if (failStart) throw IOException("connect failed")
            _connected.value = true
        }

        override suspend fun stop() {
            stopCount++
            _connected.value = false
        }

        override fun setMicMuted(muted: Boolean) = Unit
        override fun interruptAssistant() = Unit

        /** Simulate the transport dropping / closing out-of-band. */
        fun dropConnection() {
            _connected.value = false
        }
    }

    private class FakeEffects : SessionEffects {
        val wakeLockAcquired = AtomicInteger()
        val wakeLockReleased = AtomicInteger()
        val focusRequested = AtomicInteger()
        val focusAbandoned = AtomicInteger()
        val earcons = AtomicInteger()
        override fun acquireWakeLock() { wakeLockAcquired.incrementAndGet() }
        override fun releaseWakeLock() { wakeLockReleased.incrementAndGet() }
        override fun requestAudioFocus() { focusRequested.incrementAndGet() }
        override fun abandonAudioFocus() { focusAbandoned.incrementAndGet() }
        override fun playEarcon() { earcons.incrementAndGet() }
    }

    private class FakeLock(
        override var isInteractive: Boolean = true,
        override var isKeyguardLocked: Boolean = false,
    ) : DeviceLockState

    private val detections = MutableSharedFlow<WakeWordDetection>(extraBufferCapacity = 8)
    private val triggers = MutableSharedFlow<AssistTrigger>(extraBufferCapacity = 8)
    private val emittedTriggers = mutableListOf<AssistTrigger>()

    private var clockValue = 1_000L

    private fun core(
        controller: RealtimeSessionController? = FakeController(),
        effects: FakeEffects = FakeEffects(),
        lock: FakeLock = FakeLock(),
        lockedSessionsAllowed: Boolean = true,
    ): SessionOrchestratorCore = SessionOrchestratorCore(
        controller = controller,
        effects = effects,
        lockState = lock,
        emitAssistTrigger = { emittedTriggers += it },
        lockedSessionsAllowed = { lockedSessionsAllowed },
        clock = { clockValue },
        scope = CoroutineScope(SupervisorJob() + Dispatchers.Default),
    ).also { it.bind(detections, triggers) }

    private suspend fun emitDetection() {
        awaitSubscribers()
        detections.emit(WakeWordDetection("hey live ninja", 0.9f, clockValue))
    }

    private suspend fun emitAssist(ts: Long, source: AssistSource = AssistSource.MANUAL) {
        awaitSubscribers()
        triggers.emit(AssistTrigger(source, launchedWhileLocked = false, timestampMillis = ts))
    }

    private suspend fun awaitSubscribers() = withTimeout(2_000) {
        while (detections.subscriptionCount.value == 0 || triggers.subscriptionCount.value == 0) delay(5)
    }

    private suspend fun awaitUntil(message: String, predicate: () -> Boolean) {
        try {
            withTimeout(3_000) { while (!predicate()) delay(10) }
        } catch (e: kotlinx.coroutines.TimeoutCancellationException) {
            fail("timed out waiting: $message")
        }
    }

    @Test
    fun wakeDetection_startsSession_firesEffects_andEmitsUiTrigger() = runBlocking {
        val controller = FakeController()
        val effects = FakeEffects()
        val core = core(controller = controller, effects = effects)

        emitDetection()

        awaitUntil("session active") { core.sessionActive.value }
        awaitUntil("controller started") { controller.startCount == 1 }
        assertEquals(1, effects.wakeLockAcquired.get())
        assertEquals(1, effects.focusRequested.get())
        assertEquals(1, effects.earcons.get())
        // A WAKE_WORD assist trigger is surfaced for the UI (navigation + KeyguardGate).
        awaitUntil("ui trigger emitted") { emittedTriggers.any { it.source == AssistSource.WAKE_WORD } }
    }

    @Test
    fun secondTriggerWhileActive_isIgnored_duplicateGuard() = runBlocking {
        val controller = FakeController()
        val core = core(controller = controller)

        emitDetection()
        awaitUntil("first session active") { core.sessionActive.value && controller.startCount == 1 }

        // Any further trigger — wake OR assist — must be swallowed while active.
        emitAssist(ts = 5_000L)
        delay(150)
        assertEquals(1, controller.startCount)
    }

    @Test
    fun assistReplay_sameTimestamp_isDeduped() = runBlocking {
        val controller = FakeController()
        val core = core(controller = controller)

        emitAssist(ts = 4_242L)
        awaitUntil("session active") { core.sessionActive.value && controller.startCount == 1 }
        // End the session so the phase guard would otherwise allow a restart.
        controller.dropConnection()
        awaitUntil("session ended") { !core.sessionActive.value }

        // replay=1 re-delivers the SAME trigger (same timestamp) — must not restart.
        emitAssist(ts = 4_242L)
        delay(150)
        assertEquals(1, controller.startCount)
    }

    @Test
    fun wakeIgnored_whenLockedSessionsDisabled_andLocked() = runBlocking {
        val controller = FakeController()
        val core = core(
            controller = controller,
            lock = FakeLock(isInteractive = false, isKeyguardLocked = true),
            lockedSessionsAllowed = false,
        )

        emitDetection()
        delay(200)
        assertFalse(core.sessionActive.value)
        assertEquals(0, controller.startCount)
    }

    @Test
    fun transportClose_tearsDown_resumesEngine() = runBlocking {
        val controller = FakeController()
        val effects = FakeEffects()
        val core = core(controller = controller, effects = effects)

        emitDetection()
        awaitUntil("session active") { core.sessionActive.value }

        controller.dropConnection()

        awaitUntil("session inactive") { !core.sessionActive.value }
        assertEquals(1, effects.wakeLockReleased.get())
        assertEquals(1, effects.focusAbandoned.get())
    }

    @Test
    fun stop_endsSession_andCallsControllerStop() = runBlocking {
        val controller = FakeController()
        val core = core(controller = controller)

        emitDetection()
        awaitUntil("session active") { core.sessionActive.value }

        core.stop()

        assertFalse(core.sessionActive.value)
        assertEquals(1, controller.stopCount)
    }

    @Test
    fun startFailure_tearsDown_leavesIdle() = runBlocking {
        val controller = FakeController(failStart = true)
        val effects = FakeEffects()
        val core = core(controller = controller, effects = effects)

        emitDetection()

        awaitUntil("teardown after failure") { !core.sessionActive.value && effects.wakeLockReleased.get() == 1 }
        assertEquals(1, effects.focusAbandoned.get())
    }
}
