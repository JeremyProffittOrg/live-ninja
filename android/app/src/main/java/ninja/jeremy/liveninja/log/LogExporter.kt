package ninja.jeremy.liveninja.log

import android.content.Context
import android.content.Intent
import androidx.core.content.FileProvider
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import java.io.FileOutputStream
import java.util.zip.ZipEntry
import java.util.zip.ZipOutputStream
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/**
 * Export path for the Diagnostics "Export logs" action (04-logging §A7):
 * flush the ring buffer's pending disk writes, zip the current + all
 * rotated log files, and hand back an `ACTION_SEND` share-sheet [Intent]
 * pointing at the zip via [FileProvider].
 *
 * The FileProvider `<provider>` manifest entry (authority
 * [FILE_PROVIDER_AUTHORITY]) is a SEPARATE, deliberately deferred piece —
 * plan M6.1 owns `AndroidManifest.xml` and adds it there once M2's wave-1
 * manifest edits land, per the file-ownership matrix. This class and
 * `res/xml/file_paths.xml` (added alongside it) are fully real and correct
 * today; [exportZip] simply won't resolve to a live content provider until
 * M6.1's manifest entry exists. [buildZip] — the actual zip construction —
 * has no such dependency and is exercised directly by
 * `log/LogExporterTest.kt`.
 */
@Singleton
class LogExporter @Inject constructor(
    @ApplicationContext private val context: Context,
    private val logSink: LogSink,
) {

    /**
     * Builds the export zip and wraps it in a share Intent. Returns null
     * when there is nothing to export (no current-file content and no
     * rotated files yet) — callers should no-op / toast rather than launch
     * an empty share sheet.
     */
    suspend fun exportZip(): Intent? {
        logSink.awaitIdle()
        val zipFile = buildZip() ?: return null
        val uri = FileProvider.getUriForFile(context, FILE_PROVIDER_AUTHORITY, zipFile)
        return Intent(Intent.ACTION_SEND).apply {
            type = "application/zip"
            putExtra(Intent.EXTRA_STREAM, uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
    }

    /**
     * Pure zip-building logic — no FileProvider/Intent/Uri involved, so
     * it's directly unit-testable on the JVM. Zips the live current log
     * file (if non-empty) plus every rotated `.log.gz`, into
     * `filesDir/logs/exports/liveninja-logs-<ts>.zip`.
     */
    internal suspend fun buildZip(): File? = withContext(Dispatchers.IO) {
        val sources = buildList {
            val current = logSink.currentLogFile()
            if (current.exists() && current.length() > 0) add(current)
            addAll(logSink.rotatedLogFiles())
        }
        if (sources.isEmpty()) return@withContext null

        val exportDir = File(context.filesDir, "logs/$EXPORT_DIR_NAME").apply { mkdirs() }
        val zipFile = File(exportDir, "liveninja-logs-${System.currentTimeMillis()}.zip")
        ZipOutputStream(FileOutputStream(zipFile)).use { zos ->
            sources.forEach { source ->
                zos.putNextEntry(ZipEntry(source.name))
                source.inputStream().use { it.copyTo(zos) }
                zos.closeEntry()
            }
        }
        zipFile
    }

    companion object {
        const val FILE_PROVIDER_AUTHORITY = "ninja.jeremy.liveninja.fileprovider"
        private const val EXPORT_DIR_NAME = "exports"
    }
}
