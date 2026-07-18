package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonPrimitive

/**
 * Wire DTOs for the M10 Memory Layer + Guide Entities REST surface
 * (contracts/api.md "Memory Layer & Guide Entities"): entity list (memory
 * browser, FR-MEM-05), forget (`DELETE /v1/memory/{id}` — removes the entity
 * from DynamoDB *and* its embedding), and the Guide Entities list/edit
 * (FR-MEM-07/09).
 *
 * Same leniency convention as [DeliverableDto]: everything besides the id is
 * optional-with-defaults and common alias field names are all declared,
 * because the backend workstream lands in parallel from the same M10 locked
 * decisions (items under pk=USER#<uid> sk=ENT#<type>#<entityId> /
 * GUIDE#<guideId>); `ignoreUnknownKeys` + accessor fallbacks keep the screens
 * rendering across additive shape evolution.
 */

/** One memory entity (DynamoDB ENT# item: person|place|info|project|task|plan). */
@Serializable
data class EntityDto(
    val id: String? = null,
    val entityId: String? = null,
    /** Entity type discriminator from the sort key (ENT#<type>#<id>). */
    val type: String? = null,
    val entityType: String? = null,
    val name: String? = null,
    /** Free-form attribute map; values may be strings, numbers, or nested. */
    val attrs: Map<String, JsonElement>? = null,
    val relations: List<EntityRelationDto>? = null,
    val updatedAt: String? = null,
) {
    val entityKey: String? get() = id ?: entityId
    val displayType: String get() = (type ?: entityType).orEmpty()
    val displayName: String get() = name ?: entityKey.orEmpty()

    /** attrs flattened to display strings (primitives unquoted, rest as JSON). */
    val attrLines: List<Pair<String, String>>
        get() = attrs.orEmpty().map { (k, v) ->
            k to if (v is JsonPrimitive) v.content else v.toString()
        }
}

/** One relation edge on an entity ({type, targetId} per the M10 item shape). */
@Serializable
data class EntityRelationDto(
    val type: String? = null,
    val targetId: String? = null,
)

/** GET /api/v1/entities[?type=] response: one Query page + continuation cursor. */
@Serializable
data class EntityListResponse(
    val items: List<EntityDto> = emptyList(),
    val nextCursor: String? = null,
)

/** DELETE /api/v1/memory/{id} ("forget") acknowledges with {"ok": true} (or 204). */
@Serializable
data class MemoryAck(
    val ok: Boolean = true,
)

/** One Guide Entity (DynamoDB GUIDE# item, FR-MEM-07). */
@Serializable
data class GuideDto(
    val id: String? = null,
    val guideId: String? = null,
    val title: String? = null,
    /** Guide instruction text — contracts/api.md calls it `body`, the M10 item shape `text`. */
    val text: String? = null,
    val body: String? = null,
    val enabled: Boolean = true,
    /** Injection order: enabled guides are appended to persona instructions priority-ascending. */
    val priority: Int = 0,
    val version: Long? = null,
    val updatedAt: String? = null,
) {
    val guideKey: String? get() = id ?: guideId
    val displayTitle: String get() = title ?: guideKey.orEmpty()
    val displayText: String get() = text ?: body ?: ""
}

/** GET /api/v1/guides response. */
@Serializable
data class GuideListResponse(
    val items: List<GuideDto> = emptyList(),
)

/**
 * PUT /api/v1/guides/{id} body. PUT is replace semantics, so the full guide is
 * always sent (Android edits only `enabled`/`priority`, but resends
 * title/text). `text` and `body` both carry the guide text — the contract row
 * names the field `body` while the M10 locked item shape says `text`; sending
 * both keeps this client correct whichever the backend workstream finalized.
 * `version` rides along for the backend's versioned conditional write.
 */
@Serializable
data class GuidePutRequest(
    val title: String,
    val text: String,
    val body: String,
    val enabled: Boolean,
    val priority: Int,
    val version: Long? = null,
)
