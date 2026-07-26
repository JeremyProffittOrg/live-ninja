package ninja.jeremy.liveninja.realtime

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

class GeminiResumptionStateTest {

    @Test
    fun `goAway waits for playback and consumes a safe handle once`() {
        val state = GeminiResumptionState()
        state.observe(resumable = true, newHandle = "handle-1")
        state.requestReconnect()

        assertNull(state.takePendingIfReady(blocked = true))
        assertEquals("handle-1", state.takePendingIfReady(blocked = false))
        assertNull(state.takePendingIfReady(blocked = false))
        assertNull(state.takeAfterDrop())
    }

    @Test
    fun `non-resumable update clears history while keeping goAway pending`() {
        val state = GeminiResumptionState()
        state.observe(resumable = true, newHandle = "historical")
        state.requestReconnect()
        state.observe(resumable = false, newHandle = null)

        assertNull(state.takePendingIfReady(blocked = false))

        state.observe(resumable = true, newHandle = "fresh")
        assertEquals("fresh", state.takePendingIfReady(blocked = false))
        assertNull(state.takeAfterDrop())
    }

    @Test
    fun `unexpected drop uses only the latest unconsumed checkpoint`() {
        val state = GeminiResumptionState()
        state.observe(resumable = true, newHandle = "handle-2")

        assertEquals("handle-2", state.takeAfterDrop())
        assertNull(state.takeAfterDrop())

        state.observe(resumable = true, newHandle = "handle-3")
        state.reset()
        assertNull(state.takeAfterDrop())
    }

    @Test
    fun `same-frame tool call invalidates its preceding resumption update`() {
        val state = GeminiResumptionState()
        state.requestReconnect()

        // Production applies lifecycle metadata first, then all state-changing
        // fields, and considers reconnect only after the complete frame.
        state.observe(resumable = true, newHandle = "same-frame-before-tool")
        state.invalidateCheckpoint()
        assertNull(state.takePendingIfReady(blocked = false))
        assertNull(state.takeAfterDrop())

        state.observe(resumable = true, newHandle = "later-after-tool")
        assertEquals("later-after-tool", state.takePendingIfReady(blocked = false))
        assertNull(state.takeAfterDrop())
    }

    @Test
    fun `queued controls flush after activation in FIFO order exactly once`() {
        val queue = GeminiControlQueue()
        val lifecycle = GeminiSocketLifecycle()
        val sent = mutableListOf<String>()

        queue.enqueue("tool-response-1")
        queue.enqueue("typed-turn-2")
        assertTrue(sent.isEmpty())

        lifecycle.markSetupComplete()
        assertTrue(sent.isEmpty())
        lifecycle.activate()
        assertTrue(queue.flush { payload -> sent.add(payload) })
        assertEquals(listOf("tool-response-1", "typed-turn-2"), sent)
        assertEquals(0, queue.size)

        assertTrue(queue.flush { payload -> sent.add(payload) })
        assertEquals(
            "a second activation/flush must not replay accepted frames",
            listOf("tool-response-1", "typed-turn-2"),
            sent,
        )
    }

    @Test
    fun `rejected queue head retains only the unsent suffix`() {
        val queue = GeminiControlQueue()
        val accepted = mutableListOf<String>()
        var rejectSecondOnce = true
        queue.enqueue("one")
        queue.enqueue("two")
        queue.enqueue("three")

        assertFalse(
            queue.flush { payload ->
                if (payload == "two" && rejectSecondOnce) {
                    rejectSecondOnce = false
                    false
                } else {
                    accepted.add(payload)
                }
            },
        )
        assertEquals(listOf("one"), accepted)
        assertEquals(2, queue.size)

        assertTrue(queue.flush { payload -> accepted.add(payload) })
        assertEquals(listOf("one", "two", "three"), accepted)
        assertEquals(0, queue.size)
    }

    @Test
    fun `release clears queued controls instead of leaking them into another session`() {
        val queue = GeminiControlQueue()
        val sent = mutableListOf<String>()
        queue.enqueue("old-session-tool-response")

        queue.clear()
        assertEquals(0, queue.size)
        assertTrue(queue.flush { payload -> sent.add(payload) })
        assertTrue(sent.isEmpty())
    }

    @Test
    fun `socket closing after setup but before activation cannot report success`() {
        val lifecycle = GeminiSocketLifecycle()
        lifecycle.markSetupComplete()

        assertFalse(lifecycle.markTerminal(java.io.IOException("closed in setup gap")))
        try {
            lifecycle.activate()
            fail("activation must reject a socket that already closed")
        } catch (expected: java.io.IOException) {
            assertEquals("closed in setup gap", expected.message)
        }
    }

    @Test
    fun `terminal callback owns cleanup after socket activation`() {
        val lifecycle = GeminiSocketLifecycle()
        lifecycle.markSetupComplete()
        lifecycle.activate()

        assertTrue(lifecycle.markTerminal(java.io.IOException("closed after activation")))
    }
}
