package ninja.jeremy.liveninja.ui.history

import java.time.Instant
import javax.inject.Inject
import javax.inject.Singleton
import ninja.jeremy.liveninja.net.ConversationDetailDto
import ninja.jeremy.liveninja.net.ConversationListResponse
import ninja.jeremy.liveninja.net.DeviceListResponse
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.TopicListResponse

/**
 * Active history filters (FR-TOP-04/05). All facets optional; the repository
 * turns them into the `GET /api/v1/conversations` query params (Query-only
 * server side, never Scan).
 */
data class HistoryFilters(
    /** Stable topicIds from the multi-select (comma-joined on the wire). */
    val topicIds: Set<String> = emptySet(),
    val deviceId: String? = null,
    /** Inclusive local-day range from the date-range picker (UTC millis). */
    val fromMillis: Long? = null,
    val toMillis: Long? = null,
) {
    val isActive: Boolean
        get() = topicIds.isNotEmpty() || deviceId != null || fromMillis != null || toMillis != null
}

/**
 * Android access to the M11 filterable conversation history
 * (contracts/api.md "Conversation Topics & Filterable History") plus the
 * topic taxonomy and device directory that populate the filter sheet —
 * pickers are populated from these, never typed blind.
 */
@Singleton
class HistoryRepository @Inject constructor(
    private val api: LiveNinjaApi,
) {
    suspend fun listConversations(
        filters: HistoryFilters,
        cursor: String? = null,
    ): ConversationListResponse = api.listConversations(
        topic = filters.topicIds.takeIf { it.isNotEmpty() }?.sorted()?.joinToString(","),
        device = filters.deviceId,
        from = filters.fromMillis?.let { Instant.ofEpochMilli(it).toString() },
        // The picker returns start-of-day UTC millis; widen `to` to the end of
        // that day so the range is inclusive of conversations on the end date.
        to = filters.toMillis?.let { Instant.ofEpochMilli(it + END_OF_DAY_MS).toString() },
        cursor = cursor,
        limit = PAGE_SIZE,
    )

    suspend fun getConversation(id: String): ConversationDetailDto = api.getConversation(id)

    suspend fun listTopics(): TopicListResponse = api.listTopics()

    suspend fun listDevices(): DeviceListResponse = api.listDevices()

    companion object {
        const val PAGE_SIZE = 50
        private const val END_OF_DAY_MS = 86_399_999L
    }
}
