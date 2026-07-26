package ninja.jeremy.liveninja.ui.settings

import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import ninja.jeremy.liveninja.auth.DeviceIdentityStore
import ninja.jeremy.liveninja.auth.DeviceRegistrationManager
import ninja.jeremy.liveninja.net.DeviceDto
import ninja.jeremy.liveninja.net.DeviceRenameRequest
import ninja.jeremy.liveninja.net.LiveNinjaApi
import ninja.jeremy.liveninja.net.SettingsSectionEnvelope
import ninja.jeremy.liveninja.net.SettingsSectionPatchRequest
import ninja.jeremy.liveninja.net.SettingsTargetRequest
import org.json.JSONObject

enum class SettingsTargetMode(val wireValue: String) {
    CURRENT("current"),
    SELECTED("selected"),
    ALL("all"),
}

data class SettingsApplyTarget(
    val mode: SettingsTargetMode,
    val deviceIds: Set<String> = emptySet(),
)

@Singleton
class SettingsRepository @Inject constructor(
    private val api: LiveNinjaApi,
    private val identity: DeviceIdentityStore,
    private val deviceRegistration: DeviceRegistrationManager,
    private val json: Json,
) {
    val currentDeviceId: String get() = identity.deviceId
    val migrationComplete: Boolean get() = identity.settingsMigrationComplete
    val pendingSettingsSections: Set<String> get() = identity.pendingSettingsSections

    suspend fun registerCurrentDevice(): DeviceDto =
        deviceRegistration.registerCurrentDevice()

    suspend fun listDevices(): List<DeviceDto> = api.listDevices().resolvedItems

    suspend fun renameDevice(deviceId: String, name: String): DeviceDto =
        api.renameDevice(deviceId, DeviceRenameRequest(name.trim())).device

    suspend fun getEffectiveSettings(): JSONObject =
        api.getEffectiveSettings().toJSONObject()

    suspend fun getSection(section: SettingsSection): SettingsSectionEnvelope =
        api.getSettingsSection(requireNotNull(section.apiId))

    suspend fun patchSection(
        section: SettingsSection,
        version: Int,
        operation: String,
        target: SettingsApplyTarget,
        settings: JSONObject,
    ): SettingsSectionEnvelope =
        api.patchSettingsSection(
            requireNotNull(section.apiId),
            SettingsSectionPatchRequest(
                version = version,
                operation = operation,
                target = SettingsTargetRequest(
                    mode = target.mode.wireValue,
                    deviceIds = target.deviceIds.sorted(),
                ),
                settings = settings.toJsonObject(),
            ),
        )

    fun markMigrationComplete() {
        identity.settingsMigrationComplete = true
    }

    fun markSectionPending(section: SettingsSection) {
        identity.markSettingsSectionPending(requireNotNull(section.apiId))
    }

    fun clearSectionPending(section: SettingsSection) {
        identity.clearSettingsSectionPending(requireNotNull(section.apiId))
    }

    fun clearPendingSections() {
        identity.clearPendingSettingsSections()
    }

    private fun JsonObject.toJSONObject(): JSONObject = JSONObject(toString())

    private fun JSONObject.toJsonObject(): JsonObject =
        json.parseToJsonElement(toString()).jsonObject
}
