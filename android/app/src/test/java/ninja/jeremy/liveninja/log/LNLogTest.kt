package ninja.jeremy.liveninja.log

import android.content.Context
import io.mockk.every
import io.mockk.mockk
import kotlinx.coroutines.runBlocking
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
 * needed at any call site.
 */
class LNLogTest {

    @get:Rule
    val tempFolder = TemporaryFolder()

    @After
    fun tearDown() {
        // LNLog is a JVM-wide singleton object; don't leak state into other test classes.
        LNLog.sink = null
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
        val context = mockk<Context>()
        every { context.filesDir } returns tempFolder.newFolder("files")

        assertTrue(LNLog.sink == null)
        val sink = LogSink(context)
        assertTrue(LNLog.sink === sink)
    }

    @Test
    fun `entries routed through the facade reach the registered sink redacted`() = runBlocking {
        val context = mockk<Context>()
        every { context.filesDir } returns tempFolder.newFolder("files")
        val sink = LogSink(context)

        val jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
        LNLog.e(LogCategory.AUTH, "AuthInterceptor", "Authorization: Bearer $jwt")
        sink.awaitIdle()

        val recorded = sink.entries.single()
        assertTrue(recorded.category == LogCategory.AUTH)
        assertTrue(recorded.level == LogLevel.ERROR)
        assertFalse(recorded.message.contains(jwt))
    }
}
