package ninja.jeremy.liveninja.ui.jobs

import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.longOrNull
import ninja.jeremy.liveninja.auth.AuthRepository
import ninja.jeremy.liveninja.auth.AuthState
import ninja.jeremy.liveninja.net.JobDto
import ninja.jeremy.liveninja.net.JobInputDto
import ninja.jeremy.liveninja.net.JobRunDto

data class JobsVoiceProposal(
    val id: String, val sessionId: String, val operation: String, val receivedAt: Long,
    val input: JobInputDto? = null, val job: JobDto? = null,
    val jobId: String? = null, val expectedVersion: Long = 0,
    val runId: String? = null, val text: String? = null, val run: JobRunDto? = null,
)

/** Only ToolCallRouter offers authenticated backend results. Spoken/model text is never read. */
@Singleton
class JobsProposalInbox @Inject constructor(private val auth: AuthRepository) {
    private val mutablePending = MutableStateFlow<List<JobsVoiceProposal>>(emptyList())
    val pending: StateFlow<List<JobsVoiceProposal>> = mutablePending
    val authState: StateFlow<AuthState> get() = auth.state
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val json = Json { ignoreUnknownKeys = false }
    internal var now: () -> Long = System::currentTimeMillis

    init { scope.launch { auth.state.collect { state ->
        val session = (state as? AuthState.SignedIn)?.sessionId
        mutablePending.update { proposals -> proposals.filter { it.sessionId == session } }
    } } }

    fun captureSession(): String? = (auth.state.value as? AuthState.SignedIn)?.sessionId
    fun isCurrent(proposal: JobsVoiceProposal): Boolean = captureSession() == proposal.sessionId &&
        now() - proposal.receivedAt in 0..MAX_AGE && pending.value.any { it.id == proposal.id && it == proposal }
    fun dismiss(id: String) { mutablePending.update { proposals -> proposals.filterNot { it.id == id } } }

    fun offer(sessionId: String?, tool: String, callId: String, responseJson: String): Boolean {
        if (sessionId == null || sessionId != captureSession() || tool !in OPERATIONS || callId.isBlank()) return false
        val proposal = runCatching {
            val root = json.parseToJsonElement(responseJson).jsonObject
            require(root["tool"]?.jsonPrimitive?.content == tool && root["callId"]?.jsonPrimitive?.content == callId)
            require(root["ok"]?.jsonPrimitive?.booleanOrNull == false)
            val error = root["error"]!!.jsonObject
            require(error["code"]?.jsonPrimitive?.content == "confirmation_required")
            val details = error["details"]!!.jsonObject
            require(details["operation"]?.jsonPrimitive?.content == tool)
            require(details["executionAvailable"]?.jsonPrimitive?.booleanOrNull == true)
            val proposed = details["proposed"]!!.jsonObject
            if (tool == "job_create") {
                val input = json.decodeFromJsonElement(JobInputDto.serializer(), proposed)
                require(input.kind in setOf("reminder", "review"))
                JobsVoiceProposal(callId, sessionId, tool, now(), input = input)
            } else {
                val allowed = setOf("jobId", "expectedVersion", "runId", "kind", "text")
                require(proposed.keys.all { it in allowed })
                val id = proposed.string("jobId")!!
                val version = proposed["expectedVersion"]?.jsonPrimitive?.longOrNull ?: 0
                require(ID.matches(id) && version > 0)
                val job = Json { ignoreUnknownKeys = true }.decodeFromJsonElement(JobDto.serializer(), details["job"]!!)
                require(job.id == id && job.version == version)
                val runId = proposed.string("runId")
                if (runId != null) require(ID.matches(runId))
                val text = proposed.string("text")
                if (tool == "job_command") require(proposed.string("kind") == "note" && !text.isNullOrBlank() && text.toByteArray().size <= 2000)
                val run = details["run"]?.let { Json { ignoreUnknownKeys = true }.decodeFromJsonElement(JobRunDto.serializer(), it) }
                if (tool == "job_retry") require(runId != null && run?.id == runId && run.jobId == id)
                JobsVoiceProposal(callId, sessionId, tool, now(), job = job, jobId = id, expectedVersion = version, runId = runId, text = text, run = run)
            }
        }.getOrNull() ?: return false
        if (sessionId != captureSession()) return false
        mutablePending.update { current ->
            val kept = current.filter { it.sessionId == sessionId && now() - it.receivedAt in 0..MAX_AGE }
            if (kept.any { it.id == proposal.id } || kept.size >= 20) kept else kept + proposal
        }
        return pending.value.any { it == proposal }
    }

    companion object {
        const val MAX_AGE = 15 * 60 * 1000L
        private val ID = Regex("[A-Za-z0-9_-]{1,128}")
        private val OPERATIONS = setOf("job_create", "job_start", "job_cancel", "job_pause", "job_resume", "job_retry", "job_command")
        fun isProposalTool(tool: String) = tool in OPERATIONS
        private fun JsonObject.string(name: String) = this[name]?.jsonPrimitive?.contentOrNull
    }
}
