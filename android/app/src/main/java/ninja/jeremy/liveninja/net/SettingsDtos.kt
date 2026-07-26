package ninja.jeremy.liveninja.net

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonObject

/** One device's effective values for a single Settings accordion section. */
@Serializable
data class DeviceSectionSettingsDto(
    val deviceId: String,
    val name: String,
    val surface: String? = null,
    val metadata: JsonObject = JsonObject(emptyMap()),
    val capabilities: List<String> = emptyList(),
    val isCurrent: Boolean = false,
    val inherited: Boolean = true,
    val settings: JsonObject = JsonObject(emptyMap()),
)

/**
 * GET/PATCH /api/v1/settings/sections/{section}. The version is the canonical
 * document's optimistic-concurrency version, shared by every section.
 */
@Serializable
data class SettingsSectionEnvelope(
    val section: String,
    val version: Int,
    val currentDeviceId: String? = null,
    val accountDefaults: JsonObject = JsonObject(emptyMap()),
    val devices: List<DeviceSectionSettingsDto> = emptyList(),
)

@Serializable
data class SettingsTargetRequest(
    /** current | selected | all */
    val mode: String,
    val deviceIds: List<String> = emptyList(),
)

@Serializable
data class SettingsSectionPatchRequest(
    val version: Int,
    /** set | inherit */
    val operation: String,
    val target: SettingsTargetRequest,
    val settings: JsonObject = JsonObject(emptyMap()),
)

@Serializable
data class DeviceRenameRequest(
    val name: String,
)
