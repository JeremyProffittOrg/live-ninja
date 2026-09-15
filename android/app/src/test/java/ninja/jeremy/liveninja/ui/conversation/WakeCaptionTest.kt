package ninja.jeremy.liveninja.ui.conversation

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * Regression tests for the idle-screen wake caption (2026-09-15: the home screen said
 * "Or just say Hey Jarvis" on a phone where nothing was listening and Hey Live Ninja was
 * the selected phrase).
 */
class WakeCaptionTest {

    private val labels = mapOf(
        "hey-jarvis" to "“Hey Jarvis”",
        "hey-live-ninja" to "“Hey Live Ninja”",
        "hey-live-ninja-47df2e" to "“Hey Live Ninja”",
    )
    private val labelFor: (String) -> String = { wakePhraseLabel(it, labels[it]) }

    @Test
    fun `service off never advertises a phrase`() {
        val c = wakeCaption(
            selectedId = "hey-live-ninja",
            activeId = "hey-jarvis",
            serviceRunning = false,
            labelFor = labelFor,
        )
        assertEquals(WakeCaptionKind.OFF, c.kind)
        assertEquals("Hey Live Ninja", c.selectedLabel)
    }

    @Test
    fun `running with the selected model loaded is listening for the selected phrase`() {
        val c = wakeCaption(
            selectedId = "hey-live-ninja-47df2e",
            activeId = "hey-live-ninja-47df2e",
            serviceRunning = true,
            labelFor = labelFor,
        )
        assertEquals(WakeCaptionKind.LISTENING, c.kind)
        assertEquals("Hey Live Ninja", c.activeLabel)
        assertEquals("Hey Live Ninja", c.selectedLabel)
    }

    @Test
    fun `running with a different model loaded reports both phrases`() {
        // The bundled hey-jarvis asset is what listens until the selected phrase downloads.
        val c = wakeCaption(
            selectedId = "hey-live-ninja",
            activeId = "hey-jarvis",
            serviceRunning = true,
            labelFor = labelFor,
        )
        assertEquals(WakeCaptionKind.MODEL_PENDING, c.kind)
        assertEquals("Hey Jarvis", c.activeLabel)
        assertEquals("Hey Live Ninja", c.selectedLabel)
    }

    @Test
    fun `empty active id falls back to the selection instead of flagging a mismatch`() {
        val c = wakeCaption(
            selectedId = "hey-live-ninja",
            activeId = "",
            serviceRunning = true,
            labelFor = labelFor,
        )
        assertEquals(WakeCaptionKind.LISTENING, c.kind)
        assertEquals("Hey Live Ninja", c.activeLabel)
    }

    @Test
    fun `label prefers the catalog and strips its quotes`() {
        assertEquals("Hey Jarvis", wakePhraseLabel("hey-jarvis", "“Hey Jarvis”"))
    }

    @Test
    fun `label without a catalog entry drops the trained-model suffix`() {
        assertEquals("Hey Live Ninja", wakePhraseLabel("hey-live-ninja-47df2e", null))
        assertEquals("Okay Joshua", wakePhraseLabel("okay-joshua-5996e4", null))
        assertEquals("Hey Ninja", wakePhraseLabel("hey-ninja", null))
    }
}
