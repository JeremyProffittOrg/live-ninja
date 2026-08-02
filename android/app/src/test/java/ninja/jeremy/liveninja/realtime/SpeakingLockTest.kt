package ninja.jeremy.liveninja.realtime

import ninja.jeremy.liveninja.ui.conversation.MicUiState
import ninja.jeremy.liveninja.ui.conversation.NudgeDelivery
import ninja.jeremy.liveninja.ui.conversation.nudgeDelivery
import ninja.jeremy.liveninja.ui.conversation.shouldHoldEventStream
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The turn-taking lock and the defer-and-merge rules (plan.md §6 WS-5 M5.2).
 *
 * The property under test is not "the lock works" — QoS 0 gives no atomic
 * claim and none is invented here — it is that every receiver reaches the SAME
 * winner from the same set of claims regardless of the order they arrived in.
 * A first-writer-wins rule would pick a different winner on each device, which
 * is the one outcome that must never happen, so order-independence is asserted
 * over every permutation rather than one convenient ordering.
 */
class SpeakingLockTest {

    private fun claim(holder: String, claimId: String, receivedAtMs: Long = 0L) =
        LockClaim(holder, claimId, SpeakingLock.TTL_MS, receivedAtMs)

    private fun <T> permutations(items: List<T>): List<List<T>> =
        if (items.size <= 1) {
            listOf(items)
        } else {
            items.flatMap { head ->
                permutations(items - head).map { rest -> listOf(head) + rest }
            }
        }

    @Test
    fun `the lowest holder wins whatever order the claims arrived in`() {
        val claims = listOf(
            claim("web-8f2c1a", "ffffffffffffffff"),
            claim("tab-9911", "0000000000000000"),
            claim("desk-0042", "8888888888888888"),
        )
        val winners = permutations(claims).map { SpeakingLock.lockWinner(it)?.holder }.toSet()
        assertEquals(setOf("desk-0042"), winners)
    }

    @Test
    fun `a tie on holder is broken by the lowest claim id`() {
        // AWS IoT evicts a duplicate MQTT client id, so two live devices cannot
        // really share a holder — the tiebreak exists so the ordering is total
        // and the function stays a pure, decidable choice either way.
        val winner = SpeakingLock.lockWinner(
            listOf(claim("tab-9911", "bbbb"), claim("tab-9911", "aaaa")),
        )
        assertEquals("aaaa", winner?.claimId)
    }

    @Test
    fun `no claims means no winner`() {
        assertNull(SpeakingLock.lockWinner(emptyList()))
    }

    @Test
    fun `a claim older than its ttl is treated as absent`() {
        val signals = listOf(SpeakingSignal.Claimed(claim("tab-9911", "aaaa", receivedAtMs = 0L)))
        assertEquals(1, SpeakingLock.liveClaims(signals, nowMs = SpeakingLock.TTL_MS - 1).size)
        // Expiry is measured from ARRIVAL on the receiver's own clock, so a
        // holder that crashed without releasing frees the lock here regardless
        // of what its clock said.
        assertTrue(SpeakingLock.liveClaims(signals, nowMs = SpeakingLock.TTL_MS).isEmpty())
    }

    @Test
    fun `an empty payload is a release and frees the lock`() {
        val released = SpeakingLock.parse("", receivedAtMs = 10L)
        assertEquals(SpeakingSignal.Released(10L), released)

        val signals = listOf(
            SpeakingSignal.Claimed(claim("tab-9911", "aaaa", receivedAtMs = 0L)),
            released!!,
        )
        assertTrue(SpeakingLock.liveClaims(signals, nowMs = 20L).isEmpty())
    }

    @Test
    fun `a re-claim after a release binds again`() {
        val signals = listOf(
            SpeakingSignal.Claimed(claim("tab-9911", "aaaa", receivedAtMs = 0L)),
            SpeakingSignal.Released(10L),
            SpeakingSignal.Claimed(claim("web-8f2c1a", "bbbb", receivedAtMs = 20L)),
        )
        assertEquals("web-8f2c1a", SpeakingLock.lockWinner(SpeakingLock.liveClaims(signals, 30L))?.holder)
    }

    @Test
    fun `a claim payload round-trips and clamps a hostile ttl`() {
        val payload = SpeakingLock.claimPayload("web-8f2c1a", "9f3a71c0d4e8b215")
        val json = JSONObject(payload)
        assertEquals("web-8f2c1a", json.getString("holder"))
        assertEquals("9f3a71c0d4e8b215", json.getString("claimId"))
        assertEquals(30_000L, json.getLong("ttlMs"))

        val parsed = SpeakingLock.parse(payload, receivedAtMs = 5L)
        assertEquals(claim("web-8f2c1a", "9f3a71c0d4e8b215", receivedAtMs = 5L), (parsed as SpeakingSignal.Claimed).claim)

        // A claim asking to hold the fleet quiet for an hour gets 30 seconds.
        val greedy = """{"holder":"tab-9911","claimId":"aaaa","ttlMs":3600000}"""
        assertEquals(
            SpeakingLock.TTL_MS,
            (SpeakingLock.parse(greedy, 0L) as SpeakingSignal.Claimed).claim.ttlMs,
        )
    }

    @Test
    fun `a payload that is not a claim is ignored rather than guessed at`() {
        assertNull(SpeakingLock.parse("not json", 0L))
        assertNull(SpeakingLock.parse("""{"holder":"tab-9911"}""", 0L))
        assertNull(SpeakingLock.parse("""{"claimId":"aaaa"}""", 0L))
        assertNotNull(SpeakingLock.parse("""{"holder":"a","claimId":"b"}""", 0L))
    }

    // ---- who may claim at all ----

    @Test
    fun `a device already settling a claim does not claim again`() {
        // The one that mattered: arbitration is keyed by HOLDER, so a second
        // claim from this device replaces the first and then beats it. This
        // device wins its own arbitration and speaks twice for one moment.
        assertFalse(
            SpeakingLock.mayClaim(
                claiming = true,
                holding = false,
                standing = claim("tab-9911", "aaaa"),
                self = "tab-9911",
            ),
        )
        // ...including when nothing is standing yet, which is the real shape of
        // it: the first claim is published but its echo has not come back.
        assertFalse(
            SpeakingLock.mayClaim(claiming = true, holding = false, standing = null, self = "tab-9911"),
        )
    }

    @Test
    fun `a device already speaking on a turn it won does not claim again`() {
        // Worse than the double turn: the release at the end of the FIRST
        // response is a fleet-wide zero-length payload, so it would free the
        // lock for every other device while the second response is still
        // pending here.
        assertFalse(
            SpeakingLock.mayClaim(
                claiming = false,
                holding = true,
                standing = claim("tab-9911", "aaaa"),
                self = "tab-9911",
            ),
        )
    }

    @Test
    fun `another device holding the lock blocks the claim`() {
        assertFalse(
            SpeakingLock.mayClaim(
                claiming = false,
                holding = false,
                standing = claim("desk-0042", "aaaa"),
                self = "tab-9911",
            ),
        )
    }

    @Test
    fun `an idle device claims over nothing standing and over its own stale echo`() {
        assertTrue(
            SpeakingLock.mayClaim(claiming = false, holding = false, standing = null, self = "tab-9911"),
        )
        // Neither claiming nor holding, so a claim under our own name can only
        // be the echo of a turn that already finished. Refusing here would mute
        // this device until the full 30s TTL ran out.
        assertTrue(
            SpeakingLock.mayClaim(
                claiming = false,
                holding = false,
                standing = claim("tab-9911", "aaaa"),
                self = "tab-9911",
            ),
        )
    }

    // ---- topic routing ----

    private val speakingTopic = "liveninja/user/u1/presence/speaking"

    @Test
    fun `the lock topic is routed to the lock and not to the roster`() {
        // The ordering IS the contract. The lock topic now lives under
        // /presence/ — so that clients running the pre-deploy module graph
        // ignore it instead of announcing every claim as an edit that never
        // happened — which means it matches the presence test as well as its
        // own. Presence-first would file the fleet's lock as a peer device
        // literally named "speaking", and leave the lock itself unobserved.
        assertEquals(
            LiveTopics.Route.SPEAKING,
            LiveTopics.route(speakingTopic, speakingTopic),
        )
    }

    @Test
    fun `a server that predates the lock still routes a peer's claim to the lock`() {
        // Empty speakingTopic = a credential response from an older server.
        // The trailing match is what keeps an updated peer's claim off the
        // roster in that case.
        assertEquals(
            LiveTopics.Route.SPEAKING,
            LiveTopics.route(speakingTopic, speakingTopic = ""),
        )
    }

    @Test
    fun `a device presence topic is still routed to the roster`() {
        assertEquals(
            LiveTopics.Route.PRESENCE,
            LiveTopics.route("liveninja/user/u1/presence/tab-9911", speakingTopic),
        )
    }

    @Test
    fun `an event topic is routed to the change path`() {
        assertEquals(
            LiveTopics.Route.CHANGE,
            LiveTopics.route("liveninja/user/u1/events", speakingTopic),
        )
    }

    // ---- when a held change may actually be spoken ----

    @Test
    fun `a live and quiet session speaks`() {
        assertEquals(
            NudgeDelivery.SPEAK,
            nudgeDelivery(MicUiState.LISTENING, sessionConnected = true),
        )
    }

    @Test
    fun `the assistant talking holds the change back rather than cutting in`() {
        // Read a second time AFTER the 400ms settle window, which is the whole
        // point: the assistant can start a turn inside that window, and a
        // decision made only before it lands the "[Automatic update]"
        // mid-sentence — the exact interruption the guard exists to prevent.
        assertEquals(
            NudgeDelivery.HOLD,
            nudgeDelivery(MicUiState.SPEAKING, sessionConnected = true),
        )
    }

    @Test
    fun `a session that dropped during the settle window never speaks into it`() {
        // sendUserText opens with `if (text.isBlank() || !connected.value) return`,
        // so speaking here discards the text silently — with the queue already
        // emptied and the 60s quiet fallback already cancelled. QUIET is what
        // puts it on screen instead.
        assertEquals(
            NudgeDelivery.QUIET,
            nudgeDelivery(MicUiState.LISTENING, sessionConnected = false),
        )
        // Including the contradictory pairing: the mic state is this
        // ViewModel's view of the session and it lags the transport.
        assertEquals(
            NudgeDelivery.QUIET,
            nudgeDelivery(MicUiState.SPEAKING, sessionConnected = false),
        )
    }

    @Test
    fun `no session at all surfaces the change quietly`() {
        for (state in listOf(
            MicUiState.IDLE,
            MicUiState.REQUESTING_MIC,
            MicUiState.CONNECTING,
            MicUiState.ENDING,
            MicUiState.ERROR,
        )) {
            for (connected in listOf(true, false)) {
                assertEquals(
                    "$state connected=$connected",
                    NudgeDelivery.QUIET,
                    nudgeDelivery(state, sessionConnected = connected),
                )
            }
        }
    }

    // ---- how long the event stream stays open ----

    @Test
    fun `a backgrounded app with no session drops the event stream`() {
        // §6 WS-4 M4.2: One UI kills long-lived background sockets anyway, and
        // the reconnect churn costs more battery than the notification latency
        // is worth.
        assertFalse(shouldHoldEventStream(appInBackground = true, sessionActive = false))
    }

    @Test
    fun `a backgrounded app with a live session keeps the event stream`() {
        // A screen-off wake-word session is the device most likely to speak
        // unprompted, and one that cannot see the lock speaks over everyone.
        assertTrue(shouldHoldEventStream(appInBackground = true, sessionActive = true))
    }

    @Test
    fun `a foregrounded app keeps the event stream either way`() {
        assertTrue(shouldHoldEventStream(appInBackground = false, sessionActive = false))
        assertTrue(shouldHoldEventStream(appInBackground = false, sessionActive = true))
    }

    // ---- defer-and-merge ----

    private fun change(type: String, id: String, summary: String, persona: String = "") =
        LiveEventsClient.Change(
            type = type,
            id = id,
            actorDeviceId = "dev-a",
            actorPersona = persona,
            summary = summary,
        )

    @Test
    fun `the merge dedupes by type and id keeping what it last looked like`() {
        val merged = NudgeMerge.dedupe(
            listOf(
                change("doc", "plan", "edited the plan"),
                change("memory", "alice", "added a note"),
                change("doc", "plan", "renamed the plan"),
            ),
        )
        // A file edited three times is one mention, and the mention is the
        // latest state of it — not the first thing that happened to it.
        assertEquals(2, merged.size)
        assertEquals(listOf("plan", "alice"), merged.map { it.id })
        assertEquals("renamed the plan", merged[0].summary)
    }

    @Test
    fun `several held changes collapse into one prompt`() {
        val prompt = NudgeMerge.prompt(
            listOf(
                change("doc", "plan", "edited the plan", persona = "Staff SRE"),
                change("memory", "alice", "added a note"),
            ),
        )
        assertEquals(1, Regex("\\[Automatic update]").findAll(prompt).count())
        assertTrue(prompt, prompt.contains("Staff SRE just edited the plan"))
        assertTrue(prompt, prompt.contains("Another device just added a note"))
        assertTrue(prompt, prompt.contains("ONE short sentence covering all of them"))
    }

    @Test
    fun `one held change keeps the single-change wording`() {
        val prompt = NudgeMerge.prompt(listOf(change("doc", "plan", "edited the plan")))
        assertTrue(prompt, prompt.startsWith("[Automatic update] Another device just edited the plan."))
        assertTrue(prompt, prompt.contains("in one short sentence"))
    }

    @Test
    fun `beyond three changes the rest become a count`() {
        val prompt = NudgeMerge.prompt(
            (1..5).map { change("doc", "doc-$it", "edited doc $it") },
        )
        assertTrue(prompt, prompt.contains("edited doc 3"))
        assertTrue(prompt, prompt.contains(", and 2 other changes."))
        assertTrue(prompt, !prompt.contains("edited doc 4"))
    }

    @Test
    fun `the quiet notice says the same thing without the instructions`() {
        val notice = NudgeMerge.notice(
            listOf(
                change("doc", "plan", "edited the plan", persona = "Staff SRE"),
                change("doc", "plan", "edited the plan again", persona = "Staff SRE"),
            ),
        )
        assertEquals("Staff SRE edited the plan again.", notice)
        assertEquals("", NudgeMerge.notice(emptyList()))
    }
}
