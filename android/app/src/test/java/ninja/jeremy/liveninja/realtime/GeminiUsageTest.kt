package ninja.jeremy.liveninja.realtime

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class GeminiUsageTest {

    @Test
    fun `usage metadata maps to the shared input-output modality shape`() {
        val normalized = GeminiUsageNormalizer.normalize(
            metadata(
                total = 655,
                prompt = """
                    [
                      {"modality":"TEXT","tokenCount":100},
                      {"modality":"AUDIO","tokenCount":200},
                      {"modality":"VIDEO","tokenCount":5}
                    ]
                """,
                response = """
                    [
                      {"modality":"AUDIO","tokenCount":300},
                      {"modality":"TEXT","tokenCount":50}
                    ]
                """,
            ),
        )

        assertEquals(655, normalized.getInt("total_tokens"))
        normalized.getJSONObject("input_token_details").let { input ->
            assertEquals(105, input.getInt("text_tokens"))
            assertEquals(200, input.getInt("audio_tokens"))
            assertEquals(0, input.getJSONObject("cached_tokens_details").length())
        }
        normalized.getJSONObject("output_token_details").let { output ->
            assertEquals(50, output.getInt("text_tokens"))
            assertEquals(300, output.getInt("audio_tokens"))
        }
    }

    @Test
    fun `latest turn snapshot is consumed once and priced once`() {
        val buffer = GeminiTurnUsageBuffer()
        buffer.observe(
            metadata(
                total = 2,
                prompt = """[{"modality":"TEXT","tokenCount":1}]""",
                response = """[{"modality":"AUDIO","tokenCount":1}]""",
            ),
        )
        buffer.observe(
            metadata(
                total = 4_000_000,
                prompt = """
                    [
                      {"modality":"TEXT","tokenCount":1000000},
                      {"modality":"AUDIO","tokenCount":1000000}
                    ]
                """,
                response = """
                    [
                      {"modality":"TEXT","tokenCount":1000000},
                      {"modality":"AUDIO","tokenCount":1000000}
                    ]
                """,
            ),
        )

        val tracker = SessionCostTracker()
        val cost = tracker.add(buffer.consume(), GEMINI_RATES)!!

        assertEquals(20.25, cost.usd, 1e-9)
        assertEquals(2_000_000, cost.textTokens)
        assertEquals(2_000_000, cost.audioTokens)
        assertNull(buffer.consume())
        assertEquals(20.25, tracker.cost.usd, 1e-9)
    }

    @Test
    fun `thinking tool prompts and aggregate fallbacks remain billable`() {
        val normalized = GeminiUsageNormalizer.normalize(
            JSONObject()
                .put("totalTokenCount", 48)
                // Details account for only part of prompt/response; unknown
                // modality remainder is conservatively priced as audio.
                .put("promptTokenCount", 20)
                .put(
                    "promptTokensDetails",
                    org.json.JSONArray("""[{"modality":"TEXT","tokenCount":5}]"""),
                )
                .put("candidatesTokenCount", 10)
                .put(
                    "candidatesTokensDetails",
                    org.json.JSONArray("""[{"modality":"TEXT","tokenCount":2}]"""),
                )
                .put("toolUsePromptTokenCount", 6)
                .put("thoughtsTokenCount", 12),
        )

        normalized.getJSONObject("input_token_details").let { input ->
            assertEquals(11, input.getInt("text_tokens"))
            assertEquals(15, input.getInt("audio_tokens"))
        }
        normalized.getJSONObject("output_token_details").let { output ->
            assertEquals(14, output.getInt("text_tokens"))
            assertEquals(8, output.getInt("audio_tokens"))
        }
    }

    private fun metadata(total: Int, prompt: String, response: String) =
        JSONObject()
            .put("totalTokenCount", total)
            .put("promptTokensDetails", org.json.JSONArray(prompt.trimIndent()))
            .put("responseTokensDetails", org.json.JSONArray(response.trimIndent()))

    private companion object {
        val GEMINI_RATES = RealtimeRates(
            textInPer1M = 0.75,
            textOutPer1M = 4.50,
            audioInPer1M = 3.00,
            audioOutPer1M = 12.00,
            cachedTextInPer1M = 0.75,
            cachedAudioInPer1M = 3.00,
        )
    }
}
