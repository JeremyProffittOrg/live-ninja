package ninja.jeremy.liveninja.wake

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * The bundled openWakeWord heads (0.3.5): every id the shipped catalog offers as "Bundled model"
 * must resolve to an apk asset, and untrained catalog ids must not pretend to.
 */
class ModelManagerBuiltinsTest {

    @Test
    fun `every bundled phrase resolves to its asset`() {
        assertEquals("wakeword/hey_jarvis_v0.1.onnx", ModelManager.builtinAssetPath("hey-jarvis"))
        assertEquals("wakeword/alexa_v0.1.onnx", ModelManager.builtinAssetPath("alexa"))
        assertEquals("wakeword/hey_mycroft_v0.1.onnx", ModelManager.builtinAssetPath("hey-mycroft"))
        assertEquals("wakeword/hey_rhasspy_v0.1.onnx", ModelManager.builtinAssetPath("hey-rhasspy"))
    }

    @Test
    fun `untrained and trained catalog ids are not builtins`() {
        assertNull(ModelManager.builtinAssetPath("hey-ninja"))
        assertNull(ModelManager.builtinAssetPath("hey-live-ninja"))
        assertNull(ModelManager.builtinAssetPath("hey-live-ninja-47df2e"))
    }

    @Test
    fun `the default asset stays hey jarvis`() {
        assertEquals(ModelManager.ASSET_DEFAULT_HEAD, ModelManager.BUILTIN_ASSETS[ModelManager.DEFAULT_ASSET_WAKE_WORD_ID])
    }
}
