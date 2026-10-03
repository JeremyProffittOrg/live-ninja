package ninja.jeremy.liveninja.ui.jobs

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.net.*
import retrofit2.HttpException

data class GhostExplorerState(
    val nodes: List<GhostNodeDto> = emptyList(), val jobs: List<GhostScheduledEventDto> = emptyList(),
    val nodeId: String? = null, val sessions: List<GhostSessionDto> = emptyList(),
    val sessionsCursor: String? = null, val archive: GhostArchiveState = GhostArchiveState(),
    val loading: Boolean = false, val sessionsLoading: Boolean = false, val loaded: Boolean = false,
    val errorCode: Int? = null, val denied: Boolean = false, val runHistoryLimit: Int? = null,
)
sealed interface GhostExplorerEvent {
    data object Refresh : GhostExplorerEvent
    data object MoreSessions : GhostExplorerEvent
    data object MoreEvents : GhostExplorerEvent
    data object Rescan : GhostExplorerEvent
    data object Back : GhostExplorerEvent
    data class Node(val id: String) : GhostExplorerEvent
    data class Session(val id: String) : GhostExplorerEvent
}

@HiltViewModel
class GhostExplorerViewModel @Inject constructor(private val repository: GhostWorkRepository, private val auth: AuthRepository) : ViewModel() {
    private val mutableState = MutableStateFlow(GhostExplorerState())
    val state: StateFlow<GhostExplorerState> = mutableState
    private var account = (auth.state.value as? AuthState.SignedIn)?.sessionId
    private var generation = 0L
    private val archive = GhostHistoryPager(GhostHistorySource(repository::events), viewModelScope)

    init {
        viewModelScope.launch { archive.state.collect { page ->
            if (page.denied) { generation++; mutableState.value = GhostExplorerState(denied = true, errorCode = page.errorCode, archive = page) }
            else mutableState.update { it.copy(archive = page) }
        } }
        viewModelScope.launch { auth.state.collect { authState ->
            val next = (authState as? AuthState.SignedIn)?.sessionId
            if (next != account) { generation++; account = next; archive.close(); mutableState.value = GhostExplorerState() }
        } }
    }
    fun loadIfNeeded() { if (!state.value.loaded && !state.value.loading && !state.value.denied) inventory() }
    fun onForeground() { if (state.value.archive.scope != null) discovery(reset = true, rescanArchive = true) else loadIfNeeded() }
    fun poll() { if (!state.value.denied) archive.poll() }
    fun handle(event: GhostExplorerEvent) { when (event) {
        GhostExplorerEvent.Refresh -> if (state.value.nodeId == null) inventory() else discovery(reset = true, rescanArchive = true)
        GhostExplorerEvent.MoreSessions -> discovery(reset = false)
        GhostExplorerEvent.MoreEvents -> archive.more()
        GhostExplorerEvent.Rescan -> discovery(reset = true, rescanArchive = true)
        GhostExplorerEvent.Back -> {
            if (state.value.archive.scope != null) archive.close()
            else { generation++; archive.close(); mutableState.update { it.copy(nodeId = null, sessions = emptyList(), sessionsCursor = null, sessionsLoading = false, errorCode = null) } }
        }
        is GhostExplorerEvent.Node -> {
            if (!state.value.denied && state.value.nodes.any { it.node_id == event.id }) {
                generation++; archive.close(); mutableState.update { it.copy(nodeId = event.id, sessions = emptyList(), sessionsCursor = null, sessionsLoading = false, errorCode = null) }; discovery(reset = true)
            }
        }
        is GhostExplorerEvent.Session -> {
            val node = state.value.nodeId
            val sid = currentAccount()
            if (!state.value.denied && node != null && sid != null && state.value.sessions.any { it.session_id == event.id }) archive.open(GhostHistoryScope(sid, node, event.id))
        }
    } }

    // The visible selection belongs to the account that populated this state, not
    // whichever token may have appeared before the auth observer gets to run.
    private fun currentAccount() = account?.takeIf { it == (auth.state.value as? AuthState.SignedIn)?.sessionId }
    private fun current(epoch: Long, sid: String) = epoch == generation && currentAccount() == sid
    private fun inventory() {
        val sid = currentAccount() ?: return
        if (state.value.loading || state.value.denied) return
        val epoch = ++generation
        mutableState.update { it.copy(loading = true, errorCode = null) }
        viewModelScope.launch {
            try {
                val nodes = repository.nodes(sid)
                val jobs = repository.jobs(sid)
                if (!current(epoch, sid)) return@launch
                mutableState.update { it.copy(nodes = nodes.nodes.orEmpty().distinctBy(GhostNodeDto::node_id), jobs = jobs.events.orEmpty().distinctBy(GhostScheduledEventDto::event_id), runHistoryLimit = jobs.runHistoryLimit, loaded = true) }
            } catch (error: Exception) { fail(error, epoch, sid) }
            finally { if (current(epoch, sid)) mutableState.update { it.copy(loading = false) } }
        }
    }

    private fun discovery(reset: Boolean, rescanArchive: Boolean = false) {
        val sid = currentAccount() ?: return
        val before = state.value
        val node = before.nodeId ?: return
        if (before.sessionsLoading || before.denied) return
        if (!reset && before.sessionsCursor == null) return
        val epoch = generation
        mutableState.update { it.copy(sessionsLoading = true, errorCode = null) }
        viewModelScope.launch {
            var cursor = if (reset) null else before.sessionsCursor
            val visited = mutableSetOf<String?>()
            try {
                var collected = if (reset) emptyList() else before.sessions
                repeat(20) {
                    check(visited.add(cursor))
                    val page = repository.sessions(sid, node, cursor)
                    if (!current(epoch, sid) || state.value.nodeId != node) return@launch
                    collected = (collected + page.sessions.orEmpty()).distinctBy(GhostSessionDto::session_id)
                    val next = page.next_cursor?.takeIf(String::isNotBlank)
                    check(next == null || next != cursor)
                    mutableState.update { it.copy(sessions = collected, sessionsCursor = next) }
                    cursor = next
                    if (next == null) { if (rescanArchive) archive.rescan(); return@launch }
                }
                if (rescanArchive) archive.rescan()
            } catch (error: Exception) { fail(error, epoch, sid) }
            finally { if (current(epoch, sid)) mutableState.update { it.copy(sessionsLoading = false) } }
        }
    }
    private fun fail(error: Exception, epoch: Long, sid: String) {
        if (error is CancellationException) throw error
        if (!current(epoch, sid)) return
        val code = (error as? HttpException)?.code()
        if (code in listOf(401, 403) || error is JobsSessionException) {
            generation++; archive.close(); mutableState.value = GhostExplorerState(denied = true, errorCode = code ?: 401)
        } else mutableState.update { it.copy(errorCode = code ?: 0) }
    }
}
