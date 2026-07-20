package ninja.jeremy.liveninja.log

import android.content.Context
import android.content.SharedPreferences
import io.mockk.Runs
import io.mockk.every
import io.mockk.just
import io.mockk.mockk
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import ninja.jeremy.liveninja.ui.state.SettingsStore
import org.junit.After
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/**
 * Proves the pinned bridge design (plan M3.1): [LNLog.sink] starts null
 * (logcat passthrough only, never a crash), and a constructed [LogSink]
 * self-registers into it from its own `init {}` — no explicit wiring call
 * needed at any call site. Also proves the M3.2 SettingsStore-diagnostics
 * wiring end to end.
 */
class LNLogTest {

    @get:Rule
    val tempFolder = TemporaryFolder()

    @After
    fun tearDown() {
        // LNLog is a JVM-wide singleton object; don't leak state into other test classes.
        LNLog.sink = null
    }

    /** In-memory-backed real SettingsStore (mirrors ui/state/SettingsStoreTest.kt's fake). */
    private fun fakeSettingsStore(): SettingsStore {
        val storage = HashMap<String, String?>()
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

    private fun fakeFilesContext(): Context {
        val context = mockk<Context>()
        every { context.filesDir } returns tempFolder.newFolder("files-${System.nanoTime()}")
        return context
    }

    private suspend fun awaitCondition(timeoutMs: Long = 2_000, block: () -> Boolean) {
        withTimeout(timeoutMs) {
            while (!block()) delay(5)
        }
    }

    @Test
    fun `null sink is the default and logcat calls still return without throwing`() {
        assertTrue(LNLog.sink == null)
        // android.util.Log is stubbed to return defaults under
        // testOptions.unitTests.isReturnDefaultValues=true — this must not throw.
        LNLog.d(LogCategory.WAKE, "Tag", "no sink registered yet")
    }

    @Test
    fun `constructing a LogSink self-registers it into LNLog sink`() {
        assertTrue(LNLog.sink == null)
        val sink = LogSink(fakeFilesContext(), fakeSettingsStore())
        assertTrue(LNLog.sink === sink)
    }

    @Test
    fun `entries routed through the facade reach the registered sink redacted`() = runBlocking {
        val sink = LogSink(fakeFilesContext(), fakeSettingsStore())

        val jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
        LNLog.e(LogCategory.AUTH, "AuthInterceptor", "Authorization: Bearer $jwt")
        sink.awaitIdle()

        val recorded = sink.entries.single()
        assertTrue(recorded.category == LogCategory.AUTH)
        assertTrue(recorded.level == LogLevel.ERROR)
        assertFalse(recorded.message.contains(jwt))
    }

    @Test
    fun `applies the SettingsStore diagnostics config already present at construction`() = runBlocking {
        val settingsStore = fakeSettingsStore()
        settingsStore.setDiagnosticsMinLevel("ERROR")
        settingsStore.setDiagnosticsCategory("NET", false)

        val sink = LogSink(fakeFilesContext(), settingsStore)

        awaitCondition { sink.minLevel == LogLevel.ERROR }
        assertTrue(sink.categoryEnabled[LogCategory.NET] == false)
        assertTrue(sink.categoryEnabled[LogCategory.WAKE] == true)
    }

    @Test
    fun `diagnostics changes after construction propagate live`() = runBlocking {
        val settingsStore = fakeSettingsStore()
        val sink = LogSink(fakeFilesContext(), settingsStore)
        awaitCondition { sink.enabled } // initial default (owner: enabled=true)

        settingsStore.setDiagnosticsEnabled(false)
        awaitCondition { !sink.enabled }

        settingsStore.setDiagnosticsEnabled(true)
        settingsStore.setDiagnosticsMinLevel("WARN")
        awaitCondition { sink.minLevel == LogLevel.WARN }
    }
}
