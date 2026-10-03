package ninja.jeremy.liveninja.ui.jobs

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** Cursor state belongs to this exact authenticated account and job, never a global feed. */
data class JobHistoryScope(val tenantId: String, val jobId: String)
data class JobHistoryEntry(val id: String, val sequence: Long, val role: String, val text: String, val createdAt: String, val kind: String = "", val status: String? = null)
enum class HistoryDirection { OLDER, LATEST }
enum class HistoryRetention { RETAINED, PARTIAL, UNAVAILABLE, UNKNOWN }
data class JobHistoryPage(
    val scope: JobHistoryScope,
    val entries: List<JobHistoryEntry> = emptyList(),
    val olderCursor: String? = null,
    val latestCursor: String? = null,
    val retention: HistoryRetention = HistoryRetention.UNKNOWN,
    val retentionMessage: String? = null,
    val hasMoreNewer: Boolean = false,
)

/** Adapter contract only. An implementation must use the verified upstream API and auth. */
fun interface JobHistorySource {
    suspend fun read(scope: JobHistoryScope, cursor: String?, direction: HistoryDirection): JobHistoryPage
}

data class JobHistoryAnchor(val entryId: String, val offset: Int)
data class JobHistoryState(
    val scope: JobHistoryScope? = null,
    val entries: List<JobHistoryEntry> = emptyList(),
    val olderCursor: String? = null,
    val latestCursor: String? = null,
    val retention: HistoryRetention = HistoryRetention.UNKNOWN,
    val retentionMessage: String? = null,
    val loading: Boolean = false,
    val failed: Boolean = false,
    val anchor: JobHistoryAnchor? = null,
    val hasMoreNewer: Boolean = false,
)

/**
 * Explicit pagination: reading older history never loses loaded text or moves the reader.
 * Source switches fence every pending response. No guessed Ghost endpoint or sample data.
 */
class JobHistoryPager(private val source: JobHistorySource, private val coroutineScope: CoroutineScope) {
    private val mutableState = MutableStateFlow(JobHistoryState())
    val state: StateFlow<JobHistoryState> = mutableState
    private var generation = 0L

    fun open(scope: JobHistoryScope) {
        require(scope.tenantId.isNotBlank() && scope.jobId.isNotBlank())
        generation++
        mutableState.value = JobHistoryState(scope = scope)
        load(HistoryDirection.LATEST, initial = true)
    }

    fun close() { generation++; mutableState.value = JobHistoryState() }
    fun rememberPosition(entryId: String, offset: Int) {
        if (state.value.entries.any { it.id == entryId }) mutableState.update { it.copy(anchor = JobHistoryAnchor(entryId, offset.coerceAtLeast(0))) }
    }
    fun older() = load(HistoryDirection.OLDER)
    fun latest() = load(HistoryDirection.LATEST)

    private fun load(direction: HistoryDirection, initial: Boolean = false) {
        val before = state.value
        val scope = before.scope ?: return
        if (before.loading) return
        var cursor = if (direction == HistoryDirection.OLDER) before.olderCursor else before.latestCursor
        if (!initial && direction == HistoryDirection.OLDER && cursor == null) return
        val sequence = generation
        mutableState.update { it.copy(loading = true, failed = false) }
        coroutineScope.launch {
            try {
                var pages = 0
                while (true) {
                    val page = source.read(scope, cursor, direction)
                    check(page.scope == scope) { "History does not belong to this account and job." }
                    check(page.entries.all { it.id.isNotBlank() }) { "History entry identity is missing." }
                    if (sequence != generation) return@launch
                    if (page.hasMoreNewer) check(page.latestCursor != null && page.latestCursor != cursor) { "History cursor did not advance." }
                    mutableState.update { current ->
                        val byId = current.entries.associateBy { it.id }.toMutableMap()
                        page.entries.forEach { entry ->
                            check(byId[entry.id] == null || byId[entry.id] == entry) { "An immutable history entry changed." }
                            byId[entry.id] = entry
                        }
                        current.copy(
                            entries = byId.values.sortedWith(compareBy(JobHistoryEntry::sequence, JobHistoryEntry::id)),
                            olderCursor = if ((initial && pages == 0) || direction == HistoryDirection.OLDER) page.olderCursor else current.olderCursor,
                            latestCursor = if (initial || direction == HistoryDirection.LATEST) page.latestCursor else current.latestCursor,
                            retention = page.retention, retentionMessage = page.retentionMessage, failed = false,
                            hasMoreNewer = if (direction == HistoryDirection.LATEST) page.hasMoreNewer else current.hasMoreNewer,
                        )
                    }
                    pages++
                    if (direction != HistoryDirection.LATEST || !page.hasMoreNewer || pages >= 20) break
                    cursor = page.latestCursor
                }
            } catch (error: Exception) {
                if (error is CancellationException) throw error
                if (sequence == generation) mutableState.update { it.copy(failed = true) }
            } finally {
                if (sequence == generation) mutableState.update { it.copy(loading = false) }
            }
        }
    }
}

/** Full output is lazily rendered in pieces; no text is elided or silently discarded. */
fun historyTextChunks(text: String, chunkSize: Int = 4000): List<String> {
    require(chunkSize >= 2)
    val chunks = mutableListOf<String>()
    var start = 0
    while (start < text.length) {
        var end = (start + chunkSize).coerceAtMost(text.length)
        if (end < text.length && text[end - 1].isHighSurrogate() && text[end].isLowSurrogate()) end--
        chunks += text.substring(start, end)
        start = end
    }
    return chunks
}
