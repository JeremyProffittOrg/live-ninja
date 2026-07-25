package ninja.jeremy.liveninja.realtime

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.currentTime
import kotlinx.coroutines.test.runTest
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * WS-5 M21.2 regression tests for the half-duplex mic guard. The defect being
 * guarded against is the assistant's own speech coming back up the mic and being
 * transcribed as user input, so the properties that matter are: the mic is gated
 * for as long as the speaker is audible (including the tail after the server says
 * playout stopped), and it always reopens on its own — a gate that sticks shut is
 * a worse bug than the echo.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class EchoGateTest {

    /** Injected clock; tests move time explicitly rather than sleeping. */
    private var now = 10_000L

    private fun core() = EchoGateCore().apply { enabled = true }

    @Test
    fun `a disabled gate never suppresses the mic`() {
        val gate = EchoGateCore() // enabled defaults to false
        gate.onAssistantAudioStarted(now)

        // This is the comparison mode: the software AEC alone, no gating at all.
        assertFalse(gate.micSuppressed(now))
        assertFalse(gate.micSuppressed(now + 1))
        assertNull(gate.nextChangeAtMs(now))
    }

    @Test
    fun `mic is gated while the assistant plays and through the tail after it stops`() {
        val gate = core()
        gate.onAssistantAudioStarted(now)
        assertTrue(gate.micSuppressed(now))

        // 5 s into the answer, still gated: nothing said it stopped.
        assertTrue(gate.micSuppressed(now + 5_000))

        gate.onAssistantAudioStopped(now + 5_000)
        // The stop event is not the end of the sound — jitter buffer, AudioTrack
        // buffer and the room are still delivering it.
        assertTrue(gate.micSuppressed(now + 5_000 + EchoGateCore.DEFAULT_TAIL_MS - 1))
        assertFalse(gate.micSuppressed(now + 5_000 + EchoGateCore.DEFAULT_TAIL_MS))
    }

    @Test
    fun `played chunks hold the gate and the tail runs from the last one`() {
        val gate = core()
        // The PCM transports (Nova/Gemini) report real playout per written chunk —
        // 500 ms of it here…
        repeat(25) { i -> gate.onAssistantAudioPlaying(now + i * 20L) }
        // …then the server's end-of-turn arrives while the queue is still draining…
        gate.onAssistantAudioStopped(now + 500)
        // …and chunks keep landing. Real playout has to beat server turn timing, or
        // the mic reopens into the assistant's last half-second.
        repeat(25) { i -> gate.onAssistantAudioPlaying(now + 500 + i * 20L) }
        val lastChunk = now + 500 + 24 * 20L

        assertTrue(gate.micSuppressed(lastChunk))
        assertTrue(gate.micSuppressed(lastChunk + EchoGateCore.DEFAULT_TAIL_MS - 1))
        assertFalse(gate.micSuppressed(lastChunk + EchoGateCore.DEFAULT_TAIL_MS))
    }

    @Test
    fun `response done bounds the gate instead of opening it`() {
        val gate = core()
        gate.onAssistantAudioStarted(now)

        // `response.done` fires when generation completes, while audio is still
        // being played out — opening the mic here is what let the assistant hear
        // the tail of its own sentence.
        gate.onAssistantResponseDone(now + 1_000)
        assertTrue(gate.micSuppressed(now + 1_000))
        assertTrue(gate.micSuppressed(now + 1_000 + EchoGateCore.DEFAULT_RESPONSE_DONE_TAIL_MS - 1))
        // …but it still bounds the gate, so a missing playout-stopped event costs
        // seconds, not the watchdog.
        assertFalse(gate.micSuppressed(now + 1_000 + EchoGateCore.DEFAULT_RESPONSE_DONE_TAIL_MS))
    }

    @Test
    fun `a lost stop event cannot gate the mic forever`() {
        val gate = core()
        gate.onAssistantAudioStarted(now)

        // No stop, no response.done, no chunks — a dropped DataChannel event. The
        // watchdog is the only thing between that and a mic that never comes back.
        assertTrue(gate.micSuppressed(now + EchoGateCore.DEFAULT_SPEAKING_WATCHDOG_MS - 1))
        assertFalse(gate.micSuppressed(now + EchoGateCore.DEFAULT_SPEAKING_WATCHDOG_MS))
    }

    @Test
    fun `barge-in flush reopens the mic on the short tail`() {
        val gate = core()
        gate.onAssistantAudioStarted(now)

        // Playback was faded (~40 ms) and flushed locally, so the speaker is
        // already silent and the user is mid-sentence: waiting the full playout
        // tail would clip the interruption.
        gate.onPlaybackFlushed(now + 2_000)
        assertTrue(gate.micSuppressed(now + 2_000))
        assertFalse(gate.micSuppressed(now + 2_000 + EchoGateCore.DEFAULT_FLUSH_TAIL_MS))
        assertTrue(EchoGateCore.DEFAULT_FLUSH_TAIL_MS < EchoGateCore.DEFAULT_TAIL_MS)
    }

    @Test
    fun `stop and flush never gate an already open mic`() {
        val gate = core()
        // Stray events with no playout in flight (barge-in on a silent assistant,
        // a duplicate stopped event) must not mute the user.
        gate.onAssistantAudioStopped(now)
        assertFalse(gate.micSuppressed(now))
        gate.onPlaybackFlushed(now)
        assertFalse(gate.micSuppressed(now))
        gate.onAssistantResponseDone(now)
        assertFalse(gate.micSuppressed(now))
    }

    @Test
    fun `reset opens the gate for the next session`() {
        val gate = core()
        gate.onAssistantAudioStarted(now)
        assertTrue(gate.micSuppressed(now))

        gate.reset() // session teardown
        assertFalse(gate.micSuppressed(now))
    }

    @Test
    fun `nextChangeAtMs reports the reopen deadline while gated`() {
        val gate = core()
        gate.onAssistantAudioStarted(now)
        gate.onAssistantAudioStopped(now)

        assertEquals(now + EchoGateCore.DEFAULT_TAIL_MS, gate.nextChangeAtMs(now)!!)
        // Once open there is nothing left to schedule.
        assertNull(gate.nextChangeAtMs(now + EchoGateCore.DEFAULT_TAIL_MS))
    }

    @Test
    fun `gate pushes the mic back on when the tail expires`() = runTest {
        val edges = mutableListOf<Boolean>()
        val gate = EchoGate(this, clock = { currentTime }) { edges += it }
        gate.enabled = true

        gate.assistantAudioStarted()
        assertTrue(gate.micSuppressed())
        gate.assistantAudioStopped()

        // The WebRTC transport enables/disables a track, so the reopen has to be
        // pushed by the gate's own timer — nothing polls it there.
        advanceTimeBy(EchoGateCore.DEFAULT_TAIL_MS + 1)
        assertFalse(gate.micSuppressed())
        assertEquals(listOf(true, false), edges)
        gate.reset()
    }

    @Test
    fun `keepalive while playing does not report a premature reopen`() {
        // Playout evidence arrives every 20 ms and moves the deadline past the
        // timer that is already in flight; the timer must re-arm on wake instead of
        // reporting the mic open mid-sentence.
        runTest {
            val edges = mutableListOf<Boolean>()
            val gate = EchoGate(this, clock = { currentTime }) { edges += it }
            gate.enabled = true

            repeat(100) { // 2 s of continuous playout
                gate.assistantAudioPlaying()
                advanceTimeBy(20)
            }
            assertTrue(gate.micSuppressed())
            assertEquals(listOf(true), edges)

            gate.assistantAudioStopped()
            advanceTimeBy(EchoGateCore.DEFAULT_TAIL_MS + 1)
            assertFalse(gate.micSuppressed())
            assertEquals(listOf(true, false), edges)
            gate.reset()
        }
    }

    @Test
    fun `disabling the gate mid-session reopens the mic immediately`() = runTest {
        val edges = mutableListOf<Boolean>()
        val gate = EchoGate(this, clock = { currentTime }) { edges += it }
        gate.enabled = true
        gate.assistantAudioStarted()
        assertTrue(gate.micSuppressed())

        gate.enabled = false // owner switch flipped to compare against AEC alone
        assertFalse(gate.micSuppressed())
        assertEquals(listOf(true, false), edges)
        gate.reset()
    }

    @Test
    fun `the guard is on unless the settings document turns it off`() {
        // Default ON: the loop is reproduced on hardware and the AEC3 change it
        // backs up is still unverified there.
        assertTrue(EchoGuardPolicy.halfDuplexMicGuard(JSONObject()))
        assertTrue(
            EchoGuardPolicy.halfDuplexMicGuard(
                JSONObject().put(EchoGuardPolicy.KEY_HALF_DUPLEX_MIC_GUARD, true),
            ),
        )
        assertFalse(
            EchoGuardPolicy.halfDuplexMicGuard(
                JSONObject().put(EchoGuardPolicy.KEY_HALF_DUPLEX_MIC_GUARD, false),
            ),
        )
    }
}
