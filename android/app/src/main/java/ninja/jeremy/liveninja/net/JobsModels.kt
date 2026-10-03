package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/** Wire contracts shared by the web Jobs API and native Android Jobs. */
@Serializable
data class JobScheduleDto(
    val kind: String = "once",
    val at: String? = null,
    val timezone: String? = null,
    val time: String? = null,
    val weekday: Int? = null,
)

/** Editable fields only: identity, version, status and authority come from the server. */
@Serializable
data class JobInputDto(
    val title: String,
    val instructions: String = "",
    val kind: String = "reminder",
    val schedule: JobScheduleDto = JobScheduleDto(),
)

@Serializable
data class JobDto(
    val id: String = "",
    val title: String = "",
    val instructions: String = "",
    val kind: String = "",
    val schedule: JobScheduleDto = JobScheduleDto(),
    val status: String = "",
    val version: Long = 0,
    val nextRunAt: String? = null,
    val createdAt: String = "",
    val updatedAt: String = "",
    val lastRun: JobRunDto? = null,
    val error: String? = null,
)

@Serializable
data class JobRunDto(
    val id: String = "",
    val jobId: String = "",
    val status: String = "",
    val progress: String = "",
    val result: String? = null,
    val error: String? = null,
    val attempt: Int = 0,
    val title: String = "",
    val instructions: String = "",
    val kind: String = "",
    val createdAt: String = "",
    val updatedAt: String = "",
    val finishedAt: String? = null,
    val approvalExpiresAt: String? = null,
    val approvedBy: String? = null,
    val scheduledFor: String? = null,
    val generation: Long = 0,
    val retryOf: String? = null,
)

@Serializable
data class JobProviderDto(
    val id: String = "",
    val label: String = "",
    val available: Boolean? = null,
    val reason: String? = null,
)

/** Missing capabilities remain unknown. Never infer a connected provider. */
@Serializable
data class JobsCapabilitiesDto(
    val reminder: Boolean? = null,
    val review: Boolean? = null,
    val scheduling: Boolean? = null,
    val historyLimit: Int? = null,
    val providers: List<JobProviderDto>? = null,
)

@Serializable
data class JobsListResponse(
    val jobs: List<JobDto>? = null,
    val nextCursor: String? = null,
    val capabilities: JobsCapabilitiesDto? = null,
)

@Serializable
data class JobRunsResponse(
    val runs: List<JobRunDto>? = null,
    val nextCursor: String? = null,
)

@Serializable
data class JobResponse(val job: JobDto? = null)

@Serializable
data class JobRunResponse(val run: JobRunDto? = null, val job: JobDto? = null)

/** Flat body: the backend intentionally rejects nested input or authority fields. */
@Serializable
data class JobSaveRequest(
    val title: String,
    val instructions: String,
    val kind: String,
    val schedule: JobScheduleDto,
    val requestId: String,
    val expectedVersion: Long? = null,
)

@Serializable
data class JobActionRequest(val expectedVersion: Long, val requestId: String)
