package ninja.jeremy.liveninja.realtime

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class NovaBridgeSessionStartFrameTest {

    @Test
    fun frame_wrapsServerConfigWithoutChangingItsShape() {
        val config = JSONObject()
            .put("voice", "matthew")
            .put("sampleRateIn", 16_000)
            .put("sampleRateOut", 24_000)
            .put("systemPrompt", "Be concise.")
            .put(
                "tools",
                JSONArray().put(
                    JSONObject()
                        .put("name", "get_weather")
                        .put("inputSchema", JSONObject().put("type", "object")),
                ),
            )

        val frame = JSONObject(buildNovaSessionStartFrame(config))

        assertEquals(setOf("type", "config"), frame.keys().asSequence().toSet())
        assertEquals("session.start", frame.getString("type"))
        val framedConfig = frame.getJSONObject("config")
        assertEquals("matthew", framedConfig.getString("voice"))
        assertEquals(16_000, framedConfig.getInt("sampleRateIn"))
        assertEquals(24_000, framedConfig.getInt("sampleRateOut"))
        assertEquals("Be concise.", framedConfig.getString("systemPrompt"))
        assertEquals(
            "get_weather",
            framedConfig.getJSONArray("tools").getJSONObject(0).getString("name"),
        )
    }

    @Test
    fun frame_ownsADeepCopyOfConfig() {
        val nested = JSONObject().put("type", "object")
        val config = JSONObject().put("inputSchema", nested)

        val frameText = buildNovaSessionStartFrame(config)
        nested.put("type", "string")

        assertEquals(
            "object",
            JSONObject(frameText)
                .getJSONObject("config")
                .getJSONObject("inputSchema")
                .getString("type"),
        )
    }

    @Test
    fun interruptedAssistantTurnEnd_isTheImmediatePlaybackFlushSignal() {
        assertTrue(
            novaAssistantTurnWasInterrupted(
                JSONObject()
                    .put("type", "turn.end")
                    .put("role", "assistant")
                    .put("interrupted", true),
            ),
        )
        assertFalse(
            novaAssistantTurnWasInterrupted(
                JSONObject()
                    .put("type", "turn.end")
                    .put("role", "assistant")
                    .put("interrupted", false),
            ),
        )
        assertFalse(
            novaAssistantTurnWasInterrupted(
                JSONObject()
                    .put("type", "turn.end")
                    .put("role", "user")
                    .put("interrupted", true),
            ),
        )
    }
}
