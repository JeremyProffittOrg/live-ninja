package ninja.jeremy.liveninja.ui.conversation

import ninja.jeremy.liveninja.realtime.LiveEventsClient
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The roster reduction (plan.md §6 WS-5 M5.1).
 *
 * [rosterFor] is two lines, and each of them is the kind of rule a refactor
 * removes without anything else going red — which is exactly why they are
 * pinned here rather than left inside the collector.
 */
class PeerRosterTest {

    private fun peer(
        deviceId: String,
        persona: String = "",
        state: String = "listening",
    ) = LiveEventsClient.Peer(
        deviceId = deviceId,
        actorDeviceId = "",
        persona = persona,
        state = state,
    )

    @Test
    fun `this device is never on its own roster`() {
        // The `#` subscription echoes our own retained presence straight back,
        // so the self row is always present in the input. Showing it puts a row
        // a few pixels under the state pill that says the same thing — and
        // disagrees with it for up to a second, because presence is throttled.
        val roster = rosterFor(
            listOf(peer("web-self"), peer("tablet-1"), peer("desk-2")),
            self = "web-self",
        )

        assertEquals(listOf("tablet-1", "desk-2"), roster.map { it.deviceId })
    }

    @Test
    fun `a peer with no device id is dropped rather than rendered blank`() {
        // A blank id means the payload disagreed with the topic it arrived on,
        // so the row has no stable identity to de-duplicate against and would
        // flicker in and out as updates land.
        val roster = rosterFor(listOf(peer(""), peer("   "), peer("tablet-1")), self = "web-self")

        assertEquals(listOf("tablet-1"), roster.map { it.deviceId })
    }

    @Test
    fun `an unknown self id keeps every peer`() {
        // clientId is null while offline. That must not empty the roster: the
        // retained presence of the other devices is still true, and blanking it
        // on a local socket blip would read as "everyone else went away".
        val roster = rosterFor(listOf(peer("tablet-1"), peer("desk-2")), self = null)

        assertEquals(2, roster.size)
    }

    @Test
    fun `persona and state travel through untouched`() {
        // The screen owns the fallbacks (blank persona renders as Live Ninja, an
        // unrecognised state renders as ready). Doing it here as well would put
        // the same decision in two places and let them disagree.
        val roster = rosterFor(
            listOf(peer("tablet-1", persona = "Staff SRE", state = "thinking")),
            self = "web-self",
        )

        assertEquals("Staff SRE", roster[0].persona)
        assertEquals("thinking", roster[0].state)
    }

    @Test
    fun `an empty input is an empty roster`() {
        assertTrue(rosterFor(emptyList(), self = "web-self").isEmpty())
    }
}
