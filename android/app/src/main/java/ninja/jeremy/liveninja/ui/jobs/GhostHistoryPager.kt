package ninja.jeremy.liveninja.ui.jobs

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.net.GhostEventsResponse
import ninja.jeremy.liveninja.net.GhostHistoryEventDto
import ninja.jeremy.liveninja.net.JobsSessionException
import retrofit2.HttpException

data class GhostHistoryScope(val accountSessionId: String, val nodeId: String, val providerSessionId: String)
fun interface GhostHistorySource { suspend fun read(scope: GhostHistoryScope, cursor: String?): GhostEventsResponse }
data class GhostArchiveState(
    val scope: GhostHistoryScope? = null, val events: List<GhostHistoryEventDto> = emptyList(),
    val nextCursor: String? = null, val resumeCursor: String? = null, val gaps: List<String> = emptyList(),
    val loading: Boolean = false, val loaded: Boolean = false, val errorCode: Int? = null,
    val denied: Boolean = false, val rescanRequired: Boolean = false,
)

/** All retained fragments stay loaded. Source order is lexical, never timestamp or numeric order. */
class GhostHistoryPager(private val source: GhostHistorySource, private val scope: CoroutineScope) {
    private val mutableState = MutableStateFlow(GhostArchiveState())
    val state: StateFlow<GhostArchiveState> = mutableState
    private var generation = 0L
    private var scanEvents: MutableMap<String, GhostHistoryEventDto>? = null
    fun open(selection: GhostHistoryScope) { generation++; scanEvents = mutableMapOf(); mutableState.value = GhostArchiveState(scope = selection); read(null) }
    fun close() { generation++; scanEvents = null; mutableState.value = GhostArchiveState() }
    fun more() { state.value.nextCursor?.let(::read) }
    fun poll() {
        val current = state.value
        if (!current.loaded || current.nextCursor != null || current.rescanRequired || current.denied || current.errorCode != null) return
        current.resumeCursor?.let(::read)
    }
    fun rescan() {
        if (state.value.loading || state.value.denied) return
        generation++
        scanEvents = mutableMapOf()
        mutableState.update { it.copy(nextCursor = null, resumeCursor = null, gaps = emptyList(), rescanRequired = false, errorCode = null) }
        read(null)
    }

    private fun read(startCursor: String?) {
        val selection = state.value.scope ?: return
        if (state.value.loading || state.value.denied || state.value.rescanRequired) return
        val epoch = generation
        mutableState.update { it.copy(loading = true, errorCode = null) }
        scope.launch {
            var cursor = startCursor
            val visited = mutableSetOf<String?>()
            try {
                repeat(20) {
                    check(visited.add(cursor)) { "Repeated archive cursor." }
                    val page = source.read(selection, cursor)
                    if (epoch != generation) return@launch
                    check(page.version == 1 && page.node_id == selection.nodeId && page.session_id == selection.providerSessionId && page.coverage == "retained_only" && page.events != null) { "Archive scope changed." }
                    val next = page.next_cursor?.takeIf(String::isNotBlank)
                    check(next == null || next != cursor) { "Archive cursor did not advance." }
                    mutableState.update { current ->
                        val events = current.events.associateBy { it.id }.toMutableMap()
                        page.events.forEach { event ->
                            check(event.id.isNotBlank() && event.sequence.isNotBlank())
                            val previous = scanEvents?.get(event.id) ?: if (scanEvents == null) events[event.id] else null
                            if (previous != null && previous != event) throw ArchiveChangedException()
                            scanEvents?.set(event.id, event)
                            events[event.id] = event
                        }
                        // A completed fresh scan replaces the old inventory, including expired IDs.
                        val retained = if (next == null && scanEvents != null) scanEvents!!.values.toList() else events.values.toList()
                        current.copy(events = retained.sortedWith(compareBy(GhostHistoryEventDto::sequence, GhostHistoryEventDto::id)), nextCursor = next, resumeCursor = page.resume_cursor?.takeIf(String::isNotBlank) ?: current.resumeCursor, gaps = (current.gaps + page.gaps.orEmpty().map { it.toString() }).distinct(), loaded = true)
                    }
                    cursor = next
                    if (next == null) { scanEvents = null; return@launch }
                }
            } catch (error: Exception) {
                if (error is CancellationException) throw error
                if (epoch != generation) return@launch
                val status = (error as? HttpException)?.code()
                when {
                    status in listOf(401, 403) || error is JobsSessionException -> mutableState.value = GhostArchiveState(denied = true, errorCode = status ?: 401)
                    status == 409 || status == 410 || error is ArchiveChangedException -> mutableState.update { it.copy(errorCode = status ?: 409, rescanRequired = true) }
                    else -> mutableState.update { it.copy(errorCode = status ?: 0) }
                }
            } finally { if (epoch == generation) mutableState.update { it.copy(loading = false) } }
        }
    }
    private class ArchiveChangedException : IllegalStateException()
}
