package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement

/** Canonical Ghost names are retained; session IDs are never inferred from scheduled jobs. */
@Serializable data class GhostNodeDto(val node_id: String = "", val state: String? = null, val status: String? = null, val agent_version: String? = null, val last_seen: JsonElement? = null, val connected: Boolean? = null)
@Serializable data class GhostNodesResponse(val nodes: List<GhostNodeDto>? = null, val source: String = "", val binding: String = "")
@Serializable data class GhostRunSummaryDto(val run_id: String = "", val status: String? = null, val ts: JsonElement? = null, val node: String? = null, val trigger: String? = null, val output_key: String? = null, val summary: String? = null)
@Serializable data class GhostScheduledEventDto(
    val event_id: String = "", val node: String? = null, val repo: String? = null, val cli: String? = null,
    val model: String? = null, val effort: String? = null, val prompt: String? = null,
    val backup_node: String? = null, val output_file: String? = null, val cron: String? = null,
    val run_at: JsonElement? = null, val timezone: String? = null, val enabled: Boolean? = null,
    val archived: Boolean? = null, val deploy: Boolean? = null, val created_at: JsonElement? = null,
    val next_run: JsonElement? = null, val last_run_id: String? = null, val last_run_status: String? = null,
    val last_run_ts: JsonElement? = null, val last_run_node: String? = null, val runs: List<GhostRunSummaryDto>? = null,
)
@Serializable data class GhostJobsResponse(val events: List<GhostScheduledEventDto>? = null, val source: String = "", val runHistoryLimit: Int? = null, val providerSessionBindingAvailable: Boolean? = null)
@Serializable data class GhostSessionDto(val session_id: String = "", val date: String? = null)
@Serializable data class GhostSessionsResponse(val version: Int = 0, val node_id: String = "", val sessions: List<GhostSessionDto>? = null, val next_cursor: String? = null, val coverage: String = "")
@Serializable data class GhostHistoryEventDto(
    val id: String = "", val sequence: String = "", val timestamp: String? = null,
    val kind: String = "", val text: String = "", val call_id: String? = null,
    val name: String? = null, val part: Int = 0, val more: Boolean = false,
)
@Serializable data class GhostEventsResponse(
    val version: Int = 0, val node_id: String = "", val session_id: String = "",
    val events: List<GhostHistoryEventDto>? = null, val next_cursor: String? = null,
    val resume_cursor: String? = null, val coverage: String = "", val gaps: List<JsonElement>? = null,
)
