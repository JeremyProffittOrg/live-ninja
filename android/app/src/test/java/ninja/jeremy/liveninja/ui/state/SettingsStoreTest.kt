package ninja.jeremy.liveninja.ui.state

import android.content.Context
import android.content.SharedPreferences
import io.mockk.Runs
import io.mockk.every
import io.mockk.just
import io.mockk.mockk
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * SettingsStore serialization round-trip + unknown-key tolerance for the M1.2
 * schema additions (voice toggles, appStyle, diagnostics). Backed by an
 * in-memory fake of SharedPreferences so the store's persist→reload path is
 * exercised for real on the JVM (org.json is the real impl in unit tests).
 */
class SettingsStoreTest {

    /** In-memory prefs backing shared between a store and its "reloaded" twin. */
    private class FakeBacking {
        val storage = HashMap<String, String?>()

        fun store(): SettingsStore {
            val editor = mockk<SharedPreferences.Editor>()
            every { editor.putString(any(), any()) } answers {
                storage[firstArg()] = secondArg<String?>()
                editor
            }
            every { editor.apply() } just Runs

            val prefs = mockk<SharedPreferences>()
            every { prefs.getString(any(), any()) } answers {
                val key = firstArg<String>()
                if (storage.containsKey(key)) storage[key] else secondArg()
            }
            every { prefs.edit() } returns editor

            val context = mockk<Context>()
            every { context.getSharedPreferences(any(), any()) } returns prefs
            return SettingsStore(context)
        }
    }

    private fun stored(backing: FakeBacking): JSONObject =
        JSONObject(backing.storage["settings_document_v1"]!!)

    @Test
    fun defaults_carryOwnerBakedValues() {
        val store = FakeBacking().store()
        val doc = store.document.value

        assertTrue(doc.lockedSessions)
        assertTrue(doc.wakeScreenOnWake)
        assertFalse(doc.keepScreenOn)
        assertEquals("hal9000", doc.appStyle)
        assertTrue(doc.diagnostics.enabled)
        assertEquals("VERBOSE", doc.diagnostics.minLevel)
        assertEquals(8, doc.diagnostics.categories.size)
        assertTrue(doc.diagnostics.categories.values.all { it })
        assertEquals(
            DiagnosticsConfig.CATEGORY_KEYS.toSet(),
            doc.diagnostics.categories.keys,
        )
    }

    @Test
    fun setters_roundTripThroughPersistence() {
        val backing = FakeBacking()
        val store = backing.store()

        store.setLockedSessions(false)
        store.setWakeScreenOnWake(false)
        store.setKeepScreenOn(true)
        store.setAppStyle("terminal")
        store.setDiagnosticsEnabled(false)
        store.setDiagnosticsMinLevel("WARN")
        store.setDiagnosticsCategory("AUDIO", false)

        // Reload from the same backing to prove values survived serialization.
        val reloaded = backing.store().document.value
        assertFalse(reloaded.lockedSessions)
        assertFalse(reloaded.wakeScreenOnWake)
        assertTrue(reloaded.keepScreenOn)
        assertEquals("terminal", reloaded.appStyle)
        assertFalse(reloaded.diagnostics.enabled)
        assertEquals("WARN", reloaded.diagnostics.minLevel)
        assertEquals(false, reloaded.diagnostics.categories["AUDIO"])
        // Untouched categories stay on.
        assertEquals(true, reloaded.diagnostics.categories["WAKE"])
    }

    @Test
    fun setDiagnostics_replacesWholeConfig() {
        val backing = FakeBacking()
        val store = backing.store()

        val custom = DiagnosticsConfig(
            enabled = true,
            minLevel = "INFO",
            categories = DiagnosticsConfig.CATEGORY_KEYS.associateWith { false },
        )
        store.setDiagnostics(custom)

        val reloaded = backing.store().document.value
        assertEquals("INFO", reloaded.diagnostics.minLevel)
        assertTrue(reloaded.diagnostics.categories.values.none { it })
    }

    @Test
    fun unknownKeys_preservedThroughWrite() {
        val backing = FakeBacking()
        // Seed a document with a future/unknown top-level key and an unknown
        // diagnostics category, plus a legacy shape missing the new keys.
        val seed = JSONObject().apply {
            put("version", 7)
            put("wakeWord", "hey-live-ninja")
            put("futureFlag", "keep-me")
            put(
                "diagnostics",
                JSONObject().apply {
                    put("enabled", true)
                    put("minLevel", "DEBUG")
                    put(
                        "categories",
                        JSONObject().apply {
                            put("WAKE", false)
                            put("FUTURE_CATEGORY", true)
                        },
                    )
                },
            )
        }
        backing.storage["settings_document_v1"] = seed.toString()

        val store = backing.store()
        // Missing new keys read as owner defaults.
        assertTrue(store.document.value.lockedSessions)
        assertEquals("hal9000", store.document.value.appStyle)
        // Known category honored, others default on.
        assertEquals(false, store.document.value.diagnostics.categories["WAKE"])
        assertEquals(true, store.document.value.diagnostics.categories["AUDIO"])
        assertEquals("DEBUG", store.document.value.diagnostics.minLevel)

        // A write must preserve the unknown top-level key and unknown category.
        store.setAppStyle("ninja")
        val persisted = stored(backing)
        assertEquals("keep-me", persisted.optString("futureFlag"))
        assertEquals("ninja", persisted.optString("appStyle"))
        assertTrue(
            persisted.getJSONObject("diagnostics")
                .getJSONObject("categories")
                .optBoolean("FUTURE_CATEGORY"),
        )
        // Version bumped by the write path.
        assertEquals(8, persisted.optInt("version"))
    }
}
