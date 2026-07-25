package ninja.jeremy.liveninja.realtime

import java.util.Locale
import org.json.JSONObject

/**
 * Per-1M-token USD list rates for the minted model, as sent in the session
 * bootstrap's `rates` object (internal/realtime/rates.go, surfaced by
 * internal/webapp/api_routes.go). Absent for `nova-bridge`, whose usage events
 * are never surfaced to the client — a null [RealtimeRates] means "no estimate
 * is possible", not "free".
 */
data class RealtimeRates(
    val textInPer1M: Double,
    val textOutPer1M: Double,
    val audioInPer1M: Double,
    val audioOutPer1M: Double,
    val cachedTextInPer1M: Double,
    val cachedAudioInPer1M: Double,
) {
    companion object {
        /** Parses the bootstrap's `rates` object; null when absent or unusable. */
        fun from(json: JSONObject?): RealtimeRates? {
            if (json == null) return null
            val rates = RealtimeRates(
                textInPer1M = json.optDouble("textInPer1M", 0.0),
                textOutPer1M = json.optDouble("textOutPer1M", 0.0),
                audioInPer1M = json.optDouble("audioInPer1M", 0.0),
                audioOutPer1M = json.optDouble("audioOutPer1M", 0.0),
                cachedTextInPer1M = json.optDouble("cachedTextInPer1M", 0.0),
                cachedAudioInPer1M = json.optDouble("cachedAudioInPer1M", 0.0),
            )
            // An all-zero object would render a permanent "~$0.000", which reads
            // as "this is free" rather than "we don't know".
            return rates.takeIf { it.anyNonZero() }
        }
    }

    private fun anyNonZero(): Boolean =
        textInPer1M > 0 || textOutPer1M > 0 || audioInPer1M > 0 ||
            audioOutPer1M > 0 || cachedTextInPer1M > 0 || cachedAudioInPer1M > 0
}

/** Running cost estimate for one session. */
data class SessionCost(
    val usd: Double = 0.0,
    val textTokens: Int = 0,
    val audioTokens: Int = 0,
) {
    val hasData: Boolean get() = textTokens > 0 || audioTokens > 0
}

/**
 * Accumulates a session's list-price cost estimate from realtime `response.done`
 * usage payloads — the Android counterpart of the web cost badge in
 * web/static/js/conversation.mjs, deliberately using the identical formula so
 * the two surfaces cannot disagree about the same session.
 *
 * Two details carry the whole correctness of the estimate:
 *  - **Cached tokens are a subset of the input details, not a sibling.** The
 *    provider reports `input_token_details.cached_tokens_details` *within*
 *    `input_token_details`, so uncached input is the difference. Adding them
 *    instead of subtracting double-counts every cached token and (because cached
 *    rates are cheaper) silently overstates the bill.
 *  - **This is an estimate at list price, never a bill.** The server re-derives
 *    the authoritative figure; the client value is sanitized and bounded on
 *    arrival (`sanitizeSessionCost` in internal/webapp/api_routes.go).
 *
 * Not thread-safe by design: it is driven from the single session event stream.
 */
class SessionCostTracker {

    var cost: SessionCost = SessionCost()
        private set

    /** Drop all accumulated usage — call on session start, not session end. */
    fun reset() {
        cost = SessionCost()
    }

    /**
     * Fold one `response.done` usage payload in and return the new running
     * total, or null if there was nothing usable to add (no rates for this
     * engine, or a usage object with no token details).
     */
    fun add(usage: JSONObject?, rates: RealtimeRates?): SessionCost? {
        if (usage == null || rates == null) return null

        val inDetails = usage.optJSONObject("input_token_details")
        val outDetails = usage.optJSONObject("output_token_details")
        if (inDetails == null && outDetails == null) return null

        val cachedDetails = inDetails?.optJSONObject("cached_tokens_details")
        val inTextCached = cachedDetails?.optInt("text_tokens", 0) ?: 0
        val inAudioCached = cachedDetails?.optInt("audio_tokens", 0) ?: 0
        // Cached counts are included in the parent totals — subtract, don't add.
        val inText = ((inDetails?.optInt("text_tokens", 0) ?: 0) - inTextCached).coerceAtLeast(0)
        val inAudio = ((inDetails?.optInt("audio_tokens", 0) ?: 0) - inAudioCached).coerceAtLeast(0)
        val outText = outDetails?.optInt("text_tokens", 0) ?: 0
        val outAudio = outDetails?.optInt("audio_tokens", 0) ?: 0

        val deltaUSD = (
            inText * rates.textInPer1M +
                inTextCached * rates.cachedTextInPer1M +
                inAudio * rates.audioInPer1M +
                inAudioCached * rates.cachedAudioInPer1M +
                outText * rates.textOutPer1M +
                outAudio * rates.audioOutPer1M
            ) / 1_000_000.0

        val deltaText = inText + inTextCached + outText
        val deltaAudio = inAudio + inAudioCached + outAudio
        if (deltaText == 0 && deltaAudio == 0) return null

        cost = SessionCost(
            usd = cost.usd + deltaUSD,
            textTokens = cost.textTokens + deltaText,
            audioTokens = cost.audioTokens + deltaAudio,
        )
        return cost
    }
}

/**
 * Badge text for [SessionCost]. Three decimals because a whole conversation
 * routinely lands under a cent, and a "$0.00" badge would tell the user nothing
 * about a session that is actually accruing cost.
 *
 * [Locale.US] is pinned deliberately, not defensively: the default locale would render
 * "~$0,003" wherever the comma is the decimal separator, which both misreads as a
 * thousands-grouped figure next to a "$" and breaks the whole point of sharing web's
 * formula — web/static/js/conversation.mjs formats this same number with a dot, and the
 * two surfaces must not disagree about the same session.
 */
fun SessionCost.badgeText(): String = "~$" + String.format(Locale.US, "%.3f", usd)
