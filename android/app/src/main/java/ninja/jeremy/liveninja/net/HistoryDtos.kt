package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Wire DTOs for the M11 Conversation Topics & Filterable History REST surface
 * (contracts/api.md "Conversation Topics & Filterable History") plus the
 * device directory (`GET /v1/devices`) that populates the history device
 * filter.
 *
 * Same leniency convention as [DeliverableDto]/[EntityDto]: id-adjacent
 * fields carry alias names and everything else is optional-with-defaults,
 * because the backend workstream lands in parallel from the same M11 locked
 * decisions (CONV#<ts>#<sessionId> canonical items + TREF# per-topic refs +
 * TOPIC# taxonomy, Query-only).
 */

/** One conversation index row (DynamoDB CONV# item projection). */
@Serializable
data class ConversationDto(
    val id: String? = null,
    val conversationId: String? = null,
    val sessionId: String? = null,
    /** ISO-8601 (or epoch) session start — the CONV# sort-key timestamp. */
    val startedAt: String? = null,
    val ts: String? = null,
    val createdAt: String? = null,
    val deviceId: String? = null,
    val deviceName: String? = null,
    /** Voice engine that ran the session (openai-realtime | nova-sonic | ...). */
    val engine: String? = null,
    val durationSec: Long? = null,
    val durationSeconds: Long? = null,
    val summary: String? = null,
    val title: String? = null,
    /** Stable topic ids assigned by the post-session extractor (FR-TOP-01). */
    val topicIds: List<String> = emptyList(),
    val turnCount: Int? = null,
) {
    val conversationKey: String? get() = id ?: conversationId ?: sessionId
    val startTimestamp: String? get() = startedAt ?: ts ?: createdAt
    val displayTitle: String? get() = title ?: summary
    val duration: Long? get() = durationSec ?: durationSeconds
}

/** GET /api/v1/conversations response: one Query page + continuation cursor. */
@Serializable
data class ConversationListResponse(
    val items: List<ConversationDto> = emptyList(),
    val nextCursor: String? = null,
)

/** One transcript turn inside a conversation detail. */
@Serializable
data class TranscriptTurnDto(
    val role: String? = null,
    val text: String? = null,
    val content: String? = null,
    val ts: String? = null,
) {
    val displayText: String get() = text ?: content ?: ""
    val isUser: Boolean get() = role.equals("user", ignoreCase = true)
}

/**
 * GET /api/v1/conversations/{id} response: the CONV# record plus its
 * transcript turns (`turns` or `transcript`, whichever name the backend
 * finalized — both parsed).
 */
@Serializable
data class ConversationDetailDto(
    val id: String? = null,
    val conversationId: String? = null,
    val sessionId: String? = null,
    val startedAt: String? = null,
    val ts: String? = null,
    val createdAt: String? = null,
    val deviceId: String? = null,
    val deviceName: String? = null,
    val engine: String? = null,
    val durationSec: Long? = null,
    val durationSeconds: Long? = null,
    val summary: String? = null,
    val title: String? = null,
    val topicIds: List<String> = emptyList(),
    val turns: List<TranscriptTurnDto> = emptyList(),
    val transcript: List<TranscriptTurnDto> = emptyList(),
) {
    val conversationKey: String? get() = id ?: conversationId ?: sessionId
    val startTimestamp: String? get() = startedAt ?: ts ?: createdAt
    val displayTitle: String? get() = title ?: summary
    val duration: Long? get() = durationSec ?: durationSeconds
    val allTurns: List<TranscriptTurnDto> get() = turns.ifEmpty { transcript }
}

/** One topic taxonomy row (DynamoDB TOPIC# item, FR-TOP-02). */
@Serializable
data class TopicDto(
    val id: String? = null,
    val topicId: String? = null,
    val name: String? = null,
    /** Hex color (#RRGGBB) chosen in the Topic Manager. */
    val color: String? = null,
    val archived: Boolean = false,
    /** Non-null when this topic was merged into another (stable-id merge). */
    val mergedInto: String? = null,
    val convCount: Long? = null,
) {
    val topicKey: String? get() = id ?: topicId
    val displayName: String get() = name ?: topicKey.orEmpty()
}

/** GET /api/v1/topics response. */
@Serializable
data class TopicListResponse(
    val items: List<TopicDto> = emptyList(),
)

/** One registered device (GET /v1/devices — Android installs, M5Stack units, web). */
@Serializable
data class DeviceDto(
    val id: String? = null,
    val deviceId: String? = null,
    val name: String? = null,
    val label: String? = null,
    /** Surface hint: android | web | m5stack | ... */
    val platform: String? = null,
    val type: String? = null,
    val lastSeenAt: String? = null,
) {
    val deviceKey: String? get() = id ?: deviceId
    val displayName: String get() = name ?: label ?: platform ?: type ?: deviceKey.orEmpty()
}

/** GET /api/v1/devices response. */
@Serializable
data class DeviceListResponse(
    val items: List<DeviceDto> = emptyList(),
)
