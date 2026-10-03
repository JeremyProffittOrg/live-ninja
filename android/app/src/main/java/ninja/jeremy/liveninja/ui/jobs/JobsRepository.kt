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

/**
 * Uses the app's existing authenticated Retrofit stack. Mutations are performed
 * exactly once here: HTTP, transport and cancellation failures propagate.
 *
 * The caller creates a request ID when the user confirms an immutable action,
 * retains that same ID/body/version on an explicit retry after a lost response,
 * and discards it after success, a definitive conflict, or an edited intent.
 * A 409 must prompt a refresh and new confirmation; this repository never
 * silently refreshes the version, changes the intent, or repeats an action.
 * The shared 401 authenticator may replay the SAME serialized request body.
 */
@Singleton
class JobsRepository @Inject constructor(private val api: LiveNinjaApi) {
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

    suspend fun create(input: JobInputDto, requestId: String): JobResponse =
        api.createJob(saveRequest(input, requestId)).requireJob()

    suspend fun update(id: String, input: JobInputDto, version: Long, requestId: String): JobResponse {
        requireId(id)
        requireVersion(version)
        return api.updateJob(id, saveRequest(input, requestId, version)).requireJob()
    }

    suspend fun jobAction(id: String, action: String, version: Long, requestId: String): JobResponse {
        requireId(id)
        require(action in JOB_ACTIONS) { "Unsupported job action." }
        return api.jobAction(id, action, actionRequest(version, requestId)).requireJob()
    }

    suspend fun runNow(id: String, version: Long, requestId: String): JobRunResponse {
        requireId(id)
        return api.runJob(id, actionRequest(version, requestId)).requireReceipt()
    }

    suspend fun runAction(id: String, runId: String, action: String, version: Long, requestId: String): JobRunResponse {
        requireId(id)
        requireId(runId)
        require(action in RUN_ACTIONS) { "Unsupported run action." }
        return api.jobRunAction(id, runId, action, actionRequest(version, requestId)).requireReceipt()
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
