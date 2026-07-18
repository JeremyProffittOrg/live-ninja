package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Wire DTOs for the M6 custom wake-word training surface (contracts/api.md
 * "Wake-word"): `POST /api/v1/wakewords` creates an async AWS Batch training
 * job (openWakeWord only — Porcupine training is not offered server-side, per
 * the M6 locked decision; the engine field exists for additive evolution),
 * `GET /api/v1/wakewords/{id}` polls it.
 *
 * The WAKEWORD#<wwId> item tracks status pending|training|ready|failed; once
 * ready, `GET /v1/wakeword/{id}/model?platform=android` (ModelManager) serves
 * the SHA-256-pinned model manifest.
 */

/** POST /api/v1/wakewords request body. */
@Serializable
data class WakeWordCreateRequest(
    val phrase: String,
    val engine: String,
)

/** Training-job item returned by create + status poll. */
@Serializable
data class WakeWordJobDto(
    val id: String,
    val phrase: String? = null,
    val engine: String? = null,
    /** pending | training | ready | failed */
    val status: String? = null,
    val error: String? = null,
    val createdAt: String? = null,
)
