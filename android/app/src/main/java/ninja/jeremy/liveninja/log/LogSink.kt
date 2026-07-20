package ninja.jeremy.liveninja.log

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import java.io.FileOutputStream
import java.util.zip.GZIPOutputStream
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext

/** Priority ordering mirrors [android.util.Log]'s int constants (VERBOSE=2 .. ASSERT=7). */
enum class LogLevel(val priority: Int) {
    VERBOSE(2),
    DEBUG(3),
    INFO(4),
    WARN(5),
    ERROR(6),
    ASSERT(7),
}

data class LogEntry(
    val timestampMs: Long,
    val level: LogLevel,
    val category: LogCategory,
    val tag: String,
    val message: String,
    val throwable: Throwable? = null,
)

/**
 * Android-context-free core: ring buffer + severity/category gating +
 * rotating gzip file writer. Split out from [LogSink] purely for unit
 * testability without Robolectric — tests construct this directly against a
 * [java.io.File] temp dir; [LogSink] (the Hilt singleton) just supplies the
 * real `filesDir/logs` directory.
 *
 * Redaction ([Redactor]) runs on every entry BEFORE it touches the ring or
 * disk (04-logging-delivery §A6).
 *
 * Writes go through a single-parallelism IO dispatcher so hot paths
 * (wake/audio) never block on disk — [log] returns after the (synchronous,
 * cheap) ring-buffer append; the file write is fire-and-forget on
 * [ioDispatcher]. [awaitIdle] gives callers (tests, [LogExporter]) a
 * deterministic flush point: since [ioDispatcher] has parallelism 1, any
 * work submitted to it before the `awaitIdle` call runs-to-completion before
 * the `withContext` block inside `awaitIdle` gets scheduled.
 */
class LogSinkCore(
    private val logDir: File,
    private val ringCapacity: Int = RING_CAPACITY,
    private val rotateAtBytes: Long = ROTATE_AT_BYTES,
    private val keepRotations: Int = KEEP_ROTATIONS,
    ioDispatcher: CoroutineDispatcher = Dispatchers.IO.limitedParallelism(1),
) {
    @Volatile var enabled: Boolean = true
    @Volatile var minLevel: LogLevel = LogLevel.VERBOSE
    @Volatile var categoryEnabled: Map<LogCategory, Boolean> = LogCategory.entries.associateWith { true }

    private val ring = ArrayDeque<LogEntry>()
    private val ringLock = Any()
    private val fileMutex = Mutex()
    private val ioDispatcher = ioDispatcher
    private val writerScope = CoroutineScope(SupervisorJob() + ioDispatcher)

    val ringSnapshot: List<LogEntry> get() = synchronized(ringLock) { ring.toList() }

    fun log(entry: LogEntry) {
        if (!enabled) return
        if (entry.level.priority < minLevel.priority) return
        if (categoryEnabled[entry.category] == false) return
        val redacted = entry.copy(message = Redactor.redact(entry.message))
        synchronized(ringLock) {
            ring.addLast(redacted)
            while (ring.size > ringCapacity) ring.removeFirst()
        }
        writerScope.launch { writeToFile(redacted) }
    }

    /** Suspends until every write queued before this call has landed on disk. */
    suspend fun awaitIdle() = withContext(ioDispatcher) { }

    fun clear() = synchronized(ringLock) { ring.clear() }

    fun currentFile(): File = File(logDir, CURRENT_FILE_NAME)

    fun rotatedFiles(): List<File> =
        logDir.listFiles { f -> f.name.endsWith(ROTATED_SUFFIX) }
            ?.sortedByDescending { it.lastModified() }
            ?: emptyList()

    private suspend fun writeToFile(entry: LogEntry) = fileMutex.withLock {
        if (!logDir.exists()) logDir.mkdirs()
        val current = currentFile()
        current.appendText(formatLine(entry) + "\n")
        if (current.length() >= rotateAtBytes) rotate(current)
    }

    private fun rotate(current: File) {
        val rotated = File(logDir, "liveninja-${System.currentTimeMillis()}$ROTATED_SUFFIX")
        GZIPOutputStream(FileOutputStream(rotated)).use { gz ->
            current.inputStream().use { it.copyTo(gz) }
        }
        current.writeText("")
        pruneOldRotations()
    }

    private fun pruneOldRotations() {
        rotatedFiles().drop(keepRotations).forEach { it.delete() }
    }

    companion object {
        const val CURRENT_FILE_NAME = "liveninja-current.log"
        const val ROTATED_SUFFIX = ".log.gz"
        const val RING_CAPACITY = 2000
        const val ROTATE_AT_BYTES = 5L * 1024 * 1024
        const val KEEP_ROTATIONS = 10

        /** `ts|level|category|tag: message [stack]` (04-logging-delivery §A2). */
        fun formatLine(entry: LogEntry): String {
            val stack = entry.throwable?.let { " [${it.stackTraceToString().trim()}]" } ?: ""
            return "${entry.timestampMs}|${entry.level.name}|${entry.category.name}|${entry.tag}: ${entry.message}$stack"
        }
    }
}

/**
 * Hilt-facing singleton. Self-registers into [LNLog.sink] on construction —
 * once Hilt provides the first instance (eager instantiation lands in
 * M6.4 via `LiveNinjaApplication`; until then, first real injection site
 * triggers construction on demand), buffered/file logging comes online.
 * Before that, [LNLog] runs logcat-passthrough only.
 *
 * M3.2 wires [enabled]/[minLevel]/[categoryEnabled] to the `SettingsStore`
 * diagnostics flow (collector added once `DiagnosticsConfig` lands from
 * M1.2); the setters below are the seam that collector writes through.
 */
@Singleton
class LogSink @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val core = LogSinkCore(logDir = File(context.filesDir, "logs"))

    init {
        LNLog.sink = this
    }

    var enabled: Boolean
        get() = core.enabled
        set(value) { core.enabled = value }

    var minLevel: LogLevel
        get() = core.minLevel
        set(value) { core.minLevel = value }

    var categoryEnabled: Map<LogCategory, Boolean>
        get() = core.categoryEnabled
        set(value) { core.categoryEnabled = value }

    /** Live snapshot of the ring buffer, newest-last. Read by [LogViewerScreen]. */
    val entries: List<LogEntry> get() = core.ringSnapshot

    fun log(level: LogLevel, category: LogCategory, tag: String, message: String, throwable: Throwable? = null) {
        core.log(LogEntry(System.currentTimeMillis(), level, category, tag, message, throwable))
    }

    fun clear() = core.clear()

    fun currentLogFile(): File = core.currentFile()

    fun rotatedLogFiles(): List<File> = core.rotatedFiles()

    suspend fun awaitIdle() = core.awaitIdle()
}
