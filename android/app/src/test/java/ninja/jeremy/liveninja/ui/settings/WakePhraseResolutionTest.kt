package ninja.jeremy.liveninja.ui.settings

import java.io.File
import ninja.jeremy.liveninja.wake.ModelManager
import ninja.jeremy.liveninja.wake.WakeModelRef
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * WS-5 M21.3 regression tests for [resolveWakePhrase]: the phrase Settings shows/warns about
 * must come from the loaded head model ([WakeModelRef]), never the catalog selection alone.
 */
class WakePhraseResolutionTest {

    @Test
    fun `downloaded model matching the selection is not a mismatch`() {
        val active = WakeModelRef.Downloaded("hey-live-ninja", File("hey-live-ninja.onnx"), "abc123")
        val result = resolveWakePhrase(selectedId = "hey-live-ninja", active = active)

        assertEquals("hey-live-ninja", result.activeId)
        assertFalse(result.mismatched)
    }

    @Test
    fun `selection with no model synced yet warns using the phrase that truly works`() {
        // The user picked hey-live-ninja but the device is still serving the bundled
        // fallback — this is exactly the WS-5 M21.3 defect if left unreported.
        val active = WakeModelRef.Asset(ModelManager.DEFAULT_ASSET_WAKE_WORD_ID, ModelManager.ASSET_DEFAULT_HEAD)
        val result = resolveWakePhrase(selectedId = "hey-live-ninja", active = active)

        assertTrue(result.mismatched)
        assertEquals("hey-jarvis", result.activeId)
    }

    @Test
    fun `bundled offline fallback resolves truthfully when it is also the selection`() {
        val active = WakeModelRef.Asset(ModelManager.DEFAULT_ASSET_WAKE_WORD_ID, ModelManager.ASSET_DEFAULT_HEAD)
        val result = resolveWakePhrase(selectedId = ModelManager.DEFAULT_ASSET_WAKE_WORD_ID, active = active)

        assertEquals("hey-jarvis", result.activeId)
        assertFalse(result.mismatched)
    }

    @Test
    fun `downloaded custom trained phrase is honored once it matches the selection`() {
        val active = WakeModelRef.Downloaded("custom-abc123", File("custom.onnx"), "deadbeef")
        val result = resolveWakePhrase(selectedId = "custom-abc123", active = active)

        assertEquals("custom-abc123", result.activeId)
        assertFalse(result.mismatched)
    }

    @Test
    fun `an unset selection never claims a mismatch`() {
        val active = WakeModelRef.Asset(ModelManager.DEFAULT_ASSET_WAKE_WORD_ID, ModelManager.ASSET_DEFAULT_HEAD)
        val result = resolveWakePhrase(selectedId = "", active = active)

        assertFalse(result.mismatched)
    }
}
