package ninja.jeremy.liveninja.realtime

import org.json.JSONArray
import org.json.JSONObject

/**
 * Converts Gemini Live's per-turn `usageMetadata` into the usage shape shared
 * by OpenAI Realtime, [SessionCostTracker], and the web client.
 *
 * Gemini reports one row per modality. AUDIO keeps its own pricing bucket;
 * TEXT and any future non-audio modality follow the web client's conservative
 * text-rate fallback. Gemini Live has no input caching, so cached details stay
 * empty.
 */
internal object GeminiUsageNormalizer {

    fun normalize(metadata: JSONObject): JSONObject {
        val prompt = withAggregateRemainder(
            split(metadata.optJSONArray("promptTokensDetails")),
            nonNegativeInt(metadata.opt("promptTokenCount")),
            remainderIsAudio = true,
        )
        val toolPrompt = withAggregateRemainder(
            split(metadata.optJSONArray("toolUsePromptTokensDetails")),
            nonNegativeInt(metadata.opt("toolUsePromptTokenCount")),
            remainderIsAudio = false,
        )
        val input = prompt + toolPrompt

        val responseRows = metadata.optJSONArray("responseTokensDetails")
            ?: metadata.optJSONArray("candidatesTokensDetails")
        val responseCount = listOf(
            nonNegativeInt(metadata.opt("responseTokenCount")),
            nonNegativeInt(metadata.opt("candidatesTokenCount")),
        ).firstOrNull { it > 0 } ?: 0
        val response = withAggregateRemainder(
            split(responseRows),
            responseCount,
            remainderIsAudio = true,
        )
        // Gemini output pricing explicitly includes thinking tokens.
        val output = response.copy(
            text = saturatedAdd(response.text, nonNegativeInt(metadata.opt("thoughtsTokenCount"))),
        )
        val summedTotal = saturatedAdd(
            saturatedAdd(input.text, input.audio),
            saturatedAdd(output.text, output.audio),
        )
        val reportedTotal = nonNegativeInt(metadata.opt("totalTokenCount"))

        return JSONObject()
            .put("total_tokens", if (reportedTotal > 0) reportedTotal else summedTotal)
            .put(
                "input_token_details",
                JSONObject()
                    .put("text_tokens", input.text)
                    .put("audio_tokens", input.audio)
                    .put("cached_tokens_details", JSONObject()),
            )
            .put(
                "output_token_details",
                JSONObject()
                    .put("text_tokens", output.text)
                    .put("audio_tokens", output.audio),
            )
    }

    private fun split(rows: JSONArray?): TokenBreakdown {
        var text = 0
        var audio = 0
        for (index in 0 until (rows?.length() ?: 0)) {
            val row = rows?.optJSONObject(index) ?: continue
            val count = nonNegativeInt(row.opt("tokenCount"))
            if (row.optString("modality").equals("AUDIO", ignoreCase = true)) {
                audio = saturatedAdd(audio, count)
            } else {
                text = saturatedAdd(text, count)
            }
        }
        return TokenBreakdown(text, audio)
    }

    private fun withAggregateRemainder(
        detailed: TokenBreakdown,
        aggregate: Int,
        remainderIsAudio: Boolean,
    ): TokenBreakdown {
        val detailedTotal = saturatedAdd(detailed.text, detailed.audio)
        val remainder = (aggregate.toLong() - detailedTotal.toLong())
            .coerceAtLeast(0)
            .coerceAtMost(Int.MAX_VALUE.toLong())
            .toInt()
        return if (remainderIsAudio) {
            detailed.copy(audio = saturatedAdd(detailed.audio, remainder))
        } else {
            detailed.copy(text = saturatedAdd(detailed.text, remainder))
        }
    }

    private fun nonNegativeInt(value: Any?): Int {
        val number = when (value) {
            is Number -> value.toDouble()
            is String -> value.toDoubleOrNull()
            else -> null
        } ?: return 0
        if (!number.isFinite() || number <= 0.0) return 0
        return number.coerceAtMost(Int.MAX_VALUE.toDouble()).toInt()
    }

    private fun saturatedAdd(left: Int, right: Int): Int =
        (left.toLong() + right.toLong()).coerceAtMost(Int.MAX_VALUE.toLong()).toInt()

    private data class TokenBreakdown(val text: Int, val audio: Int) {
        operator fun plus(other: TokenBreakdown) = TokenBreakdown(
            text = saturatedAdd(text, other.text),
            audio = saturatedAdd(audio, other.audio),
        )
    }
}

/**
 * Latest-wins buffer for the current Gemini turn.
 *
 * A server envelope can repeat `usageMetadata` while a turn is settling. Only
 * the last snapshot is billable; [consume] clears it at `turnComplete`, so a
 * duplicate completion cannot add the same tokens twice.
 */
internal class GeminiTurnUsageBuffer {
    private var latest: JSONObject? = null

    @Synchronized
    fun observe(metadata: JSONObject) {
        latest = GeminiUsageNormalizer.normalize(metadata)
    }

    @Synchronized
    fun consume(): JSONObject? = latest.also { latest = null }

    @Synchronized
    fun reset() {
        latest = null
    }
}
