package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable
import kotlinx.serialization.SerialName

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
    @SerialName("deliverableId")
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
    @SerialName("deliverables")
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

/** Signed direct-to-S3 upload request used by device-local camera tools. */
@Serializable
data class MediaUploadIntentRequest(
    val name: String,
    val contentType: String,
    val sizeBytes: Long,
)

/**
 * The backend-authenticated part of a camera upload. [uploadUrl] is then used
 * with the credential-free OkHttp client so the app never forwards its Live
 * Ninja Bearer token to S3. Every entry in [headers] is part of the signature
 * and must be sent unchanged with the PUT.
 */
@Serializable
data class MediaUploadIntentResponse(
    val deliverableId: String,
    val name: String,
    val status: String,
    val contentType: String,
    val sizeBytes: Long,
    val uploadUrl: String,
    val expiresAt: String? = null,
    val headers: Map<String, String> = emptyMap(),
)

/** Server-verified completion after S3 HEAD confirms signed size and MIME. */
@Serializable
data class MediaUploadCompleteResponse(
    val deliverableId: String,
    val name: String,
    val status: String,
    val contentType: String,
    val sizeBytes: Long,
    val createdAt: String? = null,
)
