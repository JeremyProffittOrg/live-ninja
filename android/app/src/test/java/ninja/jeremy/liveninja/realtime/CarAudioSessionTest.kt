package ninja.jeremy.liveninja.realtime

import android.media.AudioDeviceInfo
import android.media.AudioManager
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
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
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class CarAudioSessionTest {

    private class FakePlatform : CarAudioPlatform {
        override var sdkInt = 33
        var foreground = true
        var micGranted = true
        var bluetoothGranted = true
        var callBusy = false
        var candidates = listOf(CarAudioDevice(11, AudioDeviceInfo.TYPE_BLUETOOTH_SCO, "Car"))
        var grantFocus = true
        var routeGate: CompletableDeferred<Boolean>? = null
        var immediateRoute = true
        var loseRouteDuringConnect = false
        val calls = mutableListOf<String>()
        var focusLoss: (() -> Unit)? = null
        var routeLost: (() -> Unit)? = null

        override fun isForegroundAllowed() = foreground
        override fun hasMicPermission() = micGranted
        override fun hasBluetoothConnectPermission() = bluetoothGranted
        override fun isCallAudioBusy() = callBusy
        override fun communicationCandidates() = candidates
        override fun requestFocus(onFocusLoss: () -> Unit): Boolean {
            calls += "focus"
            focusLoss = onFocusLoss
            return grantFocus
        }
        override fun abandonFocus() { calls += "abandon" }
        override fun enterCommunicationMode() { calls += "mode" }
        override fun restoreCommunicationMode() { calls += "restoreMode" }
        override suspend fun connectRoute(device: CarAudioDevice, onRouteLost: () -> Unit): Boolean {
            calls += "route"
            routeLost = onRouteLost
            if (loseRouteDuringConnect) onRouteLost()
            return routeGate?.await() ?: immediateRoute
        }
        override fun disconnectRoute() { calls += "unroute" }
        fun count(name: String) = calls.count { it == name }
    }

    private suspend fun failureOf(block: suspend () -> Unit): CarAudioFailure? =
        try {
            block()
            null
        } catch (e: CarAudioException) {
            e.failure
        }

    private fun assertFullyRestored(p: FakePlatform, owner: CarAudioRouteOwner) {
        assertEquals(1, p.count("unroute"))
        assertEquals(1, p.count("restoreMode"))
        assertEquals(1, p.count("abandon"))
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
        assertFalse(owner.isAudioSuppressed)
    }

    @Test
    fun focusDenied_failsClosed_withoutTouchingCallAudio() = runTest {
        val p = FakePlatform().apply { grantFocus = false }
        val owner = CarAudioRouteOwner()
        val core = CarAudioSessionCore(p, owner)

        assertEquals(CarAudioFailure.FOCUS_DENIED, failureOf { core.acquire {} })
        assertFalse("mode" in p.calls)
        assertFalse("route" in p.calls)
        assertEquals(1, p.count("abandon"))
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
    }

    @Test
    fun noHandsFreeDevice_neverFallsBackToA2dpOrSpeaker() = runTest {
        val p = FakePlatform().apply {
            candidates = listOf(
                CarAudioDevice(1, AudioDeviceInfo.TYPE_BLUETOOTH_A2DP, "Car stereo"),
                CarAudioDevice(2, AudioDeviceInfo.TYPE_BUILTIN_SPEAKER, "Speaker"),
                CarAudioDevice(3, AudioDeviceInfo.TYPE_BUILTIN_EARPIECE, "Earpiece"),
            )
        }
        val owner = CarAudioRouteOwner()
        assertEquals(CarAudioFailure.NO_DEVICE, failureOf { CarAudioSessionCore(p, owner).acquire {} })
        assertTrue(p.calls.isEmpty())
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
    }

    @Test
    fun deviceSelection_prefersScoThenBleHeadsetOnApi31Plus() {
        val sco = CarAudioDevice(7, AudioDeviceInfo.TYPE_BLUETOOTH_SCO, "Car")
        val ble = CarAudioDevice(8, AudioDeviceInfo.TYPE_BLE_HEADSET, "Buds")
        val a2dp = CarAudioDevice(9, AudioDeviceInfo.TYPE_BLUETOOTH_A2DP, "Car stereo")
        assertEquals(sco, selectCarCommunicationDevice(listOf(a2dp, ble, sco), 33))
        assertEquals(ble, selectCarCommunicationDevice(listOf(a2dp, ble), 31))
        assertNull(selectCarCommunicationDevice(listOf(a2dp, ble), 30))
        assertNull(selectCarCommunicationDevice(listOf(a2dp), 33))
    }

    @Test
    fun preconditions_failBeforeFocus_withActionableReason() = runTest {
        fun run(setup: FakePlatform.() -> Unit): Pair<CarAudioFailure?, FakePlatform> {
            val p = FakePlatform().apply(setup)
            var failure: CarAudioFailure? = null
            kotlinx.coroutines.runBlocking { failure = failureOf { CarAudioSessionCore(p, CarAudioRouteOwner()).acquire {} } }
            return failure to p
        }
        val notForeground: FakePlatform.() -> Unit = { foreground = false }
        val micDenied: FakePlatform.() -> Unit = { micGranted = false }
        val bluetoothDenied: FakePlatform.() -> Unit = { bluetoothGranted = false }
        val callActive: FakePlatform.() -> Unit = { callBusy = true }
        listOf<Pair<FakePlatform.() -> Unit, CarAudioFailure>>(
            notForeground to CarAudioFailure.NOT_FOREGROUND,
            micDenied to CarAudioFailure.MIC_PERMISSION,
            bluetoothDenied to CarAudioFailure.BLUETOOTH_PERMISSION,
            callActive to CarAudioFailure.CALL_ACTIVE,
        ).forEach { (setup, expected) ->
            val (failure, p) = run(setup)
            assertEquals(expected, failure)
            assertFalse("focus" in p.calls)
        }
    }

    @Test
    fun focusLostDuringRouteHandshake_cancelsCleanly_andNeverPublishes() = runTest {
        val p = FakePlatform().apply { routeGate = CompletableDeferred() }
        val owner = CarAudioRouteOwner()
        val core = CarAudioSessionCore(p, owner)
        val interrupted = AtomicInteger()

        val result = async { failureOf { core.acquire { interrupted.incrementAndGet() } } }
        advanceUntilIdle()
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, owner.audioPlan())

        p.focusLoss!!.invoke()
        advanceUntilIdle()

        assertEquals(CarAudioFailure.FOCUS_LOST, result.await())
        assertEquals(0, interrupted.get())
        assertFullyRestored(p, owner)
    }

    @Test
    fun routeLostBeforeAcquireReturns_isRecorded_andStartFails() = runTest {
        val p = FakePlatform().apply { loseRouteDuringConnect = true }
        val owner = CarAudioRouteOwner()
        val interrupted = AtomicInteger()

        val failure = failureOf { CarAudioSessionCore(p, owner).acquire { interrupted.incrementAndGet() } }

        assertEquals(CarAudioFailure.ROUTE_LOST, failure)
        assertEquals(0, interrupted.get())
        assertFullyRestored(p, owner)
    }

    @Test
    fun mayDuckWhileLive_endsOnce_suppressesAudio_andNeverReacquires() = runTest {
        assertTrue(isCarAudioFocusLoss(AudioManager.AUDIOFOCUS_LOSS_TRANSIENT_CAN_DUCK))
        assertTrue(isCarAudioFocusLoss(AudioManager.AUDIOFOCUS_LOSS_TRANSIENT))
        assertTrue(isCarAudioFocusLoss(AudioManager.AUDIOFOCUS_LOSS))
        assertFalse(isCarAudioFocusLoss(AudioManager.AUDIOFOCUS_GAIN))

        val p = FakePlatform()
        val owner = CarAudioRouteOwner()
        val core = CarAudioSessionCore(p, owner)
        val seen = mutableListOf<CarAudioFailure>()
        val lease = core.acquire { seen += it }
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, owner.audioPlan())
        assertFalse(owner.isAudioSuppressed)

        p.focusLoss!!.invoke()
        p.focusLoss!!.invoke()

        assertEquals(listOf(CarAudioFailure.FOCUS_LOST), seen)
        assertEquals(CarAudioFailure.FOCUS_LOST, lease.interruption)
        assertTrue(owner.isAudioSuppressed)
        // Route kept until the owner has torn the transport down.
        assertEquals(0, p.count("unroute"))

        lease.release()
        assertFullyRestored(p, owner)
        assertEquals(1, p.count("focus"))
        assertEquals(1, p.count("route"))
    }

    @Test
    fun routeLostWhileLive_interruptsWithRouteLost() = runTest {
        val p = FakePlatform()
        val owner = CarAudioRouteOwner()
        val seen = mutableListOf<CarAudioFailure>()
        val lease = CarAudioSessionCore(p, owner).acquire { seen += it }

        p.routeLost!!.invoke()
        p.focusLoss!!.invoke()

        assertEquals(listOf(CarAudioFailure.ROUTE_LOST), seen)
        assertTrue(owner.isAudioSuppressed)
        lease.release()
        assertFullyRestored(p, owner)
    }

    @Test
    fun staleCallbacksAndReleases_neverAffectReplacementSession() = runTest {
        val p = FakePlatform()
        val owner = CarAudioRouteOwner()
        val core = CarAudioSessionCore(p, owner)
        val firstHits = AtomicInteger()
        val secondHits = AtomicInteger()

        val first = core.acquire { firstHits.incrementAndGet() }
        val staleFocus = p.focusLoss!!
        val staleRoute = p.routeLost!!
        first.release()
        val second = core.acquire { secondHits.incrementAndGet() }

        staleFocus()
        staleRoute()
        first.release()

        assertEquals(0, firstHits.get())
        assertEquals(0, secondHits.get())
        assertNull(second.interruption)
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, owner.audioPlan())
        assertFalse(owner.isAudioSuppressed)
        assertEquals(1, p.count("unroute"))

        second.release()
        second.release()
        assertEquals(2, p.count("unroute"))
        assertEquals(2, p.count("restoreMode"))
        assertEquals(2, p.count("abandon"))
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
    }

    @Test
    fun leakedLease_isReclaimedByNextAcquire() = runTest {
        val p = FakePlatform()
        val owner = CarAudioRouteOwner()
        val core = CarAudioSessionCore(p, owner)
        val leaked = core.acquire {}
        val next = core.acquire {}
        assertEquals(1, p.count("unroute"))
        leaked.release()
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, owner.audioPlan())
        next.release()
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
    }

    @Test
    fun cancellationDuringHandshake_restoresEverything_andAllowsRetry() = runTest {
        val p = FakePlatform().apply { routeGate = CompletableDeferred() }
        val owner = CarAudioRouteOwner()
        val core = CarAudioSessionCore(p, owner)
        val interrupted = AtomicInteger()

        val job = launch { core.acquire { interrupted.incrementAndGet() } }
        advanceUntilIdle()
        job.cancelAndJoin()

        assertFullyRestored(p, owner)
        assertEquals(0, interrupted.get())

        p.routeGate = null
        val lease = core.acquire {}
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, owner.audioPlan())
        lease.release()
    }

    @Test
    fun routeNotEstablished_failsWithRouteFailed_andRestoresMode() = runTest {
        val p = FakePlatform().apply { immediateRoute = false }
        val owner = CarAudioRouteOwner()
        assertEquals(CarAudioFailure.ROUTE_FAILED, failureOf { CarAudioSessionCore(p, owner).acquire {} })
        assertFullyRestored(p, owner)
    }

    @Test
    fun routingSelection_legacyUnlessCarOwned_andStaleReleaseIgnored() {
        val owner = CarAudioRouteOwner()
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
        owner.claim(1)
        owner.claim(2)
        assertFalse(owner.release(1))
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, owner.audioPlan())
        assertFalse(owner.suppress(1))
        assertFalse(owner.isAudioSuppressed)
        assertTrue(owner.release(2))
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
    }

    @Test
    fun routeHandshake_requiresObservedRouteStillPresent_andReportsLossOnce() = runTest {
        var lost = 0
        val early = RouteHandshake { lost++ }
        early.observed()
        early.routeEnded(failIfUnseen = false)
        assertFalse(early.awaitEstablished(1_000))
        assertEquals(0, lost)

        val live = RouteHandshake { lost++ }
        live.observed()
        assertTrue(live.awaitEstablished(1_000))
        live.routeEnded(failIfUnseen = false)
        live.routeEnded(failIfUnseen = false)
        assertEquals(1, lost)

        val failed = RouteHandshake {}
        failed.routeEnded(failIfUnseen = true)
        assertFalse(failed.awaitEstablished(5_000))

        assertFalse(RouteHandshake {}.awaitEstablished(5_000)) // bounded timeout
    }

    // ---- orchestrator in car mode ----

    private class FakeController : RealtimeSessionController {
        private val _connected = MutableStateFlow(false)
        override val connected: StateFlow<Boolean> = _connected
        override val events: Flow<SessionUiEvent> = MutableSharedFlow()
        var startCount = 0
        override suspend fun start() {
            startCount++
            _connected.value = true
        }
        override suspend fun stop() { _connected.value = false }
        override fun setMicMuted(muted: Boolean) = Unit
        override fun interruptAssistant() = Unit
        override fun sendUserText(text: String) = Unit
        fun drop() { _connected.value = false }
    }

    private class FakeEffects : SessionEffects {
        val focus = AtomicInteger()
        val abandon = AtomicInteger()
        val earcons = AtomicInteger()
        override fun acquireWakeLock() = Unit
        override fun releaseWakeLock() = Unit
        override fun requestAudioFocus() { focus.incrementAndGet() }
        override fun abandonAudioFocus() { abandon.incrementAndGet() }
        override fun playEarcon() { earcons.incrementAndGet() }
    }

    @Test
    fun orchestratorCarMode_ignoresWake_noEarconOrFocus_noAutoRestart() = runTest {
        val scope = CoroutineScope(SupervisorJob() + StandardTestDispatcher(testScheduler))
        val detections = MutableSharedFlow<WakeWordDetection>(extraBufferCapacity = 8)
        val triggers = MutableSharedFlow<AssistTrigger>(extraBufferCapacity = 8)
        val controller = FakeController()
        val effects = FakeEffects()
        val emitted = mutableListOf<AssistTrigger>()
        val core = SessionOrchestratorCore(
            controller = controller,
            effects = effects,
            lockState = object : DeviceLockState {
                override val isInteractive = true
                override val isKeyguardLocked = false
            },
            emitAssistTrigger = { emitted += it },
            lockedSessionsAllowed = { true },
            clock = { 1_000L },
            scope = scope,
            carAudioSelected = { true },
        ).also { it.bind(detections, triggers) }
        advanceUntilIdle()

        detections.emit(WakeWordDetection("hey live ninja", 0.9f, 1_000L))
        advanceUntilIdle()
        assertEquals(0, controller.startCount)
        assertTrue(emitted.isEmpty())

        triggers.emit(AssistTrigger(AssistSource.MANUAL, launchedWhileLocked = false, timestampMillis = 5_000L))
        advanceUntilIdle()
        assertEquals(1, controller.startCount)
        assertEquals(0, effects.earcons.get())
        assertEquals(0, effects.focus.get())

        controller.drop()
        advanceUntilIdle()
        assertFalse(core.sessionActive.value)
        assertEquals(0, effects.abandon.get())
        assertEquals(1, controller.startCount)
    }
}
