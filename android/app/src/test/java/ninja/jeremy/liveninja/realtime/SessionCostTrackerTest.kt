package ninja.jeremy.liveninja.realtime

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The live cost badge's arithmetic. Pinned against the web implementation
 * (web/static/js/conversation.mjs) because both surfaces price the same session
 * and must not disagree.
 */
class SessionCostTrackerTest {

    private val tracker = SessionCostTracker()

    // $1 per 1M for every uncached bucket, $0.10 for cached — round numbers so an
    // expected total is readable rather than reverse-engineered from the code.
    private val rates = RealtimeRates(
        textInPer1M = 1.0,
        textOutPer1M = 1.0,
        audioInPer1M = 1.0,
        audioOutPer1M = 1.0,
        cachedTextInPer1M = 0.1,
        cachedAudioInPer1M = 0.1,
    )

    private fun usage(
        inText: Int = 0,
        inAudio: Int = 0,
        outText: Int = 0,
        outAudio: Int = 0,
        cachedText: Int = 0,
        cachedAudio: Int = 0,
    ) = JSONObject().apply {
        put(
            "input_token_details",
            JSONObject().apply {
                put("text_tokens", inText)
                put("audio_tokens", inAudio)
                if (cachedText > 0 || cachedAudio > 0) {
                    put(
                        "cached_tokens_details",
                        JSONObject().apply {
                            put("text_tokens", cachedText)
                            put("audio_tokens", cachedAudio)
                        },
                    )
                }
            },
        )
        put(
            "output_token_details",
            JSONObject().apply {
                put("text_tokens", outText)
                put("audio_tokens", outAudio)
            },
        )
    }

    @Test
    fun accumulatesAcrossTurns() {
        tracker.add(usage(inText = 1_000_000), rates)
        val cost = tracker.add(usage(outAudio = 1_000_000), rates)

        assertEquals(2.0, cost!!.usd, 1e-9)
        assertEquals(1_000_000, cost.textTokens)
        assertEquals(1_000_000, cost.audioTokens)
    }

    /**
     * The subtlety worth a test: cached counts are reported *inside*
     * input_token_details, so uncached input is the difference. Adding them
     * instead would bill 1.1M tokens for a 1M-token turn.
     */
    @Test
    fun cachedTokensAreASubsetOfInput_notAnAddition() {
        val cost = tracker.add(
            usage(inText = 1_000_000, cachedText = 400_000),
            rates,
        )!!

        // 600k uncached @ $1 + 400k cached @ $0.10 = 0.60 + 0.04
        assertEquals(0.64, cost.usd, 1e-9)
        // Token *count* still reflects every token processed.
        assertEquals(1_000_000, cost.textTokens)
    }

    @Test
    fun cachedExceedingParent_neverGoesNegative() {
        val cost = tracker.add(usage(inAudio = 100, cachedAudio = 500), rates)!!
        assertTrue(cost.usd >= 0.0)
    }

    @Test
    fun noRates_meansNoEstimate() {
        assertNull(tracker.add(usage(inText = 1000), rates = null))
        assertFalse(tracker.cost.hasData)
    }

    @Test
    fun usageWithNoTokenDetails_isIgnored() {
        assertNull(tracker.add(JSONObject(), rates))
        assertNull(tracker.add(null, rates))
        assertFalse(tracker.cost.hasData)
    }

    @Test
    fun zeroTokenUsage_doesNotEmitAnUpdate() {
        assertNull(tracker.add(usage(), rates))
    }

    @Test
    fun resetClearsTheRunningTotal() {
        tracker.add(usage(inText = 1_000_000), rates)
        tracker.reset()
        assertEquals(0.0, tracker.cost.usd, 1e-9)
        assertFalse(tracker.cost.hasData)
    }

    @Test
    fun allZeroRatesObject_isTreatedAsNoRates() {
        // Would otherwise render a permanent "~$0.000", which reads as "free".
        val json = JSONObject().apply {
            put("textInPer1M", 0.0)
            put("audioOutPer1M", 0.0)
        }
        assertNull(RealtimeRates.from(json))
        assertNull(RealtimeRates.from(null))
    }

    @Test
    fun ratesParseFromTheServerShape() {
        val parsed = RealtimeRates.from(
            JSONObject(
                """
                {"textInPer1M":4.0,"textOutPer1M":16.0,"audioInPer1M":32.0,
                 "audioOutPer1M":64.0,"cachedTextInPer1M":0.4,"cachedAudioInPer1M":0.4}
                """.trimIndent(),
            ),
        )!!
        assertEquals(4.0, parsed.textInPer1M, 1e-9)
        assertEquals(64.0, parsed.audioOutPer1M, 1e-9)
        assertEquals(0.4, parsed.cachedAudioInPer1M, 1e-9)
    }

    @Test
    fun badgeShowsThreeDecimals_soSubCentSessionsAreNotZero() {
        tracker.add(usage(inText = 1234), rates)
        assertEquals("~$0.001", tracker.cost.badgeText())
    }
}
