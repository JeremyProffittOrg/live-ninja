package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable

/**
 * Wire DTOs for `GET /api/v1/realtime/personas` (internal/webapp/
 * settings_routes.go handleListPersonas -> internal/realtime/catalog.go
 * ListPersonas).
 *
 * Only the client-visible slice crosses the wire: the persona's instruction
 * text NEVER leaves the server (personas.go anti-injection rule — clients
 * reference personas by ID and the broker resolves them), so there is
 * deliberately no `instructions` field to deserialize here.
 */

/** One persona catalog entry (internal/realtime/catalog.go PersonaInfo). */
@Serializable
data class PersonaInfoDto(
    val id: String,
    val name: String? = null,
    val description: String? = null,
    /**
     * Picker section — "General" | "PDLC" | "ESP32" | "Fun". Additive
     * (2026-08-01); a server that predates it omits the field, and an unknown
     * value from a newer server still renders under its own heading rather
     * than vanishing, so the default here is empty rather than "General".
     */
    val group: String = "",
)

/** GET /api/v1/realtime/personas response. */
@Serializable
data class PersonaCatalogResponse(
    val personas: List<PersonaInfoDto> = emptyList(),
)
