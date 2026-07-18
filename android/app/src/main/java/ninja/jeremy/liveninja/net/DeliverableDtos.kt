package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Wire DTOs for the M9 Deliverables Store REST surface (contracts/api.md
 * "Deliverables Store"): list / zip / delete. The download route returns a
 * presigned redirect, not JSON — it is resolved in
 * [ninja.jeremy.liveninja.ui.files.DeliverablesRepository] with a
 * non-redirect-following client so the presigned S3 URL itself (query-string
 * auth) is handed to DownloadManager without our Bearer header ever reaching
 * S3.
 *
 * Fields besides `id` are optional-with-defaults on purpose: the backend
 * workstream lands in parallel from the same M9 locked decisions
 * (items under pk=USER#<uid> sk=DELIV#<createdAt>#<id>), and
 * `ignoreUnknownKeys` + lenient fields keep the tab rendering across additive
 * shape evolution instead of hard-crashing the list.
 */

/** One deliverable index item (DynamoDB DELIV# item projection). */
@Serializable
data class DeliverableDto(
    val id: String,
    /** Display/file name; backend may call it `name` or `filename`. */
    val name: String? = null,
    val filename: String? = null,
    val contentType: String? = null,
    val sizeBytes: Long? = null,
    /** ISO-8601 creation timestamp (sort key component). */
    val createdAt: String? = null,
    /** Async zips surface as pending items until the zipper Lambda finishes. */
    val status: String? = null,
) {
    val displayName: String get() = name ?: filename ?: id
}

/** GET /api/v1/deliverables response: one Query page + continuation cursor. */
@Serializable
data class DeliverableListResponse(
    val items: List<DeliverableDto> = emptyList(),
    val nextCursor: String? = null,
)

/** POST /api/v1/deliverables/zip request: bundle these deliverables into one ZIP. */
@Serializable
data class DeliverableZipRequest(
    val ids: List<String>,
    val name: String? = null,
)

/**
 * POST /api/v1/deliverables/zip response. The zipper Lambda is invoked
 * async (M9 locked decision), so the new ZIP deliverable may come back
 * `pending`; the Files list shows it once refreshed.
 */
@Serializable
data class DeliverableZipResponse(
    val id: String? = null,
    val status: String? = null,
)

/** DELETE /api/v1/deliverables/{id} acknowledges with {"ok": true} (or 204). */
@Serializable
data class DeliverableAck(
    val ok: Boolean = true,
)
