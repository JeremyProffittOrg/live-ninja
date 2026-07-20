package ninja.jeremy.liveninja.log

import java.util.zip.GZIPInputStream
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/**
 * Exercises [LogSinkCore] directly (no Android [android.content.Context] /
 * Robolectric needed — it's a plain [java.io.File]-based class; [LogSink]
 * just supplies the real `filesDir/logs` directory in production).
 */
class LogSinkTest {

    @get:Rule
    val tempFolder = TemporaryFolder()

    private fun entry(
        level: LogLevel = LogLevel.DEBUG,
        category: LogCategory = LogCategory.GENERAL,
        message: String = "hello",
    ) = LogEntry(
        timestampMs = 1_000L,
        level = level,
        category = category,
        tag = "Tag",
        message = message,
    )

    @Test
    fun `ring buffer caps at capacity and keeps the most recent entries`() {
        val core = LogSinkCore(logDir = tempFolder.newFolder("logs"), ringCapacity = 5)
        repeat(8) { i -> core.log(entry(message = "msg-$i")) }
        val snapshot = core.ringSnapshot
        assertEquals(5, snapshot.size)
        assertEquals(listOf("msg-3", "msg-4", "msg-5", "msg-6", "msg-7"), snapshot.map { it.message })
    }

    @Test
    fun `entries below minLevel are dropped before buffer and disk`() = runBlocking {
        val core = LogSinkCore(logDir = tempFolder.newFolder("logs"))
        core.minLevel = LogLevel.WARN
        core.log(entry(level = LogLevel.DEBUG, message = "should be dropped"))
        core.log(entry(level = LogLevel.ERROR, message = "should survive"))
        core.awaitIdle()

        assertEquals(1, core.ringSnapshot.size)
        assertEquals("should survive", core.ringSnapshot.single().message)
        val fileContents = core.currentFile().readText()
        assertFalse(fileContents.contains("should be dropped"))
        assertTrue(fileContents.contains("should survive"))
    }

    @Test
    fun `entries for a disabled category are dropped`() {
        val core = LogSinkCore(logDir = tempFolder.newFolder("logs"))
        core.categoryEnabled = LogCategory.entries.associateWith { it != LogCategory.NET }
        core.log(entry(category = LogCategory.NET, message = "net entry"))
        core.log(entry(category = LogCategory.WAKE, message = "wake entry"))

        assertEquals(1, core.ringSnapshot.size)
        assertEquals(LogCategory.WAKE, core.ringSnapshot.single().category)
    }

    @Test
    fun `when disabled entirely, nothing is buffered or written`() = runBlocking {
        val core = LogSinkCore(logDir = tempFolder.newFolder("logs"))
        core.enabled = false
        core.log(entry(message = "silenced"))
        core.awaitIdle()

        assertTrue(core.ringSnapshot.isEmpty())
        assertFalse(core.currentFile().exists())
    }

    @Test
    fun `entries are redacted before landing in the ring buffer and on disk`() = runBlocking {
        val core = LogSinkCore(logDir = tempFolder.newFolder("logs"))
        val jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
        core.log(entry(message = "Authorization: Bearer $jwt"))
        core.awaitIdle()

        assertFalse(core.ringSnapshot.single().message.contains(jwt))
        assertFalse(core.currentFile().readText().contains(jwt))
    }

    @Test
    fun `line format is ts pipe level pipe category pipe tag colon message`() = runBlocking {
        val core = LogSinkCore(logDir = tempFolder.newFolder("logs"))
        core.log(entry(level = LogLevel.INFO, category = LogCategory.AUTH, message = "signed in"))
        core.awaitIdle()

        val line = core.currentFile().readText().trim()
        assertEquals("1000|INFO|AUTH|Tag: signed in", line)
    }

    @Test
    fun `rotates to gzip when the current file crosses the size threshold and prunes old rotations`() = runBlocking {
        val core = LogSinkCore(
            logDir = tempFolder.newFolder("logs"),
            rotateAtBytes = 50,
            keepRotations = 2,
        )
        // Each entry line is well over 50 bytes once written, so every log() call
        // after the first should trigger a rotation of what came before it.
        repeat(5) { i -> core.log(entry(message = "padding-to-exceed-threshold-$i")) }
        core.awaitIdle()

        val rotated = core.rotatedFiles()
        assertTrue("expected at least one rotation, got ${rotated.size}", rotated.isNotEmpty())
        assertTrue("keepRotations=2 must be enforced", rotated.size <= 2)

        // Every rotated file must be valid gzip containing our pipe-delimited format.
        rotated.forEach { file ->
            val decompressed = GZIPInputStream(file.inputStream()).use { it.readBytes().decodeToString() }
            assertTrue(decompressed.contains("|GENERAL|Tag: padding-to-exceed-threshold-"))
        }
    }

    @Test
    fun `clear empties the ring buffer without touching disk state`() = runBlocking {
        val core = LogSinkCore(logDir = tempFolder.newFolder("logs"))
        core.log(entry(message = "one"))
        core.awaitIdle()
        assertEquals(1, core.ringSnapshot.size)

        core.clear()
        assertTrue(core.ringSnapshot.isEmpty())
        // Disk content from before clear() is untouched (export still works after a clear).
        assertTrue(core.currentFile().readText().contains("one"))
    }
}
