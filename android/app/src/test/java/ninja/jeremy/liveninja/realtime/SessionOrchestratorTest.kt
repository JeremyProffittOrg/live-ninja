package ninja.jeremy.liveninja.realtime

import java.io.IOException
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import ninja.jeremy.liveninja.assistant.AssistSource
import ninja.jeremy.liveninja.assistant.AssistTrigger
import ninja.jeremy.liveninja.audio.WakeWordDetection
import ninja.jeremy.liveninja.ui.state.RealtimeSessionController
import ninja.jeremy.liveninja.ui.state.SessionUiEvent
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * [SessionOrchestratorCore] state machine — wake/assist → session handoff, the
 * duplicate guard (the zero-collector fix), AssistantEvents replay dedupe, the
 * locked/asleep gate, and teardown on stop / transport close. Fake flows,
 * controller, effects, lock-state, and clock; no Android.
 *
 * All tests run on a [StandardTestDispatcher] backing both the test coroutine
 * and [SessionOrchestratorCore]'s internal `scope` (same [kotlinx.coroutines.test.TestCoroutineScheduler]
 * in both, via [core]'s `scope` param). `advanceUntilIdle()` deterministically
 * drains every launched/queued coroutine — collectors starting up, the
 * beginSession/launchStart/teardown chain, dedupe checks — before each
 * assertion. This replaces an earlier design that polled real wall-clock time
 * with `delay()` inside fixed-timeout loops: correct in principle, but flaky
 * under CI/parallel-test-run CPU contention where `Dispatchers.Default`
 * scheduling could lag behind the real-time margins the assertions raced
 * against (both `assistReplay_sameTimestamp_isDeduped` and, occasionally under
 * heavy load, `transportClose_tearsDown_resumesEngine` were observed to time
 * out this way when the full suite ran in parallel). A virtual scheduler has
 * no wall clock to race.
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

    /**
     * Builds a [SessionOrchestratorCore] and [SessionOrchestratorCore.bind]s it to the
     * shared [detections]/[triggers] flows. `scope` must be backed by the same
     * [kotlinx.coroutines.test.TestCoroutineScheduler] as the caller's `runTest` block
     * so `advanceUntilIdle()` drives both the test body and the core's internal
     * coroutines deterministically.
     */
    private fun core(
        scope: CoroutineScope,
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
        scope = scope,
    ).also { it.bind(detections, triggers) }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun wakeDetection_startsSession_firesEffects_andEmitsUiTrigger() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val controller = FakeController()
        val effects = FakeEffects()
        val core = core(scope, controller = controller, effects = effects)
        advanceUntilIdle() // let bind()'s collectors start and park

        detections.emit(WakeWordDetection("hey live ninja", 0.9f, clockValue))
        advanceUntilIdle()

        assertTrue(core.sessionActive.value)
        assertEquals(1, controller.startCount)
        assertEquals(1, effects.wakeLockAcquired.get())
        assertEquals(1, effects.focusRequested.get())
        assertEquals(1, effects.earcons.get())
        // A WAKE_WORD assist trigger is surfaced for the UI (navigation + KeyguardGate).
        assertTrue(emittedTriggers.any { it.source == AssistSource.WAKE_WORD })
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun secondTriggerWhileActive_isIgnored_duplicateGuard() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val controller = FakeController()
        val core = core(scope, controller = controller)
        advanceUntilIdle()

        detections.emit(WakeWordDetection("hey live ninja", 0.9f, clockValue))
        advanceUntilIdle()
        assertTrue(core.sessionActive.value)
        assertEquals(1, controller.startCount)

        // Any further trigger — wake OR assist — must be swallowed while active.
        triggers.emit(AssistTrigger(AssistSource.MANUAL, launchedWhileLocked = false, timestampMillis = 5_000L))
        advanceUntilIdle()
        assertEquals(1, controller.startCount)
    }

    /**
     * Previously asserted "no restart" via a fixed real-time `delay(150)` margin after
     * the replayed trigger — flaky under CI/thread-pool contention because it raced
     * real wall-clock time against `Dispatchers.Default` scheduling instead of proving
     * the dedupe path had actually finished processing. See the class doc for the
     * deterministic replacement used across this whole file.
     */
    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun assistReplay_sameTimestamp_isDeduped() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val controller = FakeController()
        val core = core(scope, controller = controller)
        advanceUntilIdle()

        triggers.emit(AssistTrigger(AssistSource.MANUAL, launchedWhileLocked = false, timestampMillis = 4_242L))
        advanceUntilIdle()
        assertTrue(core.sessionActive.value)
        assertEquals(1, controller.startCount)

        // End the session so the phase guard would otherwise allow a restart.
        controller.dropConnection()
        advanceUntilIdle()
        assertFalse(core.sessionActive.value)

        // replay=1 re-delivers the SAME trigger (same timestamp) — must not restart.
        triggers.emit(AssistTrigger(AssistSource.MANUAL, launchedWhileLocked = false, timestampMillis = 4_242L))
        advanceUntilIdle()
        assertEquals(1, controller.startCount)
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun wakeIgnored_whenLockedSessionsDisabled_andLocked() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val controller = FakeController()
        val core = core(
            scope,
            controller = controller,
            lock = FakeLock(isInteractive = false, isKeyguardLocked = true),
            lockedSessionsAllowed = false,
        )
        advanceUntilIdle()

        detections.emit(WakeWordDetection("hey live ninja", 0.9f, clockValue))
        advanceUntilIdle()

        assertFalse(core.sessionActive.value)
        assertEquals(0, controller.startCount)
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun transportClose_tearsDown_resumesEngine() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val controller = FakeController()
        val effects = FakeEffects()
        val core = core(scope, controller = controller, effects = effects)
        advanceUntilIdle()

        detections.emit(WakeWordDetection("hey live ninja", 0.9f, clockValue))
        advanceUntilIdle()
        assertTrue(core.sessionActive.value)

        controller.dropConnection()
        advanceUntilIdle()

        assertFalse(core.sessionActive.value)
        assertEquals(1, effects.wakeLockReleased.get())
        assertEquals(1, effects.focusAbandoned.get())
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun stop_endsSession_andCallsControllerStop() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val controller = FakeController()
        val core = core(scope, controller = controller)
        advanceUntilIdle()

        detections.emit(WakeWordDetection("hey live ninja", 0.9f, clockValue))
        advanceUntilIdle()
        assertTrue(core.sessionActive.value)

        core.stop()
        advanceUntilIdle()

        assertFalse(core.sessionActive.value)
        assertEquals(1, controller.stopCount)
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun startFailure_tearsDown_leavesIdle() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val controller = FakeController(failStart = true)
        val effects = FakeEffects()
        val core = core(scope, controller = controller, effects = effects)
        advanceUntilIdle()

        detections.emit(WakeWordDetection("hey live ninja", 0.9f, clockValue))
        advanceUntilIdle()

        assertFalse(core.sessionActive.value)
        assertEquals(1, effects.wakeLockReleased.get())
        assertEquals(1, effects.focusAbandoned.get())
    }
}
