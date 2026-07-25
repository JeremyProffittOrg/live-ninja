package ninja.jeremy.liveninja.realtime

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The device-local session-tool mapping (owner request 2026-07-25). These tools must not
 * be forwarded to `POST /api/v1/tools/invoke`, so a regression that dropped interception
 * would send them to a backend that cannot perform them and return `not_configured` — the
 * model would then tell the user listening stopped when nothing stopped. Device-local
 * volume control has its own argument-aware mapping in DeviceVolumeToolsTest.
 */
class DeviceSessionToolsTest {

    @Test
    fun theTwoDeviceToolsAreRecognised() {
        assertEquals(
            DeviceSessionTool.STOP_LISTENING,
            DeviceSessionTool.forName("stop_listening"),
        )
        assertEquals(
            DeviceSessionTool.START_NEW_CONVERSATION,
            DeviceSessionTool.forName("start_new_conversation"),
        )
    }

    /** Everything else must fall through to the backend router, untouched. */
    @Test
    fun backendToolsAreNotIntercepted() {
        for (name in listOf(
            "get_weather", "set_timer", "memory_search", "remember_note",
            "profile_suggest", "deliverable_create", "send_email",
        )) {
            assertNull("$name must go to the backend router", DeviceSessionTool.forName(name))
        }
    }

    @Test
    fun outputMirrorsTheBackendResultShapeSoTheModelCannotTellThePathsApart() {
        val json = JSONObject(deviceToolOutput(DeviceSessionTool.STOP_LISTENING, "call-123"))

        assertEquals("stop_listening", json.getString("tool"))
        assertEquals("call-123", json.getString("callId"))
        assertTrue(json.getBoolean("ok"))
        assertTrue(json.getJSONObject("output").getBoolean("acknowledged"))
    }

    /**
     * The output must not claim the action already happened: it is deferred until the
     * assistant finishes speaking, so "acknowledged" is the honest word and the model is
     * told to keep the confirmation brief.
     */
    @Test
    fun outputAcknowledgesRatherThanClaimingDone() {
        for (tool in DeviceSessionTool.entries) {
            val out = JSONObject(deviceToolOutput(tool, "c")).getJSONObject("output")
            assertTrue(out.has("acknowledged"))
            assertTrue("must instruct the model what to say", out.getString("instruction").isNotBlank())
        }
    }

    @Test
    fun startNewConversationTellsTheModelNotToSummariseTheOldOne() {
        val out = JSONObject(deviceToolOutput(DeviceSessionTool.START_NEW_CONVERSATION, "c"))
            .getJSONObject("output").getString("instruction")
        assertTrue(out.contains("not summarise", ignoreCase = true))
    }
}
