package ninja.jeremy.liveninja.realtime

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The device roster reducer and the presence payload (plan.md §6 WS-5 M5.1).
 *
 * Both are pure functions over a topic and a payload precisely so this can be a
 * JVM test: the removal path is driven by the broker's Last Will, which no
 * amount of in-app testing exercises, so the reducer has to be provably right
 * without a socket in front of it.
 */
class LiveEventsPresenceTest {

    private fun peers(vararg p: LiveEventsClient.Peer) = p.toList()

    private fun presence(deviceId: String, state: String, persona: String = "Live Ninja") =
        JSONObject()
            .put("deviceId", deviceId)
            .put("actorDeviceId", "dev-$deviceId")
            .put("persona", persona)
            .put("state", state)
            .toString()

    @Test
    fun `a payload adds a device to the roster`() {
        val roster = PresenceRoster.reduce(
            emptyList(),
            "liveninja/user/u1/presence/web-8f2c1a",
            presence("web-8f2c1a", "listening"),
        )
        assertEquals(1, roster.size)
        assertEquals("web-8f2c1a", roster[0].deviceId)
        assertEquals("dev-web-8f2c1a", roster[0].actorDeviceId)
        assertEquals("Live Ninja", roster[0].persona)
        assertEquals("listening", roster[0].state)
    }

    @Test
    fun `an empty payload removes the device`() {
        val roster = peers(
            LiveEventsClient.Peer("web-8f2c1a", "dev-a", "Live Ninja", "listening"),
            LiveEventsClient.Peer("tab-9911", "dev-b", "Live Ninja", "speaking"),
        )
        val after = PresenceRoster.reduce(roster, "liveninja/user/u1/presence/tab-9911", "")
        assertEquals(listOf("web-8f2c1a"), after.map { it.deviceId })
    }

    @Test
    fun `a second payload replaces the entry in place`() {
        var roster = peers(
            LiveEventsClient.Peer("aaa", "dev-a", "Live Ninja", "listening"),
            LiveEventsClient.Peer("bbb", "dev-b", "Live Ninja", "idle"),
        )
        roster = PresenceRoster.reduce(
            roster,
            "liveninja/user/u1/presence/aaa",
            presence("aaa", "speaking"),
        )
        assertEquals(2, roster.size)
        // Position is part of the contract: a roster that reshuffles every time
        // a device starts speaking is unreadable.
        assertEquals(listOf("aaa", "bbb"), roster.map { it.deviceId })
        assertEquals("speaking", roster[0].state)
    }

    @Test
    fun `the topic segment wins over a disagreeing body deviceId`() {
        val roster = PresenceRoster.reduce(
            emptyList(),
            "liveninja/user/u1/presence/tab-9911",
            presence("something-else", "listening"),
        )
        // Keyed by the topic, because the topic is what the Last Will clears.
        // Keying by the body would leave an entry nothing can ever remove.
        assertEquals("tab-9911", roster[0].deviceId)

        val after = PresenceRoster.reduce(roster, "liveninja/user/u1/presence/tab-9911", "")
        assertTrue(after.isEmpty())
    }

    @Test
    fun `an unknown state normalizes to idle`() {
        val roster = PresenceRoster.reduce(
            emptyList(),
            "liveninja/user/u1/presence/aaa",
            presence("aaa", "live-thinking-about-lunch"),
        )
        assertEquals("idle", roster[0].state)
    }

    @Test
    fun `a malformed payload leaves the roster alone`() {
        val roster = peers(LiveEventsClient.Peer("aaa", "dev-a", "Live Ninja", "listening"))
        val after = PresenceRoster.reduce(roster, "liveninja/user/u1/presence/aaa", "not json")
        assertEquals(roster, after)
    }

    @Test
    fun `the published payload is exactly the four contract fields`() {
        val json = JSONObject(
            PresenceRoster.payload(
                clientId = "web-8f2c1a",
                actorDeviceId = "dev-tab-s9",
                persona = "Live Ninja",
                state = "speaking",
            ),
        )
        assertEquals(4, json.length())
        // deviceId is the MQTT client id, NOT actorDeviceId: it has to be the
        // same string as the presence topic's last segment or a roster entry can
        // never be matched to the device that published it.
        assertEquals("web-8f2c1a", json.getString("deviceId"))
        assertEquals("dev-tab-s9", json.getString("actorDeviceId"))
        assertEquals("Live Ninja", json.getString("persona"))
        assertEquals("speaking", json.getString("state"))
    }

    @Test
    fun `a published state outside the vocabulary is normalized before it reaches the wire`() {
        val json = JSONObject(PresenceRoster.payload("aaa", "dev-a", "", "ending"))
        assertEquals("idle", json.getString("state"))
    }

    @Test
    fun `the published payload round-trips through the reducer`() {
        val roster = PresenceRoster.reduce(
            emptyList(),
            "liveninja/user/u1/presence/web-8f2c1a",
            PresenceRoster.payload("web-8f2c1a", "dev-tab-s9", "Custom", "thinking"),
        )
        assertEquals(
            LiveEventsClient.Peer("web-8f2c1a", "dev-tab-s9", "Custom", "thinking"),
            roster.single(),
        )
    }

    @Test
    fun `the lock topic must never reach this reducer`() {
        // Not a test of the reducer so much as a record of why
        // LiveTopics.route tries the lock BEFORE presence. The lock now lives
        // at liveninja/user/<uid>/presence/speaking, and this is what routing
        // it here would produce: a phantom device named "speaking" on every
        // roster in the fleet — while the lock itself goes unobserved, so every
        // device answers every change at once.
        val roster = PresenceRoster.reduce(
            emptyList(),
            "liveninja/user/u1/presence/speaking",
            """{"holder":"tab-9911","claimId":"aaaa","ttlMs":30000}""",
        )
        assertEquals("speaking", roster.single().deviceId)
        assertEquals(
            LiveTopics.Route.SPEAKING,
            LiveTopics.route("liveninja/user/u1/presence/speaking", "liveninja/user/u1/presence/speaking"),
        )
    }

    // ---- the persona field ----

    @Test
    fun `a catalog persona publishes its display name, not its slug`() {
        // Web publishes personaLabelFor(id) — a human name. Android resolved
        // through the two-entry offline preset list and fell back to the id, so
        // a device running "pirate-captain" put the slug in the same field, and
        // the web roster rendered "pirate-captain · Listening" beside properly
        // named rows.
        val catalog = mapOf("pirate-captain" to "Pirate Captain", "default" to "Live Ninja")
        assertEquals("Pirate Captain", PersonaLabels.label("pirate-captain", catalog))
    }

    @Test
    fun `the default persona publishes the empty string exactly as web does`() {
        // Both rosters substitute the plain "Live Ninja" label for an empty
        // persona at render time. Substituting on the wire instead would be
        // indistinguishable, on the receiving side, from a device actually
        // running a persona by that name.
        assertEquals("", PersonaLabels.label("default", mapOf("default" to "Live Ninja")))
        assertEquals("", PersonaLabels.label("default", emptyMap()))
        // An untouched document can also read as no preset at all.
        assertEquals("", PersonaLabels.label("", emptyMap()))
    }

    @Test
    fun `custom is labelled from the preset list the server catalog never lists`() {
        // `custom` is a client-side concept the schema defines; the server
        // catalog has no row for it, so the fallback list is where its label
        // has to come from.
        assertEquals("Custom", PersonaLabels.label("custom", emptyMap()))
    }

    @Test
    fun `an id the catalog does not know falls back to the id`() {
        // Before the fetch lands, or against a server whose catalog dropped
        // the persona this device still has selected. The id is a poor label
        // but an honest one, and it is what web falls back to as well.
        assertEquals("pirate-captain", PersonaLabels.label("pirate-captain", emptyMap()))
    }
}
