package ninja.jeremy.liveninja.ui.jobs

import java.io.IOException
import java.util.UUID
import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.net.JobActionRequest
import ninja.jeremy.liveninja.net.JobInputDto
import ninja.jeremy.liveninja.net.JobResponse
import ninja.jeremy.liveninja.net.JobRunResponse
import ninja.jeremy.liveninja.net.JobRunsResponse
import ninja.jeremy.liveninja.net.JobSaveRequest
import ninja.jeremy.liveninja.net.JobsListResponse
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.JobHistoryResponse
import ninja.jeremy.liveninja.net.JobCommandRequest
import ninja.jeremy.liveninja.net.JobCommandResponse
import ninja.jeremy.liveninja.net.AuthBoundRequest
import ninja.jeremy.liveninja.auth.TokenStore
import ninja.jeremy.liveninja.net.BoundJobsSession
import ninja.jeremy.liveninja.net.JobsSessionException

/**
 * Uses the app's existing authenticated Retrofit stack. Mutations are performed
 * exactly once here: HTTP, transport and cancellation failures propagate.
 *
 * The caller creates a request ID when the user confirms an immutable action,
 * retains that same ID/body/version on an explicit retry after a lost response,
 * and discards it after success, a definitive conflict, or an edited intent.
 * A 409 must prompt a refresh and new confirmation; this repository never
 * silently refreshes the version, changes the intent, or repeats an action.
 * Production mutations proactively refresh only the UI's captured session,
 * then pin its bearer and disable authenticator replay for that request.
 */
@Singleton
class JobsRepository @Inject constructor(private val api: LiveNinjaApi, private val tokenStore: TokenStore? = null, private val boundSessions: BoundJobsSession? = null) {
    suspend fun list(cursor: String? = null): JobsListResponse {
        val response = api.listJobs(cursor = cursor?.takeIf { it.isNotBlank() }, limit = PAGE_SIZE)
        return response.copy(jobs = response.jobs.orEmpty(), nextCursor = response.nextCursor?.takeIf { it.isNotBlank() })
    }

    suspend fun get(id: String): JobResponse {
        requireId(id)
        return api.getJob(id).requireJob()
    }

    suspend fun runs(id: String, cursor: String? = null): JobRunsResponse {
        requireId(id)
        val response = api.listJobRuns(id, cursor?.takeIf { it.isNotBlank() }, PAGE_SIZE)
        return response.copy(runs = response.runs.orEmpty(), nextCursor = response.nextCursor?.takeIf { it.isNotBlank() })
    }

    suspend fun create(input: JobInputDto, requestId: String, expectedSessionId: String? = null): JobResponse {
        val body = saveRequest(input, requestId)
        val bound = bind(expectedSessionId)
        return (if (bound == null) api.createJob(body) else api.createJobReviewed(body, bound.first, bound.second)).requireJob()
    }

    suspend fun update(id: String, input: JobInputDto, version: Long, requestId: String, expectedSessionId: String? = null): JobResponse {
        requireId(id)
        requireVersion(version)
        val body = saveRequest(input, requestId, version); val bound = bind(expectedSessionId)
        return (if (bound == null) api.updateJob(id, body) else api.updateJobReviewed(id, body, bound.first, bound.second)).requireJob()
    }

    suspend fun jobAction(id: String, action: String, version: Long, requestId: String, expectedSessionId: String? = null): JobResponse {
        requireId(id)
        require(action in JOB_ACTIONS) { "Unsupported job action." }
        val body = actionRequest(version, requestId); val bound = bind(expectedSessionId)
        return (if (bound == null) api.jobAction(id, action, body) else api.jobActionReviewed(id, action, body, bound.first, bound.second)).requireJob()
    }

    suspend fun runNow(id: String, version: Long, requestId: String, expectedSessionId: String? = null): JobRunResponse {
        requireId(id)
        val body = actionRequest(version, requestId); val bound = bind(expectedSessionId)
        return (if (bound == null) api.runJob(id, body) else api.runJobReviewed(id, body, bound.first, bound.second)).requireReceipt()
    }

    suspend fun runAction(id: String, runId: String, action: String, version: Long, requestId: String, expectedSessionId: String? = null): JobRunResponse {
        requireId(id)
        requireId(runId)
        require(action in RUN_ACTIONS) { "Unsupported run action." }
        val body = actionRequest(version, requestId); val bound = bind(expectedSessionId)
        return (if (bound == null) api.jobRunAction(id, runId, action, body) else api.jobRunActionReviewed(id, runId, action, body, bound.first, bound.second)).requireReceipt()
    }

    suspend fun history(id: String, cursor: String? = null, after: String? = null): JobHistoryResponse {
        requireId(id)
        require(cursor == null || after == null)
        val response = api.jobHistory(id, cursor, after, PAGE_SIZE)
        if (response.entries == null || response.entries.any { it.jobId != id || it.id.isBlank() }) throw IOException("Unverified job history response.")
        return response
    }

    suspend fun historyPage(scope: JobHistoryScope, cursor: String?, direction: HistoryDirection): JobHistoryPage {
        val bound = bind(scope.tenantId) ?: throw JobsSessionException()
        requireId(scope.jobId)
        val response = api.jobHistory(scope.jobId, cursor.takeIf { direction == HistoryDirection.OLDER }, cursor.takeIf { direction == HistoryDirection.LATEST }, PAGE_SIZE, bound.first, bound.second)
        if (response.entries == null || response.entries.any { it.jobId != scope.jobId || it.id.isBlank() }) throw IOException("Unverified job history response.")
        check(tokenStore?.session()?.sessionId == scope.tenantId) { "The signed-in session changed." }
        return JobHistoryPage(scope, response.entries.orEmpty().map { entry ->
            val text = buildList {
                entry.text?.takeIf { it.isNotBlank() }?.let(::add)
                entry.run?.title?.takeIf { it.isNotBlank() && it != entry.text }?.let(::add)
                entry.run?.instructions?.takeIf { it.isNotBlank() && it != entry.text }?.let(::add)
                entry.run?.progress?.takeIf { it.isNotBlank() }?.let(::add)
                entry.run?.result?.takeIf { it.isNotBlank() }?.let(::add)
                entry.run?.error?.takeIf { it.isNotBlank() }?.let(::add)
            }.joinToString("\n\n")
            JobHistoryEntry(entry.id, entry.sequence, entry.role, text, entry.createdAt, entry.kind, entry.status)
        }, response.olderCursor, response.newerCursor, HistoryRetention.PARTIAL, response.retentionBoundary, direction == HistoryDirection.LATEST && cursor != null && response.hasMore)
    }

    suspend fun note(id: String, text: String, version: Long, requestId: String, runId: String? = null, expectedSessionId: String? = null): JobCommandResponse {
        requireId(id); requireVersion(version); requireRequestId(requestId)
        require(text.isNotBlank() && text.toByteArray(Charsets.UTF_8).size <= 2000)
        if (runId != null) requireId(runId)
        val bound = bind(expectedSessionId)
        return api.jobCommand(id, JobCommandRequest(kind = "note", text = text, runId = runId, expectedVersion = version, requestId = requestId), bound?.first, bound?.second).requireCommand(id)
    }

    /** Called only from a trusted review click; the captured bearer cannot change account on retry. */
    suspend fun approveProposal(proposal: JobsVoiceProposal, requestId: String): JobResponse {
        requireRequestId(requestId)
        val binding = bind(proposal.sessionId) ?: throw JobsSessionException()
        val bearer = binding.first
        val bound = binding.second
        if (proposal.operation == "job_create") return api.createJobReviewed(saveRequest(requireNotNull(proposal.input), requestId), bearer, bound).requireJob()
        val id = requireNotNull(proposal.jobId); requireId(id); requireVersion(proposal.expectedVersion)
        val body = actionRequest(proposal.expectedVersion, requestId)
        val job = when (proposal.operation) {
            "job_start" -> api.runJobReviewed(id, body, bearer, bound).requireReceipt().job
            "job_pause", "job_resume", "job_cancel" -> api.jobActionReviewed(id, proposal.operation.removePrefix("job_"), body, bearer, bound).requireJob().job
            "job_retry" -> { val runId = requireNotNull(proposal.runId); requireId(runId); api.retryJobRunReviewed(id, runId, body, bearer, bound).requireReceipt().job }
            "job_command" -> api.jobCommand(id, JobCommandRequest(kind = "note", text = requireNotNull(proposal.text), runId = proposal.runId, expectedVersion = proposal.expectedVersion, requestId = requestId), bearer, bound).requireCommand(id).job
            else -> throw IllegalArgumentException("Unsupported reviewed operation.")
        }
        return JobResponse(job).requireJob()
    }

    private fun JobCommandResponse.requireCommand(id: String): JobCommandResponse {
        if (command == null || command.id.isBlank() || command.jobId != id || command.kind != "note" || command.status != "recorded" || job == null || job.id != id || job.version < 1) throw IOException("The note receipt could not be verified.")
        return this
    }

    private suspend fun bind(expectedSessionId: String?): Pair<String, AuthBoundRequest>? {
        // The bare-api constructor is a hermetic test seam. Hilt supplies both
        // session dependencies in the app; production callers must name the UI session.
        if (tokenStore == null) { check(expectedSessionId == null); return null }
        if (expectedSessionId.isNullOrBlank()) throw JobsSessionException()
        val session = boundSessions?.credentials(expectedSessionId) ?: tokenStore.session() ?: throw JobsSessionException()
        if (session.sessionId != expectedSessionId) throw JobsSessionException()
        return "Bearer ${session.accessToken}" to AuthBoundRequest()
    }

    private fun saveRequest(input: JobInputDto, requestId: String, version: Long? = null): JobSaveRequest {
        requireRequestId(requestId)
        require(input.kind == "reminder" || input.kind == "review") { "This execution provider is not connected to Jobs." }
        require(input.title.isNotBlank() && input.title.toByteArray(Charsets.UTF_8).size <= 160) { "A title of up to 160 UTF-8 bytes is required." }
        require(input.instructions.toByteArray(Charsets.UTF_8).size <= 2000) { "Instructions must be at most 2000 UTF-8 bytes." }
        // Preserve the exact submitted content. The server is the schedule and
        // authorization authority; never infer account, status or approval.
        return JobSaveRequest(input.title, input.instructions, input.kind, input.schedule, requestId, version)
    }

    private fun actionRequest(version: Long, requestId: String): JobActionRequest {
        requireVersion(version)
        requireRequestId(requestId)
        return JobActionRequest(version, requestId)
    }

    private fun JobResponse.requireJob(): JobResponse {
        if (job == null || job.id.isBlank() || job.version < 1) {
            throw IOException("The server did not return a saved job. Refresh Jobs before retrying.")
        }
        return this
    }

    private fun JobRunResponse.requireReceipt(): JobRunResponse {
        if (run == null || run.id.isBlank() || run.status.isBlank() || job == null || job.id.isBlank() || job.version < 1 || run.jobId != job.id) {
            throw IOException("The server did not return a verified run receipt. Refresh Jobs before retrying.")
        }
        return this
    }

    companion object {
        const val PAGE_SIZE = 30
        private val ID = Regex("[A-Za-z0-9_-]{1,128}")
        private val JOB_ACTIONS = setOf("pause", "resume", "cancel")
        private val RUN_ACTIONS = setOf("retry", "approve", "cancel")

        fun newRequestId(): String = UUID.randomUUID().toString()
        private fun requireId(id: String) = require(ID.matches(id)) { "Invalid Jobs identifier." }
        private fun requireVersion(version: Long) = require(version > 0) { "Refresh this job before making changes." }
        private fun requireRequestId(requestId: String) = require(requestId.toByteArray(Charsets.UTF_8).size in 8..128) { "A stable request ID of 8 through 128 bytes is required." }
    }
}
