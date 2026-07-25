package ninja.jeremy.liveninja.wake

import android.content.Context
import android.content.SharedPreferences
import io.mockk.Runs
import io.mockk.every
import io.mockk.just
import io.mockk.mockk
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * [WakePreferences] is the persisted half of the WS-5 M21.0/M21.4 switch state machine
 * ([WakeSwitchState]): its setters are the only place [serviceEnabled]/[WakePreferences.muted]/
 * etc. are written, and each one must mirror synchronously into the matching
 * `MutableStateFlow` — a Settings screen collecting `serviceEnabledFlow` has to see the write
 * immediately, not on the next process start. Backed by an in-memory fake of
 * SharedPreferences, same pattern as SettingsStoreTest.
 */
class WakePreferencesTest {

    /** In-memory prefs backing so writes and the constructed defaults agree. */
    private class FakeBacking {
        val storage = HashMap<String, Any?>()

        fun prefs(): WakePreferences {
            val editor = mockk<SharedPreferences.Editor>()
            every { editor.putBoolean(any(), any()) } answers {
                storage[firstArg()] = secondArg<Boolean>()
                editor
            }
            every { editor.putString(any(), any()) } answers {
                storage[firstArg()] = secondArg<String?>()
                editor
            }
            every { editor.putFloat(any(), any()) } answers {
                storage[firstArg()] = secondArg<Float>()
                editor
            }
            every { editor.apply() } just Runs

            val sharedPrefs = mockk<SharedPreferences>()
            every { sharedPrefs.getBoolean(any(), any()) } answers {
                storage[firstArg()] as? Boolean ?: secondArg()
            }
            every { sharedPrefs.getString(any(), any()) } answers {
                if (storage.containsKey(firstArg())) storage[firstArg()] as? String else secondArg()
            }
            every { sharedPrefs.getFloat(any(), any()) } answers {
                storage[firstArg()] as? Float ?: secondArg()
            }
            every { sharedPrefs.edit() } returns editor

            val context = mockk<Context>()
            every { context.getSharedPreferences(any(), any()) } returns sharedPrefs
            return WakePreferences(context)
        }
    }

    @Test
    fun `serviceEnabled defaults false and mirrors into the observable flow on write`() {
        val prefs = FakeBacking().prefs()
        assertFalse(prefs.serviceEnabled)
        assertFalse(prefs.serviceEnabledFlow.value)

        prefs.serviceEnabled = true

        assertTrue(prefs.serviceEnabled)
        assertTrue(
            "a collector of serviceEnabledFlow must see the new value immediately",
            prefs.serviceEnabledFlow.value,
        )

        prefs.serviceEnabled = false
        assertFalse(prefs.serviceEnabledFlow.value)
    }

    @Test
    fun `muted mirrors into mutedFlow both directions`() {
        val prefs = FakeBacking().prefs()
        assertFalse(prefs.muted)

        prefs.muted = true
        assertTrue(prefs.mutedFlow.value)

        prefs.muted = false
        assertFalse(prefs.mutedFlow.value)
    }

    @Test
    fun `wakeWordId defaults to the platform default and round-trips`() {
        val prefs = FakeBacking().prefs()
        assertEquals(WakePreferences.DEFAULT_WAKE_WORD_ID, prefs.wakeWordId)

        prefs.wakeWordId = "hey-jarvis"
        assertEquals("hey-jarvis", prefs.wakeWordId)
    }

    @Test
    fun `wakeEngine defaults to openWakeWord and round-trips`() {
        val prefs = FakeBacking().prefs()
        assertEquals(WakePreferences.ENGINE_OPENWAKEWORD, prefs.wakeEngine)

        prefs.wakeEngine = WakePreferences.ENGINE_PORCUPINE
        assertEquals(WakePreferences.ENGINE_PORCUPINE, prefs.wakeEngine)
    }

    @Test
    fun `sensitivity clamps into 0 to 1 and mirrors the flow`() {
        val prefs = FakeBacking().prefs()
        assertEquals(0.5f, prefs.sensitivity, 0f)

        prefs.sensitivity = 1.4f
        assertEquals(1f, prefs.sensitivity, 0f)
        assertEquals(1f, prefs.sensitivityFlow.value, 0f)

        prefs.sensitivity = -0.2f
        assertEquals(0f, prefs.sensitivity, 0f)
        assertEquals(0f, prefs.sensitivityFlow.value, 0f)
    }
}
