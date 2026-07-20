package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Wire DTOs for `GET /api/v1/realtime/voices` (internal/webapp/
 * settings_routes.go handleListVoices): the static voice catalogs backing the
 * Settings pickers. The response is additive — `voices` is the OpenAI
 * Realtime set, `accents` the accent-directive catalog, and `geminiVoices`
 * (M13) the spike-validated Gemini Live prebuilt-HD set shown when the
 * engine selection is `gemini-flash-live`.
 */

/** One selectable voice (internal/realtime/catalog.go VoiceInfo). */
@Serializable
data class VoiceInfoDto(
    val id: String,
    val name: String? = null,
    val description: String? = null,
    /** Perceived gender presentation tag ("female" | "male" | "neutral"). */
    val gender: String? = null,
    val default: Boolean = false,
)

/** One selectable accent (internal/realtime/catalog.go AccentInfo). */
@Serializable
data class AccentInfoDto(
    val id: String,
    val label: String? = null,
)

/** GET /api/v1/realtime/voices response. */
@Serializable
data class VoiceCatalogResponse(
    val voices: List<VoiceInfoDto> = emptyList(),
    val accents: List<AccentInfoDto> = emptyList(),
    val geminiVoices: List<VoiceInfoDto> = emptyList(),
)
