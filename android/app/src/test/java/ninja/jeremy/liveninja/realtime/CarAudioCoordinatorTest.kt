package ninja.jeremy.liveninja.realtime

import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.every
import io.mockk.mockk
import io.mockk.verify
import java.io.IOException
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference
import kotlin.concurrent.thread
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.ui.settings.CarAudioModeController
import ninja.jeremy.liveninja.ui.settings.CarAudioPermissionOutcome
import ninja.jeremy.liveninja.ui.settings.CarAudioToggleDecision
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Car-audio orchestration through the REAL [RealtimeSessionCoordinator]
 * (fixture pattern from DeviceMediaCoordinatorTest) with a mocked
 * [CarAudioSessionManager] handing out real leases/handles over a real
 * [CarAudioForegroundBroker]. Deferred barriers, no sleeps.
 */
class CarAudioCoordinatorTest {

    private class FakeTransport : RealtimeTransport {
        private val _state = MutableStateFlow(TransportState.IDLE)
        override val state: StateFlow<TransportState> = _state
        private val _events = MutableSharedFlow<RealtimeEvent>(extraBufferCapacity = 64)
        override val events: SharedFlow<RealtimeEvent> = _events
        override var halfDuplex: Boolean = false

        val prepareCalls = AtomicInteger()
        val abortPrepareCalls = AtomicInteger()
        val connectCalls = AtomicInteger()
        val disconnects = AtomicInteger()
        val micMuted = CopyOnWriteArrayList<Boolean>()
        val stopPlaybackCalls = AtomicInteger()
        @Volatile var connectEntered: CompletableDeferred<Unit>? = null
        @Volatile var connectGate: CompletableDeferred<Unit>? = null
        @Volatile var connectFailure: Throwable? = null
        @Volatile var disconnectFailure: Throwable? = null

        override fun prime(session: RealtimeSession) = Unit
        override fun preWarm() = Unit
        override fun prepare() {
            prepareCalls.incrementAndGet()
        }

        override suspend fun abortPrepare() {
            abortPrepareCalls.incrementAndGet()
        }

        override suspend fun connect(ephemeralToken: String, callsUrl: String) {
            connectCalls.incrementAndGet()
            connectEntered?.complete(Unit)
            connectGate?.await()
            connectFailure?.let { throw it }
            _state.value = TransportState.CONNECTED
        }

        override fun sendEvent(event: JSONObject) = Unit

        override fun setMicMuted(muted: Boolean) {
            micMuted += muted
        }

        override fun stopPlayback() {
            stopPlaybackCalls.incrementAndGet()
        }

        override suspend fun disconnect() {
            disconnects.incrementAndGet()
            disconnectFailure?.let { throw it }
            _state.value = TransportState.CLOSED
        }
    }

    private val transport = FakeTransport()
    private val novaTransport = FakeTransport()
    private val geminiTransport = FakeTransport()
    private val voiceLiveTransport = FakeTransport()
    private val authState = MutableStateFlow<AuthState>(AuthState.SignedIn("dummy-auth-a"))
    private val auth = mockk<AuthRepository> { every { state } returns authState }
    private val sessionApi = mockk<RealtimeSessionApi>()
    private val broker = CarAudioForegroundBroker()
    private val leaseReleases = AtomicInteger()
    private val interrupts = CopyOnWriteArrayList<(CarAudioFailure) -> Unit>()
    private val carSelected = AtomicBoolean(true)
    private val carAudio = mockk<CarAudioSessionManager>()
    @Volatile private var carStartFailure: CarAudioFailure? = null

    private fun session() = RealtimeSession(
        clientSecret = "dummy-ephemeral-token-not-a-secret",
        expiresAt = null,
        model = "gpt-realtime",
        voice = "cedar",
        sessionId = "rs-car",
        quotaWarning = null,
    )

    private fun coordinator(): RealtimeSessionCoordinator {
        coEvery { sessionApi.fetchSession(any()) } returns session()
        every { carAudio.isSelected } answers { carSelected.get() }
        coEvery { carAudio.startSession(any(), any()) } coAnswers {
            carStartFailure?.let { throw CarAudioException(it) }
            val onEnd = firstArg<() -> Unit>()
            interrupts += secondArg<(CarAudioFailure) -> Unit>()
            val ticket = broker.open(onEnd)
            val lease = CarAudioLease(
                ticket.token,
                CarAudioDevice(1, 7, "synthetic-car-hfp"),
                { null },
                { leaseReleases.incrementAndGet() },
            )
            CarAudioSessionHandle(ticket, lease, broker)
        }
        val coord = RealtimeSessionCoordinator(
            mockk<android.content.Context>(relaxed = true),
            transport, novaTransport, geminiTransport, voiceLiveTransport, sessionApi,
            mockk<ToolCallRouter>(), mockk<DeviceVolumeToolExecutor>(), mockk<DeviceCameraToolExecutor>(),
            TranscriptStore(), TranscriptUploader(NoopTranscriptSink, CoroutineScope(SupervisorJob())), auth,
        )
        coord.carAudio = carAudio
        return coord
    }

    /** Preferences mock backed by [carSelected] (what carAudio.isSelected reports). */
    private fun mockPreferences(writes: MutableList<Boolean> = CopyOnWriteArrayList()): CarAudioPreferences {
        val prefs = mockk<CarAudioPreferences>()
        every { prefs.isEnabled } answers { carSelected.get() }
        every { prefs.setEnabled(any()) } answers {
            val value = firstArg<Boolean>()
            writes += value
            carSelected.set(value)
        }
        return prefs
    }

    /** True when [trace] shows the thread inside a SessionBusyGate entry point. */
    private fun isInBusyGate(trace: Array<StackTraceElement>): Boolean =
        trace.any { frame ->
            frame.className.contains("SessionBusyGate") &&
                (frame.methodName.startsWith("markBusyAndRead") || frame.methodName.startsWith("runIfIdle"))
        }

    /**
     * Barrier without sleeps: spins (yielding) until [t] is BLOCKED on the
     * coordinator's session gate monitor, failing on timeout or thread exit.
     * Uses only java.lang.Thread APIs: the thread must be BLOCKED, its stack
     * must be inside SessionBusyGate.markBusyAndRead/runIfIdle, and it must
     * still be BLOCKED after the stack was sampled.
     */
    private fun awaitBlockedOnBusyGate(t: Thread) {
        val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5)
        while (true) {
            if (t.state == Thread.State.BLOCKED) {
                val trace = t.stackTrace
                if (isInBusyGate(trace) && t.state == Thread.State.BLOCKED) return
            }
            check(t.isAlive) { "thread finished without blocking on the session gate" }
            check(System.nanoTime() < deadline) { "timed out waiting for the session gate" }
            Thread.yield()
        }
    }

    @Test
    fun cancelledDuringFetch_abortsPrepareReleasesLeaseAndResetsBusy() = runBlocking {
        val coord = coordinator()
        val fetchEntered = CompletableDeferred<Unit>()
        val fetchGate = CompletableDeferred<RealtimeSession>()
        coEvery { sessionApi.fetchSession(any()) } coAnswers {
            fetchEntered.complete(Unit)
            fetchGate.await()
        }
        val start = launch(Dispatchers.Default) { coord.start() }
        withTimeout(5_000) { fetchEntered.await() }
        assertTrue(coord.sessionBusy.value)
        assertEquals(1, interrupts.size)

        start.cancelAndJoin()

        assertEquals(1, leaseReleases.get())
        assertEquals(1, transport.prepareCalls.get())
        assertEquals(1, transport.abortPrepareCalls.get())
        assertEquals(0, transport.connectCalls.get())
        assertFalse(coord.sessionBusy.value)
        assertFalse(coord.connected.value)
        assertFalse(broker.isCurrent(1L))
    }

    @Test
    fun carStartFailure_neverPreparesOrFetchesAndResetsBusy() = runBlocking {
        val coord = coordinator()
        carStartFailure = CarAudioFailure.NO_DEVICE
        val error = runCatching { coord.start() }.exceptionOrNull()
        assertTrue(error is CarAudioException)
        assertEquals(CarAudioFailure.NO_DEVICE, (error as CarAudioException).failure)
        coVerify(exactly = 0) { sessionApi.fetchSession(any()) }
        assertEquals(0, transport.prepareCalls.get())
        assertEquals(0, leaseReleases.get())
        assertFalse(coord.sessionBusy.value)
        assertFalse(coord.connected.value)
    }

    @Test
    fun connectFailureWithDisconnectFailure_stillSilencesAndReleasesLease() = runBlocking {
        val coord = coordinator()
        transport.connectFailure = IOException("synthetic connect failure")
        transport.disconnectFailure = IllegalStateException("synthetic disconnect failure")
        val error = runCatching { coord.start() }.exceptionOrNull()
        assertEquals("synthetic connect failure", error?.message)
        assertEquals(1, transport.disconnects.get())
        assertTrue(transport.micMuted.contains(true))
        assertEquals(1, leaseReleases.get())
        assertFalse(coord.sessionBusy.value)
        assertFalse(coord.connected.value)
        assertFalse(broker.isCurrent(1L))
    }

    @Test
    fun stopWithDisconnectFailure_releasesLeaseAndResetsBusy() = runBlocking {
        val coord = coordinator()
        coord.start()
        assertTrue(coord.connected.value)
        assertTrue(coord.sessionBusy.value)
        transport.disconnectFailure = IllegalStateException("synthetic disconnect failure")
        coord.stop()
        assertEquals(1, leaseReleases.get())
        assertFalse(coord.sessionBusy.value)
        assertFalse(coord.connected.value)
    }

    @Test
    fun lateInterruptionFromFirstSession_doesNotMuteOrEndSecond() = runBlocking {
        val coord = coordinator()
        coord.start()
        coord.stop()
        assertEquals(1, leaseReleases.get())
        coord.start()
        assertTrue(coord.connected.value)
        assertEquals(2, interrupts.size)
        transport.micMuted.clear()
        val stopsBefore = transport.stopPlaybackCalls.get()

        interrupts[0](CarAudioFailure.FOCUS_LOST)

        assertTrue(transport.micMuted.isEmpty())
        assertEquals(stopsBefore, transport.stopPlaybackCalls.get())
        assertTrue(coord.connected.value)
        assertTrue(coord.sessionBusy.value)
        assertEquals(1, transport.disconnects.get())

        interrupts[1](CarAudioFailure.FOCUS_LOST)
        assertTrue(transport.micMuted.contains(true))
        withTimeout(5_000) { coord.sessionBusy.first { !it } }
        assertFalse(coord.connected.value)
        assertEquals(2, transport.disconnects.get())
        assertEquals(2, leaseReleases.get())
    }

    @Test
    fun routeLossDuringDelayedConnect_cannotPublishConnected() = runBlocking {
        val coord = coordinator()
        val entered = CompletableDeferred<Unit>()
        val gate = CompletableDeferred<Unit>()
        transport.connectEntered = entered
        transport.connectGate = gate
        val published = CopyOnWriteArrayList<Boolean>()
        val watcher = launch(start = CoroutineStart.UNDISPATCHED) {
            coord.connected.collect { published += it }
        }
        val result = async(Dispatchers.Default) { runCatching { coord.start() } }
        withTimeout(5_000) { entered.await() }
        assertTrue(coord.sessionBusy.value)

        interrupts.single()(CarAudioFailure.ROUTE_LOST)
        gate.complete(Unit)

        val error = withTimeout(5_000) { result.await() }.exceptionOrNull()
        assertTrue(error is CarAudioException)
        assertEquals(CarAudioFailure.ROUTE_LOST, (error as CarAudioException).failure)
        assertFalse(coord.connected.value)
        assertTrue(published.none { it })
        assertTrue(transport.micMuted.contains(true))
        assertEquals(1, transport.disconnects.get())
        assertEquals(1, leaseReleases.get())
        assertFalse(coord.sessionBusy.value)
        watcher.cancel()
    }

    @Test
    fun permissionResultDuringNonCarStart_cannotSwitchModeAndNothingAutoStarts() = runBlocking {
        carSelected.set(false)
        val coord = coordinator()
        val required = listOf("android.permission.RECORD_AUDIO", "android.permission.BLUETOOTH_CONNECT")
        val granted = mutableSetOf<String>()
        val controller = CarAudioModeController(
            isBusy = { coord.sessionBusy.value },
            isEnabled = { carSelected.get() },
            tryCommit = { value -> coord.runIfSessionIdle { carSelected.set(value) } },
            missingPermissions = { required.filterNot { it in granted } },
        )
        assertTrue(controller.onToggle(true, true) is CarAudioToggleDecision.RequestPermissions)

        val fetchEntered = CompletableDeferred<Unit>()
        val fetchGate = CompletableDeferred<RealtimeSession>()
        coEvery { sessionApi.fetchSession(any()) } coAnswers {
            fetchEntered.complete(Unit)
            fetchGate.await()
        }
        val start = launch(Dispatchers.Default) { coord.start() }
        withTimeout(5_000) { fetchEntered.await() }

        granted += required
        assertEquals(CarAudioPermissionOutcome.REJECT_BUSY, controller.onPermissionResult(true))
        assertFalse(carSelected.get())

        fetchGate.complete(session())
        start.join()
        assertTrue(coord.connected.value)
        assertEquals(CarAudioToggleDecision.RejectBusy, controller.onToggle(true, true))
        coord.stop()
        assertFalse(coord.sessionBusy.value)

        assertEquals(CarAudioToggleDecision.Enable, controller.onToggle(true, true))
        assertTrue(carSelected.get())
        assertFalse(coord.connected.value)
        assertFalse(coord.sessionBusy.value)
        coVerify(exactly = 1) { sessionApi.fetchSession(any()) }
        coVerify(exactly = 0) { carAudio.startSession(any(), any()) }
    }

    @Test
    fun idleModeCommitWins_concurrentStartWaitsAtGateAndUsesCommittedPreference() = runBlocking {
        carSelected.set(false)
        val coord = coordinator()
        val writes = CopyOnWriteArrayList<Boolean>()
        val prefs = mockPreferences(writes)
        val startThread = AtomicReference<Thread>()
        val startError = AtomicReference<Throwable?>()
        val busyWhileCommitting = AtomicReference<Boolean>()
        val carStartsWhileCommitting = AtomicInteger(-1)
        every { prefs.setEnabled(any()) } answers {
            // Holding the gate: launch a real start and wait until it is
            // blocked on this very gate before writing.
            val t = thread(name = "car-start-racer") {
                try {
                    runBlocking { coord.start() }
                } catch (e: Throwable) {
                    startError.set(e)
                }
            }
            startThread.set(t)
            awaitBlockedOnBusyGate(t)
            busyWhileCommitting.set(coord.sessionBusy.value)
            carStartsWhileCommitting.set(interrupts.size)
            val value = firstArg<Boolean>()
            writes += value
            carSelected.set(value)
        }
        coord.carAudioPreferences = prefs

        assertTrue(coord.tryChangeCarAudioMode(true))
        assertEquals(false, busyWhileCommitting.get())
        assertEquals(0, carStartsWhileCommitting.get())

        val t = startThread.get()
        t.join(5_000)
        assertFalse(t.isAlive)
        assertNull(startError.get())
        assertTrue(coord.connected.value)
        assertTrue(coord.sessionBusy.value)
        // The start read the committed preference: car audio was used.
        coVerify(exactly = 1) { carAudio.startSession(any(), any()) }
        assertEquals(1, interrupts.size)
        assertEquals(listOf(true), writes)

        // Selection is stable while busy.
        assertFalse(coord.tryChangeCarAudioMode(false))
        assertTrue(carSelected.get())
        verify(exactly = 1) { prefs.setEnabled(any()) }

        coord.stop()
        assertFalse(coord.sessionBusy.value)
        assertEquals(1, leaseReleases.get())
    }

    @Test
    fun sessionStartupWins_concurrentModeChangeIsRejectedAndPreferenceUnchanged() = runBlocking {
        carSelected.set(false)
        val coord = coordinator()
        val readEntered = CountDownLatch(1)
        val releaseRead = CountDownLatch(1)
        // The start reads the selection while holding the gate it marked busy.
        every { carAudio.isSelected } answers {
            readEntered.countDown()
            check(releaseRead.await(5, TimeUnit.SECONDS)) { "selection read never released" }
            carSelected.get()
        }
        val writes = CopyOnWriteArrayList<Boolean>()
        val prefs = mockPreferences(writes)
        coord.carAudioPreferences = prefs

        val start = async(Dispatchers.Default) { runCatching { coord.start() } }
        assertTrue(readEntered.await(5, TimeUnit.SECONDS))
        assertTrue(coord.sessionBusy.value)

        val committed = AtomicReference<Boolean?>()
        val committer = thread(name = "car-mode-racer") {
            committed.set(coord.tryChangeCarAudioMode(true))
        }
        awaitBlockedOnBusyGate(committer)
        releaseRead.countDown()
        committer.join(5_000)
        assertFalse(committer.isAlive)

        assertEquals(false, committed.get())
        assertTrue(writes.isEmpty())
        verify(exactly = 0) { prefs.setEnabled(any()) }
        assertFalse(carSelected.get())

        assertNull(withTimeout(5_000) { start.await() }.exceptionOrNull())
        assertTrue(coord.connected.value)
        // The start used the unchanged (legacy) selection.
        coVerify(exactly = 0) { carAudio.startSession(any(), any()) }

        assertFalse(coord.tryChangeCarAudioMode(true))
        assertTrue(writes.isEmpty())

        coord.stop()
        assertFalse(coord.sessionBusy.value)
        assertTrue(coord.tryChangeCarAudioMode(true))
        assertEquals(listOf(true), writes)
        assertFalse(coord.connected.value)
        coVerify(exactly = 1) { sessionApi.fetchSession(any()) }
    }

    @Test
    fun withoutInjectedPreferences_modeChangeIsRefused() = runBlocking {
        val coord = coordinator()
        assertFalse(coord.tryChangeCarAudioMode(false))
        assertTrue(carSelected.get())
        assertFalse(coord.sessionBusy.value)
    }

    @Test
    fun endIntentIdentity_isUniquePerTokenAndValidated() {
        // Intent.filterEquals ignores extras; the data URI is the identity.
        assertNotEquals(carAudioEndDataUri(1), carAudioEndDataUri(2))
        assertEquals(7L, carAudioTokenFromEndDataUri(carAudioEndDataUri(7)))
        assertEquals(7L, resolveCarAudioEndToken(carAudioEndDataUri(7), 7L))
        assertEquals(7L, resolveCarAudioEndToken(carAudioEndDataUri(7), null))
        assertNull(resolveCarAudioEndToken(carAudioEndDataUri(7), 8L))
        assertNull(resolveCarAudioEndToken(null, 7L))
        assertNull(carAudioTokenFromEndDataUri("liveninja-caraudio://session/end"))
        assertNull(carAudioTokenFromEndDataUri("liveninja-caraudio://session/0/end"))
        assertNull(carAudioTokenFromEndDataUri("https://example.invalid/session/7/end"))
    }

    @Test
    fun staleEndToken_neverEndsReplacementSession() {
        val endsA = AtomicInteger()
        val endsB = AtomicInteger()
        val a = broker.open { endsA.incrementAndGet() }
        val b = broker.open { endsB.incrementAndGet() }
        val stale = resolveCarAudioEndToken(carAudioEndDataUri(a.token), a.token)!!
        assertFalse(broker.requestEnd(stale))
        assertEquals(0, endsA.get())
        assertEquals(0, endsB.get())
        assertTrue(broker.requestEnd(resolveCarAudioEndToken(carAudioEndDataUri(b.token), b.token)!!))
        assertEquals(1, endsB.get())
    }

    private object NoopTranscriptSink : TranscriptSink {
        override suspend fun upload(body: ninja.jeremy.liveninja.net.TranscriptUploadRequest, expectedSessionId: String) = Unit
    }
}
