package ninja.jeremy.liveninja.log

import android.content.Context
import android.content.SharedPreferences
import io.mockk.Runs
import io.mockk.every
import io.mockk.just
import io.mockk.mockk
import java.io.File
import java.io.FileOutputStream
import java.util.zip.GZIPOutputStream
import java.util.zip.ZipFile
import java.util.zip.ZipInputStream
import kotlinx.coroutines.runBlocking
import ninja.jeremy.liveninja.ui.state.SettingsStore
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/**
 * Exercises [LogExporter.buildZip] directly — the pure, Android-Intent-free
 * zip construction (04-logging §A7). [LogExporter.exportZip]'s
 * FileProvider/Intent wrapping needs a real Android runtime and isn't
 * covered here (no Robolectric in this project); `buildZip` is where all
 * the actual export logic lives.
 */
class LogExporterTest {

    @get:Rule
    val tempFolder = TemporaryFolder()

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

    private fun fixture(): Pair<LogExporter, File> {
        val filesDir = tempFolder.newFolder("files")
        val context = mockk<Context>()
        every { context.filesDir } returns filesDir
        val logSink = LogSink(context, fakeSettingsStore())
        return LogExporter(context, logSink) to filesDir
    }

    @Test
    fun `returns null when there is nothing to export`() = runBlocking {
        val (exporter, _) = fixture()
        assertNull(exporter.buildZip())
    }

    @Test
    fun `returns null when the current log file exists but is empty`() = runBlocking {
        val (exporter, filesDir) = fixture()
        File(filesDir, "logs").apply { mkdirs() }
        File(filesDir, "logs/liveninja-current.log").createNewFile()

        assertNull(exporter.buildZip())
    }

    @Test
    fun `zips the current log file and reports its content unchanged`() = runBlocking {
        val (exporter, filesDir) = fixture()
        val logDir = File(filesDir, "logs").apply { mkdirs() }
        File(logDir, "liveninja-current.log").writeText("1000|INFO|GENERAL|Tag: hello world\n")

        val zip = exporter.buildZip()
        requireNotNull(zip)
        assertTrue(zip.exists())
        assertTrue("zip lands under filesDir/logs/exports/", zip.parentFile?.path?.endsWith("logs${File.separator}exports") == true)

        ZipFile(zip).use { zf ->
            val entry = zf.getEntry("liveninja-current.log")
            requireNotNull(entry)
            val content = zf.getInputStream(entry).bufferedReader().readText()
            assertEquals("1000|INFO|GENERAL|Tag: hello world\n", content)
        }
    }

    @Test
    fun `zips rotated gz files alongside the current file`() = runBlocking {
        val (exporter, filesDir) = fixture()
        val logDir = File(filesDir, "logs").apply { mkdirs() }
        File(logDir, "liveninja-current.log").writeText("2000|WARN|NET|Tag: current entry\n")
        val rotatedBytes = "1000|INFO|GENERAL|Tag: old entry\n".toByteArray()
        GZIPOutputStream(FileOutputStream(File(logDir, "liveninja-999.log.gz"))).use { it.write(rotatedBytes) }

        val zip = exporter.buildZip()
        requireNotNull(zip)

        val entryNames = ZipInputStream(zip.inputStream()).use { zis ->
            generateSequence { zis.nextEntry }.map { it.name }.toList()
        }
        assertTrue(entryNames.contains("liveninja-current.log"))
        assertTrue(entryNames.contains("liveninja-999.log.gz"))
        assertEquals(2, entryNames.size)
    }

    @Test
    fun `zips rotated files even when the current file has nothing yet`() = runBlocking {
        val (exporter, filesDir) = fixture()
        val logDir = File(filesDir, "logs").apply { mkdirs() }
        GZIPOutputStream(FileOutputStream(File(logDir, "liveninja-111.log.gz"))).use {
            it.write("old".toByteArray())
        }

        val zip = exporter.buildZip()
        requireNotNull(zip)
        ZipFile(zip).use { zf ->
            assertTrue(zf.getEntry("liveninja-111.log.gz") != null)
        }
    }
}
