package ninja.jeremy.liveninja.ui.jobs

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import ninja.jeremy.liveninja.R
import ninja.jeremy.liveninja.net.JobDto
import ninja.jeremy.liveninja.net.JobInputDto
import ninja.jeremy.liveninja.net.JobRunDto
import ninja.jeremy.liveninja.net.JobsCapabilitiesDto
import retrofit2.HttpException
import java.util.UUID
import javax.inject.Inject
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.net.JobsSessionException

data class JobConfirmation(val job: JobDto, val action: String, val run: JobRunDto? = null, val note: String? = null)

sealed interface JobsEvent {
    data object OpenGhost : JobsEvent
    data object Refresh : JobsEvent
    data object New : JobsEvent
    data object Edit : JobsEvent
    data object CloseDetail : JobsEvent
    data object More : JobsEvent
    data object OlderRuns : JobsEvent
    data object Save : JobsEvent
    data object CloseEditor : JobsEvent
    data object ReloadDraft : JobsEvent
    data object Confirm : JobsEvent
    data object DismissConfirmation : JobsEvent
    data object DismissNotice : JobsEvent
    data object OlderHistory : JobsEvent
    data object LatestHistory : JobsEvent
    data object ReviewNote : JobsEvent
    data object ConfirmVoice : JobsEvent
    data object DismissVoice : JobsEvent
    data class Note(val text: String) : JobsEvent
    data class ReviewVoice(val proposal: JobsVoiceProposal) : JobsEvent
    data class DismissProposal(val id: String) : JobsEvent
    data class HistoryPosition(val id: String, val offset: Int) : JobsEvent
    data class Filter(val value: String) : JobsEvent
    data class Open(val job: JobDto) : JobsEvent
    data class Draft(val value: JobDraft) : JobsEvent
    data class Action(val action: String, val run: JobRunDto? = null) : JobsEvent
}

data class JobsUiState(
    val jobs: List<JobDto> = emptyList(),
    val capabilities: JobsCapabilitiesDto? = null,
    val loading: Boolean = false,
    val loaded: Boolean = false,
    val error: Int? = null,
    val nextCursor: String? = null,
    val loadingMore: Boolean = false,
    val pagesExpanded: Boolean = false,
    val filter: String = "all",
    val selected: JobDto? = null,
    val detailLoading: Boolean = false,
    val runs: List<JobRunDto> = emptyList(),
    val runsLoading: Boolean = false,
    val runsLoaded: Boolean = false,
    val runsError: Int? = null,
    val runsCursor: String? = null,
    val runPagesExpanded: Boolean = false,
    val draft: JobDraft? = null,
    val draftError: Int? = null,
    val draftConflict: Boolean = false,
    val confirmation: JobConfirmation? = null,
    val confirmationError: Int? = null,
    val confirmationStale: Boolean = false,
    val busy: Boolean = false,
    val uncertain: Boolean = false,
    val notice: Int? = null,
    val history: JobHistoryState = JobHistoryState(),
    val noteDraft: String = "",
    val proposals: List<JobsVoiceProposal> = emptyList(),
    val voiceReview: JobsVoiceProposal? = null,
    val voiceError: Int? = null,
    val voiceStale: Boolean = false,
)

private data class MutationKey(val path: String, val version: Long = 0, val input: JobInputDto? = null, val note: String? = null)

/** Native Jobs state retains mutation identity and exact review snapshots across rotation. */
@HiltViewModel
class JobsViewModel @Inject constructor(private val repository: JobsRepository, private val proposalInbox: JobsProposalInbox? = null) : ViewModel() {
    private val mutableState = MutableStateFlow(JobsUiState())
    val state: StateFlow<JobsUiState> = mutableState
    private val requestIds = mutableMapOf<MutationKey, String>()
    private var detailSequence = 0
    private var listSequence = 0
    private val timeline = JobHistoryPager(JobHistorySource(repository::historyPage), viewModelScope)
    private var sessionId = proposalInbox?.captureSession()
    private var accountGeneration = 0L

    init {
        viewModelScope.launch { timeline.state.collect { history -> mutableState.update { it.copy(history = history) } } }
        proposalInbox?.let { inbox ->
            viewModelScope.launch { inbox.pending.collect { proposals ->
                mutableState.update { current -> current.copy(proposals = proposals.filter { it.sessionId == inbox.captureSession() }, voiceReview = current.voiceReview?.takeIf(inbox::isCurrent)) }
            } }
            viewModelScope.launch { inbox.authState.collect { auth ->
                val next = (auth as? AuthState.SignedIn)?.sessionId
                if (next != sessionId) {
                    accountGeneration++; sessionId = next; invalidateReads(); timeline.close(); requestIds.clear()
                    mutableState.value = JobsUiState()
                    if (next != null) refresh()
                }
            } }
        }
    }

    fun handle(event: JobsEvent) { when (event) {
        JobsEvent.OpenGhost -> Unit // Navigation belongs to the signed-in screen.
        JobsEvent.Refresh -> refresh()
        JobsEvent.New -> newJob()
        JobsEvent.Edit -> editJob()
        JobsEvent.CloseDetail -> closeDetail()
        JobsEvent.More -> loadMore()
        JobsEvent.OlderRuns -> loadOlderRuns()
        JobsEvent.Save -> saveDraft()
        JobsEvent.CloseEditor -> closeEditor()
        JobsEvent.ReloadDraft -> reloadDraft()
        JobsEvent.Confirm -> confirmAction()
        JobsEvent.DismissConfirmation -> dismissConfirmation()
        JobsEvent.DismissNotice -> dismissNotice()
        JobsEvent.OlderHistory -> timeline.older()
        JobsEvent.LatestHistory -> timeline.latest()
        JobsEvent.ReviewNote -> reviewNote()
        JobsEvent.ConfirmVoice -> confirmVoice()
        JobsEvent.DismissVoice -> if (!state.value.busy) mutableState.update { it.copy(voiceReview = null, voiceError = null, voiceStale = false) }
        is JobsEvent.Note -> if (!state.value.busy) mutableState.update { it.copy(noteDraft = event.text) }
        is JobsEvent.ReviewVoice -> if (!state.value.busy && proposalInbox?.isCurrent(event.proposal) == true) mutableState.update { it.copy(voiceReview = event.proposal, voiceError = null, voiceStale = false) }
        is JobsEvent.DismissProposal -> if (!state.value.busy) proposalInbox?.dismiss(event.id)
        is JobsEvent.HistoryPosition -> timeline.rememberPosition(event.id, event.offset)
        is JobsEvent.Filter -> setFilter(event.value)
        is JobsEvent.Open -> openJob(event.job)
        is JobsEvent.Draft -> updateDraft { event.value }
        is JobsEvent.Action -> requestAction(event.action, event.run)
    } }

    fun loadIfNeeded() { if (!state.value.loaded && !state.value.loading) refresh() }
    fun setFilter(filter: String) { if (filter in listOf("all", "active", "paused", "cancelled")) mutableState.update { it.copy(filter = filter) } }
    fun dismissNotice() = mutableState.update { it.copy(notice = null) }

    fun refresh(quiet: Boolean = false) {
        val old = state.value
        if (old.loading || old.loadingMore || old.detailLoading || old.runsLoading || old.busy || (quiet && (old.draft != null || old.confirmation != null || old.voiceReview != null || old.uncertain))) return
        val sequence = ++listSequence
        mutableState.update { it.copy(loading = true, error = if (quiet) it.error else null) }
        viewModelScope.launch {
            try {
                val keepPages = quiet && state.value.pagesExpanded
                if (!keepPages) {
                    val response = repository.list()
                    if (sequence != listSequence) return@launch
                    mutableState.update { it.copy(jobs = response.jobs.orEmpty(), capabilities = response.capabilities, nextCursor = response.nextCursor, pagesExpanded = false, loaded = true, error = null) }
                }
                if (sequence == listSequence) state.value.selected?.id?.let { refreshDetail(it, keepRuns = quiet && state.value.runPagesExpanded) }
            } catch (error: Exception) {
                rethrowCancellation(error)
                if (sequence == listSequence) mutableState.update { it.copy(error = errorLabel(error)) }
            } finally { if (sequence == listSequence) mutableState.update { it.copy(loading = false) } }
        }
    }

    fun loadMore() {
        val cursor = state.value.nextCursor ?: return
        if (state.value.loadingMore || state.value.loading || state.value.busy) return
        val sequence = ++listSequence
        mutableState.update { it.copy(loadingMore = true) }
        viewModelScope.launch {
            try {
                val response = repository.list(cursor)
                if (sequence != listSequence) return@launch
                mutableState.update { it.copy(jobs = (it.jobs + response.jobs.orEmpty()).distinctBy { job -> job.id }, nextCursor = response.nextCursor, pagesExpanded = true, error = null) }
            } catch (error: Exception) { rethrowCancellation(error); if (sequence == listSequence) mutableState.update { it.copy(error = errorLabel(error)) } }
            finally { if (sequence == listSequence) mutableState.update { it.copy(loadingMore = false) } }
        }
    }

    fun openJob(job: JobDto) {
        if (state.value.busy) return
        detailSequence++
        mutableState.update { it.copy(selected = job, detailLoading = true, runs = emptyList(), runsLoaded = false, runsCursor = null, runPagesExpanded = false, runsError = null) }
        sessionId?.let { timeline.open(JobHistoryScope(it, job.id)) }
        viewModelScope.launch { refreshDetail(job.id) }
    }

    fun closeDetail() {
        if (state.value.busy) return
        detailSequence++
        timeline.close()
        mutableState.update { it.copy(selected = null, detailLoading = false, runs = emptyList(), runsCursor = null, runsError = null) }
    }

    private suspend fun refreshDetail(id: String, keepRuns: Boolean = false) {
        val sequence = detailSequence
        try {
            val job = repository.get(id).job ?: error("Missing job response")
            if (sequence != detailSequence || state.value.selected?.id != id) return
            applyJob(job)
            if (!keepRuns) loadRunsPage(id, append = false)
        } catch (error: Exception) {
            rethrowCancellation(error)
            if (sequence == detailSequence && state.value.selected?.id == id) mutableState.update { it.copy(runsError = errorLabel(error)) }
        } finally { if (sequence == detailSequence) mutableState.update { it.copy(detailLoading = false) } }
    }

    fun loadOlderRuns() {
        val id = state.value.selected?.id ?: return
        if (state.value.loading || state.value.detailLoading || state.value.runsLoading || state.value.runsCursor == null || state.value.busy) return
        mutableState.update { it.copy(runsLoading = true) }
        viewModelScope.launch { loadRunsPage(id, append = true) }
    }

    private suspend fun loadRunsPage(id: String, append: Boolean) {
        val sequence = detailSequence
        val cursor = if (append) state.value.runsCursor else null
        mutableState.update { it.copy(runsLoading = true, runsError = null) }
        try {
            val response = repository.runs(id, cursor)
            if (sequence != detailSequence || state.value.selected?.id != id) return
            mutableState.update { it.copy(runs = (if (append) it.runs + response.runs.orEmpty() else response.runs.orEmpty()).distinctBy { run -> run.id }, runsCursor = response.nextCursor, runsLoaded = true, runPagesExpanded = append, runsError = null) }
        } catch (error: Exception) {
            rethrowCancellation(error)
            if (sequence == detailSequence) mutableState.update { it.copy(runsError = errorLabel(error)) }
        } finally { if (sequence == detailSequence) mutableState.update { it.copy(runsLoading = false) } }
    }

    fun newJob() {
        val capabilities = state.value.capabilities ?: return
        if (state.value.busy || (capabilities.reminder != true && capabilities.review != true)) return
        mutableState.update { it.copy(draft = JobDraft(kind = if (capabilities.reminder == true) "reminder" else "review"), draftError = null, draftConflict = false) }
    }
    fun editJob() { val job = state.value.selected ?: return; if (!state.value.busy && job.status != "cancelled") mutableState.update { it.copy(draft = JobDraft.from(job), draftError = null, draftConflict = false) } }
    fun updateDraft(change: (JobDraft) -> JobDraft) { if (!state.value.busy) mutableState.update { it.copy(draft = it.draft?.let(change), draftError = null) } }
    fun closeEditor() { if (!state.value.busy) mutableState.update { it.copy(draft = null, draftError = null, draftConflict = false) } }

    fun reloadDraft() {
        val id = state.value.draft?.id ?: return
        if (state.value.busy) return
        val account = accountGeneration to sessionId
        mutableState.update { it.copy(busy = true) }
        viewModelScope.launch {
            try {
                val job = repository.get(id).job ?: error("Missing job response")
                if (!isAccount(account)) return@launch
                if (job.status == "cancelled") { mutableState.update { it.copy(draftError = R.string.jobs_cancelled_edit, draftConflict = false) }; return@launch }
                mutableState.update { it.copy(draft = JobDraft.from(job), draftError = null, draftConflict = false, uncertain = false) }
            } catch (error: Exception) { rethrowCancellation(error); if (isAccount(account)) mutableState.update { it.copy(draftError = errorLabel(error)) } }
            finally { if (isAccount(account)) mutableState.update { it.copy(busy = false) } }
        }
    }

    fun saveDraft() {
        val draft = state.value.draft ?: return
        if (state.value.busy || state.value.draftConflict) return
        val input = try { draft.toInput() } catch (error: JobValidationException) { mutableState.update { it.copy(draftError = error.messageRes) }; return }
        if (state.value.capabilities?.scheduling != true && (input.schedule.kind != "once" || !input.schedule.at.isNullOrBlank())) { mutableState.update { it.copy(draftError = R.string.jobs_scheduler_disabled) }; return }
        val key = MutationKey("save:${draft.id ?: "new"}", draft.version, input)
        val requestId = requestIds.getOrPut(key) { UUID.randomUUID().toString() }
        val account = accountGeneration to sessionId
        invalidateReads()
        mutableState.update { it.copy(busy = true, draftError = null) }
        viewModelScope.launch {
            try {
                val response = if (draft.id == null) repository.create(input, requestId, account.second) else repository.update(draft.id, input, draft.version, requestId, account.second)
                if (!isAccount(account)) return@launch
                val job = response.job ?: error("Missing job response")
                requestIds.remove(key); detailSequence++
                mutableState.update { it.copy(draft = null, draftError = null, draftConflict = false, selected = job, jobs = (listOf(job) + it.jobs).distinctBy { saved -> saved.id }, filter = "all", runs = emptyList(), runsLoaded = false, runsCursor = null, uncertain = false, notice = if (draft.id == null) R.string.jobs_created else R.string.jobs_saved) }
                loadRunsPage(job.id, append = false)
            } catch (error: Exception) {
                rethrowCancellation(error)
                if (!isAccount(account)) return@launch
                val conflict = (error as? HttpException)?.code() == 409
                mutableState.update { it.copy(draftError = errorLabel(error), draftConflict = conflict, uncertain = !conflict) }
            } finally { if (isAccount(account)) mutableState.update { it.copy(busy = false) } }
        }
    }

    fun requestAction(action: String, run: JobRunDto? = null) {
        val job = state.value.selected ?: return
        if (state.value.busy) return
        val request = JobConfirmation(job, action, run)
        if (action in listOf("run", "resume")) perform(request)
        else mutableState.update { it.copy(confirmation = request, confirmationError = null, confirmationStale = false) }
    }

    private fun reviewNote() {
        val current = state.value
        val job = current.selected ?: return
        val text = current.noteDraft.trim()
        if (current.busy) return
        if (text.isBlank() || text.toByteArray(Charsets.UTF_8).size > 2000) { mutableState.update { it.copy(notice = R.string.jobs_invalid_notes) }; return }
        mutableState.update { it.copy(confirmation = JobConfirmation(job, "note", note = text), confirmationError = null, confirmationStale = false) }
    }

    private fun confirmVoice() {
        val proposal = state.value.voiceReview ?: return
        if (state.value.busy || state.value.voiceStale) return
        if (proposalInbox?.isCurrent(proposal) != true) { mutableState.update { it.copy(voiceError = R.string.jobs_proposal_expired, voiceStale = true) }; return }
        val key = MutationKey("voice:${proposal.sessionId}:${proposal.id}")
        val requestId = requestIds.getOrPut(key) { UUID.randomUUID().toString() }
        invalidateReads()
        mutableState.update { it.copy(busy = true, voiceError = null) }
        viewModelScope.launch {
            try {
                val job = repository.approveProposal(proposal, requestId).job ?: error("Missing job response")
                if (proposalInbox.captureSession() != proposal.sessionId) return@launch
                requestIds.remove(key); proposalInbox.dismiss(proposal.id)
                mutableState.update { it.copy(voiceReview = null, voiceError = null, uncertain = false, selected = job, jobs = (listOf(job) + it.jobs).distinctBy(JobDto::id), notice = R.string.jobs_action_recorded) }
                sessionId?.let { timeline.open(JobHistoryScope(it, job.id)) }
                loadRunsPage(job.id, append = false)
            } catch (error: Exception) {
                rethrowCancellation(error)
                if (proposalInbox.captureSession() == proposal.sessionId) mutableState.update { it.copy(voiceError = if ((error as? HttpException)?.code() in listOf(401, 403)) R.string.jobs_bound_auth_error else errorLabel(error), voiceStale = (error as? HttpException)?.code() == 409, uncertain = (error as? HttpException)?.code() != 409) }
            } finally { if (proposalInbox.captureSession() == proposal.sessionId) mutableState.update { it.copy(busy = false) } }
        }
    }
    fun dismissConfirmation() { if (!state.value.busy) mutableState.update { it.copy(confirmation = null, confirmationError = null, confirmationStale = false) } }
    fun confirmAction() { if (!state.value.confirmationStale) state.value.confirmation?.let(::perform) }

    private fun perform(request: JobConfirmation) {
        if (state.value.busy) return
        val key = MutationKey("${request.job.id}:${request.run?.id ?: "job"}:${request.action}", request.job.version, note = request.note)
        val requestId = requestIds.getOrPut(key) { UUID.randomUUID().toString() }
        val account = accountGeneration to sessionId
        invalidateReads()
        mutableState.update { it.copy(busy = true, confirmationError = null, notice = null) }
        viewModelScope.launch {
            try {
                val job = when {
                    request.action == "note" -> repository.note(request.job.id, requireNotNull(request.note), request.job.version, requestId, expectedSessionId = account.second).job
                    request.run != null -> repository.runAction(request.job.id, request.run.id, request.action, request.job.version, requestId, account.second).job
                    request.action == "run" -> repository.runNow(request.job.id, request.job.version, requestId, account.second).job
                    else -> repository.jobAction(request.job.id, request.action, request.job.version, requestId, account.second).job
                } ?: error("Missing job response")
                if (!isAccount(account)) return@launch
                requestIds.remove(key); applyJob(job)
                mutableState.update { it.copy(confirmation = null, confirmationError = null, confirmationStale = false, uncertain = false, notice = R.string.jobs_action_recorded) }
                if (request.action == "note") mutableState.update { it.copy(noteDraft = "") }
                timeline.latest()
                loadRunsPage(job.id, append = false)
            } catch (error: Exception) {
                rethrowCancellation(error)
                if (!isAccount(account)) return@launch
                val conflict = (error as? HttpException)?.code() == 409
                mutableState.update { it.copy(confirmationError = if (it.confirmation != null) errorLabel(error) else null, confirmationStale = conflict, notice = if (it.confirmation == null) errorLabel(error) else null, uncertain = !conflict) }
            } finally { if (isAccount(account)) mutableState.update { it.copy(busy = false) } }
        }
    }

    // A GET begun before a write must never restore its older version/receipt.
    // Invalidate its finalizer too so it cannot alter newer loading indicators.
    private fun invalidateReads() {
        detailSequence++; listSequence++
        mutableState.update { it.copy(loading = false, loadingMore = false, detailLoading = false, runsLoading = false) }
    }
    private fun applyJob(job: JobDto) = mutableState.update { it.copy(selected = if (it.selected?.id == job.id && job.version >= it.selected.version) job else it.selected, jobs = it.jobs.map { row -> if (row.id == job.id && job.version >= row.version) job else row }) }
    private fun rethrowCancellation(error: Exception) { if (error is CancellationException) throw error }
    private fun isAccount(captured: Pair<Long, String?>): Boolean = captured.first == accountGeneration && (proposalInbox == null || captured.second == proposalInbox.captureSession())
    private fun errorLabel(error: Exception): Int = if (error is JobsSessionException) R.string.jobs_bound_auth_error else when ((error as? HttpException)?.code()) {
        401, 403 -> R.string.jobs_auth_error
        404 -> R.string.jobs_missing
        409 -> R.string.jobs_conflict
        429 -> R.string.jobs_limit_error
        400, 413 -> R.string.jobs_invalid_request
        503 -> R.string.jobs_unavailable
        else -> R.string.jobs_network_error
    }
}
