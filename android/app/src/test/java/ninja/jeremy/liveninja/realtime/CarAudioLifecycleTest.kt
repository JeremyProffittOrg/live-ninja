package ninja.jeremy.liveninja.realtime

import android.media.AudioDeviceInfo
import java.util.Collections
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.Job
import kotlinx.coroutines.async
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.currentTime
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.withTimeout
import kotlinx.coroutines.withTimeoutOrNull
import ninja.jeremy.liveninja.wake.WakeRunMode
import ninja.jeremy.liveninja.wake.decideWakeRunMode
import ninja.jeremy.liveninja.wake.shouldForwardWakeDetection
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class CarAudioLifecycleTest {

    private class Platform : CarAudioPlatform {
        override val sdkInt = 33
        @Volatile var foreground = true
        var micGranted = true
        var bluetoothGranted = true
        var routeGate: CompletableDeferred<Boolean>? = null
        @Volatile var onDisconnect: (() -> Unit)? = null
        @Volatile var focusLoss: (() -> Unit)? = null
        val calls: MutableList<String> = Collections.synchronizedList(mutableListOf())

        override fun isForegroundAllowed() = foreground
        override fun hasMicPermission() = micGranted
        override fun hasBluetoothConnectPermission() = bluetoothGranted
        override fun isCallAudioBusy() = false
        override fun communicationCandidates() =
            listOf(CarAudioDevice(11, AudioDeviceInfo.TYPE_BLUETOOTH_SCO, "Car"))
        override fun requestFocus(onFocusLoss: () -> Unit): Boolean {
            calls += "focus"
            focusLoss = onFocusLoss
            return true
        }
        override fun abandonFocus() { calls += "abandon" }
        override fun enterCommunicationMode() { calls += "mode" }
        override fun restoreCommunicationMode() { calls += "restoreMode" }
        override suspend fun connectRoute(device: CarAudioDevice, onRouteLost: () -> Unit): Boolean {
            calls += "route"
            return routeGate?.await() ?: true
        }
        override fun disconnectRoute() {
            calls += "unroute"
            onDisconnect?.invoke()
        }
        fun count(name: String) = synchronized(calls) { calls.count { it == name } }
        fun snapshot() = synchronized(calls) { calls.toList() }
    }

    private class ServiceActions : CarAudioServiceController.Actions {
        var micGranted = true
        var allowForeground = true
        val log = mutableListOf<String>()
        override fun hasMicPermission() = micGranted
        override fun enterForeground(token: Long): Boolean {
            log += "fg:$token"
            return allowForeground
        }
        override fun satisfyForegroundContract() { log += "contract" }
        override fun leaveForeground() { log += "leave" }
        override fun stopSelf(startId: Int) { log += "stop:$startId" }
    }

    /** Real broker + service controller + starter + core; only Android effects are faked. */
    private class Rig(setup: Platform.() -> Unit = {}) {
        val platform = Platform().apply(setup)
        val owner = CarAudioRouteOwner()
        val broker = CarAudioForegroundBroker()
        val actions = ServiceActions()
        val controller = CarAudioServiceController(broker, actions)
        /** Stop requests posted to the service's main thread, delivered on demand. */
        val queuedStops = mutableListOf<Long>()
        val launched = mutableListOf<Long>()
        var deliverStart = true
        var launchAllowed = true
        var audioUntouchedAtLaunch: Boolean? = null
        var onLaunch: () -> Unit = {}
        private var startIds = 0

        val starter = CarAudioSessionStarter(
            CarAudioSessionCore(platform, owner),
            broker,
            CarAudioForegroundLauncher { token ->
                launched += token
                audioUntouchedAtLaunch = platform.snapshot().isEmpty()
                onLaunch()
                if (launchAllowed && deliverStart) controller.onStartRequest(token, nextStartId())
                launchAllowed
            },
            READY_TIMEOUT_MS,
        )

        init {
            broker.addStopListener { queuedStops += it }
        }

        fun nextStartId() = ++startIds

        fun deliverQueuedStops() {
            val stops = queuedStops.toList()
            queuedStops.clear()
            stops.forEach(controller::onStopRequested)
        }
    }

    private suspend fun failureOf(block: suspend () -> Unit): CarAudioFailure? =
        try {
            block()
            null
        } catch (e: CarAudioException) {
            e.failure
        }

    // ---- foreground readiness ----

    @Test
    fun start_waitsForVerifiedForegroundService_beforeTouchingCarAudio() = runTest {
        val rig = Rig()
        // Tapped in the visible app, then backgrounded while the service came up:
        // only the verified session service may vouch for foreground.
        rig.onLaunch = { rig.platform.foreground = false }

        val handle = rig.starter.start(onEnd = {}, onInterrupted = {})

        assertEquals(listOf(1L), rig.launched)
        assertEquals(true, rig.audioUntouchedAtLaunch)
        assertTrue(rig.broker.isForeground(handle.ticket.token))
        assertEquals(1, rig.platform.count("focus"))
        assertEquals(listOf("fg:1"), rig.actions.log)
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, rig.owner.audioPlan())

        handle.release()
        handle.release()
        assertEquals(1, rig.platform.count("unroute"))
        assertEquals(1, rig.platform.count("abandon"))
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, rig.owner.audioPlan())
        assertEquals(listOf(1L), rig.queuedStops)
        rig.deliverQueuedStops()
        assertEquals(listOf("fg:1", "leave", "stop:1"), rig.actions.log)
        assertNull(rig.controller.ownedToken)
    }

    @Test
    fun backgroundStart_neverLaunchesServiceOrTouchesAudio() = runTest {
        val rig = Rig { foreground = false }
        assertEquals(CarAudioFailure.NOT_FOREGROUND, failureOf { rig.starter.start({}, {}) })
        assertTrue(rig.launched.isEmpty())
        assertTrue(rig.platform.snapshot().isEmpty())
        assertTrue(rig.queuedStops.isEmpty())
    }

    @Test
    fun missingRuntimePermissions_neverLaunchService() = runTest {
        val noBluetooth = Rig { bluetoothGranted = false }
        assertEquals(CarAudioFailure.BLUETOOTH_PERMISSION, failureOf { noBluetooth.starter.start({}, {}) })
        assertTrue(noBluetooth.launched.isEmpty())

        val noMic = Rig { micGranted = false }
        assertEquals(CarAudioFailure.MIC_PERMISSION, failureOf { noMic.starter.start({}, {}) })
        assertTrue(noMic.launched.isEmpty())
        assertTrue(noMic.platform.snapshot().isEmpty())
    }

    @Test
    fun foregroundRefusedByAndroid_failsClosed_neverTouchesAudio() = runTest {
        val rig = Rig()
        rig.actions.allowForeground = false

        assertEquals(CarAudioFailure.FOREGROUND_DENIED, failureOf { rig.starter.start({}, {}) })

        assertTrue(rig.platform.snapshot().isEmpty())
        assertEquals(listOf("fg:1", "contract", "leave", "stop:1"), rig.actions.log)
        assertFalse(rig.broker.isCurrent(1L))
        assertEquals(listOf(1L), rig.queuedStops)
    }

    @Test
    fun serviceWithoutMicGrant_neverGoesForeground() = runTest {
        val rig = Rig()
        rig.actions.micGranted = false
        assertEquals(CarAudioFailure.FOREGROUND_DENIED, failureOf { rig.starter.start({}, {}) })
        assertEquals(listOf("contract", "leave", "stop:1"), rig.actions.log)
        assertTrue(rig.platform.snapshot().isEmpty())
    }

    @Test
    fun launcherRefused_failsClosed() = runTest {
        val rig = Rig().apply { launchAllowed = false }
        assertEquals(CarAudioFailure.FOREGROUND_DENIED, failureOf { rig.starter.start({}, {}) })
        assertEquals(listOf(1L), rig.queuedStops)
        assertTrue(rig.platform.snapshot().isEmpty())
    }

    @Test
    fun readinessWait_isBounded_andLateStartStopsItself() = runTest {
        val rig = Rig().apply { deliverStart = false }
        val result = async { failureOf { rig.starter.start({}, {}) } }
        runCurrent()
        assertFalse(result.isCompleted)

        advanceTimeBy(READY_TIMEOUT_MS + 1)

        assertEquals(CarAudioFailure.FOREGROUND_DENIED, result.await())
        assertTrue(rig.platform.snapshot().isEmpty())
        assertEquals(listOf(1L), rig.queuedStops)
        // The start command finally arrives for the abandoned token.
        rig.controller.onStartRequest(1L, rig.nextStartId())
        assertEquals(listOf("contract", "leave", "stop:1"), rig.actions.log)
        assertNull(rig.controller.ownedToken)
    }

    // ---- End / cancellation ----

    @Test
    fun notificationEnd_cancelsPendingStart_promptly_andReleasesLifecycleLock() = runTest {
        val rig = Rig().apply { deliverStart = false }
        val lifecycle = Mutex()
        var endCalls = 0
        var startJob: Job? = null
        startJob = launch {
            lifecycle.withLock {
                rig.starter.start(
                    onEnd = {
                        endCalls++
                        startJob?.cancel()
                    },
                    onInterrupted = {},
                )
            }
        }
        runCurrent()
        assertTrue(lifecycle.isLocked)
        assertEquals(listOf(1L), rig.launched)

        rig.controller.onEndRequest(1L, rig.nextStartId())
        runCurrent()

        assertTrue(startJob.isCancelled)
        assertFalse(lifecycle.isLocked)
        assertEquals(0L, currentTime)
        assertEquals(1, endCalls)
        assertTrue(rig.platform.snapshot().isEmpty())
        assertEquals(listOf(1L), rig.queuedStops)
        assertFalse(rig.broker.isCurrent(1L))

        rig.controller.onEndRequest(1L, rig.nextStartId())
        assertEquals(1, endCalls)
    }

    @Test
    fun endDuringPendingStart_failsWithEndedByUser_withoutWaitingForTimeout() = runTest {
        val rig = Rig().apply { deliverStart = false }
        var endCalls = 0
        val result = async { failureOf { rig.starter.start(onEnd = { endCalls++ }, onInterrupted = {}) } }
        runCurrent()

        rig.controller.onEndRequest(1L, rig.nextStartId())

        assertEquals(CarAudioFailure.ENDED_BY_USER, result.await())
        assertEquals(0L, currentTime)
        assertEquals(1, endCalls)
        assertTrue(rig.platform.snapshot().isEmpty())
    }

    @Test
    fun cancellationDuringRouteHandshake_restoresAudio_andStopsService() = runTest {
        val rig = Rig { routeGate = CompletableDeferred() }
        val job = launch { rig.starter.start({}, {}) }
        runCurrent()
        assertEquals(1, rig.platform.count("route"))

        job.cancelAndJoin()

        assertEquals(1, rig.platform.count("unroute"))
        assertEquals(1, rig.platform.count("restoreMode"))
        assertEquals(1, rig.platform.count("abandon"))
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, rig.owner.audioPlan())
        assertEquals(listOf(1L), rig.queuedStops)
        rig.deliverQueuedStops()
        assertEquals(listOf("fg:1", "leave", "stop:1"), rig.actions.log)
    }

    // ---- generation safety ----

    @Test
    fun staleStopStartAndEnd_neverAffectReplacementService() = runTest {
        val rig = Rig()
        var secondEnds = 0
        val first = rig.starter.start({}, {})
        first.release()
        val staleStops = rig.queuedStops.toList()
        rig.queuedStops.clear()

        val second = rig.starter.start({ secondEnds++ }, {})
        assertEquals(2L, rig.controller.ownedToken)
        val before = rig.actions.log.toList()

        staleStops.forEach(rig.controller::onStopRequested) // late main-thread stop for token 1
        rig.controller.onStartRequest(1L, rig.nextStartId()) // late duplicate start for token 1
        rig.controller.onEndRequest(1L, rig.nextStartId()) // End tapped on the old notification

        assertEquals(before, rig.actions.log)
        assertEquals(0, secondEnds)
        assertEquals(2L, rig.controller.ownedToken)
        assertTrue(rig.broker.isForeground(2L))
        assertNull(second.interruption)

        second.release()
        rig.deliverQueuedStops()
        assertEquals(listOf("leave", "stop:4"), rig.actions.log.takeLast(2))
    }

    @Test
    fun taskRemoved_endsSession_thenStopCleansUpOnce() = runTest {
        val rig = Rig()
        var ends = 0
        var handle: CarAudioSessionHandle? = null
        // Coordinator stand-in: on End it shuts the transport down, then releases.
        handle = rig.starter.start(onEnd = {
            ends++
            handle?.release()
        }, onInterrupted = {})

        rig.controller.onTaskRemoved()

        assertEquals(1, ends)
        assertEquals(1, rig.platform.count("unroute"))
        assertEquals(1, rig.platform.count("abandon"))
        rig.deliverQueuedStops()
        assertEquals(listOf("fg:1", "leave", "stop:1"), rig.actions.log)
        rig.controller.onDestroy()
        assertEquals(1, ends)
    }

    @Test
    fun serviceDestroyedWhileLive_endsThatSessionOnce() = runTest {
        val rig = Rig()
        var ends = 0
        rig.starter.start({ ends++ }, {})
        rig.controller.onDestroy()
        rig.controller.onDestroy()
        assertEquals(1, ends)
        assertFalse(rig.broker.isForeground(1L))
    }

    @Test
    fun staleInterruptionCallback_cannotMuteReplacementSession() = runTest {
        val rig = Rig()
        val guard = SessionIdentityGuard<Any>()
        val a = Any()
        val b = Any()
        val mutes = mutableListOf<String>()
        guard.set(a)
        val first = rig.starter.start({}, { guard.withCurrent(a) { mutes += "A" } })
        val staleFocusLoss = rig.platform.focusLoss!!
        first.release()
        guard.set(null)

        guard.set(b)
        val second = rig.starter.start({}, { guard.withCurrent(b) { mutes += "B" } })

        staleFocusLoss()
        assertTrue(mutes.isEmpty())
        assertNull(second.interruption)
        // A callback captured for A, delivered now, is refused by identity.
        assertNull(guard.withCurrent(a) { mutes += "A" })

        rig.platform.focusLoss!!.invoke()
        assertEquals(listOf("B"), mutes)
        assertEquals(CarAudioFailure.FOCUS_LOST, second.interruption)
        second.release()
    }

    @Test
    fun replacementAcquire_waitsForPhysicalCleanupOfPrevious() = runBlocking {
        val p = Platform()
        val owner = CarAudioRouteOwner()
        val core = CarAudioSessionCore(p, owner)
        val first = core.acquire {}
        val entered = CountDownLatch(1)
        val proceed = CountDownLatch(1)
        p.onDisconnect = {
            entered.countDown()
            proceed.await(5, TimeUnit.SECONDS)
        }
        val releaser = thread { first.release() }
        assertTrue(entered.await(5, TimeUnit.SECONDS))
        p.onDisconnect = null

        val second = async(Dispatchers.Default) { core.acquire {} }
        assertNull(withTimeoutOrNull(300) { second.await() })
        assertEquals(1, p.count("focus"))

        proceed.countDown()
        val lease = withTimeout(5_000) { second.await() }
        releaser.join()

        val order = p.snapshot()
        assertTrue(order.indexOf("abandon") < order.lastIndexOf("focus"))
        assertEquals(TransportAudioPlan.CAR_SESSION_OWNED, owner.audioPlan())
        lease.release()
        assertEquals(TransportAudioPlan.LEGACY_TRANSPORT_ROUTING, owner.audioPlan())
    }

    // ---- wake suppression in car mode ----

    @Test
    fun carAudioEnabled_keepsWakeRecorderStopped_andDropsDetections() {
        assertEquals(WakeRunMode.CAR_AUDIO, decideWakeRunMode(false, true, false, false))
        // Degraded power or unmute never restart the recorder while car mode is on.
        assertEquals(WakeRunMode.CAR_AUDIO, decideWakeRunMode(false, true, false, true))
        assertEquals(WakeRunMode.CAR_AUDIO, decideWakeRunMode(false, true, true, false))
        // A session (e.g. after focus regain) ending keeps it stopped.
        assertEquals(WakeRunMode.SESSION, decideWakeRunMode(true, true, false, false))
        assertEquals(WakeRunMode.CAR_AUDIO, decideWakeRunMode(false, true, false, false))
        assertFalse(shouldForwardWakeDetection(carAudioEnabled = true))
    }

    @Test
    fun carAudioDisabled_preservesExistingWakeBehaviour() {
        assertEquals(WakeRunMode.CONTINUOUS, decideWakeRunMode(false, false, false, false))
        assertEquals(WakeRunMode.DUTY_CYCLE, decideWakeRunMode(false, false, false, true))
        assertEquals(WakeRunMode.MUTED, decideWakeRunMode(false, false, true, false))
        assertEquals(WakeRunMode.SESSION, decideWakeRunMode(true, false, false, false))
        assertTrue(shouldForwardWakeDetection(carAudioEnabled = false))
    }

    private companion object {
        const val READY_TIMEOUT_MS = 5_000L
    }
}
