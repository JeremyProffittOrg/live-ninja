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
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import ninja.jeremy.liveninja.ui.state.DiagnosticsConfig
import ninja.jeremy.liveninja.ui.state.SettingsStore

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
    private val _entriesFlow = MutableStateFlow<List<LogEntry>>(emptyList())

    val ringSnapshot: List<LogEntry> get() = synchronized(ringLock) { ring.toList() }

    /** Live ring-buffer snapshot, newest-last — for [LogViewerScreen] to `collectAsState()`. */
    val entriesFlow: StateFlow<List<LogEntry>> = _entriesFlow.asStateFlow()

    fun log(entry: LogEntry) {
        if (!enabled) return
        if (entry.level.priority < minLevel.priority) return
        if (categoryEnabled[entry.category] == false) return
        val redacted = entry.copy(message = Redactor.redact(entry.message))
        val snapshot = synchronized(ringLock) {
            ring.addLast(redacted)
            while (ring.size > ringCapacity) ring.removeFirst()
            ring.toList()
        }
        _entriesFlow.value = snapshot
        writerScope.launch { writeToFile(redacted) }
    }

    /** Suspends until every write queued before this call has landed on disk. */
    suspend fun awaitIdle() = withContext(ioDispatcher) { }

    fun clear() {
        synchronized(ringLock) { ring.clear() }
        _entriesFlow.value = emptyList()
    }

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
 * M3.2: observes `SettingsStore.document` for its nested `diagnostics`
 * config (owner defaults: enabled=true, minLevel=VERBOSE, all 8 categories
 * on — troubleshooting phase, 04-logging §A4) and applies it live to
 * [enabled]/[minLevel]/[categoryEnabled] on every emission, so a Settings
 * change (M6.2 Diagnostics section) takes effect on the next log call with
 * no restart. [enabled]/[minLevel]/[categoryEnabled] remain directly
 * settable too (used by tests, and available as an escape hatch) — the
 * collector below simply re-asserts them whenever the document changes.
 */
@Singleton
class LogSink @Inject constructor(
    @ApplicationContext context: Context,
    settingsStore: SettingsStore,
) {
    private val core = LogSinkCore(logDir = File(context.filesDir, "logs"))
    private val configScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    init {
        LNLog.sink = this
        configScope.launch {
            settingsStore.document.collect { document -> applyDiagnostics(document.diagnostics) }
        }
    }

    private fun applyDiagnostics(config: DiagnosticsConfig) {
        core.enabled = config.enabled
        core.minLevel = LogLevel.entries.firstOrNull { it.name == config.minLevel } ?: LogLevel.VERBOSE
        core.categoryEnabled = LogCategory.entries.associateWith { category ->
            config.categories[category.name] ?: true
        }
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

    /** Live ring-buffer flow, newest-last — [LogViewerScreen] collects this for reactive updates. */
    val entriesFlow: StateFlow<List<LogEntry>> get() = core.entriesFlow

    fun log(level: LogLevel, category: LogCategory, tag: String, message: String, throwable: Throwable? = null) {
        core.log(LogEntry(System.currentTimeMillis(), level, category, tag, message, throwable))
    }

    fun clear() = core.clear()

    fun currentLogFile(): File = core.currentFile()

    fun rotatedLogFiles(): List<File> = core.rotatedFiles()

    suspend fun awaitIdle() = core.awaitIdle()
}
